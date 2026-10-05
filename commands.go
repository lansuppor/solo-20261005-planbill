package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 用量导入文件（CSV，UTF-8，首行为固定表头，之后每行一条用量）：
//
//	usage_id,customer_id,time,quantity
//	u-001,cust-1,2026-09-15T10:00:00Z,100
//
//	usage_id   全局唯一用量标识，非空
//	customer_id 必须是已登记的客户标识
//	time       RFC3339 时间，如 2026-09-15T10:00:00Z 或 2026-09-15T18:00:00+08:00
//	quantity   正整数数量（有符号 64 位整数范围内）
const usageHeader = "usage_id,customer_id,time,quantity"

// runCmd 解析并执行子命令。业务/用法错误通过 error 返回，由 main 统一
// 写入标准错误并以非零状态退出。
func runCmd(args []string, dataDir string) error {
	switch args[0] {
	case "customer":
		if len(args) < 2 {
			return usageError("缺少子命令，应为：customer add <标识> <名称> <单价分>")
		}
		switch args[1] {
		case "add":
			if len(args) != 5 {
				return usageError("用法：customer add <标识> <名称> <单价分>")
			}
			return cmdCustomerAdd(dataDir, args[2], args[3], args[4])
		default:
			return usageError("未知 customer 子命令 %q；可用：add", args[1])
		}

	case "usage":
		if len(args) < 2 {
			return usageError("缺少子命令，应为：usage import <文件>")
		}
		switch args[1] {
		case "import":
			if len(args) != 3 {
				return usageError("用法：usage import <文件>（- 表示标准输入）")
			}
			return cmdUsageImport(dataDir, args[2])
		default:
			return usageError("未知 usage 子命令 %q；可用：import", args[1])
		}

	case "bill":
		if len(args) < 2 {
			return usageError("缺少子命令，应为：bill settle|show|adjust|revoke|pay|remit|unpay ...")
		}
		switch args[1] {
		case "settle":
			if len(args) != 4 {
				return usageError("用法：bill settle <客户标识> <YYYY-MM>")
			}
			return cmdBillSettle(dataDir, args[2], args[3])
		case "show":
			if len(args) != 4 {
				return usageError("用法：bill show <客户标识> <YYYY-MM>")
			}
			return cmdBillShow(dataDir, args[2], args[3])
		case "adjust":
			if len(args) != 7 {
				return usageError("用法：bill adjust <客户标识> <YYYY-MM> <调整标识> <金额分> <原因>")
			}
			return cmdBillAdjust(dataDir, args[2], args[3], args[4], args[5], args[6])
		case "revoke":
			if len(args) != 4 {
				return usageError("用法：bill revoke <调整标识> <原因>")
			}
			return cmdBillRevoke(dataDir, args[2], args[3])
		case "pay":
			if len(args) != 7 {
				return usageError("用法：bill pay <客户标识> <YYYY-MM> <收款标识> <金额分> <备注>")
			}
			return cmdBillPay(dataDir, args[2], args[3], args[4], args[5], args[6])
		case "remit":
			if len(args) < 6 {
				return usageError("用法：bill remit <客户标识> <收款标识> <总金额分> <备注> <YYYY-MM:金额分> [更多 月份:金额 ...]")
			}
			return cmdBillRemit(dataDir, args[2], args[3], args[4], args[5], args[6:])
		case "unpay":
			if len(args) != 4 {
				return usageError("用法：bill unpay <收款标识> <原因>")
			}
			return cmdBillUnpay(dataDir, args[2], args[3])
		default:
			return usageError("未知 bill 子命令 %q；可用：settle、show、adjust、revoke、pay、remit、unpay", args[1])
		}

	default:
		return usageError("未知命令 %q；可用：customer、usage、bill", args[0])
	}
}

type usageErrorf string

func (e usageErrorf) Error() string { return string(e) }

func usageError(format string, a ...any) error {
	return usageErrorf(fmt.Sprintf(format, a...))
}

func cmdCustomerAdd(dir, id, name, priceText string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("客户标识不能为空")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("客户名称不能为空")
	}
	price, err := parsePrice(priceText)
	if err != nil {
		return err
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	if _, exists := s.Customers[id]; exists {
		return fmt.Errorf("客户标识 %q 已存在，客户单价创建后不可修改", id)
	}
	s.Customers[id] = &customer{ID: id, Name: name, Price: price}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已登记客户 %q（%s），固定单价 %d 分（%s）\n", id, name, price, moneyFen(price))
	return nil
}

func parsePrice(text string) (int64, error) {
	text = strings.TrimSpace(text)
	price, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("单价 %q 不是有符号 64 位整数范围内的整数: %w", text, err)
	}
	if price < 0 {
		return 0, fmt.Errorf("单价必须是非负整数分，收到 %d", price)
	}
	return price, nil
}

// parsedRow 是导入文件中一行解析后的候选用量。
type parsedRow struct {
	line int // 文件中的行号（含表头），用于报错定位
	rec  usageRecord
	t    time.Time
}

