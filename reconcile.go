package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// --- 客户跨账期对账报表 ---
//
// bill reconcile 用一份只读报表核对一个客户在一段账后操作（全局操作序号
// 区间）内、多个账期（包含首尾的 YYYY-MM 范围）的余额变化：只选择范围内
// 当前已存在的账单（不补结算、不按操作时间筛选），各账单在序号 0 以原总
// 金额为应付、零实收起算；起止余额分别取对应序号操作完成后的状态，变动
// 区间为起点之后、终点以内。逐月两端余额与相同截止的 bill ledger 一致，
// 终点为存档最新序号时与 bill show 一致。区间内的调整/撤销、收款、更正、
// 收款撤销与退款按全局序号合并展示，跨月退款作为一次操作列出逐月变化。

// reconBill 是报表选中的一张账单及其在起止序号后的余额。
type reconBill struct {
	b         *bill
	startPay  int64 // 起点序号完成后的应付
	startRecv int64 // 起点序号完成后的实收
	endPay    int64 // 终点序号完成后的应付
	endRecv   int64 // 终点序号完成后的实收
}

// reconMonthChange 是一次账后操作对范围内某一个账期的金额影响。
type reconMonthChange struct {
	month         string
	deltaPayable  int64 // 对应付的影响（分，带符号）
	deltaReceived int64 // 对实收的影响（分，带符号）
	alloc         int64 // 收款：登记分配；撤销收款：被取消的最新分配；退款：该月退款额
	beforeAlloc   int64 // 更正：更正前分配
	afterAlloc    int64 // 更正：更正后分配
}

// reconEvent 是报表中合并展示的一次账后操作：调整、撤销调整、收款、
// 分配更正、归属转出、归属转入、撤销收款或退款。每次操作只出现一次，范围
// 内各月的变化合并在同一条目下；同名的调整、收款、更正与退款标识属于不同
// 类型的记录，按类型区分，互不混淆。归属转出与归属转入共用同一全局序号，
// 在两侧客户各自只出现一次、只计本侧金额；跨月退款作为一次操作列出逐月
// 变化，不按整笔总额重复累减。
type reconEvent struct {
	seq          int64
	kind         string // 调整 / 撤销调整 / 收款 / 更正 / 归属转出 / 归属转入 / 撤销收款 / 退款
	refID        string // 记录标识（调整、收款、更正、归属更正或退款标识）
	note         string // 原因（调整/更正/退款/撤销类）或备注（收款）
	linkSeq      int64  // 撤销事件关联的原操作序号；非撤销为 0
	payTotal     int64  // 收款类事件：汇款总额
	payID        string // 更正/归属/退款事件：关联收款标识
	fromCustomer string // 归属转入事件：转出客户
	toCustomer   string // 归属转出事件：转入客户标识
	changes      []reconMonthChange
	afterPay     int128 // 事件后范围内汇总应付（回放时填充）
	afterRecv    int128 // 事件后范围内汇总实收（回放时填充）
}

