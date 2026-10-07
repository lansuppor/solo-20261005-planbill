package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 整批自动分配收款导入文件（CSV，UTF-8，首行为固定表头，之后每行一笔汇款）：
//
//	customer_id,payment_id,total_fen,note
//	acme,pay-001,900,季度汇款
//
//	customer_id 必须是已登记的客户标识
//	payment_id  全局收款标识，非空；与 bill pay / bill remit / bill remit-auto
//	            共用同一命名空间，自动与显式分配登记之间不得复用
//	total_fen   总金额，正整数人民币分（有符号 64 位整数范围内）
//	note        备注，非空（CSV 字段，可含逗号，按 RFC 4180 加引号）
const remitImportHeader = "customer_id,payment_id,total_fen,note"

// autoAllocate 是 bill remit-auto 与整批导入共用的自动分配核心：只选择该
// 客户当前已有账单，按 UTC 账期月升序依次偿还当前未收余额（调整、最新收款
// 分配、撤销及退款后的净额），跳过余额为 0 的月份，前一月份还清后才分配
// 下一月，最后一月可部分偿还；不补结算、不跨客户、不留未分配款项。
// 无欠款或总金额超过全部欠款时返回错误。
//
// 分配依据调用时的存档当前状态：整批导入按输入顺序逐笔处理，前序已接受的
// 新收款在同一 state 上生效，本笔即按其形成的新余额分配。remaining 从总额
// （正整数分）起只减不增，各月余额与分配全程整数计算——即使全部账单欠款
// 合计超过 64 位上限，分配过程也只涉及不超过总额的中间值，不会误拒合法金额。
func autoAllocate(s *state, customerID string, total int64) ([]paymentAllocation, error) {
	// 归集该客户当前已有账单的月份，按 UTC 账期月升序排列
	// （YYYY-MM 字典序即时间序）。不补结算，只有已存在账单参与分配。
	var months []string
	for _, b := range s.Bills {
		if b.CustomerID == customerID {
			months = append(months, b.Month)
		}
	}
	sort.Strings(months)

	remaining := total
	var allocs []paymentAllocation
	for _, m := range months {
		if remaining == 0 {
			break
		}
		b := s.Bills[billKey(customerID, m)]
		_, payable, err := billTotals(b, adjustmentsFor(s, customerID, m))
		if err != nil {
			return nil, fmt.Errorf("客户 %s 的 %s 当前应付异常，拒绝登记收款: %w", customerID, m, err)
		}
		received, err := paymentReceived(s, customerID, m)
		if err != nil {
			return nil, fmt.Errorf("客户 %s 的 %s 实收累计溢出有符号 64 位整数范围，拒绝登记收款: %w", customerID, m, err)
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
		return nil, fmt.Errorf("客户 %s 当前没有欠款（全部已存在账单的未收余额均为 0），整笔拒绝；不补结算、不跨客户、不留未分配款项", customerID)
	}
	if remaining > 0 {
		return nil, fmt.Errorf("收款总额 %d 分超过客户 %s 全部已存在账单的欠款合计 %d 分，整笔拒绝；不补结算、不跨客户、不留未分配款项",
			total, customerID, total-remaining)
	}
	return allocs, nil
}

// parsedRemitRow 是导入文件中一行解析后的候选汇款。
type parsedRemitRow struct {
	line       int    // 文件中的行号（含表头），用于报错定位
	customerID string // 已 TrimSpace
	payID      string
	total      int64
	note       string // 备注保留原文，仅做首尾空白裁剪
}

// remitImportResult 记录一行在整批预演中的处置：dup 为重复跳过时 p 指向
// 存档已有收款；新增时 p 指向已在预演状态中登记（尚未持久化）的新收款。
type remitImportResult struct {
	row *parsedRemitRow
	dup bool
	p   *payment
}

func cmdBillRemitImport(dir, file string) error {
	var reader io.ReadCloser
	var source string
	if file == "-" {
		reader = io.NopCloser(stdin)
		source = "标准输入"
	} else {
		f, err := os.Open(file)
		if err != nil {
			return fmt.Errorf("无法打开收款文件 %s: %w", file, err)
		}
		reader = f
		source = file
	}
	defer reader.Close()

	rows, parseErrs := parseRemitImportFile(reader)

	s, err := loadStore(dir)
	if err != nil {
		return err
	}

	// 先校验、预演全部行：任何问题都在此阶段收集，全部通过后才一次性保存，
	// 保证“整批先校验再生效”，失败不留下任何新收款或序号。
	problems := append([]string(nil), parseErrs...)

	// results 保留每行的处置，供成功后按输入顺序报告。
	results := make([]*remitImportResult, 0, len(rows))
	newCount, dupCount := 0, 0

	for _, r := range rows {
		res := &remitImportResult{row: r}
		results = append(results, res)

		if _, ok := s.Customers[r.customerID]; !ok {
			problems = append(problems, fmt.Sprintf("第 %d 行：客户标识 %q 不存在", r.line, r.customerID))
			continue
		}

		// 判重先于欠款分配，且同时对“存档已有”和“本批先前出现”生效：
		// 与 bill remit-auto 共用收款标识与自动登记身份，按客户、总金额、
		// 备注判重；存档已有或本批先前出现的相同记录计为重复跳过。
		if existing, ok := s.Payments[r.payID]; ok {
			switch {
			case !existing.Auto:
				// 显式登记（bill pay / bill remit）占用的收款标识不能复用，
				// 即使内容碰巧相同也整批拒绝。
				problems = append(problems, fmt.Sprintf("第 %d 行：收款标识 %q 已由显式分配登记占用（已有：客户=%s 总额=%d 备注=%q 分配=%s），自动与显式分配登记之间不得复用",
					r.line, r.payID, existing.CustomerID, existing.Total, existing.Note, formatAllocations(existing.Allocations)))
			case existing.CustomerID == r.customerID && existing.Total == r.total && existing.Note == r.note:
				// 相同重放：不重新分配、不恢复款项或旧分配，不占序号；
				// 后来新增账单、更正、退款或撤销均不阻止相同重放。
				res.dup = true
				res.p = existing
				dupCount++
			default:
				problems = append(problems, fmt.Sprintf("第 %d 行：收款标识 %q 已存在但内容不同（已有：客户=%s 总额=%d 备注=%q），拒绝复用",
					r.line, r.payID, existing.CustomerID, existing.Total, existing.Note))
			}
			continue
		}

		// 全新收款：基于原存档及本批前面已接受的新收款形成的余额自动分配。
		allocs, aerr := autoAllocate(s, r.customerID, r.total)
		if aerr != nil {
			problems = append(problems, fmt.Sprintf("第 %d 行：%v", r.line, aerr))
			continue
		}

		// 在同一 state 上登记本笔（先不持久化），后续行即按包含本笔的新
		// 余额分配；首次分配永久保留，各笔按首次出现顺序各占一个连续递增
		// 的账后全局序号，重复行不占序号，不按月份拆笔。
		p := &payment{
			ID:          r.payID,
			CustomerID:  r.customerID,
			Total:       r.total,
			Note:        r.note,
			Allocations: allocs,
			Auto:        true,
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		}
		s.NextSeq++
		p.Seq = s.NextSeq
		s.Payments[r.payID] = p
		res.p = p
		newCount++
	}

	if len(problems) > 0 {
		// 整批拒绝：预演阶段对 state 的内存修改从未落盘，磁盘存档保持原样，
		// 不占任何新标识或序号，可排除故障后原样重试。
		return fmt.Errorf("收款整批导入失败，整批未生效（共 %d 个问题）：\n  %s", len(problems), strings.Join(problems, "\n  "))
	}

	// 全为重复时不写盘：判重跳过不改变存档，返回原记录当前状态。
	if newCount == 0 {
		fmt.Fprintf(stdout, "收款整批导入完成（%s）：全部为已有记录，新增 0 笔，重复跳过 %d 行；未改写存档\n", source, dupCount)
		printRemitImportResults(s, results)
		return nil
	}

	// 整批新增记录、分配与序号一次原子保存完成后才报告成功；保存失败则
	// 一切不生效，不占任何新标识或序号，可原样重试。
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "收款整批导入完成（%s）：新增 %d 笔，重复跳过 %d 行\n", source, newCount, dupCount)
	printRemitImportResults(s, results)
	return nil
}