func cmdUsageImport(dir, file string) error {
	var reader io.ReadCloser
	var source string
	if file == "-" {
		reader = io.NopCloser(os.Stdin)
		source = "标准输入"
	} else {
		f, err := os.Open(file)
		if err != nil {
			return fmt.Errorf("无法打开用量文件 %s: %w", file, err)
		}
		reader = f
		source = file
	}
	defer reader.Close()

	rows, parseErrs := parseUsageFile(reader)

	s, err := loadStore(dir)
	if err != nil {
		return err
	}

	// 先校验：任何问题都在此阶段收集，全部通过后才一次性修改并保存，
	// 保证“整批导入先校验再生效”，失败不留下任何新用量。
	var problems []string
	problems = append(problems, parseErrs...)

	accepted := make(map[string]*parsedRow) // 本批内首次出现、内容确定的记录
	var ordered []*parsedRow
	newCount, dupCount := 0, 0

	for _, r := range rows {
		cust, ok := s.Customers[r.rec.CustomerID]
		if !ok {
			problems = append(problems, fmt.Sprintf("第 %d 行：客户标识 %q 不存在", r.line, r.rec.CustomerID))
			continue
		}
		// 金额可行性预检：数量×固定单价不得溢出（金额单位：分）。
		if _, err := mul64(r.rec.Quantity, cust.Price); err != nil {
			problems = append(problems, fmt.Sprintf("第 %d 行：数量 %d × 单价 %d 金额溢出有符号 64 位整数范围", r.line, r.rec.Quantity, cust.Price))
			continue
		}

		// 去重判定同时对“库中已有”和“本文件内已出现”生效。
		var existing *usageRecord
		if inStore, ok := s.Usage[r.rec.ID]; ok {
			existing = inStore
		} else if inBatch, ok := accepted[r.rec.ID]; ok {
			existing = &inBatch.rec
		}

		if existing != nil {
			if !sameUsage(existing, &r.rec, r.t) {
				problems = append(problems, fmt.Sprintf("第 %d 行：用量标识 %q 已存在但内容不同（已有客户=%s 时间=%s 数量=%d）",
					r.line, r.rec.ID, existing.CustomerID, existing.Time, existing.Quantity))
				continue
			}
			dupCount++
			continue
		}

		// 全新标识：不得进入该客户已封账的 UTC 自然月。
		month := r.t.UTC().Format("2006-01")
		if s.sealed(r.rec.CustomerID, month) {
			problems = append(problems, fmt.Sprintf("第 %d 行：客户 %s 的 %s 已封账，新用量 %q 不得进入",
				r.line, r.rec.CustomerID, month, r.rec.ID))
			continue
		}

		rr := r
		accepted[r.rec.ID] = rr
		ordered = append(ordered, rr)
		newCount++
	}

	if len(problems) > 0 {
		return fmt.Errorf("导入失败，整批未生效（共 %d 个问题）：\n  %s", len(problems), strings.Join(problems, "\n  "))
	}

	for _, r := range ordered {
		rec := r.rec
		s.Usage[rec.ID] = &rec
	}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "导入完成（%s）：新增 %d 条，重复跳过 %d 条\n", source, newCount, dupCount)
	return nil
}

// parseUsageFile 完整读取 CSV，做格式层面的解析；业务层面的校验在调用方完成。
func parseUsageFile(r io.Reader) ([]*parsedRow, []string) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = 4
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err == io.EOF {
		return nil, []string{"文件为空：首行必须是表头 " + usageHeader}
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("无法读取表头：%v（首行必须是 %s）", err, usageHeader)}
	}
	for i := range header {
		header[i] = strings.TrimSpace(header[i])
	}
	if len(header) != 4 || header[0] != "usage_id" || header[1] != "customer_id" ||
		header[2] != "time" || header[3] != "quantity" {
		return nil, []string{fmt.Sprintf("表头必须是 %s，实际为 %s", usageHeader, strings.Join(header, ","))}
	}

	var rows []*parsedRow
	var problems []string
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			problems = append(problems, fmt.Sprintf("第 %d 行：CSV 格式错误：%v", line, err))
			if pe, ok := err.(*csv.ParseError); ok && pe.Err == csv.ErrFieldCount {
				// 字段数错误后继续读取仍可能拿到错乱记录，直接终止解析。
				break
			}
			continue
		}
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}

		id, custID, ts, qtyText := rec[0], rec[1], rec[2], rec[3]
		if id == "" {
			problems = append(problems, fmt.Sprintf("第 %d 行：用量标识不能为空", line))
			continue
		}
		if custID == "" {
			problems = append(problems, fmt.Sprintf("第 %d 行：客户标识不能为空", line))
			continue
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			problems = append(problems, fmt.Sprintf("第 %d 行：时间 %q 不是 RFC3339 格式：%v", line, ts, err))
			continue
		}
		qty, err := strconv.ParseInt(qtyText, 10, 64)
		if err != nil {
			problems = append(problems, fmt.Sprintf("第 %d 行：数量 %q 不是有符号 64 位整数", line, qtyText))
			continue
		}
		if qty <= 0 {
			problems = append(problems, fmt.Sprintf("第 %d 行：数量必须是正整数，收到 %d", line, qty))
			continue
		}

		rows = append(rows, &parsedRow{
			line: line,
			rec:  usageRecord{ID: id, CustomerID: custID, Time: ts, Quantity: qty},
			t:    t,
		})
	}
	return rows, problems
}

