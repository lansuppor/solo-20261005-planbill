package main

import (
	"fmt"
	"sort"
)

// 账后操作共享计算（仅供 bill ledger 单账单流水与 bill reconcile 跨账期报表
// 使用；结算、账单展示、载入校验与账后登记的持久化行为不经过这里）。
//
// 调整、调整撤销、收款、分配更正、收款撤销与退款的“业务归属（影响哪些账单
// 月份）与金额影响”只在本文件推导一次：先把存档记录展开为客户维度、按全局
// 操作序号升序的中立事件流，再由两类查询分别做范围筛选与展示。
//
// 共同的业务口径：
//   - 流水按全局序号升序，以原总金额为初始应付、零实收起算；
//   - 更正按发生时的前后分配逐月计算（后值减前值），撤销取消撤销当时的最新
//     分配，退款只减少对应月份实收；
//   - 撤销在其发生序号抵消原操作，已撤销记录在撤销之前仍计入；
//   - 截止后的操作由查询方自行筛选，不能提前影响历史余额；
//   - 同名的调整、收款、更正、退款按类型关联，互不混淆。

const (
	postOpAdjust       = "调整"
	postOpRevokeAdjust = "撤销调整"
	postOpPayment      = "收款"
	postOpCorrect      = "更正"
	postOpRevokePay    = "撤销收款"
	postOpRefund       = "退款"
)

// postMonthDelta 是一次账后操作对单个月份的金额影响。deltaPayable/
// deltaReceived 为带符号的实际影响（撤销与退款已取负）；alloc 为展示用的
// 业务金额（收款登记分配、撤销取消的最新分配、退款该月额），beforeAlloc/
// afterAlloc 仅更正事件使用。中立流中同一月份不会前后分配均为零。
type postMonthDelta struct {
	month         string
	deltaPayable  int64
	deltaReceived int64
	alloc         int64
	beforeAlloc   int64
	afterAlloc    int64
}

// postOp 是一次账后操作的中立表示。调整类事件填 adjustID；收款相关事件填
// paymentID，更正与退款另填 correctionID/refundID。linkSeq 仅撤销类事件
// 非零（关联原操作序号）；payTotal 为收款类事件的汇款总额。
type postOp struct {
	seq          int64
	kind         string
	adjustID     string
	paymentID    string
	correctionID string
	refundID     string
	note         string
	linkSeq      int64
	payTotal     int64
	deltas       []postMonthDelta // 涉及月份，按月份升序
}

// refID 返回事件的原记录标识；同名标识在不同类型事件中各自独立。
func (op postOp) refID() string {
	switch op.kind {
	case postOpAdjust, postOpRevokeAdjust:
		return op.adjustID
	case postOpPayment, postOpRevokePay:
		return op.paymentID
	case postOpCorrect:
		return op.correctionID
	default: // 退款
		return op.refundID
	}
}