// printRemitImportResults 按输入顺序逐行列出新增或跳过、收款标识、首次与
// 最新分配；重复行展示存档记录的首次与最新（当前）分配。
func printRemitImportResults(s *state, results []*remitImportResult) {
	for i, res := range results {
		r := res.row
		head := fmt.Sprintf("%d. 第 %d 行：", i+1, r.line)
		if res.dup {
			fmt.Fprintf(stdout, "%s重复跳过 收款 %q（客户 %s，总额 %d 分（%s），备注：%s）\n",
				head, r.payID, r.customerID, r.total, moneyFen(r.total), r.note)
		} else {
			fmt.Fprintf(stdout, "%s新增 收款 %q（客户 %s，总额 %d 分（%s），备注：%s，账后序号 %d）\n",
				head, r.payID, r.customerID, r.total, moneyFen(r.total), r.note, res.p.Seq)
		}
		if res.p != nil {
			current := currentAllocations(s, res.p)
			fmt.Fprintf(stdout, "   首次分配：%s\n", formatAllocations(res.p.Allocations))
			if sameAllocations(current, res.p.Allocations) {
				fmt.Fprintf(stdout, "   最新分配：%s（与首次分配相同）\n", formatAllocations(current))
			} else {
				fmt.Fprintf(stdout, "   最新分配（经更正，以最新为准）：%s\n", formatAllocations(current))
			}
		}
	}
}

