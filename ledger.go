package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 账后对账流水（bill ledger）：
//
// 把一张已结算账单的调整/调整撤销、收款/收款撤销合并为一条按全局操作序号
// （调整、收款及其撤销共用同一个已保存的全局序号池）升序排列的流水，
// 只回看账后变化：初始应付 = 账单原总金额，初始实收 = 0，
// 不推断查询时该账单是否已结算或某笔记录当前是否已撤销。
//
// 关键规则：
//   - 调整只改变应付；收款只按其在本账单的分配改变实收；
//   - 撤销在其发生序号抵消对应原操作：已撤销记录在撤销之前仍须计入，
//     不能按当前撤销状态提前扣除或删除原事件；
//   - 每个事件后都必须满足 0 ≤ 实收 ≤ 应付 ≤ MaxInt64，不只检查最终余额；
//   - 查询前核验目标账单的完整流水，异常即使发生在截止序号之后也拒绝；
//   - 查询只读：绝不落盘、不占用序号、不新增记录。

// ledgerEventKind 描述流水事件类型。
type ledgerEventKind int

const (
	evAdjust        ledgerEventKind = iota // 调整登记
	evAdjustRevoke                         // 调整撤销
	evPayment                              // 收款登记
	evPaymentRevoke                        // 收款撤销
)

// ledgerEvent 是合并后流水中的一个条目。
type ledgerEvent struct {
	seq       int64
	kind      ledgerEventKind
	recordID  string // 原记录标识（调整/收款自身的标识）
	dPayable  int64  // 对应付的影响：调整 ±amount；其余为 0
	dReceived int64  // 对实收的影响：收款 +本账单分配；撤销为 -原影响
	reason    string // 登记原因/备注，或撤销原因
	payTotal  int64  // 收款类事件的汇款总额；其余为 0

	revokeSeq  int64           // 原操作事件：撤销发生序号（0 表示存档中未撤销）
	originSeq  int64           // 撤销事件：被抵消原操作的序号
	originKind ledgerEventKind // 撤销事件：被抵消原操作的类型

	post ledgerSnapshot // 事件后余额（cutoff 回放时写入）
}

// ledgerSnapshot 是某个时点该账单的三项余额。
type ledgerSnapshot struct {
	payable     int64
	received    int64
	outstanding int64
}

// ledgerReport 是一次流水查询的结果。
type ledgerReport struct {
	bill     *bill
	cust     *customer
	maxSeq   int64 // 存档全局序号上限（state.NextSeq）
	cutoff   int64 // 查询采用的截止序号（省略等于 maxSeq；0 只看初始余额）
	explicit bool  // 截止序号是否由命令行显式给出（影响“最新/只看初始”标注）
	events   []*ledgerEvent
	final    ledgerSnapshot
}

