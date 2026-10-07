package main

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// 跨账期对账报表（bill report）：只读命令，用一份报告核对一个客户多个账期
// 在一段账后操作中的余额变化。账单选择只看账期范围（含首尾）内当前已存在
// 的账单，不补结算、不按操作时间筛选；所有选中账单在序号 0 以原总金额为
// 应付、零实收起算，与 bill ledger 共用同一份流水回放，保证两端逐月余额
// 与相同截止的 bill ledger 一致、终点为最新时与 bill show 一致。
// 只读：不修改库、不占用序号、不新增记录，成功或失败均不改写存档。

// reportBill 是报表中一张选中账单及其完整（已核验）账后流水。
type reportBill struct {
	bill   *bill
	events []ledgerEvent // 按全局操作序号升序，回放时已逐步核验
}

// reportMonthDelta 是一次账后操作对范围内某个月账单的金额影响。
type reportMonthDelta struct {
	month         string
	deltaPayable  int64 // 对当前应付的影响（分，带符号）
	deltaReceived int64 // 对实收的影响（分，带符号）
	payAlloc      int64 // 收款/撤销收款事件：该月分配（撤销为被取消的最新分配）
	beforeAlloc   int64 // 更正事件：该月更正前分配
	afterAlloc    int64 // 更正事件：该月更正后分配
}

// reportEvent 是报表中合并后的一次账后操作：同一全局序号的操作只展示一次，
// 范围内各月的变化合并到同一条目。调整与收款可同名，按类型分别关联。
type reportEvent struct {
	seq      int64              // 全局操作序号
	kind     string             // 调整 / 撤销调整 / 收款 / 更正 / 撤销收款
	refID    string             // 原记录标识（调整标识、收款标识或更正标识）
	note     string             // 原因（调整/更正类）或备注（收款类）
	linkSeq  int64              // 撤销事件关联的原操作序号；非撤销事件为 0
	payTotal int64              // 收款类事件：汇款总额
	payID    string             // 更正事件：关联收款标识
	months   []reportMonthDelta // 范围内各月变化，按月份升序
}

// balanceAtSeq 返回账单流水在指定序号操作完成后的应付与实收：序号 0 为初始
// 余额（原总金额、零实收），截止包含该序号。events 须按序号升序。
func balanceAtSeq(b *bill, events []ledgerEvent, seq int64) (payable, received int64) {
	payable, received = b.TotalFee, 0
	for _, ev := range events {
		if ev.seq > seq {
			break
		}
		payable, received = ev.afterPayable, ev.afterReceived
	}
	return payable, received
}

// allocTouchesRange 报告分配列表是否涉及账期范围 [startMonth, endMonth]
// （含首尾）内的月份；分配金额恒为正整数分。
func allocTouchesRange(allocs []paymentAllocation, startMonth, endMonth string) bool {
	for _, al := range allocs {
		if al.Month >= startMonth && al.Month <= endMonth {
			return true
		}
	}
	return false
}