func cmdBillReconcile(dir, customerID, startMonth, endMonth string, seqArgs []string) error {
	if !validMonth(startMonth) {
		return fmt.Errorf("起始账期 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", startMonth)
	}
	if !validMonth(endMonth) {
		return fmt.Errorf("截止账期 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", endMonth)
	}
	if endMonth < startMonth {
		return fmt.Errorf("起始账期 %s 晚于截止账期 %s，账期范围包含首尾且起月不得晚于止月", startMonth, endMonth)
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
	// 0 至存档最新序号；须满足 0 ≤ 起点 ≤ 终点 ≤ 存档全局序号上限，因其他
	// 客户或月份操作造成的序号空档合法。
	startSeq, endSeq := int64(0), s.NextSeq
	seqDesc := "省略，按 0 至存档最新序号"
	if len(seqArgs) == 2 {
		from, err := parseReconSeq(seqArgs[0], "起始操作序号")
		if err != nil {
			return err
		}
		to, err := parseReconSeq(seqArgs[1], "截止操作序号")
		if err != nil {
			return err
		}
		if from > to {
			return fmt.Errorf("起始操作序号 %d 晚于截止操作序号 %d，须满足 0 ≤ 起点 ≤ 终点", from, to)
		}
		if to > s.NextSeq {
			return fmt.Errorf("截止操作序号 %d 超过存档全局序号上限 %d", to, s.NextSeq)
		}
		startSeq, endSeq = from, to
		seqDesc = "指定"
	}

	// 只选择该客户账期范围内当前已存在的账单，不补结算、不按操作时间筛选。
	var months []string
	for _, b := range s.Bills {
		if b.CustomerID == customerID && b.Month >= startMonth && b.Month <= endMonth {
			months = append(months, b.Month)
		}
	}
	sort.Strings(months)

	// 输出前核验所有选中账单的完整流水：业务归属、金额影响与逐步核验都由
	// 与 bill ledger 共用的 postbill 计算一次性完成。任何异常（即使发生在
	// 终点序号之后）都拒绝，不输出部分报表。逐月两端余额取自同一批已核验
	// 流水，与相同截止序号的 bill ledger 一致。
	ops, err := postbillOps(s, customerID)
	if err != nil {
		return err
	}
	selected := make([]*bill, 0, len(months))
	for _, m := range months {
		selected = append(selected, s.Bills[billKey(customerID, m)])
	}
	ledgers, err := verifyPostbillLedgers(ops, selected)
	if err != nil {
		return err
	}
	bills := make([]reconBill, 0, len(months))
	for _, m := range months {
		b := s.Bills[billKey(customerID, m)]
		events := ledgers[m]
		rb := reconBill{b: b}
		rb.startPay, rb.startRecv = ledgerBalanceAt(b, events, startSeq)
		rb.endPay, rb.endRecv = ledgerBalanceAt(b, events, endSeq)
		bills = append(bills, rb)
	}

	// 区间内操作与逐月余额来自同一套共享计算：范围筛选只决定哪些操作、哪些
	// 账期参与展示，不重复推导业务归属与金额影响。
	opsInRange := postbillRangeEvents(ops, startMonth, endMonth, startSeq, endSeq)

	// 汇总以 128 位整数精确累加：单账单余额保证在有符号 64 位内，多账单
	// 合计可能超出有符号 64 位上限，仍须精确输出。
	sumOrig := int128Of(0)
	startPaySum, startRecvSum := int128Of(0), int128Of(0)
	endPaySum, endRecvSum := int128Of(0), int128Of(0)
	for _, rb := range bills {
		sumOrig = sumOrig.add(int128Of(rb.b.TotalFee))
		startPaySum = startPaySum.add(int128Of(rb.startPay))
		startRecvSum = startRecvSum.add(int128Of(rb.startRecv))
		endPaySum = endPaySum.add(int128Of(rb.endPay))
		endRecvSum = endRecvSum.add(int128Of(rb.endRecv))
	}

	// 回放区间内操作，填充每次操作后的范围内汇总余额。
	curPay, curRecv := startPaySum, startRecvSum
	for i := range opsInRange {
		for _, ch := range opsInRange[i].changes {
			curPay = curPay.add(int128Of(ch.deltaPayable))
			curRecv = curRecv.add(int128Of(ch.deltaReceived))
		}
		opsInRange[i].afterPay, opsInRange[i].afterRecv = curPay, curRecv
	}
	// 操作变化与逐月余额来自同一批已核验流水，终点处必然一致。
	if curPay != endPaySum || curRecv != endRecvSum {
		return fmt.Errorf("区间内操作汇总与逐月终点余额不一致，数据异常，拒绝输出报表")
	}

	printReconcile(cust, startMonth, endMonth, startSeq, endSeq, seqDesc, s.NextSeq,
		bills, sumOrig, startPaySum, startRecvSum, endPaySum, endRecvSum, opsInRange)
	return nil
}

// parseReconSeq 解析操作序号：非负有符号 64 位整数。
func parseReconSeq(text, name string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q 不是非负整数: %w", name, text, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s不能为负数，收到 %d", name, v)
	}
	return v, nil
}