// buildLedger 构建并核验目标账单的完整账后流水，再按 cutoff 回放。
func buildLedger(s *state, customerID, month string, cutoff int64) (*ledgerReport, error) {
	b, ok := s.Bills[billKey(customerID, month)]
	if !ok {
		return nil, fmt.Errorf("客户 %s 的 %s 尚无账单（未结算）", customerID, month)
	}
	cust := s.Customers[customerID] // loadStore 已校验存在

	adjs := adjustmentsFor(s, customerID, month)
	pays := paymentsFor(s, customerID, month)

	// 1) 全局序号占用核验：序号重复一律拒绝（即使 loadStore.validate 已查过，
	//    流水核验不依赖这一前提）。
	if err := checkGlobalSeqOwners(s); err != nil {
		return nil, err
	}

	// 2) 收集本账单的全部事件到 序号 -> 事件 映射。其他客户/月份造成的
	//    序号空档不进入映射，回放时自然跳过，属合法。
	events := make(map[int64]*ledgerEvent)
	addEvent := func(ev *ledgerEvent) error {
		if ev.seq < 1 || ev.seq > s.NextSeq {
			return fmt.Errorf("操作序号 %d（%s）越出存档序号范围 [1, %d]，流水引用失效",
				ev.seq, ledgerEventName(ev.kind, ev.recordID), s.NextSeq)
		}
		if prev, dup := events[ev.seq]; dup {
			return fmt.Errorf("操作序号 %d 重复：%s 与 %s", ev.seq,
				ledgerEventName(prev.kind, prev.recordID), ledgerEventName(ev.kind, ev.recordID))
		}
		events[ev.seq] = ev
		return nil
	}

	for _, a := range adjs {
		if err := addEvent(&ledgerEvent{
			seq:       a.Seq,
			kind:      evAdjust,
			recordID:  a.ID,
			dPayable:  a.Amount,
			reason:    a.Reason,
			revokeSeq: a.revokeSeqOrZero(),
		}); err != nil {
			return nil, err
		}
	}
	for _, a := range adjs {
		if !a.Revoked {
			continue
		}
		// 撤销先于原操作（或同序号）拒绝。
		if a.RevokeSeq <= a.Seq {
			return nil, fmt.Errorf("调整 %q 的撤销序号 %d 不晚于原操作序号 %d，撤销引用失效",
				a.ID, a.RevokeSeq, a.Seq)
		}
		neg, err := neg64(a.Amount)
		if err != nil {
			return nil, fmt.Errorf("调整 %q 金额 %d 取反超出有符号 64 位整数范围，流水无法核验", a.ID, a.Amount)
		}
		if err := addEvent(&ledgerEvent{
			seq:        a.RevokeSeq,
			kind:       evAdjustRevoke,
			recordID:   a.ID,
			dPayable:   neg,
			reason:     a.RevokeReason,
			originSeq:  a.Seq,
			originKind: evAdjust,
		}); err != nil {
			return nil, err
		}
	}
	for _, p := range pays {
		alloc := p.amountFor(month)
		if err := addEvent(&ledgerEvent{
			seq:       p.Seq,
			kind:      evPayment,
			recordID:  p.ID,
			dReceived: alloc,
			reason:    p.Note,
			payTotal:  p.Total,
			revokeSeq: p.revokeSeqOrZero(),
		}); err != nil {
			return nil, err
		}
	}
	for _, p := range pays {
		if !p.Revoked {
			continue
		}
		if p.RevokeSeq <= p.Seq {
			return nil, fmt.Errorf("收款 %q 的撤销序号 %d 不晚于原操作序号 %d，撤销引用失效",
				p.ID, p.RevokeSeq, p.Seq)
		}
		alloc := p.amountFor(month)
		if err := addEvent(&ledgerEvent{
			seq:        p.RevokeSeq,
			kind:       evPaymentRevoke,
			recordID:   p.ID,
			dReceived:  -alloc, // 整笔撤销在同一序号取消该账单分配
			reason:     p.RevokeReason,
			payTotal:   p.Total,
			originSeq:  p.Seq,
			originKind: evPayment,
		}); err != nil {
			return nil, err
		}
	}

	// 3) 撤销引用的原操作必须能在本账单流水中找到（引用失效拒绝）。
	for _, a := range adjs {
		if a.Revoked {
			if _, ok := events[a.Seq]; !ok {
				return nil, fmt.Errorf("调整 %q 的撤销找不到原操作（序号 %d），引用失效", a.ID, a.Seq)
			}
		}
	}
	for _, p := range pays {
		if p.Revoked {
			if _, ok := events[p.Seq]; !ok {
				return nil, fmt.Errorf("收款 %q 的撤销找不到原操作（序号 %d），引用失效", p.ID, p.Seq)
			}
		}
	}

	ordered := sortLedgerEvents(events)

	// 4) 先回放“完整流水”逐事件核验不变量：异常即使在 cutoff 之后也拒绝。
	//    累计补收或收款发生额本身可能超过 MaxInt64，只要每一步余额合法即通过。
	snap := ledgerSnapshot{payable: b.TotalFee, received: 0, outstanding: b.TotalFee}
	if snap.payable < 0 {
		return nil, fmt.Errorf("账单 %s 原总金额为负（%d），流水无法核验", b.ID, snap.payable)
	}
	for _, ev := range ordered {
		next, err := applyLedgerEvent(snap, ev)
		if err != nil {
			return nil, fmt.Errorf("完整流水核验失败：序号 %d（%s）之后余额不合法：%w",
				ev.seq, ledgerEventName(ev.kind, ev.recordID), err)
		}
		snap = next
	}

	// 5) 按 cutoff 回放历史：截止包含该序号；截止之后的事件既不影响历史
	//    余额，也不混入流水。
	shown := make([]*ledgerEvent, 0, len(ordered))
	hist := ledgerSnapshot{payable: b.TotalFee, received: 0, outstanding: b.TotalFee}
	for _, ev := range ordered {
		if ev.seq > cutoff {
			break
		}
		next, err := applyLedgerEvent(hist, ev)
		if err != nil {
			return nil, fmt.Errorf("序号 %d 回放失败：%w", ev.seq, err) // 理论不可达：第 4 步已核验
		}
		hist = next
		ev.post = hist
		shown = append(shown, ev)
	}

	return &ledgerReport{
		bill:   b,
		cust:   cust,
		maxSeq: s.NextSeq,
		cutoff: cutoff,
		events: shown,
		final:  hist,
	}, nil
}