// mergeReportEvents 把各选中账单流水中的事件按全局序号合并：同一序号的操作
// 只保留一条，范围内各月变化并入 months。收款按登记时分配、收款撤销按撤销
// 发生时最新分配、更正按前后分配判断是否涉及范围——撤销时最新分配不含范围
// 内月份的收款撤销与本报表无关，不展示。
func mergeReportEvents(s *state, bills []*reportBill, startMonth, endMonth string) []reportEvent {
	bySeq := make(map[int64]*reportEvent)
	for _, rb := range bills {
		for _, ev := range rb.events {
			re := bySeq[ev.seq]
			if re == nil {
				re = &reportEvent{
					seq: ev.seq, kind: ev.kind, refID: ev.refID, note: ev.note,
					linkSeq: ev.linkSeq, payTotal: ev.payTotal, payID: ev.payID,
				}
				bySeq[ev.seq] = re
			}
			re.months = append(re.months, reportMonthDelta{
				month:         rb.bill.Month,
				deltaPayable:  ev.deltaPayable,
				deltaReceived: ev.deltaReceived,
				payAlloc:      ev.payAlloc,
				beforeAlloc:   ev.beforeAlloc,
				afterAlloc:    ev.afterAlloc,
			})
		}
	}
	out := make([]reportEvent, 0, len(bySeq))
	for _, re := range bySeq {
		if re.kind == "撤销收款" {
			// 更正不得晚于撤销（载入时已校验），当前最新分配即撤销发生时的分配。
			p := s.Payments[re.refID]
			if !allocTouchesRange(currentAllocations(s, p), startMonth, endMonth) {
				continue
			}
		}
		// 金额无变化的月份不列入“范围内变化”（如更正只改动范围外月份、
		// 或历史涉及但该月最新分配已为 0）。
		kept := re.months[:0]
		for _, md := range re.months {
			if md.deltaPayable != 0 || md.deltaReceived != 0 {
				kept = append(kept, md)
			}
		}
		re.months = kept
		sort.Slice(re.months, func(i, j int) bool { return re.months[i].month < re.months[j].month })
		out = append(out, *re)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// moneyFenBig 将任意精度“分”渲染为人民币金额，规则与 moneyFen 相同；
// 多账单汇总可能超出有符号 64 位上限，仍须精确输出。
func moneyFenBig(z *big.Int) string {
	neg := z.Sign() < 0
	s := z.String()
	if neg {
		s = s[1:]
	}
	if len(s) < 3 {
		s = strings.Repeat("0", 3-len(s)) + s
	}
	out := s[:len(s)-2] + "." + s[len(s)-2:] + " 元"
	if neg {
		out = "-" + out
	}
	return out
}

// reportBalance 是若干账单在同一序号截止后的汇总余额；合计可超出有符号
// 64 位上限，用任意精度整数精确累加。
type reportBalance struct {
	payable  *big.Int
	received *big.Int
}

// sumBalanceAt 汇总全部选中账单在指定序号操作完成后的应付与实收。
func sumBalanceAt(bills []*reportBill, seq int64) reportBalance {
	sum := reportBalance{payable: new(big.Int), received: new(big.Int)}
	for _, rb := range bills {
		p, r := balanceAtSeq(rb.bill, rb.events, seq)
		sum.payable.Add(sum.payable, big.NewInt(p))
		sum.received.Add(sum.received, big.NewInt(r))
	}
	return sum
}

// outstanding 返回未收余额（应付 − 实收），不修改入参。
func (bal reportBalance) outstanding() *big.Int {
	return new(big.Int).Sub(bal.payable, bal.received)
}

func cmdBillReport(dir, customerID, startMonth, endMonth string, seqArgs []string) error {
	if !validMonth(startMonth) {
		return fmt.Errorf("起月 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", startMonth)
	}
	if !validMonth(endMonth) {
		return fmt.Errorf("末月 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", endMonth)
	}
	if startMonth > endMonth {
		return fmt.Errorf("起月 %s 晚于末月 %s，账期范围包含首尾且起月不得晚于末月", startMonth, endMonth)
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	cust, ok := s.Customers[customerID]
	if !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}

	// 起止操作序号使用调整/收款/更正及其撤销共用的已保存全局序号：省略表示
	// 0 至存档最新；须满足 0 ≤ 起点 ≤ 终点 ≤ 全局序号上限，空档合法。
	startSeq, endSeq := int64(0), s.NextSeq
	seqDesc := "省略，取 0 至存档最新"
	if len(seqArgs) == 2 {
		start, err := parseReportSeq(seqArgs[0], "起始操作序号")
		if err != nil {
			return err
		}
		end, err := parseReportSeq(seqArgs[1], "截止操作序号")
		if err != nil {
			return err
		}
		if start > end {
			return fmt.Errorf("起始操作序号 %d 晚于截止操作序号 %d，须满足 0 ≤ 起点 ≤ 终点 ≤ 全局序号上限", start, end)
		}
		if end > s.NextSeq {
			return fmt.Errorf("截止操作序号 %d 超过存档全局序号上限 %d", end, s.NextSeq)
		}
		startSeq, endSeq = start, end
		seqDesc = "指定"
	}

	// 只选择该客户账期范围内当前已存在的账单：不补结算、不按操作时间筛选。
	var selected []*bill
	for _, b := range s.Bills {
		if b.CustomerID == customerID && b.Month >= startMonth && b.Month <= endMonth {
			selected = append(selected, b)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Month < selected[j].Month })

	// 输出前核验所有选中账单的完整流水：任何异常（即使发生在终点序号之后）
	// 都拒绝，不输出部分报表。
	bills := make([]*reportBill, 0, len(selected))
	for _, b := range selected {
		events, err := billLedger(s, b)
		if err != nil {
			return err
		}
		bills = append(bills, &reportBill{bill: b, events: events})
	}

	events := mergeReportEvents(s, bills, startMonth, endMonth)
	printBillReport(cust, s.NextSeq, startMonth, endMonth, startSeq, endSeq, seqDesc, bills, events)
	return nil
}

// parseReportSeq 解析报表的起止操作序号：非负有符号 64 位整数。
func parseReportSeq(text, name string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q 不是非负整数: %w", name, text, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s不能为负数，收到 %d", name, v)
	}
	return v, nil
}

