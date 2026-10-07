package main

import (
	"fmt"
	"sort"
)

// 账后对账共享计算（仅供 bill ledger 与 bill reconcile 两类只读查询使用）。
//
// 现有结算、账单展示、载入校验与账后登记（adjust/revoke/pay/remit/correct/
// unpay/refund）的校验和持久化行为不在本文件内；本文件只把已保存的调整、
// 调整撤销、收款、分配更正、收款撤销与退款，按统一的业务归属与金额影响规则
// 推导成一组带全局序号的账后操作，再由两类查询各自展开、筛选与展示：
//
//   - postbillOps 一次构建一个客户全部账后操作（每笔调整/收款/更正/撤销/
//     退款各一条），操作自带各账期的应付/实收影响，是两类查询唯一的推导入口；
//   - postbillLedger 把操作展开到单张账单的流水（ledger 视图）；
//   - postbillRangeEvents 把操作按账期范围与序号区间筛选为报表条目
//     （reconcile 视图）；
//   - verifyPostbillLedgers 对选中账单做完整流水逐步核验，任何中间异常
//     （即使在查询截止之后）都整体拒绝。
//
// 业务归属与金额影响规则（两类查询共用同一套）：
//
//   - 流水按全局序号升序，以原总金额为初始应付、零实收起算；指定截止包含
//     该序号，对账区间为起点之后、终点以内，合法空档与序号 0 保持；
//   - 调整只影响所属账期应付；撤销调整在其发生序号抵消原调整金额；
//   - 收款按登记时分配增加对应账期实收；更正按发生时的前后分配替换，撤销
//     取消撤销当时的最新分配；退款只减少对应月实收，截止后的操作不能提前
//     影响历史余额；
//   - 同名调整、收款、更正、退款按类型关联（分别来自各自记录表）。

// pbKind 标识账后操作的业务类型。同名标识在不同类型间互不混淆。
const (
	pbAdjust        = "调整"
	pbAdjustRevoke  = "撤销调整"
	pbPayment       = "收款"
	pbCorrection    = "更正"
	pbPaymentRevoke = "撤销收款"
	pbRefund        = "退款"
)

// postbillEffect 是一次账后操作对某一个账期（月份）的金额影响。
type postbillEffect struct {
	month         string
	deltaPayable  int64 // 对该月应付的影响（分，带符号）
	deltaReceived int64 // 对该月实收的影响（分，带符号）
	alloc         int64 // 收款：登记分配；撤销收款：被取消的最新分配；退款：该月退款额
	beforeAlloc   int64 // 更正：更正前该收款在该月的分配
	afterAlloc    int64 // 更正：更正后该收款在该月的分配
}

// postbillOp 是一笔已保存账后业务在共享计算中的统一表示：调整、撤销调整、
// 收款、分配更正、撤销收款或退款。effects 给出该操作涉及各账期的金额影响：
//
//   - 调整/撤销调整：只含所属账期一项应付影响；
//   - 收款：按登记时分配，每个分配月份一项实收影响；
//   - 更正：以前后分配并集的每个月份一项实收影响（含前后均为 0 时不会
//     出现的月份被跳过；前后都涉及但金额相同的月份保留零差额明细）；
//   - 撤销收款：撤销当时最新分配的每个月份一项实收影响；
//   - 退款：每个退款月份一项实收影响。
type postbillOp struct {
	seq      int64
	kind     string
	refID    string // 记录标识（调整、收款、更正或退款标识）
	note     string // 原因（调整/更正/退款/撤销类）或备注（收款）
	linkSeq  int64  // 撤销事件关联的原操作序号；非撤销为 0
	payID    string // 收款自身标识；更正/退款/撤销收款：关联收款标识
	payTotal int64  // 收款类操作：汇款总额（仅展示用，不计入范围内实收）
	effects  []postbillEffect
}