// ledgerBalanceAt 返回账单在指定操作序号完成后的应付与实收：序号 0 以原
// 总金额为应付、零实收起算，与 bill ledger 相同截止的余额一致。
func ledgerBalanceAt(b *bill, events []ledgerEvent, seq int64) (payable, received int64) {
	payable, received = b.TotalFee, 0
	for _, ev := range events {
		if ev.seq > seq {
			break
		}
		payable, received = ev.afterPayable, ev.afterReceived
	}
	return payable, received
}

func printReconcile(cust *customer, startMonth, endMonth string, startSeq, endSeq int64, seqDesc string, nextSeq int64,
	bills []reconBill, sumOrig, startPaySum, startRecvSum, endPaySum, endRecvSum int128, ops []reconEvent) {

	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "账期范围：%s 至 %s（UTC 自然月，包含首尾）\n", startMonth, endMonth)
	fmt.Fprintf(stdout, "操作序号区间：起点 %d、终点 %d（%s）\n", startSeq, endSeq, seqDesc)
	fmt.Fprintf(stdout, "存档全局序号上限：%d\n", nextSeq)
	fmt.Fprintf(stdout, "范围内账单：%d 张（当前已存在账单，不补结算；各账单在序号 0 以原总金额为应付、零实收起算）\n", len(bills))

	if len(bills) == 0 {
		fmt.Fprintln(stdout, "逐月余额：无（该客户在账期范围内当前没有已存在账单）")
	} else {
		fmt.Fprintf(stdout, "逐月余额（起点为序号 %d 完成后，终点为序号 %d 完成后）：\n", startSeq, endSeq)
		for i, rb := range bills {
			fmt.Fprintf(stdout, "  %d. 月份 %s：账单 %s，原总金额 %d 分（%s）\n",
				i+1, rb.b.Month, rb.b.ID, rb.b.TotalFee, moneyFen(rb.b.TotalFee))
			fmt.Fprintf(stdout, "     起点：应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
				rb.startPay, moneyFen(rb.startPay), rb.startRecv, moneyFen(rb.startRecv),
				rb.startPay-rb.startRecv, moneyFen(rb.startPay-rb.startRecv))
			fmt.Fprintf(stdout, "     终点：应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
				rb.endPay, moneyFen(rb.endPay), rb.endRecv, moneyFen(rb.endRecv),
				rb.endPay-rb.endRecv, moneyFen(rb.endPay-rb.endRecv))
		}
	}
	fmt.Fprintf(stdout, "各列汇总（%d 张账单）：\n", len(bills))
	fmt.Fprintf(stdout, "  原总金额合计：%s 分（%s）\n", sumOrig, moneyFen128(sumOrig))
	fmt.Fprintf(stdout, "  起点合计：应付 %s 分（%s），实收 %s 分（%s），未收余额 %s 分（%s）\n",
		startPaySum, moneyFen128(startPaySum), startRecvSum, moneyFen128(startRecvSum),
		startPaySum.sub(startRecvSum), moneyFen128(startPaySum.sub(startRecvSum)))
	fmt.Fprintf(stdout, "  终点合计：应付 %s 分（%s），实收 %s 分（%s），未收余额 %s 分（%s）\n",
		endPaySum, moneyFen128(endPaySum), endRecvSum, moneyFen128(endRecvSum),
		endPaySum.sub(endRecvSum), moneyFen128(endPaySum.sub(endRecvSum)))

	if len(ops) == 0 {
		fmt.Fprintf(stdout, "区间内账后操作：无（序号 %d 之后、%d 以内无涉及范围内账单的操作）\n", startSeq, endSeq)
	} else {
		fmt.Fprintf(stdout, "区间内账后操作（序号 %d 之后、%d 以内，按全局操作序号升序）：\n", startSeq, endSeq)
		for i, ev := range ops {
			fmt.Fprintf(stdout, "  %d. %s\n", i+1, formatReconEvent(ev))
		}
	}
	fmt.Fprintln(stdout, "说明：逐月两端余额与相同截止序号的 bill ledger 一致；终点为存档最新序号时与 bill show 一致。")
}