func printBillReport(cust *customer, nextSeq int64, startMonth, endMonth string,
	startSeq, endSeq int64, seqDesc string, bills []*reportBill, events []reportEvent) {
	fmt.Fprintln(stdout, "跨账期对账报表（只读，不改写存档）：")
	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "账期范围：%s 至 %s（含首尾，UTC 自然月）\n", startMonth, endMonth)
	fmt.Fprintf(stdout, "存档全局序号上限：%d\n", nextSeq)
	fmt.Fprintf(stdout, "操作序号起点：%d（%s）\n", startSeq, seqDesc)
	fmt.Fprintf(stdout, "操作序号终点：%d（%s）\n", endSeq, seqDesc)
	fmt.Fprintf(stdout, "选中账单：%d 张（范围内当前已存在的账单，不补结算）\n", len(bills))

	// 按月展示账单标识、原总金额及两端余额；起点为起点序号操作完成后、
	// 终点为终点序号操作完成后的状态。
	if len(bills) == 0 {
		fmt.Fprintln(stdout, "按月余额：无（范围内当前无已存在账单）")
	} else {
		fmt.Fprintf(stdout, "按月余额（起点为序号 %d 之后、终点为序号 %d 之后）：\n", startSeq, endSeq)
	}
	totalFee := new(big.Int)
	for i, rb := range bills {
		sp, sr := balanceAtSeq(rb.bill, rb.events, startSeq)
		ep, er := balanceAtSeq(rb.bill, rb.events, endSeq)
		totalFee.Add(totalFee, big.NewInt(rb.bill.TotalFee))
		fmt.Fprintf(stdout, "  %d. 月份 %s 账单 %s 原总金额 %d 分（%s）\n",
			i+1, rb.bill.Month, rb.bill.ID, rb.bill.TotalFee, moneyFen(rb.bill.TotalFee))
		fmt.Fprintf(stdout, "     起点：应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
			sp, moneyFen(sp), sr, moneyFen(sr), sp-sr, moneyFen(sp-sr))
		fmt.Fprintf(stdout, "     终点：应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
			ep, moneyFen(ep), er, moneyFen(er), ep-er, moneyFen(ep-er))
	}

	// 各列汇总：单账单余额恒在 [0, 有符号 64 位最大值] 内，多账单合计可能
	// 超出该上限，用任意精度整数精确输出。
	startSum := sumBalanceAt(bills, startSeq)
	endSum := sumBalanceAt(bills, endSeq)
	fmt.Fprintf(stdout, "汇总（%d 张账单，合计精确计算，可超有符号 64 位上限）：\n", len(bills))
	fmt.Fprintf(stdout, "  原总金额合计：%s 分（%s）\n", totalFee.String(), moneyFenBig(totalFee))
	fmt.Fprintf(stdout, "  起点合计：应付 %s 分（%s），实收 %s 分（%s），未收余额 %s 分（%s）\n",
		startSum.payable, moneyFenBig(startSum.payable),
		startSum.received, moneyFenBig(startSum.received),
		startSum.outstanding(), moneyFenBig(startSum.outstanding()))
	fmt.Fprintf(stdout, "  终点合计：应付 %s 分（%s），实收 %s 分（%s），未收余额 %s 分（%s）\n",
		endSum.payable, moneyFenBig(endSum.payable),
		endSum.received, moneyFenBig(endSum.received),
		endSum.outstanding(), moneyFenBig(endSum.outstanding()))

	// 区间内账后操作：起点之后、终点以内，按全局序号升序，每次操作只展示一次。
	var shown []reportEvent
	for _, ev := range events {
		if ev.seq > startSeq && ev.seq <= endSeq {
			shown = append(shown, ev)
		}
	}
	if len(shown) == 0 {
		fmt.Fprintf(stdout, "区间内账后操作（序号大于 %d 且不超过 %d）：无\n", startSeq, endSeq)
		return
	}
	fmt.Fprintf(stdout, "区间内账后操作（序号大于 %d 且不超过 %d，按全局序号升序）：\n", startSeq, endSeq)
	for i, ev := range shown {
		fmt.Fprintf(stdout, "  %d. %s\n", i+1, formatReportEvent(ev, bills))
	}
}