// postOpsForCustomer 把某客户的全部账后记录展开为按全局操作序号升序的中立
// 事件流，并做与具体查询无关的完整性核验：序号重复、撤销不晚于原操作、调整
// 金额无法取负都视为数据异常。载入校验已覆盖大部分约束，这里是两类查询共用
// 的最后防线。
func postOpsForCustomer(s *state, customerID string) ([]postOp, error) {
	var ops []postOp
	seen := make(map[int64]string) // 全局操作序号 -> 来源描述
	add := func(op postOp) error {
		desc := op.kind + " " + op.refID()
		if prev, dup := seen[op.seq]; dup {
			return fmt.Errorf("流水序号 %d 重复（%s 与 %s），数据异常，拒绝输出", op.seq, prev, desc)
		}
		seen[op.seq] = desc
		if op.linkSeq != 0 && op.linkSeq >= op.seq {
			return fmt.Errorf("%s 的撤销序号 %d 不晚于原操作序号 %d，数据异常，拒绝输出", desc, op.seq, op.linkSeq)
		}
		ops = append(ops, op)
		return nil
	}

	// 调整与其撤销：每个事件只影响所属账期的应付。
	for _, a := range s.Adjustments {
		if a.CustomerID != customerID {
			continue
		}
		if err := add(postOp{
			seq: a.Seq, kind: postOpAdjust, adjustID: a.ID, note: a.Reason,
			deltas: []postMonthDelta{{month: a.Month, deltaPayable: a.Amount}},
		}); err != nil {
			return nil, err
		}
		if a.Revoked {
			neg, err := neg64(a.Amount)
			if err != nil {
				return nil, fmt.Errorf("调整 %q 的金额 %d 分无法抵消（越界），数据异常，拒绝输出", a.ID, a.Amount)
			}
			if err := add(postOp{
				seq: a.RevokeSeq, kind: postOpRevokeAdjust, adjustID: a.ID,
				note: a.RevokeReason, linkSeq: a.Seq,
				deltas: []postMonthDelta{{month: a.Month, deltaPayable: neg}},
			}); err != nil {
				return nil, err
			}
		}
	}

	for _, p := range s.Payments {
		if p.CustomerID != customerID {
			continue
		}
		// 收款登记：多月汇款在中立流中保留全部月份分配，范围筛选交给查询方。
		if err := add(postOp{
			seq: p.Seq, kind: postOpPayment, paymentID: p.ID, note: p.Note,
			payTotal: p.Total, deltas: paymentDeltas(p.Allocations),
		}); err != nil {
			return nil, err
		}
		// 以登记分配为起点按序号回放更正：每次更正按当时的前后分配逐月给出
		// 差额；running 始终是该更正发生时生效（最新）的分配。
		running := p.Allocations
		for _, c := range correctionsFor(s, p.ID) {
			var ds []postMonthDelta
			for _, m := range unionMonths(running, c.Allocations) {
				before := allocAmountFor(running, m)
				after := allocAmountFor(c.Allocations, m)
				if before == 0 && after == 0 {
					continue // 前后均不涉及该月：不列更正
				}
				ds = append(ds, postMonthDelta{
					month: m, deltaReceived: after - before,
					beforeAlloc: before, afterAlloc: after,
				})
			}
			if err := add(postOp{
				seq: c.Seq, kind: postOpCorrect, paymentID: p.ID,
				correctionID: c.ID, note: c.Reason, deltas: ds,
			}); err != nil {
				return nil, err
			}
			running = c.Allocations
		}
		// 整笔撤销取消撤销发生时的最新分配（running 即此时最新分配）。
		if p.Revoked {
			if err := add(postOp{
				seq: p.RevokeSeq, kind: postOpRevokePay, paymentID: p.ID,
				note: p.RevokeReason, linkSeq: p.Seq, payTotal: p.Total,
				deltas: revokeDeltas(running),
			}); err != nil {
				return nil, err
			}
		}
		// 退款在其发生序号只减少对应月份的实收，不可撤销；跨月退款是一次
		// 操作、携带逐月变化，不按整笔总额重复累减。
		for _, r := range refundsFor(s, p.ID) {
			if err := add(postOp{
				seq: r.Seq, kind: postOpRefund, paymentID: p.ID,
				refundID: r.ID, note: r.Reason, deltas: refundDeltas(r.Allocations),
			}); err != nil {
				return nil, err
			}
		}
	}

	sort.Slice(ops, func(i, j int) bool { return ops[i].seq < ops[j].seq })
	return ops, nil
}

// paymentDeltas 把收款登记分配转为逐月实收影响（分配即实收增加额）。
func paymentDeltas(allocs []paymentAllocation) []postMonthDelta {
	ds := make([]postMonthDelta, 0, len(allocs))
	for _, al := range allocs {
		ds = append(ds, postMonthDelta{month: al.Month, deltaReceived: al.Amount, alloc: al.Amount})
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].month < ds[j].month })
	return ds
}

// revokeDeltas 把撤销当时的最新分配转为逐月实收影响（取消即实收减少）。
func revokeDeltas(allocs []paymentAllocation) []postMonthDelta {
	ds := make([]postMonthDelta, 0, len(allocs))
	for _, al := range allocs {
		ds = append(ds, postMonthDelta{month: al.Month, deltaReceived: -al.Amount, alloc: al.Amount})
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].month < ds[j].month })
	return ds
}