// formatReconEvent 渲染一条合并后的账后操作：序号、类型、记录标识、原因
// 或备注、撤销或更正关联、范围内各月的应付及实收变化，以及事件后的范围
// 内汇总余额；收款类事件同时说明汇款总额（总额不计入范围内实收）。
func formatReconEvent(ev reconEvent) string {
	parts := make([]string, len(ev.changes))
	for i, ch := range ev.changes {
		switch ev.kind {
		case "调整", "撤销调整":
			parts[i] = fmt.Sprintf("%s 应付 %+d 分", ch.month, ch.deltaPayable)
		case "收款":
			parts[i] = fmt.Sprintf("%s 实收 %+d 分（分配 %d 分）", ch.month, ch.deltaReceived, ch.alloc)
		case "更正":
			parts[i] = fmt.Sprintf("%s 实收 %+d 分（分配 %d 分 → %d 分）", ch.month, ch.deltaReceived, ch.beforeAlloc, ch.afterAlloc)
		case "归属转出":
			parts[i] = fmt.Sprintf("%s 实收 %+d 分（转出 %d 分至客户 %s）", ch.month, ch.deltaReceived, ch.alloc, ev.toCustomer)
		case "归属转入":
			parts[i] = fmt.Sprintf("%s 实收 %+d 分（转入 %d 分）", ch.month, ch.deltaReceived, ch.alloc)
		case "退款":
			parts[i] = fmt.Sprintf("%s 实收 %+d 分（退款 %d 分）", ch.month, ch.deltaReceived, ch.alloc)
		default: // 撤销收款
			parts[i] = fmt.Sprintf("%s 实收 %+d 分（取消分配 %d 分）", ch.month, ch.deltaReceived, ch.alloc)
		}
	}
	changes := strings.Join(parts, "，")
	after := fmt.Sprintf("事后汇总：应付 %s 分，实收 %s 分，未收余额 %s 分",
		ev.afterPay, ev.afterRecv, ev.afterPay.sub(ev.afterRecv))
	switch ev.kind {
	case "调整":
		return fmt.Sprintf("序号 %d 调整 %s：原因：%s；范围内变化：%s → %s",
			ev.seq, ev.refID, ev.note, changes, after)
	case "撤销调整":
		return fmt.Sprintf("序号 %d 撤销调整 %s（关联序号 %d 的调整 %s）：原因：%s；范围内变化：%s → %s",
			ev.seq, ev.refID, ev.linkSeq, ev.refID, ev.note, changes, after)
	case "收款":
		return fmt.Sprintf("序号 %d 收款 %s：备注：%s；汇款总额 %d 分（%s）；范围内变化：%s → %s",
			ev.seq, ev.refID, ev.note, ev.payTotal, moneyFen(ev.payTotal), changes, after)
	case "更正":
		return fmt.Sprintf("序号 %d 更正 %s（关联收款 %s）：原因：%s；范围内变化：%s → %s",
			ev.seq, ev.refID, ev.payID, ev.note, changes, after)
	case "归属转出":
		return fmt.Sprintf("序号 %d 归属转出 %s（关联收款 %s，转入客户 %s）：原因：%s；汇款总额 %d 分（%s）；范围内变化：%s → %s",
			ev.seq, ev.refID, ev.payID, ev.toCustomer, ev.note, ev.payTotal, moneyFen(ev.payTotal), changes, after)
	case "归属转入":
		return fmt.Sprintf("序号 %d 归属转入 %s（关联收款 %s，自客户 %s 转入）：原因：%s；汇款总额 %d 分（%s）；范围内变化：%s → %s",
			ev.seq, ev.refID, ev.payID, ev.fromCustomer, ev.note, ev.payTotal, moneyFen(ev.payTotal), changes, after)
	case "退款":
		return fmt.Sprintf("序号 %d 退款 %s（关联收款 %s）：原因：%s；范围内变化：%s → %s",
			ev.seq, ev.refID, ev.payID, ev.note, changes, after)
	default: // 撤销收款
		return fmt.Sprintf("序号 %d 撤销收款 %s（关联序号 %d 的收款 %s）：原因：%s；汇款总额 %d 分（%s）；范围内变化：%s → %s",
			ev.seq, ev.refID, ev.linkSeq, ev.refID, ev.note, ev.payTotal, moneyFen(ev.payTotal), changes, after)
	}
}