// sameUsage 按（客户、解析后的时间点、数量）判定两条记录内容是否相同，
// 时间比较的是瞬间，因此 Z 与 +08:00 表示同一时刻视为相同。
func sameUsage(existing *usageRecord, candidate *usageRecord, candidateT time.Time) bool {
	if existing.CustomerID != candidate.CustomerID || existing.Quantity != candidate.Quantity {
		return false
	}
	et, err := time.Parse(time.RFC3339, existing.Time)
	if err != nil {
		return existing.Time == candidate.Time
	}
	return et.Equal(candidateT)
}

// validMonth 严格校验 YYYY-MM（月份两位、01..12）。
func validMonth(m string) bool {
	t, err := time.Parse("2006-01", m)
	if err != nil || t.Format("2006-01") != m || t.Year() < 1 {
		return false
	}
	return true
}

// inMonth 判定一条 RFC3339 时间（换算 UTC 后）是否落在给定自然月。
func inMonth(rfc3339, month string) bool {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return false
	}
	return t.UTC().Format("2006-01") == month
}

func cmdBillSettle(dir, customerID, month string) error {
	if !validMonth(month) {
		return fmt.Errorf("月份 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", month)
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	cust, ok := s.Customers[customerID]
	if !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}

	key := billKey(customerID, month)
	if existing, ok := s.Bills[key]; ok {
		// 幂等：同一客户同一月份重复结算，直接返回原账单，
		// 不重新计费、不产生新账单或调整、不落盘。
		fmt.Fprintf(stdout, "客户 %s 的 %s 已结算，返回原账单（幂等，不重新计费）：\n\n", customerID, month)
		printBill(existing, cust, adjustmentsFor(s, customerID, month), paymentsFor(s, customerID, month))
		return nil
	}

	// 归集该客户 UTC 自然月内的全部用量，区间为左闭右开 [月初, 下月初)。
	var inMonthRecs []*usageRecord
	for _, u := range s.Usage {
		if u.CustomerID == customerID && inMonth(u.Time, month) {
			inMonthRecs = append(inMonthRecs, u)
		}
	}
	if len(inMonthRecs) == 0 {
		return fmt.Errorf("客户 %s 在 %s 没有用量，拒绝结算且不封账", customerID, month)
	}
	sortByInstant(inMonthRecs)

	lines := make([]billLine, 0, len(inMonthRecs))
	var totalQty, totalFee int64
	for _, u := range inMonthRecs {
		lineFee, err := mul64(u.Quantity, cust.Price)
		if err != nil {
			return fmt.Errorf("用量 %s：数量 %d × 单价 %d 金额溢出，拒绝结算且不封账", u.ID, u.Quantity, cust.Price)
		}
		totalQty, err = add64(totalQty, u.Quantity)
		if err != nil {
			return fmt.Errorf("汇总数量溢出有符号 64 位整数范围，拒绝结算且不封账")
		}
		totalFee, err = add64(totalFee, lineFee)
		if err != nil {
			return fmt.Errorf("汇总金额溢出有符号 64 位整数范围，拒绝结算且不封账")
		}
		lines = append(lines, billLine{
			UsageID:  u.ID,
			Time:     u.Time,
			Quantity: u.Quantity,
			LineFee:  lineFee,
		})
	}

	b := &bill{
		ID:         stableBillID(customerID, month),
		CustomerID: customerID,
		Month:      month,
		TotalQty:   totalQty,
		UnitPrice:  cust.Price,
		TotalFee:   totalFee,
		Lines:      lines,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.Bills[key] = b

	// 账单与封账状态在同一次原子保存中一起持久化；保存失败则一切不生效。
	if err := s.save(); err != nil {
		delete(s.Bills, key)
		return err
	}
	fmt.Fprintf(stdout, "结算完成，客户 %s 的 %s 已封账：\n\n", customerID, month)
	printBill(b, cust, nil, nil)
	return nil
}

func cmdBillShow(dir, customerID, month string) error {
	if !validMonth(month) {
		return fmt.Errorf("月份 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", month)
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	cust, ok := s.Customers[customerID]
	if !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}
	b, ok := s.Bills[billKey(customerID, month)]
	if !ok {
		return fmt.Errorf("客户 %s 的 %s 尚无账单（未结算）", customerID, month)
	}
	printBill(b, cust, adjustmentsFor(s, customerID, month), paymentsFor(s, customerID, month))
	return nil
}

func sortByInstant(us []*usageRecord) {
	// 入参时间均已在载入/导入时校验为 RFC3339。
	sort.Slice(us, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, us[i].Time)
		tj, _ := time.Parse(time.RFC3339, us[j].Time)
		ui, uj := ti.UTC(), tj.UTC()
		if !ui.Equal(uj) {
			return ui.Before(uj)
		}
		return us[i].ID < us[j].ID
	})
}