// refundDeltas 把一笔退款的各月退款额转为逐月实收影响。
func refundDeltas(allocs []paymentAllocation) []postMonthDelta {
	ds := make([]postMonthDelta, 0, len(allocs))
	for _, al := range allocs {
		ds = append(ds, postMonthDelta{month: al.Month, deltaReceived: -al.Amount, alloc: al.Amount})
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].month < ds[j].month })
	return ds
}

// deltaForMonth 返回事件在指定月份的逐月影响；该月不在事件涉及月份中时为 nil。
func (op postOp) deltaForMonth(month string) *postMonthDelta {
	for i := range op.deltas {
		if op.deltas[i].month == month {
			return &op.deltas[i]
		}
	}
	return nil
}

// ledgerEvent 是某张已结算账单账后流水中的一个事件：调整、撤销调整、收款、
// 分配更正、撤销收款或退款。它是共享中立事件流的单账单投影；事件按全局操作
// 序号升序回放，afterPayable/afterReceived 在逐步核验时填充。
type ledgerEvent struct {
	seq           int64  // 全局操作序号（调整/收款/更正/退款及其撤销共用）
	kind          string // 事件类型：调整 / 撤销调整 / 收款 / 更正 / 撤销收款 / 退款
	refID         string // 原记录标识（调整标识、收款标识、更正标识或退款标识）
	deltaPayable  int64  // 对当前应付的影响（分，带符号）
	deltaReceived int64  // 对实收的影响（分，带符号）
	note          string // 原因（调整/更正/退款/撤销类）或备注（收款类）
	linkSeq       int64  // 撤销事件关联的原操作序号；非撤销事件为 0
	payTotal      int64  // 收款类事件：汇款总额
	payAlloc      int64  // 收款类事件：本账单分配（撤销收款事件为被取消的最新分配；退款事件为本账单退款额）
	payID         string // 更正/退款事件：关联收款标识
	beforeAlloc   int64  // 更正事件：本账单更正前分配
	afterAlloc    int64  // 更正事件：本账单更正后分配
	afterPayable  int64  // 事件后的应付（回放时填充）
	afterReceived int64  // 事件后的实收（回放时填充）
}

// billLedger 把目标账单的调整、调整撤销、收款、分配更正、收款撤销与退款合并
// 为一条按操作序号升序的流水，并以原总金额为初始应付、零实收为起点逐步回放，
// 填充每个事件之后的三项余额。业务归属与金额影响取自 postOpsForCustomer 的
// 共享事件流，本函数只做单账单投影与逐步核验。
//
// 回放覆盖完整流水（不受查询截止序号限制）：序号重复、撤销先于原操作，或任
// 一事件之后不满足 0 ≤ 实收 ≤ 应付 ≤ 有符号 64 位最大值，都视为数据异常并
// 拒绝——只检查最终余额会放过中间越界的存档。曾涉及该账单的收款即使后来被
// 更正移出该月，其零金额的整笔撤销事件仍在流水中保留。只读：不修改库、不占
// 用序号、不新增记录。
func billLedger(s *state, b *bill) ([]ledgerEvent, error) {
	ops, err := postOpsForCustomer(s, b.CustomerID)
	if err != nil {
		return nil, err
	}
	return billLedgerFromOps(ops, b)
}