// revokeSeqOrZero 返回记录的撤销序号；未撤销时为 0。
func (a *adjustment) revokeSeqOrZero() int64 {
	if a.Revoked {
		return a.RevokeSeq
	}
	return 0
}

func (p *payment) revokeSeqOrZero() int64 {
	if p.Revoked {
		return p.RevokeSeq
	}
	return 0
}

// checkGlobalSeqOwners 核验全库调整/收款及其撤销的序号互不重复、均落在
// [1, NextSeq] 内。loadStore.validate 已做同等检查，这里再独立核验一遍，
// 使流水查询不依赖“载入时一定校验过”的假设。
func checkGlobalSeqOwners(s *state) error {
	owner := make(map[int64]string)
	claim := func(seq int64, who string) error {
		if seq < 1 || seq > s.NextSeq {
			return fmt.Errorf("%s 的操作序号 %d 越出存档序号范围 [1, %d]，引用失效", who, seq, s.NextSeq)
		}
		if prev, dup := owner[seq]; dup {
			return fmt.Errorf("操作序号 %d 重复：%s 与 %s", seq, prev, who)
		}
		owner[seq] = who
		return nil
	}
	// 遍历顺序不确定，但只用于发现重复/越界，与顺序无关。
	for id, a := range s.Adjustments {
		if err := claim(a.Seq, "调整 "+id); err != nil {
			return err
		}
		if a.Revoked {
			if err := claim(a.RevokeSeq, "撤销调整 "+id); err != nil {
				return err
			}
		}
	}
	for id, p := range s.Payments {
		if err := claim(p.Seq, "收款 "+id); err != nil {
			return err
		}
		if p.Revoked {
			if err := claim(p.RevokeSeq, "撤销收款 "+id); err != nil {
				return err
			}
		}
	}
	return nil
}