// stableBillID 由（客户标识，月份）确定性派生：跨进程稳定，且客户/月份间唯一。
func stableBillID(customerID, month string) string {
	sum := sha256.Sum256([]byte(customerID + "\x00" + month))
	return "BILL-" + hex.EncodeToString(sum[:])[:16]
}

func printBill(b *bill, cust *customer, adjs []*adjustment, pays []*payment) {
	fmt.Fprintf(stdout, "账单标识：%s\n", b.ID)
	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "月份：%s（UTC 自然月，左闭右开）\n", b.Month)
	fmt.Fprintf(stdout, "单价：%d 分（%s）\n", b.UnitPrice, moneyFen(b.UnitPrice))
	fmt.Fprintf(stdout, "总数量：%d\n", b.TotalQty)
	fmt.Fprintf(stdout, "总金额：%d 分（%s）\n", b.TotalFee, moneyFen(b.TotalFee))
	// 数据在载入时已校验一致，此处计算不会出错。
	net, payable, _ := billTotals(b, adjs)
	received, _ := paymentReceived(pays, b.Month)
	outstanding := payable - received // 不变量保证 0 ≤ 实收 ≤ 当前应付
	fmt.Fprintf(stdout, "调整净额：%+d 分（%s）\n", net, moneyFen(net))
	fmt.Fprintf(stdout, "当前应付：%d 分（%s）\n", payable, moneyFen(payable))
	fmt.Fprintf(stdout, "实收：%d 分（%s）\n", received, moneyFen(received))
	fmt.Fprintf(stdout, "未收余额：%d 分（%s）\n", outstanding, moneyFen(outstanding))
	fmt.Fprintln(stdout, "明细：")
	for i, ln := range b.Lines {
		fmt.Fprintf(stdout, "  %d. 用量标识=%s 时间=%s 数量=%d 小计=%d 分（%s）\n",
			i+1, ln.UsageID, ln.Time, ln.Quantity, ln.LineFee, moneyFen(ln.LineFee))
	}
	if len(adjs) == 0 {
		fmt.Fprintln(stdout, "调整与撤销历史：无")
	} else {
		fmt.Fprintln(stdout, "调整与撤销历史（按成功操作顺序）：")
		for i, ev := range historyEvents(adjs) {
			fmt.Fprintf(stdout, "  %d. %s\n", i+1, ev)
		}
	}
	if len(pays) == 0 {
		fmt.Fprintln(stdout, "收款与撤销历史：无")
	} else {
		fmt.Fprintln(stdout, "收款与撤销历史（按成功操作顺序）：")
		for i, ev := range paymentHistoryEvents(pays, b.Month) {
			fmt.Fprintf(stdout, "  %d. %s\n", i+1, ev)
		}
	}
}

// historyEvents 把调整与撤销展开为按操作序号排序的可读历史条目。
func historyEvents(adjs []*adjustment) []string {
	type event struct {
		seq  int64
		text string
	}
	var events []event
	for _, a := range adjs {
		events = append(events, event{a.Seq, fmt.Sprintf("调整 %s：%s %+d 分（%s），原因：%s，当前状态：%s",
			a.ID, adjustKind(a.Amount), a.Amount, moneyFen(a.Amount), a.Reason, adjustStatus(a))})
		if a.Revoked {
			events = append(events, event{a.RevokeSeq, fmt.Sprintf("撤销 %s：原因：%s（关联调整 %s 的%s %+d 分）",
				a.ID, a.RevokeReason, a.ID, adjustKind(a.Amount), a.Amount)})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].seq < events[j].seq })
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.text
	}
	return out
}

func adjustKind(amount int64) string {
	if amount > 0 {
		return "补收"
	}
	return "减免"
}

func adjustStatus(a *adjustment) string {
	if a.Revoked {
		return "已撤销"
	}
	return "生效中"
}

// paymentHistoryEvents 把收款登记与撤销展开为按操作序号排序的可读历史条目；
// month 为当前展示账单所在月份，用于给出该收款在本账单的分配。
func paymentHistoryEvents(pays []*payment, month string) []string {
	type event struct {
		seq  int64
		text string
	}
	var events []event
	for _, p := range pays {
		alloc := p.amountFor(month)
		events = append(events, event{p.Seq, fmt.Sprintf("收款 %s：总额 %d 分（%s），本账单分配 %d 分（%s），备注：%s，当前状态：%s（客户 %s）",
			p.ID, p.Total, moneyFen(p.Total), alloc, moneyFen(alloc), p.Note, paymentStatus(p), p.CustomerID)})
		if p.Revoked {
			events = append(events, event{p.RevokeSeq, fmt.Sprintf("撤销收款 %s：原因：%s（关联收款 %s，总额 %d 分，本账单分配 %d 分）",
				p.ID, p.RevokeReason, p.ID, p.Total, alloc)})
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].seq < events[j].seq })
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.text
	}
	return out
}

func paymentStatus(p *payment) string {
	if p.Revoked {
		return "已撤销"
	}
	return "实收中"
}

