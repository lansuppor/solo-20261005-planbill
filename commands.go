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
			return usageError("缺少子命令，应为：bill settle|show <客户标识> <YYYY-MM>")
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
		default:
			return usageError("未知 bill 子命令 %q；可用：settle、show", args[1])
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
		// 不重新计费、不产生新账单、不落盘。
		fmt.Fprintf(stdout, "客户 %s 的 %s 已结算，返回原账单（幂等，不重新计费）：\n\n", customerID, month)
		printBill(existing, cust)
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
	printBill(b, cust)
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
	printBill(b, cust)
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

func printBill(b *bill, cust *customer) {
	fmt.Fprintf(stdout, "账单标识：%s\n", b.ID)
	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "月份：%s（UTC 自然月，左闭右开）\n", b.Month)
	fmt.Fprintf(stdout, "单价：%d 分（%s）\n", b.UnitPrice, moneyFen(b.UnitPrice))
	fmt.Fprintf(stdout, "总数量：%d\n", b.TotalQty)
	fmt.Fprintf(stdout, "总金额：%d 分（%s）\n", b.TotalFee, moneyFen(b.TotalFee))
	fmt.Println("明细：")
	for i, ln := range b.Lines {
		fmt.Fprintf(stdout, "  %d. 用量标识=%s 时间=%s 数量=%d 小计=%d 分（%s）\n",
			i+1, ln.UsageID, ln.Time, ln.Quantity, ln.LineFee, moneyFen(ln.LineFee))
	}
}