// parseRemitImportFile 完整读取 CSV，做格式层面的解析；业务层面的校验在
// 调用方完成。字段恰好四个，首行为固定表头，至少一行数据。
func parseRemitImportFile(r io.Reader) ([]*parsedRemitRow, []string) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = 4
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err == io.EOF {
		return nil, []string{"文件为空：首行必须是表头 " + remitImportHeader}
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("无法读取表头：%v（首行必须是 %s）", err, remitImportHeader)}
	}
	for i := range header {
		header[i] = strings.TrimSpace(header[i])
	}
	if len(header) != 4 || header[0] != "customer_id" || header[1] != "payment_id" ||
		header[2] != "total_fen" || header[3] != "note" {
		return nil, []string{fmt.Sprintf("表头必须是 %s，实际为 %s", remitImportHeader, strings.Join(header, ","))}
	}

	var rows []*parsedRemitRow
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
		customerID := strings.TrimSpace(rec[0])
		payID := strings.TrimSpace(rec[1])
		totalText := strings.TrimSpace(rec[2])
		note := strings.TrimSpace(rec[3])

		rowBad := false
		if customerID == "" {
			problems = append(problems, fmt.Sprintf("第 %d 行：客户标识不能为空", line))
			rowBad = true
		}
		if payID == "" {
			problems = append(problems, fmt.Sprintf("第 %d 行：收款标识不能为空", line))
			rowBad = true
		}
		total, perr := strconv.ParseInt(totalText, 10, 64)
		if perr != nil {
			problems = append(problems, fmt.Sprintf("第 %d 行：总金额 %q 不是有符号 64 位整数范围内的整数", line, totalText))
			rowBad = true
		} else if total <= 0 {
			problems = append(problems, fmt.Sprintf("第 %d 行：总金额必须是正整数人民币分，收到 %d", line, total))
			rowBad = true
		}
		if note == "" {
			problems = append(problems, fmt.Sprintf("第 %d 行：收款备注不能为空", line))
			rowBad = true
		}
		// 格式有问题的行不进入业务校验阶段。
		if rowBad {
			continue
		}
		rows = append(rows, &parsedRemitRow{
			line:       line,
			customerID: customerID,
			payID:      payID,
			total:      total,
			note:       note,
		})
	}
	if len(rows) == 0 && len(problems) == 0 {
		problems = append(problems, "文件没有数据行：收款整批导入至少需要一条记录")
	}
	return rows, problems
}