// parseAmount 解析调整金额：有符号 64 位整数、非零（正数补收、负数减免）。
func parseAmount(text string) (int64, error) {
	text = strings.TrimSpace(text)
	amount, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("调整金额 %q 不是有符号 64 位整数范围内的整数: %w", text, err)
	}
	if amount == 0 {
		return 0, fmt.Errorf("调整金额不能为零分（正数补收、负数减免）")
	}
	return amount, nil
}

func cmdBillAdjust(dir, customerID, month, adjID, amountText, reason string) error {
	if !validMonth(month) {
		return fmt.Errorf("月份 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", month)
	}
	if strings.TrimSpace(adjID) == "" {
		return fmt.Errorf("调整标识不能为空")
	}
	amount, err := parseAmount(amountText)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("调整原因不能为空")
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	if _, ok := s.Customers[customerID]; !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}
	b, ok := s.Bills[billKey(customerID, month)]
	if !ok {
		return fmt.Errorf("客户 %s 的 %s 尚无账单（未结算），费用调整只能作用于已存在账单", customerID, month)
	}

	if existing, ok := s.Adjustments[adjID]; ok {
		// 相同标识按客户、月份、金额、原因判断重复：内容相同返回已保存
		// 记录，不再次增减应付；已撤销的重放仍返回已撤销状态，不重新生效。
		if existing.CustomerID == customerID && existing.Month == month &&
			existing.Amount == amount && existing.Reason == reason {
			fmt.Fprintf(stdout, "调整 %q 已存在且内容相同，返回已保存记录（不重复增减应付）：\n\n", adjID)
			printAdjustment(existing, s)
			return nil
		}
		return fmt.Errorf("调整标识 %q 已存在但内容不同（已有：客户=%s 月份=%s 金额=%d 原因=%q），拒绝复用",
			adjID, existing.CustomerID, existing.Month, existing.Amount, existing.Reason)
	}

	// 应付可行性预检：原总金额 + 全部未撤销调整 + 本次金额必须落在
	// [0, 有符号 64 位最大值] 内，且不得低于已登记实收，否则拒绝且
	// 不占用该调整标识。
	_, payable, err := billTotals(b, adjustmentsFor(s, customerID, month))
	if err != nil {
		return fmt.Errorf("账单当前应付异常，拒绝调整: %w", err)
	}
	received, err := paymentReceived(paymentsFor(s, customerID, month), month)
	if err != nil {
		return fmt.Errorf("实收累计异常，拒绝调整: %w", err)
	}
	newPayable, err := addSigned64(payable, amount)
	if err != nil {
		return fmt.Errorf("调整后当前应付超出有符号 64 位最大值（当前 %d 分，本次 %+d 分），拒绝调整", payable, amount)
	}
	if newPayable < 0 {
		return fmt.Errorf("调整后当前应付将为 %d 分（小于 0，当前 %d 分，本次 %+d 分），拒绝调整", newPayable, payable, amount)
	}
	if newPayable < received {
		return fmt.Errorf("调整后当前应付 %d 分将低于实收 %d 分（当前 %d 分，本次 %+d 分），拒绝调整；可先撤销误登记的收款再重试",
			newPayable, received, payable, amount)
	}

	a := &adjustment{
		ID:         adjID,
		CustomerID: customerID,
		Month:      month,
		Amount:     amount,
		Reason:     reason,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.NextSeq++
	a.Seq = s.NextSeq
	s.Adjustments[adjID] = a

	// 调整记录与序号在同一次原子保存中持久化；保存失败则一切不生效，
	// 该调整标识不被占用。
	if err := s.save(); err != nil {
		delete(s.Adjustments, adjID)
		s.NextSeq--
		return err
	}
	fmt.Fprintf(stdout, "已登记%s调整 %q，客户 %s 的 %s 当前应付 %d 分（%s）：\n\n",
		adjustKind(amount), adjID, customerID, month, newPayable, moneyFen(newPayable))
	printAdjustment(a, s)
	return nil
}