// sortLedgerEvents 按序号升序返回事件。
func sortLedgerEvents(events map[int64]*ledgerEvent) []*ledgerEvent {
	out := make([]*ledgerEvent, 0, len(events))
	for _, ev := range events {
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// applyLedgerEvent 把单个事件作用到余额上，全程有符号 64 位整数运算，
// 事件后校验 0 ≤ 实收 ≤ 应付 ≤ MaxInt64。
func applyLedgerEvent(cur ledgerSnapshot, ev *ledgerEvent) (ledgerSnapshot, error) {
	payable, err := addSigned64(cur.payable, ev.dPayable)
	if err != nil {
		return ledgerSnapshot{}, fmt.Errorf("应付运算溢出（%d %+d）", cur.payable, ev.dPayable)
	}
	received, err := addSigned64(cur.received, ev.dReceived)
	if err != nil {
		return ledgerSnapshot{}, fmt.Errorf("实收运算溢出（%d %+d）", cur.received, ev.dReceived)
	}
	if payable < 0 || payable > int64Max {
		return ledgerSnapshot{}, fmt.Errorf("应付 %d 越出 [0, %d]", payable, int64Max)
	}
	if received < 0 {
		return ledgerSnapshot{}, fmt.Errorf("实收 %d 小于 0（应付 %d）", received, payable)
	}
	if received > payable {
		return ledgerSnapshot{}, fmt.Errorf("实收 %d 超过应付 %d", received, payable)
	}
	return ledgerSnapshot{
		payable:     payable,
		received:    received,
		outstanding: payable - received,
	}, nil
}

// int64Max 是有符号 64 位整数最大值。
const int64Max = 1<<63 - 1

// ledgerEventName 返回“调整 xxx”/“撤销收款 yyy”之类的可读描述。
func ledgerEventName(kind ledgerEventKind, id string) string {
	switch kind {
	case evAdjust:
		return "调整 " + id
	case evAdjustRevoke:
		return "撤销调整 " + id
	case evPayment:
		return "收款 " + id
	case evPaymentRevoke:
		return "撤销收款 " + id
	default:
		return "未知事件 " + id
	}
}

// --- 命令行入口与渲染 -----------------------------------------------------

// cmdBillLedger 处理 bill ledger <客户标识> <YYYY-MM> [截止操作序号]。
// 查询只读：成功或失败都不落盘、不占用序号、不新增记录。
func cmdBillLedger(dir, customerID, month, cutoffText string) error {
	if !validMonth(month) {
		return fmt.Errorf("月份 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", month)
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	if _, ok := s.Customers[customerID]; !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}
	if _, ok := s.Bills[billKey(customerID, month)]; !ok {
		return fmt.Errorf("客户 %s 的 %s 尚无账单（未结算）", customerID, month)
	}

	// 截止序号省略表示最新（存档全局序号上限）；0 只返回初始余额；
	// 截止包含该序号本身。
	cutoff := s.NextSeq
	if cutoffText != "" {
		v, err := parseCutoffSeq(cutoffText)
		if err != nil {
			return err
		}
		if v > s.NextSeq {
			return fmt.Errorf("截止操作序号 %d 超过存档全局序号上限 %d，拒绝查询", v, s.NextSeq)
		}
		cutoff = v
	}

	report, err := buildLedger(s, customerID, month, cutoff)
	if err != nil {
		return err
	}
	report.explicit = cutoffText != ""
	printLedger(report)
	return nil
}

// parseCutoffSeq 解析截止操作序号：只接受非负整数；负数、小数或其他
// 非整数内容一律拒绝（超过存档上限由调用方判断）。
func parseCutoffSeq(text string) (int64, error) {
	text = strings.TrimSpace(text)
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("截止操作序号 %q 必须是非负整数（0 只返回初始余额，省略表示最新）: %w", text, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("截止操作序号不能为负数，收到 %d", v)
	}
	return v, nil
}

// printLedger 渲染对账流水报告。
func printLedger(r *ledgerReport) {
	fmt.Fprintln(stdout, "账后对账流水")
	fmt.Fprintf(stdout, "账单标识：%s\n", r.bill.ID)
	fmt.Fprintf(stdout, "客户：%s（%s）\n", r.cust.ID, r.cust.Name)
	fmt.Fprintf(stdout, "月份：%s（UTC 自然月，左闭右开）\n", r.bill.Month)
	fmt.Fprintf(stdout, "原总金额：%d 分（%s）——账后流水起点（初始应付，初始实收为 0）\n",
		r.bill.TotalFee, moneyFen(r.bill.TotalFee))
	fmt.Fprintf(stdout, "存档全局序号上限：%d\n", r.maxSeq)
	fmt.Fprintf(stdout, "查询截止序号：%d%s\n", r.cutoff, cutoffNote(r))
	if len(r.events) == 0 {
		fmt.Fprintln(stdout, "流水：空（截止以内没有调整、收款或其撤销事件）")
	} else {
		fmt.Fprintln(stdout, "流水（按成功操作序号升序，截止包含该序号）：")
		for i, ev := range r.events {
			fmt.Fprintf(stdout, "  %d. %s\n", i+1, formatLedgerEvent(r, ev))
		}
	}
	fmt.Fprintln(stdout, "截止时余额：")
	fmt.Fprintf(stdout, "  应付：%d 分（%s）\n", r.final.payable, moneyFen(r.final.payable))
	fmt.Fprintf(stdout, "  实收：%d 分（%s）\n", r.final.received, moneyFen(r.final.received))
	fmt.Fprintf(stdout, "  未收余额：%d 分（%s）\n", r.final.outstanding, moneyFen(r.final.outstanding))
}

func cutoffNote(r *ledgerReport) string {
	// 显式给出 0：只看初始余额；省略时 cutoff 取存档上限，表示最新
	//（上限为 0 时最新本身就是初始余额）。
	if r.explicit && r.cutoff == 0 {
		return "（0：只返回初始余额）"
	}
	if r.cutoff >= r.maxSeq {
		return "（最新）"
	}
	return ""
}

// formatLedgerEvent 渲染单个事件：序号、类型、原记录标识、金额影响、
// 原因/备注、撤销关联，以及事件后的应付/实收/未收余额。
func formatLedgerEvent(r *ledgerReport, ev *ledgerEvent) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "序号=%d 类型=%s 原记录=%s", ev.seq, ledgerKindName(ev.kind), ev.recordID)
	switch ev.kind {
	case evAdjust:
		fmt.Fprintf(&sb, " 金额影响：应付 %+d 分（%s）", ev.dPayable, moneyFen(ev.dPayable))
	case evAdjustRevoke:
		fmt.Fprintf(&sb, " 金额影响：应付 %+d 分（%s）", ev.dPayable, moneyFen(ev.dPayable))
	case evPayment:
		fmt.Fprintf(&sb, " 汇款总额=%d 分（%s） 本账单分配=%d 分（%s） 金额影响：实收 +%d 分",
			ev.payTotal, moneyFen(ev.payTotal), ev.dReceived, moneyFen(ev.dReceived), ev.dReceived)
	case evPaymentRevoke:
		fmt.Fprintf(&sb, " 汇款总额=%d 分（%s） 本账单分配=%d 分（%s） 金额影响：实收 %+d 分（整笔撤销在同一序号取消该分配）",
			ev.payTotal, moneyFen(ev.payTotal), -ev.dReceived, moneyFen(-ev.dReceived), ev.dReceived)
	}
	if ev.reason != "" {
		fmt.Fprintf(&sb, " %s：%s", ledgerReasonLabel(ev.kind), ev.reason)
	}
	switch {
	case ev.kind == evAdjust || ev.kind == evPayment:
		// 历史视图只反映截至 cutoff 的状态：撤销发生在截止之后时，
		// 原事件在当时仍未撤销，故不显示撤销关联（该撤销事件也不混入流水）。
		if ev.revokeSeq > 0 && ev.revokeSeq <= r.cutoff {
			fmt.Fprintf(&sb, " 撤销关联：已于序号 %d 撤销（原因见该撤销事件）", ev.revokeSeq)
		}
	case ev.kind == evAdjustRevoke || ev.kind == evPaymentRevoke:
		fmt.Fprintf(&sb, " 撤销关联：抵消序号 %d 的%s（原记录 %s）",
			ev.originSeq, ledgerKindName(ev.originKind), ev.recordID)
	}
	fmt.Fprintf(&sb, " | 事件后：应付=%d 分 实收=%d 分 未收=%d 分",
		ev.post.payable, ev.post.received, ev.post.outstanding)
	return sb.String()
}

func ledgerKindName(k ledgerEventKind) string {
	switch k {
	case evAdjust:
		return "调整"
	case evAdjustRevoke:
		return "调整撤销"
	case evPayment:
		return "收款"
	case evPaymentRevoke:
		return "收款撤销"
	default:
		return "未知"
	}
}

func ledgerReasonLabel(k ledgerEventKind) string {
	switch k {
	case evAdjust:
		return "原因"
	case evAdjustRevoke, evPaymentRevoke:
		return "撤销原因"
	case evPayment:
		return "备注"
	default:
		return "说明"
	}
}