// formatReportEvent 渲染一条报表操作：序号、类型、记录标识、原因或备注、
// 撤销或更正关联、范围内各月的应付/实收变化，以及事件后的汇总余额。
func formatReportEvent(ev reportEvent, bills []*reportBill) string {
	var head string
	switch ev.kind {
	case "调整":
		head = fmt.Sprintf("序号 %d 调整 %s：原因：%s", ev.seq, ev.refID, ev.note)
	case "撤销调整":
		head = fmt.Sprintf("序号 %d 撤销调整 %s：原因：%s（关联序号 %d 的调整 %s）",
			ev.seq, ev.refID, ev.note, ev.linkSeq, ev.refID)
	case "收款":
		head = fmt.Sprintf("序号 %d 收款 %s：备注：%s（汇款总额 %d 分（%s））",
			ev.seq, ev.refID, ev.note, ev.payTotal, moneyFen(ev.payTotal))
	case "更正":
		head = fmt.Sprintf("序号 %d 更正 %s：原因：%s（关联收款 %s）",
			ev.seq, ev.refID, ev.note, ev.payID)
	default: // 撤销收款
		head = fmt.Sprintf("序号 %d 撤销收款 %s：原因：%s（关联序号 %d 的收款 %s，汇款总额 %d 分（%s））",
			ev.seq, ev.refID, ev.note, ev.linkSeq, ev.refID, ev.payTotal, moneyFen(ev.payTotal))
	}

	var b strings.Builder
	b.WriteString(head)
	if len(ev.months) == 0 {
		b.WriteString("\n     范围内变化：无（范围内各月余额不变）")
	} else {
		b.WriteString("\n     范围内变化：")
		for _, md := range ev.months {
			b.WriteString("\n       ")
			b.WriteString(formatReportMonthDelta(ev.kind, md))
		}
	}
	// 事件后的范围内汇总余额（全部选中账单在该序号完成后的合计）。
	sum := sumBalanceAt(bills, ev.seq)
	fmt.Fprintf(&b, "\n     事后汇总：应付 %s 分（%s），实收 %s 分（%s），未收余额 %s 分（%s）",
		sum.payable, moneyFenBig(sum.payable),
		sum.received, moneyFenBig(sum.received),
		sum.outstanding(), moneyFenBig(sum.outstanding()))
	return b.String()
}

// formatReportMonthDelta 渲染一次操作对范围内某个月账单的应付或实收影响。
func formatReportMonthDelta(kind string, md reportMonthDelta) string {
	switch kind {
	case "调整", "撤销调整":
		return fmt.Sprintf("%s 应付 %+d 分（%s，%s）",
			md.month, md.deltaPayable, moneyFen(md.deltaPayable), adjustKind(md.deltaPayable))
	case "收款":
		return fmt.Sprintf("%s 实收 %+d 分（%s，本账单分配 %d 分）",
			md.month, md.deltaReceived, moneyFen(md.deltaReceived), md.payAlloc)
	case "更正":
		return fmt.Sprintf("%s 实收 %+d 分（%s，本账单分配 %d 分 → %d 分）",
			md.month, md.deltaReceived, moneyFen(md.deltaReceived), md.beforeAlloc, md.afterAlloc)
	default: // 撤销收款
		return fmt.Sprintf("%s 实收 %+d 分（%s，取消本账单分配 %d 分）",
			md.month, md.deltaReceived, moneyFen(md.deltaReceived), md.payAlloc)
	}
}