func cmdBillRevoke(dir, adjID, reason string) error {
	if strings.TrimSpace(adjID) == "" {
		return fmt.Errorf("调整标识不能为空")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("撤销原因不能为空")
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	a, ok := s.Adjustments[adjID]
	if !ok {
		return fmt.Errorf("调整标识 %q 不存在，无法撤销", adjID)
	}

	if a.Revoked {
		// 相同标识与相同原因重复撤销：幂等成功，不新增记录；
		// 改用其他原因则拒绝。
		if a.RevokeReason == reason {
			fmt.Fprintf(stdout, "调整 %q 已撤销且撤销原因相同，幂等返回（不新增记录）：\n\n", adjID)
			printAdjustment(a, s)
			return nil
		}
		return fmt.Errorf("调整 %q 已撤销（撤销原因 %q），改用其他原因重复撤销被拒绝", adjID, a.RevokeReason)
	}

	// 撤销只取消该笔调整的金额影响；若撤销后当前应付越界或低于实收则
	// 拒绝，该调整仍保持有效，可先撤销误登记的收款或在其他合法调整
	// 改变余额后重试。
	b := s.Bills[billKey(a.CustomerID, a.Month)] // 载入时已校验存在
	_, payable, err := billTotals(b, adjustmentsFor(s, a.CustomerID, a.Month))
	if err != nil {
		return fmt.Errorf("账单当前应付异常，拒绝撤销: %w", err)
	}
	received, err := paymentReceived(paymentsFor(s, a.CustomerID, a.Month), a.Month)
	if err != nil {
		return fmt.Errorf("实收累计异常，拒绝撤销: %w", err)
	}
	neg, err := neg64(a.Amount)
	if err != nil {
		return fmt.Errorf("撤销后当前应付将超出有符号 64 位最大值，拒绝撤销；该调整仍有效，可在其他合法调整改变余额后重试")
	}
	newPayable, err := addSigned64(payable, neg)
	if err != nil {
		return fmt.Errorf("撤销后当前应付将超出有符号 64 位最大值（当前 %d 分，取消 %+d 分），拒绝撤销；该调整仍有效，可在其他合法调整改变余额后重试",
			payable, a.Amount)
	}
	if newPayable < 0 {
		return fmt.Errorf("撤销后当前应付将为 %d 分（小于 0，当前 %d 分，取消 %+d 分），拒绝撤销；该调整仍有效，可在其他合法调整改变余额后重试",
			newPayable, payable, a.Amount)
	}
	if newPayable < received {
		return fmt.Errorf("撤销后当前应付 %d 分将低于实收 %d 分（当前 %d 分，取消 %+d 分），拒绝撤销；可先撤销误登记的收款再重试",
			newPayable, received, payable, a.Amount)
	}

	a.Revoked = true
	a.RevokeReason = reason
	a.RevokedAt = time.Now().UTC().Format(time.RFC3339)
	s.NextSeq++
	a.RevokeSeq = s.NextSeq

	// 撤销信息与原记录在同一次原子保存中持久化；保存失败则一切不生效。
	if err := s.save(); err != nil {
		a.Revoked = false
		a.RevokeReason = ""
		a.RevokedAt = ""
		a.RevokeSeq = 0
		s.NextSeq--
		return err
	}
	fmt.Fprintf(stdout, "已撤销调整 %q（%s %+d 分），客户 %s 的 %s 当前应付 %d 分（%s）：\n\n",
		adjID, adjustKind(a.Amount), a.Amount, a.CustomerID, a.Month, newPayable, moneyFen(newPayable))
	printAdjustment(a, s)
	return nil
}

// printAdjustment 输出单笔调整记录及其当前状态；payable 取自当前库状态。
func printAdjustment(a *adjustment, s *state) {
	b := s.Bills[billKey(a.CustomerID, a.Month)]
	_, payable, _ := billTotals(b, adjustmentsFor(s, a.CustomerID, a.Month))
	fmt.Fprintf(stdout, "调整标识：%s\n", a.ID)
	fmt.Fprintf(stdout, "客户：%s\n", a.CustomerID)
	fmt.Fprintf(stdout, "月份：%s\n", a.Month)
	fmt.Fprintf(stdout, "调整金额：%+d 分（%s，%s）\n", a.Amount, moneyFen(a.Amount), adjustKind(a.Amount))
	fmt.Fprintf(stdout, "原因：%s\n", a.Reason)
	if a.Revoked {
		fmt.Fprintf(stdout, "当前状态：已撤销（撤销原因：%s）\n", a.RevokeReason)
	} else {
		fmt.Fprintf(stdout, "当前状态：生效中\n")
	}
	fmt.Fprintf(stdout, "当前应付：%d 分（%s）\n", payable, moneyFen(payable))
}

// parsePositiveAmount 解析收款金额：正整数分（不接受零、负数、小数）。
func parsePositiveAmount(text string) (int64, error) {
	text = strings.TrimSpace(text)
	amount, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("收款金额 %q 不是有符号 64 位整数范围内的整数: %w", text, err)
	}
	if amount <= 0 {
		return 0, fmt.Errorf("收款金额必须是正整数分，收到 %d", amount)
	}
	return amount, nil
}

func cmdBillPay(dir, customerID, month, payID, amountText, note string) error {
	if !validMonth(month) {
		return fmt.Errorf("月份 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", month)
	}
	amount, err := parsePositiveAmount(amountText)
	if err != nil {
		return err
	}
	// 单账单收款视为只有一项分配的汇款，与 bill remit 共用同一核心。
	return registerPayment(dir, customerID, payID, amount, note,
		[]paymentAllocation{{Month: month, Amount: amount}})
}

func cmdBillRemit(dir, customerID, payID, totalText, note string, allocArgs []string) error {
	total, err := parsePositiveAmount(totalText)
	if err != nil {
		return err
	}
	allocs, err := parseAllocations(allocArgs)
	if err != nil {
		return err
	}
	// 分配合计必须等于总额，全程整数运算，溢出拒绝。
	var sum int64
	for _, al := range allocs {
		sum, err = add64(sum, al.Amount)
		if err != nil {
			return fmt.Errorf("分配金额合计溢出有符号 64 位整数范围，拒绝登记")
		}
	}
	if sum != total {
		return fmt.Errorf("分配合计 %d 分与收款总额 %d 分不一致，拒绝登记", sum, total)
	}
	return registerPayment(dir, customerID, payID, total, note, allocs)
}