// postbillOps 构建一个客户的全部账后操作，按全局操作序号升序返回。同一全局
// 序号的唯一性、撤销先后等完整性问题在 verifyPostbillLedgers 中统一核验；
// 这里只做业务归属与金额影响的推导。调整金额为 math.MinInt64 无法抵消时
// 直接按数据异常报错。
func postbillOps(s *state, customerID string) ([]postbillOp, error) {
	var ops []postbillOp

	for _, a := range s.Adjustments {
		if a.CustomerID != customerID {
			continue
		}
		ops = append(ops, postbillOp{
			seq: a.Seq, kind: pbAdjust, refID: a.ID, note: a.Reason,
			effects: []postbillEffect{{
				month: a.Month, deltaPayable: a.Amount,
			}},
		})
		if a.Revoked {
			neg, err := neg64(a.Amount)
			if err != nil {
				return nil, fmt.Errorf("调整 %q 的金额 %d 分无法抵消（越界），数据异常，拒绝输出对账目", a.ID, a.Amount)
			}
			ops = append(ops, postbillOp{
				seq: a.RevokeSeq, kind: pbAdjustRevoke, refID: a.ID, note: a.RevokeReason,
				linkSeq: a.Seq,
				effects: []postbillEffect{{
					month: a.Month, deltaPayable: neg,
				}},
			})
		}
	}

	for _, p := range s.Payments {
		if p.CustomerID != customerID {
			continue
		}
		// 收款按登记时分配计入实收；多月汇款在每个分配月份各有一项影响，
		// 调用方按账期筛选时只取本账期/范围内月份，不重复累加汇款总额。
		op := postbillOp{
			seq: p.Seq, kind: pbPayment, refID: p.ID, note: p.Note,
			payID: p.ID, payTotal: p.Total,
		}
		for _, al := range p.Allocations {
			op.effects = append(op.effects, postbillEffect{
				month: al.Month, deltaReceived: al.Amount, alloc: al.Amount,
			})
		}
		ops = append(ops, op)

		// 以登记分配为起点按序号回放更正：每次更正按发生时的前后分配计算
		// 影响。running 始终是该更正发生前生效的分配。
		running := p.Allocations
		for _, c := range correctionsFor(s, p.ID) {
			cop := postbillOp{
				seq: c.Seq, kind: pbCorrection, refID: c.ID, note: c.Reason,
				payID: p.ID, payTotal: p.Total,
			}
			for _, m := range unionMonths(running, c.Allocations) {
				b4, af := allocAmountFor(running, m), allocAmountFor(c.Allocations, m)
				if b4 == 0 && af == 0 {
					continue
				}
				cop.effects = append(cop.effects, postbillEffect{
					month: m, deltaReceived: af - b4, beforeAlloc: b4, afterAlloc: af,
				})
			}
			ops = append(ops, cop)
			running = c.Allocations
		}

		// 撤销取消的是撤销发生时的最新分配（running 即回放全部更正后的
		// 分配；更正序号均先于撤销序号）。
		if p.Revoked {
			rop := postbillOp{
				seq: p.RevokeSeq, kind: pbPaymentRevoke, refID: p.ID, note: p.RevokeReason,
				linkSeq: p.Seq, payID: p.ID, payTotal: p.Total,
			}
			for _, al := range running {
				rop.effects = append(rop.effects, postbillEffect{
					month: al.Month, deltaReceived: -al.Amount, alloc: al.Amount,
				})
			}
			ops = append(ops, rop)
		}

		// 退款只减少对应月实收、不可撤销；退款月份必属于最新分配。
		// 跨月退款是一次操作，多个月份的影响合并在同一条目下。
		for _, r := range refundsFor(s, p.ID) {
			rop := postbillOp{
				seq: r.Seq, kind: pbRefund, refID: r.ID, note: r.Reason,
				payID: p.ID, payTotal: p.Total,
			}
			for _, al := range r.Allocations {
				rop.effects = append(rop.effects, postbillEffect{
					month: al.Month, deltaReceived: -al.Amount, alloc: al.Amount,
				})
			}
			ops = append(ops, rop)
		}
	}

	sort.Slice(ops, func(i, j int) bool { return ops[i].seq < ops[j].seq })
	return ops, nil
}

// effectFor 返回一次操作在指定账期上的金额影响；不涉及时第二个返回值为
// false。“涉及”以效果项为准：更正前后都不涉及的月份没有效果项，而前后
// 分配相同（零差额）的月份仍有效果项。
func effectFor(op postbillOp, month string) (postbillEffect, bool) {
	for _, e := range op.effects {
		if e.month == month {
			return e, true
		}
	}
	return postbillEffect{}, false
}

