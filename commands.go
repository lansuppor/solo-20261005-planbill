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
			return usageError("缺少子命令，应为：customer add|add-plan|suspend|resume|suspensions ...")
		}
		switch args[1] {
		case "add":
			if len(args) != 5 {
				return usageError("用法：customer add <标识> <名称> <单价分>")
			}
			return cmdCustomerAdd(dataDir, args[2], args[3], args[4])
		case "add-plan":
			if len(args) != 5 {
				return usageError("用法：customer add-plan <标识> <名称> <方案标识>")
			}
			return cmdCustomerAddPlan(dataDir, args[2], args[3], args[4])
		case "suspend":
			if len(args) != 6 {
				return usageError("用法：customer suspend <客户标识> <起月 YYYY-MM> <结束月 YYYY-MM> <原因>")
			}
			return cmdCustomerSuspend(dataDir, args[2], args[3], args[4], args[5])
		case "resume":
			if len(args) != 6 {
				return usageError("用法：customer resume <客户标识> <原起月 YYYY-MM> <恢复月 YYYY-MM> <原因>")
			}
			return cmdCustomerResume(dataDir, args[2], args[3], args[4], args[5])
		case "suspensions":
			if len(args) != 3 && len(args) != 4 {
				return usageError("用法：customer suspensions <客户标识> [YYYY-MM]")
			}
			month, hasMonth := "", false
			if len(args) == 4 {
				month, hasMonth = args[3], true
			}
			return cmdCustomerSuspensions(dataDir, args[2], month, hasMonth)
		default:
			return usageError("未知 customer 子命令 %q；可用：add、add-plan、suspend、resume、suspensions", args[1])
		}

	case "plan":
		if len(args) < 2 {
			return usageError("缺少子命令，应为：plan add|add-fee|show|list|change|schedule ...")
		}
		switch args[1] {
		case "add":
			if len(args) < 5 {
				return usageError("用法：plan add <标识> <名称> <上限:单价分>... <-:单价分>")
			}
			return cmdPlanAdd(dataDir, args[2], args[3], args[4:])
		case "add-fee":
			if len(args) < 6 {
				return usageError("用法：plan add-fee <标识> <名称> <月费分> <上限:单价分>... <-:单价分>")
			}
			return cmdPlanAddFee(dataDir, args[2], args[3], args[4], args[5:])
		case "show":
			if len(args) != 3 {
				return usageError("用法：plan show <标识>")
			}
			return cmdPlanShow(dataDir, args[2])
		case "list":
			if len(args) != 2 {
				return usageError("用法：plan list")
			}
			return cmdPlanList(dataDir)
		case "change":
			if len(args) != 6 {
				return usageError("用法：plan change <客户标识> <YYYY-MM> <方案标识> <原因>")
			}
			return cmdPlanChange(dataDir, args[2], args[3], args[4], args[5])
		case "schedule":
			if len(args) != 3 && len(args) != 4 {
				return usageError("用法：plan schedule <客户标识> [YYYY-MM]")
			}
			month, hasMonth := "", false
			if len(args) == 4 {
				month, hasMonth = args[3], true
			}
			return cmdPlanSchedule(dataDir, args[2], month, hasMonth)
		default:
			return usageError("未知 plan 子命令 %q；可用：add、add-fee、show、list、change、schedule", args[1])
		}

	case "usage":
		if len(args) < 2 {
			return usageError("缺少子命令，应为：usage import|withdraw|correct|show ...")
		}
		switch args[1] {
		case "import":
			if len(args) != 3 {
				return usageError("用法：usage import <文件>（- 表示标准输入）")
			}
			return cmdUsageImport(dataDir, args[2])
		case "withdraw":
			if len(args) != 4 {
				return usageError("用法：usage withdraw <用量标识> <原因>")
			}
			return cmdUsageWithdraw(dataDir, args[2], args[3])
		case "correct":
			if len(args) != 8 {
				return usageError("用法：usage correct <原用量标识> <新用量标识> <新客户> <RFC3339 时间> <正整数数量> <原因>")
			}
			return cmdUsageCorrect(dataDir, args[2], args[3], args[4], args[5], args[6], args[7])
		case "show":
			if len(args) != 3 {
				return usageError("用法：usage show <用量标识>")
			}
			return cmdUsageShow(dataDir, args[2])
		default:
			return usageError("未知 usage 子命令 %q；可用：import、withdraw、correct、show", args[1])
		}

	case "bill":
		if len(args) < 2 {
			return usageError("缺少子命令，应为：bill settle|settle-batch|show|adjust|revoke|pay|remit|remit-auto|correct|unpay|refund|ledger|reconcile ...")
		}
		switch args[1] {
		case "settle":
			if len(args) != 4 {
				return usageError("用法：bill settle <客户标识> <YYYY-MM>")
			}
			return cmdBillSettle(dataDir, args[2], args[3])
		case "settle-batch":
			return cmdBillSettleBatch(dataDir, args[2:])
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
		case "remit-auto":
			if len(args) != 6 {
				return usageError("用法：bill remit-auto <客户标识> <收款标识> <总金额分> <备注>")
			}
			return cmdBillRemitAuto(dataDir, args[2], args[3], args[4], args[5])
		case "correct":
			if len(args) < 6 {
				return usageError("用法：bill correct <收款标识> <更正标识> <原因> <YYYY-MM:金额分> [更多 月份:金额 ...]")
			}
			return cmdBillCorrect(dataDir, args[2], args[3], args[4], args[5:])
		case "unpay":
			if len(args) != 4 {
				return usageError("用法：bill unpay <收款标识> <原因>")
			}
			return cmdBillUnpay(dataDir, args[2], args[3])
		case "refund":
			if len(args) < 5 {
				return usageError("用法：bill refund <收款标识> <退款标识> <原因> <YYYY-MM:金额分> [更多 月份:金额 ...]")
			}
			return cmdBillRefund(dataDir, args[2], args[3], args[4], args[5:])
		case "ledger":
			if len(args) != 4 && len(args) != 5 {
				return usageError("用法：bill ledger <客户标识> <YYYY-MM> [截止操作序号]")
			}
			cutoff, hasCutoff := "", false
			if len(args) == 5 {
				cutoff, hasCutoff = args[4], true
			}
			return cmdBillLedger(dataDir, args[2], args[3], cutoff, hasCutoff)
		case "reconcile":
			if len(args) != 5 && len(args) != 7 {
				return usageError("用法：bill reconcile <客户标识> <起月 YYYY-MM> <止月 YYYY-MM> [起始操作序号 截止操作序号]")
			}
			return cmdBillReconcile(dataDir, args[2], args[3], args[4], args[5:])
		default:
			return usageError("未知 bill 子命令 %q；可用：settle、settle-batch、show、adjust、revoke、pay、remit、remit-auto、correct、unpay、refund、ledger、reconcile", args[1])
		}

	default:
		return usageError("未知命令 %q；可用：customer、plan、usage、bill", args[0])
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

		// 去重判定同时对“库中已有”和“本文件内已出现”生效，且先于暂停、
		// 封账与金额预检：已存在记录（含已撤回记录）按客户、解析后的时间点
		// 和数量与原内容判重，相同重放计入重复跳过——即使该月后来已封账、
		// 暂停或后续方案计价会溢出也不受阻；已撤回记录的重放不会恢复记录。
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

		// 以下为全新用量：暂停、封账及当前方案下金额预检只约束全新用量。
		// 暂停月的新用量一律拒绝：按记录时间换算的 UTC 月份判断，处于任一
		// 暂停区间（含起月、不含结束月）即整批失败并指出行号。
		rowMonth := r.t.UTC().Format("2006-01")
		if s.isSuspendedMonth(r.rec.CustomerID, rowMonth) {
			problems = append(problems, fmt.Sprintf("第 %d 行：客户 %s 的 %s 处于暂停区间，暂停服务期间不接收用量（新用量 %q 被拒绝）",
				r.line, r.rec.CustomerID, rowMonth, r.rec.ID))
			continue
		}
		// 金额可行性预检：固定单价客户检查 数量×单价；阶梯客户按记录时间
		// 换算的 UTC 月份的有效方案，以单条数量从零计价，检查分段金额不
		// 溢出（金额单位：分）。
		if cust.PlanID == "" {
			if _, err := mul64(r.rec.Quantity, cust.Price); err != nil {
				problems = append(problems, fmt.Sprintf("第 %d 行：数量 %d × 单价 %d 金额溢出有符号 64 位整数范围", r.line, r.rec.Quantity, cust.Price))
				continue
			}
		} else {
			rec := r.rec
			planID := s.effectivePlanID(cust, rowMonth)
			if _, err := tieredPrice(s.Plans[planID].Tiers, []*usageRecord{&rec}); err != nil {
				problems = append(problems, fmt.Sprintf("第 %d 行：按 %s 月有效方案 %q 对单条数量从零计价失败：%v", r.line, rowMonth, planID, err))
				continue
			}
		}

		// 全新标识：不得进入该客户已封账的 UTC 自然月。
		if s.sealed(r.rec.CustomerID, rowMonth) {
			problems = append(problems, fmt.Sprintf("第 %d 行：客户 %s 的 %s 已封账，新用量 %q 不得进入",
				r.line, r.rec.CustomerID, rowMonth, r.rec.ID))
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

// utcMonth 返回 RFC3339 时间换算 UTC 后的自然月（YYYY-MM）。
// 入参时间均已在载入/导入时校验为 RFC3339。
func utcMonth(rfc3339 string) string {
	t, _ := time.Parse(time.RFC3339, rfc3339)
	return t.UTC().Format("2006-01")
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

	// 暂停月无论方案月费多少都拒绝结算：不封账、不生成零金额账单。
	if s.isSuspendedMonth(customerID, month) {
		return fmt.Errorf("客户 %s 的 %s 处于暂停区间，暂停服务期间不产生月费账单，拒绝结算且不封账", customerID, month)
	}

	key := billKey(customerID, month)
	if existing, ok := s.Bills[key]; ok {
		// 幂等：同一客户同一月份重复结算，直接返回原账单，
		// 不重新计费、不产生新账单或调整、不落盘。
		fmt.Fprintf(stdout, "客户 %s 的 %s 已结算，返回原账单（幂等，不重新计费）：\n\n", customerID, month)
		printBill(existing, cust, s)
		return nil
	}

	// 归集该客户 UTC 自然月内的全部有效（未撤回）用量，区间为左闭右开
	// [月初, 下月初)。有效方案月费大于 0 的阶梯客户即使无用量也须出账
	// （收取一次整月月费）；月费为 0 的方案与固定单价客户无用量时仍拒绝
	// 结算且不封账。
	inMonthRecs := monthUsage(s, customerID, month)
	if len(inMonthRecs) == 0 && !monthlyFeeBillable(s, cust, month) {
		return fmt.Errorf("客户 %s 在 %s 没有用量，拒绝结算且不封账", customerID, month)
	}

	// 绑定阶梯方案的客户走按月累计分档计价；固定单价客户保持原路径。
	if cust.PlanID != "" {
		return settleTiered(s, cust, month, inMonthRecs)
	}

	b, err := buildFixedBill(cust, month, inMonthRecs)
	if err != nil {
		return fmt.Errorf("%v，拒绝结算且不封账", err)
	}
	s.Bills[key] = b

	// 账单与封账状态在同一次原子保存中一起持久化；保存失败则一切不生效。
	if err := s.save(); err != nil {
		delete(s.Bills, key)
		return err
	}
	fmt.Fprintf(stdout, "结算完成，客户 %s 的 %s 已封账：\n\n", customerID, month)
	printBill(b, cust, s)
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
	printBill(b, cust, s)
	return nil
}

// monthUsage 归集客户在某 UTC 自然月（左闭右开 [月初, 下月初)）内的全部
// 有效（未撤回）用量，并按计价顺序（解析后的时间点升序、同一时间按标识
// 字典序）排列。已撤回用量不计费用，不参与单笔及批量结算；阶梯计价将剩余
// 记录按原有时间点及标识顺序从零累计重新分档。
func monthUsage(s *state, customerID, month string) []*usageRecord {
	var recs []*usageRecord
	for _, u := range s.Usage {
		if s.isWithdrawn(u.ID) {
			continue
		}
		if u.CustomerID == customerID && inMonth(u.Time, month) {
			recs = append(recs, u)
		}
	}
	sortByInstant(recs)
	return recs
}

// buildFixedBill 为固定单价客户构造账单（不落盘）：逐条 数量×单价 并汇总，
// 全程整数运算，任何一步溢出都返回错误。recs 须已按计价顺序排列。
func buildFixedBill(cust *customer, month string, recs []*usageRecord) (*bill, error) {
	lines := make([]billLine, 0, len(recs))
	var totalQty, totalFee int64
	for _, u := range recs {
		lineFee, err := mul64(u.Quantity, cust.Price)
		if err != nil {
			return nil, fmt.Errorf("用量 %s：数量 %d × 单价 %d 金额溢出", u.ID, u.Quantity, cust.Price)
		}
		if totalQty, err = add64(totalQty, u.Quantity); err != nil {
			return nil, fmt.Errorf("汇总数量溢出有符号 64 位整数范围")
		}
		if totalFee, err = add64(totalFee, lineFee); err != nil {
			return nil, fmt.Errorf("汇总金额溢出有符号 64 位整数范围")
		}
		lines = append(lines, billLine{
			UsageID:  u.ID,
			Time:     u.Time,
			Quantity: u.Quantity,
			LineFee:  lineFee,
		})
	}
	return &bill{
		ID:         stableBillID(cust.ID, month),
		CustomerID: cust.ID,
		Month:      month,
		TotalQty:   totalQty,
		UnitPrice:  cust.Price,
		TotalFee:   totalFee,
		Lines:      lines,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}, nil
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

func printBill(b *bill, cust *customer, s *state) {
	adjs := adjustmentsFor(s, b.CustomerID, b.Month)
	// 收款历史保留所有曾涉及本账单的收款：首次登记或任一次更正分配
	// 涉及该月即纳入，即使后来被更正移出。
	pays := paymentsEverFor(s, b.CustomerID, b.Month)
	fmt.Fprintf(stdout, "账单标识：%s\n", b.ID)
	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "月份：%s（UTC 自然月，左闭右开）\n", b.Month)
	tiered := b.Pricing == "tiered"
	if tiered {
		// 阶梯账单明确计价类型并展示方案与完整规则，不伪造统一单价。
		fmt.Fprintf(stdout, "计价类型：阶梯计费（按 UTC 自然月累计用量分档计价，每月从零累计）\n")
		fmt.Fprintf(stdout, "方案：%s（%s）\n", b.PlanID, b.PlanName)
		fmt.Fprintln(stdout, "阶梯规则：")
		for i, t := range b.PlanTiers {
			if t.Limit == 0 {
				fmt.Fprintf(stdout, "  第 %d 档：累计数量无上限，单价 %d 分（%s）\n", i+1, t.Price, moneyFen(t.Price))
			} else {
				fmt.Fprintf(stdout, "  第 %d 档：累计上限 %d，单价 %d 分（%s）\n", i+1, t.Limit, t.Price, moneyFen(t.Price))
			}
		}
	} else {
		fmt.Fprintf(stdout, "单价：%d 分（%s）\n", b.UnitPrice, moneyFen(b.UnitPrice))
	}
	fmt.Fprintf(stdout, "总数量：%d\n", b.TotalQty)
	if tiered {
		// 阶梯账单分列月费与用量费：原总金额 = 月费 + 全月用量费；
		// 月费不计入总数量、各档金额或逐条小计。
		usageFee := b.TotalFee - b.MonthlyFee
		fmt.Fprintf(stdout, "月费：%d 分（%s，整月收取，不按天折算）\n", b.MonthlyFee, moneyFen(b.MonthlyFee))
		fmt.Fprintf(stdout, "用量费：%d 分（%s）\n", usageFee, moneyFen(usageFee))
		fmt.Fprintf(stdout, "原总金额：%d 分（%s，月费加全月用量费）\n", b.TotalFee, moneyFen(b.TotalFee))
	} else {
		fmt.Fprintf(stdout, "总金额：%d 分（%s）\n", b.TotalFee, moneyFen(b.TotalFee))
	}
	if tiered {
		fmt.Fprintln(stdout, "各档合计：")
		for i, tt := range b.TierTotals {
			fmt.Fprintf(stdout, "  第 %d 档：数量 %d，单价 %d 分，金额 %d 分（%s）\n",
				i+1, tt.Quantity, b.PlanTiers[i].Price, tt.Fee, moneyFen(tt.Fee))
		}
	}
	// 数据在载入时已校验一致，此处计算不会出错。
	net, payable, _ := billTotals(b, adjs)
	received, _ := paymentReceived(s, b.CustomerID, b.Month)
	outstanding := payable - received // 不变量保证 0 ≤ 实收 ≤ 当前应付
	fmt.Fprintf(stdout, "调整净额：%+d 分（%s）\n", net, moneyFen(net))
	fmt.Fprintf(stdout, "当前应付：%d 分（%s）\n", payable, moneyFen(payable))
	fmt.Fprintf(stdout, "实收：%d 分（%s）\n", received, moneyFen(received))
	fmt.Fprintf(stdout, "未收余额：%d 分（%s）\n", outstanding, moneyFen(outstanding))
	if len(b.Lines) == 0 {
		fmt.Fprintln(stdout, "明细：无（本账期无用量，仅收取月费）")
	} else {
		fmt.Fprintln(stdout, "明细：")
	}
	for i, ln := range b.Lines {
		fmt.Fprintf(stdout, "  %d. 用量标识=%s 时间=%s 数量=%d 小计=%d 分（%s）\n",
			i+1, ln.UsageID, ln.Time, ln.Quantity, ln.LineFee, moneyFen(ln.LineFee))
		// 阶梯账单逐条展示跨档分段的数量、单价与小计。
		for j, seg := range ln.Segments {
			fmt.Fprintf(stdout, "     分段 %d：第 %d 档 数量=%d 单价=%d 分 小计=%d 分（%s）\n",
				j+1, seg.Tier+1, seg.Quantity, b.PlanTiers[seg.Tier].Price, seg.Fee, moneyFen(seg.Fee))
		}
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
		for i, ev := range paymentHistoryEvents(s, pays, b.Month) {
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

// paymentHistoryEvents 把收款登记、分配更正与撤销展开为按操作序号排序的
// 可读历史条目；month 为当前展示账单所在月份。收款事件展示首次登记时在
// 本账单的分配，更正事件展示本账单分配的前后变化，撤销事件展示被整笔取消
// 的最新分配。
func paymentHistoryEvents(s *state, pays []*payment, month string) []string {
	type event struct {
		seq  int64
		text string
	}
	var events []event
	for _, p := range pays {
		alloc := p.amountFor(month)
		events = append(events, event{p.Seq, fmt.Sprintf("收款 %s：总额 %d 分（%s），本账单分配 %d 分（%s），备注：%s，当前状态：%s（客户 %s）",
			p.ID, p.Total, moneyFen(p.Total), alloc, moneyFen(alloc), p.Note, paymentStatus(p), p.CustomerID)})
		// 以首次登记分配为起点，按序号回放该收款在本账单的分配变化。
		running := alloc
		for _, c := range correctionsFor(s, p.ID) {
			after := allocAmountFor(c.Allocations, month)
			if running == 0 && after == 0 {
				continue // 本次更正不涉及本账单
			}
			events = append(events, event{c.Seq, fmt.Sprintf("更正 %s：原因：%s（关联收款 %s，本账单分配 %d 分 → %d 分）",
				c.ID, c.Reason, p.ID, running, after)})
			running = after
		}
		if p.Revoked {
			// 整笔撤销取消的是撤销时的最新分配。
			events = append(events, event{p.RevokeSeq, fmt.Sprintf("撤销收款 %s：原因：%s（关联收款 %s，总额 %d 分，本账单分配 %d 分）",
				p.ID, p.RevokeReason, p.ID, p.Total, running)})
		}
		// 退款在其发生序号减少本账单实收；退款不可撤销，记录永久保留。
		for _, r := range refundsFor(s, p.ID) {
			amt := allocAmountFor(r.Allocations, month)
			if amt <= 0 {
				continue // 本次退款不涉及本账单
			}
			events = append(events, event{r.Seq, fmt.Sprintf("退款 %s：原因：%s（关联收款 %s，本账单退款 %d 分（%s））",
				r.ID, r.Reason, p.ID, amt, moneyFen(amt))})
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
	received, err := paymentReceived(s, customerID, month)
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
	received, err := paymentReceived(s, a.CustomerID, a.Month)
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

// --- 自动分配汇款（bill remit-auto） ---

// cmdBillRemitAuto 登记一笔自动分配汇款：调用方只提供客户、收款标识、总金额
// 与备注，月份金额由工具确定——首次登记只选择该客户当前已有账单，按 UTC
// 账期月升序依次偿还当前未收余额（调整、最新收款分配、撤销及退款后的净额），
// 跳过余额为 0 的月份，前一月份还清后才分配下一月，最后一月可部分偿还；
// 不补结算、不跨客户、不留未分配款项。无欠款或总金额超过全部欠款时整笔拒绝。
func cmdBillRemitAuto(dir, customerID, payID, totalText, note string) error {
	total, err := parsePositiveAmount(totalText)
	if err != nil {
		return err
	}
	if strings.TrimSpace(payID) == "" {
		return fmt.Errorf("收款标识不能为空")
	}
	if strings.TrimSpace(note) == "" {
		return fmt.Errorf("收款备注不能为空")
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	if _, ok := s.Customers[customerID]; !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}

	if existing, ok := s.Payments[payID]; ok {
		// 自动登记按客户、总金额、备注判重：相同请求返回原收款及当前状态，
		// 不写盘、不重新分配、不增加序号；已撤销或已退款的仍返回当前状态，
		// 不恢复旧分配或已退、已撤销金额。
		if existing.Auto && existing.CustomerID == customerID &&
			existing.Total == total && existing.Note == note {
			fmt.Fprintf(stdout, "收款 %q 已存在且内容相同（自动分配登记），返回原收款及当前状态（不写盘、不重新分配、不增加序号）：\n\n", payID)
			printAutoPayment(s, existing)
			return nil
		}
		// 同一标识在自动与显式分配登记间复用拒绝；内容不同同样拒绝。
		if !existing.Auto {
			return fmt.Errorf("收款标识 %q 已由显式分配登记占用（已有：客户=%s 总额=%d 备注=%q 分配=%s），自动与显式分配登记之间不得复用，拒绝",
				payID, existing.CustomerID, existing.Total, existing.Note, formatAllocations(existing.Allocations))
		}
		return fmt.Errorf("收款标识 %q 已存在但内容不同（已有：客户=%s 总额=%d 备注=%q），拒绝复用",
			payID, existing.CustomerID, existing.Total, existing.Note)
	}

	// 首次登记：归集该客户当前已有账单的月份，按 UTC 账期月升序排列
	// （YYYY-MM 字典序即时间序）。不补结算，只有已存在账单参与分配。
	var months []string
	for _, b := range s.Bills {
		if b.CustomerID == customerID {
			months = append(months, b.Month)
		}
	}
	sort.Strings(months)

	// 依次偿还当前未收余额：跳过余额为 0 的月份，前一月份还清后才分配
	// 下一月，最后一月可部分偿还。每月未收余额 = 当前应付 − 实收，均在
	// [0, 有符号 64 位最大值] 内；remaining 从总额（正整数分）起只减不增，
	// 各月余额与分配全程整数计算——即使全部账单欠款合计超过 64 位上限，
	// 分配过程也只涉及不超过总额的中间值，不会误拒合法金额。
	remaining := total
	var allocs []paymentAllocation
	for _, m := range months {
		if remaining == 0 {
			break
		}
		b := s.Bills[billKey(customerID, m)]
		_, payable, err := billTotals(b, adjustmentsFor(s, customerID, m))
		if err != nil {
			return fmt.Errorf("客户 %s 的 %s 当前应付异常，拒绝登记收款: %w", customerID, m, err)
		}
		received, err := paymentReceived(s, customerID, m)
		if err != nil {
			return fmt.Errorf("客户 %s 的 %s 实收累计溢出有符号 64 位整数范围，拒绝登记收款: %w", customerID, m, err)
		}
		outstanding := payable - received // 不变量保证 0 ≤ 实收 ≤ 当前应付
		if outstanding <= 0 {
			continue // 跳过余额为 0 的月份
		}
		amt := outstanding
		if amt > remaining {
			amt = remaining // 最后一月可部分偿还
		}
		allocs = append(allocs, paymentAllocation{Month: m, Amount: amt})
		remaining -= amt
	}
	if len(allocs) == 0 {
		return fmt.Errorf("客户 %s 当前没有欠款（全部已存在账单的未收余额均为 0），整笔拒绝；不补结算、不跨客户、不留未分配款项", customerID)
	}
	if remaining > 0 {
		return fmt.Errorf("收款总额 %d 分超过客户 %s 全部已存在账单的欠款合计 %d 分，整笔拒绝；不补结算、不跨客户、不留未分配款项",
			total, customerID, total-remaining)
	}

	// 成功保存为一笔普通收款及首次自动分配：占用一个账后全局序号，
	// 不按月份拆成多笔；可沿用分配更正、整笔撤销、部分退款及退款后
	// 固定分配的规则。
	p := &payment{
		ID:          payID,
		CustomerID:  customerID,
		Total:       total,
		Note:        note,
		Allocations: allocs,
		Auto:        true,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	s.NextSeq++
	p.Seq = s.NextSeq
	s.Payments[payID] = p

	// 收款记录、首次分配与序号在同一次原子保存中持久化；保存失败则一切
	// 不生效，该收款标识与序号不被占用，可原样重试。
	if err := s.save(); err != nil {
		delete(s.Payments, payID)
		s.NextSeq--
		return err
	}
	fmt.Fprintf(stdout, "已按最早欠款账期自动分配并登记收款 %q（总额 %d 分，%s；一次操作，占用一个账后序号）：\n\n",
		payID, total, moneyFen(total))
	printAutoPayment(s, p)
	return nil
}

// printAutoPayment 输出一笔自动分配登记的收款：收款标识、总额、备注、当前
// 状态、首次分配与最新分配，以及这两份分配所涉月份的当前余额。首次分配
// 永久保留；最新分配经更正变化时以最新为准，退款与撤销不改变首次分配。
func printAutoPayment(s *state, p *payment) {
	current := currentAllocations(s, p)
	fmt.Fprintf(stdout, "收款标识：%s\n", p.ID)
	fmt.Fprintf(stdout, "客户：%s\n", p.CustomerID)
	fmt.Fprintf(stdout, "收款总额：%d 分（%s）\n", p.Total, moneyFen(p.Total))
	fmt.Fprintf(stdout, "备注：%s\n", p.Note)
	if p.Revoked {
		fmt.Fprintf(stdout, "当前状态：已撤销（撤销原因：%s）\n", p.RevokeReason)
	} else {
		fmt.Fprintf(stdout, "当前状态：实收中\n")
	}
	fmt.Fprintf(stdout, "首次分配（登记时按最早欠款账期自动确定，永久保留）：%s\n", formatAllocations(p.Allocations))
	if sameAllocations(current, p.Allocations) {
		fmt.Fprintf(stdout, "最新分配：%s（与首次分配相同）\n", formatAllocations(current))
	} else {
		fmt.Fprintf(stdout, "最新分配（经更正，以最新为准）：%s\n", formatAllocations(current))
	}
	fmt.Fprintln(stdout, "首次与最新分配所涉月份当前余额：")
	for _, m := range unionMonths(p.Allocations, current) {
		b := s.Bills[billKey(p.CustomerID, m)] // 载入时已校验存在
		_, payable, _ := billTotals(b, adjustmentsFor(s, p.CustomerID, m))
		received, _ := paymentReceived(s, p.CustomerID, m)
		fmt.Fprintf(stdout, "  月份 %s：首次分配 %d 分，最新分配 %d 分；当前应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
			m, allocAmountFor(p.Allocations, m), allocAmountFor(current, m),
			payable, moneyFen(payable), received, moneyFen(received), payable-received, moneyFen(payable-received))
	}
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
		// 自动与显式分配登记之间不得复用同一收款标识：标识由 bill remit-auto
		// 占用时，bill pay / bill remit 一律拒绝（即使内容碰巧相同）。
		if existing.Auto {
			return fmt.Errorf("收款标识 %q 已由自动分配登记占用（已有：客户=%s 总额=%d 备注=%q 分配=%s），自动与显式分配登记之间不得复用，拒绝",
				payID, existing.CustomerID, existing.Total, existing.Note, formatAllocations(existing.Allocations))
		}
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
		received, err := paymentReceived(s, customerID, al.Month)
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
	printAllocationBalances(s, p, allocs)
	fmt.Fprintln(stdout)
	printPayment(p, s)
	return nil
}

// samePaymentContent 按客户、总额、备注与月份-金额对应关系判定两笔收款
// 内容相同；分配顺序不影响身份。判重依据首次登记内容，不受后续更正影响。
func samePaymentContent(p *payment, customerID string, total int64, note string, allocs []paymentAllocation) bool {
	if p.CustomerID != customerID || p.Total != total || p.Note != note {
		return false
	}
	return sameAllocations(p.Allocations, allocs)
}

// sameAllocations 判定两个分配列表的月份-金额对应关系相同（顺序无关）。
func sameAllocations(a, b []paymentAllocation) bool {
	if len(a) != len(b) {
		return false
	}
	for _, al := range b {
		if allocAmountFor(a, al.Month) != al.Amount {
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

// printAllocationBalances 输出给定分配列表每个分配月份当前的实收与未收余额。
func printAllocationBalances(s *state, p *payment, allocs []paymentAllocation) {
	for _, al := range allocs {
		b := s.Bills[billKey(p.CustomerID, al.Month)] // 载入时已校验存在
		_, payable, _ := billTotals(b, adjustmentsFor(s, p.CustomerID, al.Month))
		received, _ := paymentReceived(s, p.CustomerID, al.Month)
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

	// 首次退款后该收款的最新分配固定，拒绝整笔撤销；退款记录永久保留且
	// 不可撤销，实收按扣除退款后的余额继续计算。无退款的收款仍按原规则
	// 处理。
	if paymentHasRefunds(s, payID) {
		return fmt.Errorf("收款 %q 已发生退款，最新分配已固定，拒绝整笔撤销；如需退回余款请继续登记部分退款", payID)
	}

	// 撤销整笔生效：取消该笔在全部分配月份上的最新实收（经更正的以最新
	// 分配为准），不接受部分撤销；不改变应付、其他收款或调整，原收款、
	// 分配、更正与撤销原因永久保留。
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
	fmt.Fprintf(stdout, "已撤销收款 %q（总额 %d 分，%s），取消最新分配：\n", payID, p.Total, moneyFen(p.Total))
	printAllocationBalances(s, p, currentAllocations(s, p))
	fmt.Fprintln(stdout)
	printPayment(p, s)
	return nil
}

// printPayment 输出单笔收款记录：总额、首次登记的全部分配（永久保留，
// 判重依据）与当前状态；经历过更正时同时展示当前生效的最新分配。
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
	fmt.Fprintln(stdout, "分配明细（首次登记，永久保留）：")
	for i, al := range p.Allocations {
		b := s.Bills[billKey(p.CustomerID, al.Month)] // 载入时已校验存在
		_, payable, _ := billTotals(b, adjustmentsFor(s, p.CustomerID, al.Month))
		received, _ := paymentReceived(s, p.CustomerID, al.Month)
		fmt.Fprintf(stdout, "  %d. 月份 %s：分配 %d 分（%s）；当前应付 %d 分，实收合计 %d 分，未收余额 %d 分\n",
			i+1, al.Month, al.Amount, moneyFen(al.Amount), payable, received, payable-received)
	}
	if corrs := correctionsFor(s, p.ID); len(corrs) > 0 {
		fmt.Fprintf(stdout, "当前分配（经 %d 次更正，以最新为准）：%s\n",
			len(corrs), formatAllocations(currentAllocations(s, p)))
	}
	if refs := refundsFor(s, p.ID); len(refs) > 0 {
		current := currentAllocations(s, p)
		fmt.Fprintf(stdout, "退款记录（%d 笔，永久保留、不可撤销；首次退款后最新分配已固定）：\n", len(refs))
		for _, r := range refs {
			fmt.Fprintf(stdout, "  退款 %s：%s，原因：%s\n", r.ID, formatAllocations(r.Allocations), r.Reason)
		}
		fmt.Fprintln(stdout, "各月剩余可退额（最新分配减累计退款）：")
		for _, al := range current {
			refunded := refundedForMonth(s, p.ID, al.Month)
			fmt.Fprintf(stdout, "  月份 %s：分配 %d 分，累计已退 %d 分，剩余可退 %d 分（%s）\n",
				al.Month, al.Amount, refunded, al.Amount-refunded, moneyFen(al.Amount-refunded))
		}
	}
}

// --- 收款分配更正 ---

func cmdBillCorrect(dir, payID, corrID, reason string, allocArgs []string) error {
	if strings.TrimSpace(payID) == "" {
		return fmt.Errorf("收款标识不能为空")
	}
	if strings.TrimSpace(corrID) == "" {
		return fmt.Errorf("更正标识不能为空")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("更正原因不能为空")
	}
	allocs, err := parseAllocations(allocArgs)
	if err != nil {
		return err
	}
	// 分配顺序不影响更正身份：统一按月份升序保存与比较。
	sort.Slice(allocs, func(i, j int) bool { return allocs[i].Month < allocs[j].Month })

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	p, ok := s.Payments[payID]
	if !ok {
		return fmt.Errorf("收款标识 %q 不存在，无法更正分配", payID)
	}

	if existing, ok := s.Corrections[corrID]; ok {
		// 相同标识按目标收款、原因与新月份-金额对应关系判重（列表顺序无关）：
		// 内容相同返回原更正记录，不改分配、不增历史；后续更正或收款撤销
		// 不影响此规则。任何一项不同（含跨收款复用）均拒绝。
		if existing.PaymentID == payID && existing.Reason == reason && sameAllocations(existing.Allocations, allocs) {
			fmt.Fprintf(stdout, "更正 %q 已存在且内容相同，返回原更正记录（不改分配、不增历史）：\n\n", corrID)
			printCorrection(existing, s)
			return nil
		}
		return fmt.Errorf("更正标识 %q 已存在但内容不同（已有：收款=%s 原因=%q 新分配=%s），拒绝复用",
			corrID, existing.PaymentID, existing.Reason, formatAllocations(existing.Allocations))
	}

	// 新增更正只允许未撤销收款。
	if p.Revoked {
		return fmt.Errorf("收款 %q 已撤销（撤销原因 %q），不能新增分配更正", payID, p.RevokeReason)
	}

	// 首次退款后该收款的最新分配固定，拒绝新增分配更正；已有更正的相同
	// 重放已在上面按原记录返回，不受此限。无退款的收款仍按原规则处理。
	if paymentHasRefunds(s, payID) {
		return fmt.Errorf("收款 %q 已发生退款，最新分配已固定，拒绝新增分配更正", payID)
	}

	// 新分配合计必须等于原收款总额（不重复收钱也不多收），全程整数运算。
	var sum int64
	for _, al := range allocs {
		sum, err = add64(sum, al.Amount)
		if err != nil {
			return fmt.Errorf("新分配金额合计溢出有符号 64 位整数范围，拒绝更正")
		}
	}
	if sum != p.Total {
		return fmt.Errorf("新分配合计 %d 分与原收款 %q 的总额 %d 分不一致，拒绝更正", sum, payID, p.Total)
	}
	// 新分配月份须是原客户的已结算账单。
	for _, al := range allocs {
		if _, ok := s.Bills[billKey(p.CustomerID, al.Month)]; !ok {
			return fmt.Errorf("客户 %s 的 %s 尚无账单（未结算），更正只能分配到已存在账单", p.CustomerID, al.Month)
		}
	}

	// 以当前最新分配为起点：撤去该笔当前分配再计入新分配后，涉及月份
	// 均须满足 0 ≤ 实收 ≤ 应付；其他收款不变，全部合法才整笔生效。
	current := currentAllocations(s, p)
	for _, m := range unionMonths(current, allocs) {
		b := s.Bills[billKey(p.CustomerID, m)] // 载入时已校验存在
		_, payable, err := billTotals(b, adjustmentsFor(s, p.CustomerID, m))
		if err != nil {
			return fmt.Errorf("客户 %s 的 %s 当前应付异常，拒绝更正: %w", p.CustomerID, m, err)
		}
		received, err := paymentReceived(s, p.CustomerID, m)
		if err != nil {
			return fmt.Errorf("客户 %s 的 %s 实收累计异常，拒绝更正: %w", p.CustomerID, m, err)
		}
		// 该笔收款未撤销，其实收必然包含当前分配，撤去不会变负。
		received -= allocAmountFor(current, m)
		received, err = add64(received, allocAmountFor(allocs, m))
		if err != nil {
			return fmt.Errorf("客户 %s 的 %s 更正后实收溢出有符号 64 位整数范围，拒绝更正", p.CustomerID, m)
		}
		if received > payable {
			return fmt.Errorf("更正后客户 %s 的 %s 实收 %d 分将超过当前应付 %d 分，拒绝更正；全部状态保持不变",
				p.CustomerID, m, received, payable)
		}
	}

	c := &correction{
		ID:          corrID,
		PaymentID:   payID,
		Reason:      reason,
		Allocations: allocs,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	s.NextSeq++
	c.Seq = s.NextSeq
	s.Corrections[corrID] = c

	// 更正记录、序号与余额状态在同一次原子保存中持久化；保存失败则
	// 一切不生效，该更正标识不被占用。
	if err := s.save(); err != nil {
		delete(s.Corrections, corrID)
		s.NextSeq--
		return err
	}
	fmt.Fprintf(stdout, "已登记收款分配更正 %q（关联收款 %q）：\n\n", corrID, payID)
	printCorrection(c, s)
	return nil
}

// unionMonths 返回两个分配列表涉及月份的并集，按月份升序。
func unionMonths(a, b []paymentAllocation) []string {
	set := make(map[string]bool, len(a)+len(b))
	for _, al := range a {
		set[al.Month] = true
	}
	for _, al := range b {
		set[al.Month] = true
	}
	months := make([]string, 0, len(set))
	for m := range set {
		months = append(months, m)
	}
	sort.Strings(months)
	return months
}

// allocationBefore 返回一次更正发生前该收款生效的分配：上一条更正的
// 新分配，或（首次更正时）收款的原始分配。
func allocationBefore(s *state, c *correction) []paymentAllocation {
	before := s.Payments[c.PaymentID].Allocations // 载入时已校验存在
	var latest *correction
	for _, o := range s.Corrections {
		if o.PaymentID == c.PaymentID && o.Seq < c.Seq && (latest == nil || o.Seq > latest.Seq) {
			latest = o
		}
	}
	if latest != nil {
		before = latest.Allocations
	}
	return before
}

// printCorrection 输出单笔更正记录：关联收款、标识、原因、完整前后分配，
// 以及前后分配涉及月份的当前余额。
func printCorrection(c *correction, s *state) {
	p := s.Payments[c.PaymentID] // 载入时已校验存在
	before := allocationBefore(s, c)
	fmt.Fprintf(stdout, "更正标识：%s\n", c.ID)
	fmt.Fprintf(stdout, "关联收款：%s（客户 %s，总额 %d 分（%s），备注：%s）\n",
		p.ID, p.CustomerID, p.Total, moneyFen(p.Total), p.Note)
	fmt.Fprintf(stdout, "原因：%s\n", c.Reason)
	fmt.Fprintf(stdout, "更正前分配：%s\n", formatAllocations(before))
	fmt.Fprintf(stdout, "更正后分配：%s\n", formatAllocations(c.Allocations))
	fmt.Fprintln(stdout, "涉及月份当前余额：")
	for _, m := range unionMonths(before, c.Allocations) {
		b := s.Bills[billKey(p.CustomerID, m)] // 载入时已校验存在
		_, payable, _ := billTotals(b, adjustmentsFor(s, p.CustomerID, m))
		received, _ := paymentReceived(s, p.CustomerID, m)
		fmt.Fprintf(stdout, "  月份 %s：本笔分配 %d 分 → %d 分；当前应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
			m, allocAmountFor(before, m), allocAmountFor(c.Allocations, m),
			payable, moneyFen(payable), received, moneyFen(received), payable-received, moneyFen(payable-received))
	}
}

// --- 账后对账流水 ---

// ledgerEvent 是某张已结算账单账后流水中的一个事件：调整、撤销调整、收款、
// 分配更正、撤销收款或退款。事件按全局操作序号升序回放；撤销在其发生序号抵消
// 对应原操作，已撤销记录在撤销之前仍计入，不按当前撤销状态删除原事件；
// 更正在其发生序号把关联收款在本账单的分配由前值替换为后值；退款在其发生
// 序号减少本账单实收，不可撤销。
type ledgerEvent struct {
	seq           int64  // 全局操作序号（调整/收款/更正/退款及其撤销共用）
	kind          string // 事件类型：调整 / 撤销调整 / 收款 / 更正 / 撤销收款 / 退款
	refID         string // 原记录标识（调整标识、收款标识或更正标识）
	deltaPayable  int64  // 对当前应付的影响（分，带符号）
	deltaReceived int64  // 对实收的影响（分，带符号）
	note          string // 原因（调整/更正类）或备注（收款类）
	linkSeq       int64  // 撤销事件关联的原操作序号；非撤销事件为 0
	payTotal      int64  // 收款类事件：汇款总额
	payAlloc      int64  // 收款类事件：本账单分配（撤销收款事件为被取消的最新分配；退款事件为本账单退款额）
	payID         string // 更正/退款事件：关联收款标识
	beforeAlloc   int64  // 更正事件：本账单更正前分配
	afterAlloc    int64  // 更正事件：本账单更正后分配
	afterPayable  int64  // 事件后的应付（回放时填充）
	afterReceived int64  // 事件后的实收（回放时填充）
}

// billLedger 返回目标账单的一条按操作序号升序的账后流水，事件之后的三项
// 余额已逐步填充。业务归属、金额影响（调整/撤销/收款/更正/撤销收款/退款）
// 与完整性核验都来自与 bill reconcile 共用的 postbill 计算，本处只取该账单
// 那一条流水：
//
//   - 以原总金额为初始应付、零实收为起点；
//   - 回放覆盖完整流水（不受查询截止序号限制），序号重复、撤销先于原操作，
//     或任一事件之后不满足 0 ≤ 实收 ≤ 应付 ≤ 有符号 64 位最大值，都视为
//     数据异常并拒绝，即使异常发生在截止序号之后——只检查最终余额会放过
//     中间越界的存档；
//   - 曾涉及该账单的收款在被更正移出该月后，仍保留零金额撤销事件。
//
// 只读：不修改库、不占用序号、不新增记录。
func billLedger(s *state, b *bill) ([]ledgerEvent, error) {
	ops, err := postbillOps(s, b.CustomerID)
	if err != nil {
		return nil, err
	}
	ledgers, err := verifyPostbillLedgers(ops, []*bill{b})
	if err != nil {
		return nil, err
	}
	return ledgers[b.Month], nil
}

func cmdBillLedger(dir, customerID, month, cutoffText string, hasCutoff bool) error {
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

	// 截止序号使用调整/收款/更正/退款及其撤销共用的已保存全局序号：省略表示最新，
	// 0 只返回初始余额，截止包含该序号；因其他客户或月份操作造成的序号
	// 空档合法。负数、非整数或超过存档全局序号上限一律拒绝。
	cutoff := s.NextSeq
	cutoffDesc := "省略，按最新"
	if hasCutoff {
		v, perr := strconv.ParseInt(strings.TrimSpace(cutoffText), 10, 64)
		if perr != nil {
			return fmt.Errorf("截止操作序号 %q 不是非负整数: %w", cutoffText, perr)
		}
		if v < 0 {
			return fmt.Errorf("截止操作序号不能为负数，收到 %d", v)
		}
		if v > s.NextSeq {
			return fmt.Errorf("截止操作序号 %d 超过存档全局序号上限 %d", v, s.NextSeq)
		}
		cutoff = v
		cutoffDesc = "指定"
	}

	// 先核验目标账单的完整流水：任何异常（即使发生在截止序号之后）都
	// 拒绝，不输出部分正常的报告。
	events, err := billLedger(s, b)
	if err != nil {
		return err
	}
	printBillLedger(b, cust, s.NextSeq, cutoff, cutoffDesc, events)
	return nil
}

func printBillLedger(b *bill, cust *customer, nextSeq, cutoff int64, cutoffDesc string, events []ledgerEvent) {
	fmt.Fprintf(stdout, "账单标识：%s\n", b.ID)
	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "月份：%s（UTC 自然月，左闭右开）\n", b.Month)
	fmt.Fprintf(stdout, "原总金额：%d 分（%s）\n", b.TotalFee, moneyFen(b.TotalFee))
	fmt.Fprintf(stdout, "存档全局序号上限：%d\n", nextSeq)
	fmt.Fprintf(stdout, "截止操作序号：%d（%s）\n", cutoff, cutoffDesc)
	fmt.Fprintf(stdout, "初始余额：应付 %d 分（%s），实收 0 分（%s），未收余额 %d 分（%s）\n",
		b.TotalFee, moneyFen(b.TotalFee), moneyFen(0), b.TotalFee, moneyFen(b.TotalFee))

	// 截止包含该序号；截止之后的事件（含撤销）既不出现也不影响历史余额。
	payable, received := b.TotalFee, int64(0)
	var shown []ledgerEvent
	for _, ev := range events {
		if ev.seq > cutoff {
			continue
		}
		shown = append(shown, ev)
		payable, received = ev.afterPayable, ev.afterReceived
	}
	if len(shown) == 0 {
		fmt.Fprintln(stdout, "流水：无（截止序号以内该账单无账后事件）")
	} else {
		fmt.Fprintln(stdout, "流水（按操作序号升序）：")
		for i, ev := range shown {
			fmt.Fprintf(stdout, "  %d. %s\n", i+1, formatLedgerEvent(ev))
		}
	}
	fmt.Fprintf(stdout, "截止时余额：应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
		payable, moneyFen(payable), received, moneyFen(received), payable-received, moneyFen(payable-received))
}

// formatLedgerEvent 渲染一条流水事件：序号、类型、原记录标识、金额影响、
// 原因或备注、撤销关联，以及事件后的应付、实收、未收余额；收款类事件同时
// 说明汇款总额与本账单分配。
func formatLedgerEvent(ev ledgerEvent) string {
	after := fmt.Sprintf("事后：应付 %d 分，实收 %d 分，未收余额 %d 分",
		ev.afterPayable, ev.afterReceived, ev.afterPayable-ev.afterReceived)
	switch ev.kind {
	case "调整":
		return fmt.Sprintf("序号 %d 调整 %s：应付 %+d 分（%s，%s），原因：%s → %s",
			ev.seq, ev.refID, ev.deltaPayable, moneyFen(ev.deltaPayable), adjustKind(ev.deltaPayable), ev.note, after)
	case "撤销调整":
		return fmt.Sprintf("序号 %d 撤销调整 %s：应付 %+d 分（%s，关联序号 %d 的调整 %s），原因：%s → %s",
			ev.seq, ev.refID, ev.deltaPayable, moneyFen(ev.deltaPayable), ev.linkSeq, ev.refID, ev.note, after)
	case "收款":
		return fmt.Sprintf("序号 %d 收款 %s：实收 %+d 分（%s，汇款总额 %d 分，本账单分配 %d 分），备注：%s → %s",
			ev.seq, ev.refID, ev.deltaReceived, moneyFen(ev.deltaReceived), ev.payTotal, ev.payAlloc, ev.note, after)
	case "更正":
		return fmt.Sprintf("序号 %d 更正 %s：实收 %+d 分（%s，关联收款 %s，本账单分配 %d 分 → %d 分），原因：%s → %s",
			ev.seq, ev.refID, ev.deltaReceived, moneyFen(ev.deltaReceived), ev.payID, ev.beforeAlloc, ev.afterAlloc, ev.note, after)
	case "退款":
		return fmt.Sprintf("序号 %d 退款 %s：实收 %+d 分（%s，关联收款 %s，本账单退款 %d 分），原因：%s → %s",
			ev.seq, ev.refID, ev.deltaReceived, moneyFen(ev.deltaReceived), ev.payID, ev.payAlloc, ev.note, after)
	default: // 撤销收款
		return fmt.Sprintf("序号 %d 撤销收款 %s：实收 %+d 分（%s，关联序号 %d 的收款 %s，汇款总额 %d 分，取消本账单分配 %d 分），原因：%s → %s",
			ev.seq, ev.refID, ev.deltaReceived, moneyFen(ev.deltaReceived), ev.linkSeq, ev.refID, ev.payTotal, ev.payAlloc, ev.note, after)
	}
}