// parseAllocations 解析 <YYYY-MM:金额分> 形式的分配列表：至少一项，
// 月份不得重复，每项金额为正整数分。
func parseAllocations(args []string) ([]paymentAllocation, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("分配列表不能为空，至少需要一项 <YYYY-MM:金额分>")
	}
	seen := make(map[string]bool)
	allocs := make([]paymentAllocation, 0, len(args))
	for _, arg := range args {
		parts := strings.Split(arg, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("分配项 %q 格式非法，应为 YYYY-MM:金额分（如 2026-09:500）", arg)
		}
		month := strings.TrimSpace(parts[0])
		if !validMonth(month) {
			return nil, fmt.Errorf("分配项 %q 的月份 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", arg, parts[0])
		}
		if seen[month] {
			return nil, fmt.Errorf("分配月份 %s 重复，拒绝登记", month)
		}
		seen[month] = true
		amount, err := parsePositiveAmount(parts[1])
		if err != nil {
			return nil, fmt.Errorf("分配项 %q 的金额无效: %w", arg, err)
		}
		allocs = append(allocs, paymentAllocation{Month: month, Amount: amount})
	}
	return allocs, nil
}

// registerPayment 是 bill pay 与 bill remit 共用的收款登记核心：
// 先整体校验再整笔生效，任何一项不合法都不落盘、不占用收款标识。
func registerPayment(dir, customerID, payID string, total int64, note string, allocs []paymentAllocation) error {
	if strings.TrimSpace(payID) == "" {
		return fmt.Errorf("收款标识不能为空")
	}
	if strings.TrimSpace(note) == "" {
		return fmt.Errorf("收款备注不能为空")
	}
	// 分配顺序不影响收款身份：统一按月份升序保存与比较。
	sort.Slice(allocs, func(i, j int) bool { return allocs[i].Month < allocs[j].Month })

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	if _, ok := s.Customers[customerID]; !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}
	for _, al := range allocs {
		if _, ok := s.Bills[billKey(customerID, al.Month)]; !ok {
			return fmt.Errorf("客户 %s 的 %s 尚无账单（未结算），收款只能登记到已存在账单", customerID, al.Month)
		}
	}

	if existing, ok := s.Payments[payID]; ok {
		// 相同标识按客户、总额、备注与月份-金额对应关系判断重复：内容相同
		// 返回已保存记录，不再次计入实收，也不受当前余额变化影响；
		// 已撤销的重放仍返回已撤销状态，不恢复实收。
		if samePaymentContent(existing, customerID, total, note, allocs) {
			fmt.Fprintf(stdout, "收款 %q 已存在且内容相同，返回已保存记录（不重复计入实收）：\n\n", payID)
			printPayment(existing, s)
			return nil
		}
		return fmt.Errorf("收款标识 %q 已存在但内容不同（已有：客户=%s 总额=%d 备注=%q 分配=%s），拒绝复用",
			payID, existing.CustomerID, existing.Total, existing.Note, formatAllocations(existing.Allocations))
	}

	// 首次登记：每项分配不得超过对应账单的未收余额，全部合法才整笔生效。
	for _, al := range allocs {
		b := s.Bills[billKey(customerID, al.Month)]
		_, payable, err := billTotals(b, adjustmentsFor(s, customerID, al.Month))
		if err != nil {
			return fmt.Errorf("客户 %s 的 %s 当前应付异常，拒绝登记收款: %w", customerID, al.Month, err)
		}
		received, err := paymentReceived(paymentsFor(s, customerID, al.Month), al.Month)
		if err != nil {
			return fmt.Errorf("客户 %s 的 %s 实收累计溢出有符号 64 位整数范围，拒绝登记收款: %w", customerID, al.Month, err)
		}
		if payable == 0 {
			return fmt.Errorf("客户 %s 的 %s 当前应付为 0 分，不能登记正额收款", customerID, al.Month)
		}
		if al.Amount > payable-received {
			return fmt.Errorf("分配 %d 分超过未收余额 %d 分（客户 %s 的 %s：当前应付 %d 分，实收 %d 分），拒绝登记",
				al.Amount, payable-received, customerID, al.Month, payable, received)
		}
	}

	p := &payment{
		ID:          payID,
		CustomerID:  customerID,
		Total:       total,
		Note:        note,
		Allocations: allocs,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	s.NextSeq++
	p.Seq = s.NextSeq
	s.Payments[payID] = p

	// 收款记录、序号与余额状态在同一次原子保存中持久化；保存失败则
	// 一切不生效，该收款标识不被占用。
	if err := s.save(); err != nil {
		delete(s.Payments, payID)
		s.NextSeq--
		return err
	}
	fmt.Fprintf(stdout, "已登记收款 %q（总额 %d 分，%s）：\n", payID, total, moneyFen(total))
	printAllocationBalances(s, p)
	fmt.Fprintln(stdout)
	printPayment(p, s)
	return nil
}