// postbillLedger 把客户级账后操作展开为单张账单的一条按序号升序流水。
// 本函数只负责业务归属与事件展开，不计算事件后余额、不做完整性核验；
// 逐步回放、越界检查与事件后余额填充统一由 verifyPostbillLedgers 完成，
// 调用方应使用其返回的已核验流水。
//
// 与报表视图的归属差异仅在筛选层：单账单流水保留曾涉及该账单的收款在被
// 更正移出该月之后的零金额撤销事件（撤销当时最新分配已不含本月），以便
// 完整呈现该账单历史上出现过的收款；这类撤销在本月金额影响为 0。
//
// ops 须为 postbillOps 的结果（按序号升序）。
func postbillLedger(ops []postbillOp, b *bill) []ledgerEvent {
	var events []ledgerEvent
	for _, op := range ops {
		switch op.kind {
		case pbAdjust, pbAdjustRevoke:
			e, ok := effectFor(op, b.Month)
			if !ok {
				continue
			}
			events = append(events, ledgerEvent{
				seq: op.seq, kind: op.kind, refID: op.refID,
				deltaPayable: e.deltaPayable, note: op.note, linkSeq: op.linkSeq,
			})
		case pbPayment:
			e, ok := effectFor(op, b.Month)
			if !ok {
				continue // 登记时不涉及本账单的收款，不会因后续更正“补登”收款事件
			}
			events = append(events, ledgerEvent{
				seq: op.seq, kind: op.kind, refID: op.refID,
				deltaReceived: e.deltaReceived, note: op.note,
				payTotal: op.payTotal, payAlloc: e.alloc,
			})
		case pbCorrection:
			e, ok := effectFor(op, b.Month)
			if !ok {
				continue // 前后分配都不涉及本账单的更正不列示
			}
			events = append(events, ledgerEvent{
				seq: op.seq, kind: op.kind, refID: op.refID,
				deltaReceived: e.deltaReceived, note: op.note, payID: op.payID,
				beforeAlloc: e.beforeAlloc, afterAlloc: e.afterAlloc,
			})
		case pbPaymentRevoke:
			// 曾涉及该账单的收款（登记分配或任一次更正的新分配曾落在本
			// 月）即使在撤销时已被更正移出，也保留零金额撤销事件。
			if !paymentEverTouched(ops, op.payID, b.Month) {
				continue
			}
			e, ok := effectFor(op, b.Month)
			var amt int64
			if ok {
				amt = e.alloc // 撤销当时最新分配中本账单的金额；移出后为 0
			}
			events = append(events, ledgerEvent{
				seq: op.seq, kind: op.kind, refID: op.refID,
				deltaReceived: -amt, note: op.note, linkSeq: op.linkSeq,
				payTotal: op.payTotal, payAlloc: amt,
			})
		case pbRefund:
			e, ok := effectFor(op, b.Month)
			if !ok {
				continue
			}
			events = append(events, ledgerEvent{
				seq: op.seq, kind: op.kind, refID: op.refID,
				deltaReceived: e.deltaReceived, note: op.note, payID: op.payID,
				payAlloc: e.alloc,
			})
		}
	}
	return events
}

// paymentEverTouched 报告某收款是否曾在登记或任一次更正中把分配落到指定
// 账期（即使后来被更正移出）。零金额撤销事件的保留只依据“曾经涉及”。
func paymentEverTouched(ops []postbillOp, payID, month string) bool {
	for _, op := range ops {
		if op.payID != payID {
			continue
		}
		switch op.kind {
		case pbPayment, pbCorrection:
			if _, ok := effectFor(op, month); ok {
				return true
			}
		}
	}
	return false
}