// billLedgerFromOps 从共享事件流投影单张账单并逐步回放核验。
func billLedgerFromOps(ops []postOp, b *bill) ([]ledgerEvent, error) {
	// 先找出历史上曾涉及本账单的收款（首次登记分配或任一次更正的前后分配
	// 涉及该月）：这些收款即使被更正全部移出，撤销时仍要在本账单流水里保留
	// 一条零金额撤销事件。
	everInvolved := make(map[string]bool)
	for _, op := range ops {
		switch op.kind {
		case postOpPayment:
			if d := op.deltaForMonth(b.Month); d != nil {
				everInvolved[op.paymentID] = true
			}
		case postOpCorrect:
			if d := op.deltaForMonth(b.Month); d != nil && (d.beforeAlloc > 0 || d.afterAlloc > 0) {
				everInvolved[op.paymentID] = true
			}
		}
	}

	var events []ledgerEvent
	for _, op := range ops {
		d := op.deltaForMonth(b.Month)
		switch op.kind {
		case postOpAdjust, postOpRevokeAdjust:
			if d == nil {
				continue
			}
			events = append(events, ledgerEvent{
				seq: op.seq, kind: op.kind, refID: op.refID(), note: op.note,
				linkSeq: op.linkSeq, deltaPayable: d.deltaPayable,
			})
		case postOpPayment:
			if d == nil {
				continue // 登记时未分配到本账单
			}
			events = append(events, ledgerEvent{
				seq: op.seq, kind: postOpPayment, refID: op.paymentID, note: op.note,
				deltaReceived: d.alloc, payTotal: op.payTotal, payAlloc: d.alloc,
			})
		case postOpCorrect:
			if d == nil {
				continue // 前后均不涉及本账单的更正不列明
			}
			events = append(events, ledgerEvent{
				seq: op.seq, kind: postOpCorrect, refID: op.correctionID, note: op.note,
				deltaReceived: d.afterAlloc - d.beforeAlloc, payID: op.paymentID,
				beforeAlloc: d.beforeAlloc, afterAlloc: d.afterAlloc,
			})
		case postOpRevokePay:
			// 撤销取消撤销时最新分配；该收款曾涉及本账单、当月已被更正移出时
			// 保留零金额撤销事件，d == nil 即此情形。
			if d == nil && !everInvolved[op.paymentID] {
				continue
			}
			var alloc int64
			if d != nil {
				alloc = d.alloc
			}
			events = append(events, ledgerEvent{
				seq: op.seq, kind: postOpRevokePay, refID: op.paymentID, note: op.note,
				linkSeq: op.linkSeq, payTotal: op.payTotal, payAlloc: alloc,
				deltaReceived: -alloc,
			})
		case postOpRefund:
			if d == nil {
				continue // 本次退款不涉及本账单（不因收款曾涉及而增列）
			}
			events = append(events, ledgerEvent{
				seq: op.seq, kind: postOpRefund, refID: op.refundID, note: op.note,
				deltaReceived: -d.alloc, payID: op.paymentID, payAlloc: d.alloc,
			})
		}
	}

	// 逐步回放并核验：只把每个事件的金额影响加到运行余额上，不对发生额做
	// 累计求和，因此累计补收/收款发生额超过 64 位上限但每步余额合法时仍能
	// 成功。
	payable := b.TotalFee
	var received int64
	for i := range events {
		ev := &events[i]
		desc := ev.kind + " " + ev.refID
		var err error
		if payable, err = addSigned64(payable, ev.deltaPayable); err != nil {
			return nil, fmt.Errorf("序号 %d（%s）之后应付越出有符号 64 位整数范围，数据异常，拒绝输出", ev.seq, desc)
		}
		if received, err = addSigned64(received, ev.deltaReceived); err != nil {
			return nil, fmt.Errorf("序号 %d（%s）之后实收越出有符号 64 位整数范围，数据异常，拒绝输出", ev.seq, desc)
		}
		if payable < 0 {
			return nil, fmt.Errorf("序号 %d（%s）之后应付为 %d 分（小于 0），数据异常，拒绝输出", ev.seq, desc, payable)
		}
		if received < 0 {
			return nil, fmt.Errorf("序号 %d（%s）之后实收为 %d 分（小于 0），数据异常，拒绝输出", ev.seq, desc, received)
		}
		if received > payable {
			return nil, fmt.Errorf("序号 %d（%s）之后实收 %d 分超过应付 %d 分，数据异常，拒绝输出", ev.seq, desc, received, payable)
		}
		ev.afterPayable = payable
		ev.afterReceived = received
	}
	return events, nil
}