// samePaymentContent 按客户、总额、备注与月份-金额对应关系判定两笔收款
// 内容相同；分配顺序不影响身份。
func samePaymentContent(p *payment, customerID string, total int64, note string, allocs []paymentAllocation) bool {
	if p.CustomerID != customerID || p.Total != total || p.Note != note {
		return false
	}
	if len(p.Allocations) != len(allocs) {
		return false
	}
	for _, al := range allocs {
		if p.amountFor(al.Month) != al.Amount {
			return false
		}
	}
	return true
}

// formatAllocations 以 "2026-09:500,2026-10:300" 形式展示分配列表。
func formatAllocations(allocs []paymentAllocation) string {
	parts := make([]string, len(allocs))
	for i, al := range allocs {
		parts[i] = fmt.Sprintf("%s:%d", al.Month, al.Amount)
	}
	return strings.Join(parts, ",")
}

// printAllocationBalances 输出该收款每个分配月份当前的实收与未收余额。
func printAllocationBalances(s *state, p *payment) {
	for _, al := range p.Allocations {
		b := s.Bills[billKey(p.CustomerID, al.Month)] // 载入时已校验存在
		_, payable, _ := billTotals(b, adjustmentsFor(s, p.CustomerID, al.Month))
		received, _ := paymentReceived(paymentsFor(s, p.CustomerID, al.Month), al.Month)
		fmt.Fprintf(stdout, "  月份 %s：分配 %d 分，实收 %d 分（%s），未收余额 %d 分（%s）\n",
			al.Month, al.Amount, received, moneyFen(received), payable-received, moneyFen(payable-received))
	}
}

func cmdBillUnpay(dir, payID, reason string) error {
	if strings.TrimSpace(payID) == "" {
		return fmt.Errorf("收款标识不能为空")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("撤销原因不能为空")
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	p, ok := s.Payments[payID]
	if !ok {
		return fmt.Errorf("收款标识 %q 不存在，无法撤销", payID)
	}

	if p.Revoked {
		// 相同标识与相同原因重复撤销：幂等成功，不新增历史；
		// 改用其他原因则拒绝。
		if p.RevokeReason == reason {
			fmt.Fprintf(stdout, "收款 %q 已撤销且撤销原因相同，幂等返回（不新增历史）：\n\n", payID)
			printPayment(p, s)
			return nil
		}
		return fmt.Errorf("收款 %q 已撤销（撤销原因 %q），改用其他原因重复撤销被拒绝", payID, p.RevokeReason)
	}

	// 撤销整笔生效：取消该笔在全部分配月份上的实收，不接受部分撤销；
	// 不改变应付、其他收款或调整，原收款、分配与撤销原因永久保留。
	p.Revoked = true
	p.RevokeReason = reason
	p.RevokedAt = time.Now().UTC().Format(time.RFC3339)
	s.NextSeq++
	p.RevokeSeq = s.NextSeq

	// 撤销信息与原记录在同一次原子保存中持久化；保存失败则一切不生效。
	if err := s.save(); err != nil {
		p.Revoked = false
		p.RevokeReason = ""
		p.RevokedAt = ""
		p.RevokeSeq = 0
		s.NextSeq--
		return err
	}
	fmt.Fprintf(stdout, "已撤销收款 %q（总额 %d 分，%s）：\n", payID, p.Total, moneyFen(p.Total))
	printAllocationBalances(s, p)
	fmt.Fprintln(stdout)
	printPayment(p, s)
	return nil
}

// printPayment 输出单笔收款记录：总额、全部分配与当前状态；
// 各月应付/实收取自当前库状态。
func printPayment(p *payment, s *state) {
	fmt.Fprintf(stdout, "收款标识：%s\n", p.ID)
	fmt.Fprintf(stdout, "客户：%s\n", p.CustomerID)
	fmt.Fprintf(stdout, "收款总额：%d 分（%s）\n", p.Total, moneyFen(p.Total))
	fmt.Fprintf(stdout, "备注：%s\n", p.Note)
	if p.Revoked {
		fmt.Fprintf(stdout, "当前状态：已撤销（撤销原因：%s）\n", p.RevokeReason)
	} else {
		fmt.Fprintf(stdout, "当前状态：实收中\n")
	}
	fmt.Fprintln(stdout, "分配明细：")
	for i, al := range p.Allocations {
		b := s.Bills[billKey(p.CustomerID, al.Month)] // 载入时已校验存在
		_, payable, _ := billTotals(b, adjustmentsFor(s, p.CustomerID, al.Month))
		received, _ := paymentReceived(paymentsFor(s, p.CustomerID, al.Month), al.Month)
		fmt.Fprintf(stdout, "  %d. 月份 %s：分配 %d 分（%s）；当前应付 %d 分，实收合计 %d 分，未收余额 %d 分\n",
			i+1, al.Month, al.Amount, moneyFen(al.Amount), payable, received, payable-received)
	}
}