// verifyPostbillLedgers 对选中账单逐张做完整流水核验（不受任何查询截止
// 序号限制）：逐事件检查该账单流水内序号不重复、撤销严格晚于原操作，且
// 每个事件之后都满足 0 ≤ 实收 ≤ 应付 ≤ 有符号 64 位最大值。任一账单的
// 任何中间步骤异常都会返回错误，调用方应整体拒绝、不输出部分报告——只
// 检查最终余额会放过中间越界的存档。返回按账单月份键控的已核验流水（含
// 事件后余额），供两类查询直接复用。
//
// 逐步累加只把每个事件的金额影响加到运行余额上，不对发生额做累计求和，
// 因此累计补收/收款发生额超过 64 位上限但每步余额合法时仍能成功。
func verifyPostbillLedgers(ops []postbillOp, bills []*bill) (map[string][]ledgerEvent, error) {
	ledgers := make(map[string][]ledgerEvent, len(bills))
	for _, b := range bills {
		events := postbillLedger(ops, b)
		payable, received := b.TotalFee, int64(0)
		seen := make(map[int64]string, len(events))
		for i := range events {
			ev := &events[i]
			desc := ev.kind + " " + ev.refID
			if prev, dup := seen[ev.seq]; dup {
				return nil, fmt.Errorf("流水序号 %d 重复（%s 与 %s），数据异常，拒绝输出对账目", ev.seq, prev, desc)
			}
			seen[ev.seq] = desc
			if ev.linkSeq != 0 && ev.linkSeq >= ev.seq {
				return nil, fmt.Errorf("%s 的撤销序号 %d 不晚于原操作序号 %d，数据异常，拒绝输出对账目", desc, ev.seq, ev.linkSeq)
			}
			var err error
			if payable, err = addSigned64(payable, ev.deltaPayable); err != nil {
				return nil, fmt.Errorf("序号 %d（%s）之后应付越出有符号 64 位整数范围，数据异常，拒绝输出对账目", ev.seq, desc)
			}
			if received, err = addSigned64(received, ev.deltaReceived); err != nil {
				return nil, fmt.Errorf("序号 %d（%s）之后实收越出有符号 64 位整数范围，数据异常，拒绝输出对账目", ev.seq, desc)
			}
			if payable < 0 {
				return nil, fmt.Errorf("序号 %d（%s）之后应付为 %d 分（小于 0），数据异常，拒绝输出对账目", ev.seq, desc, payable)
			}
			if received < 0 {
				return nil, fmt.Errorf("序号 %d（%s）之后实收为 %d 分（小于 0），数据异常，拒绝输出对账目", ev.seq, desc, received)
			}
			if received > payable {
				return nil, fmt.Errorf("序号 %d（%s）之后实收 %d 分超过应付 %d 分，数据异常，拒绝输出对账目", ev.seq, desc, received, payable)
			}
			ev.afterPayable = payable
			ev.afterReceived = received
		}
		ledgers[b.Month] = events
	}
	return ledgers, nil
}

// postbillRangeEvents 把客户级账后操作筛选为跨账期报表区间内的操作条目：
// 账期范围 [startMonth, endMonth]（包含首尾），序号区间 (startSeq, endSeq]
// （起点之后、终点以内）。每次操作只出现一次，只保留落在范围内账期的
// 效果项；操作在范围内没有任何账期效果时不列示。
//
// 撤销收款只按撤销当时最新分配判断是否涉及范围（effects 即最新分配），
// 不因曾经涉及范围而增加操作——这是与单账单流水零金额撤销事件的展示
// 差异，差异只发生在本筛选层。
func postbillRangeEvents(ops []postbillOp, startMonth, endMonth string, startSeq, endSeq int64) []reconEvent {
	inRange := func(m string) bool { return m >= startMonth && m <= endMonth }
	var out []reconEvent
	for _, op := range ops {
		if op.seq <= startSeq || op.seq > endSeq {
			continue
		}
		var changes []reconMonthChange
		for _, e := range op.effects {
			if !inRange(e.month) {
				continue
			}
			changes = append(changes, reconMonthChange{
				month:         e.month,
				deltaPayable:  e.deltaPayable,
				deltaReceived: e.deltaReceived,
				alloc:         e.alloc,
				beforeAlloc:   e.beforeAlloc,
				afterAlloc:    e.afterAlloc,
			})
		}
		if len(changes) == 0 {
			continue
		}
		out = append(out, reconEvent{
			seq: op.seq, kind: op.kind, refID: op.refID, note: op.note,
			linkSeq: op.linkSeq, payTotal: op.payTotal, payID: op.payID,
			changes: changes,
		})
	}
	return out
}
