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

// 本文件实现收款文件的整批自动分配导入（bill remit-import）：一个文件可含
// 多个客户及同一客户的多笔汇款，每行只给出客户、全局收款标识、总金额与
// 备注。整份文件先校验再生效：全新收款基于原存档及本批前面已接受的新收款
// 形成的余额，按 UTC 账期月升序偿还已有账单欠款；任一行非法（含无欠款或
// 金额超出欠款）整批拒绝，不写盘、不占任何标识或序号。全部新增记录、分配
// 与序号在同一次原子保存中落盘后才报告成功；全为重复时不写盘。
//
// 导入与 bill remit-auto 共用收款标识和自动登记身份（payment.Auto），按
// 客户、总额、备注判重；存档已有或本批先前出现的相同记录计为重复跳过，
// 内容不同整批拒绝；显式登记（bill pay / bill remit）的收款标识不能复用。

// 收款导入文件（CSV，UTF-8，首行为固定表头，之后每行一笔汇款）：
//
//	payment_id,customer_id,total_fen,note
//	pay-001,acme,900,季度汇款
//	pay-002,globex,500,"货款, 含九月尾款"
//
//	payment_id   全局收款标识，非空，与 bill remit-auto 共用，自动与显式
//	             分配登记之间不得复用
//	customer_id  必须是已登记客户
//	total_fen    总金额，正整数人民币分（有符号 64 位整数范围内）
//	note         备注，非空（可含逗号，按 CSV 规则加引号）
const remitHeader = "payment_id,customer_id,total_fen,note"

// remitKey 是自动登记收款的判重三元组：客户、总额、备注。
type remitKey struct {
	customerID string
	total      int64
	note       string
}

// parsedRemitRow 是导入文件中一行解析后的候选汇款。
type parsedRemitRow struct {
	line       int // 文件中的行号（含表头），用于报错定位
	customerID string
	payID      string
	total      int64
	note       string
}

// acceptedRemit 是批处理中一笔已接受新增收款的登记结果。
type acceptedRemit struct {
	row    *parsedRemitRow
	p      *payment
	allocs []paymentAllocation // 首次分配（== 保存时的 p.Allocations）
}

// remitRowResult 是一行的分类结果：新增或重复跳过。
type remitRowResult struct {
	row   *parsedRemitRow
	isNew bool
	dupOf string // 重复来源说明："存档已有" 或 "本批先前出现"
	p     *payment
}

func cmdBillRemitImport(dir, file string) error {
	var reader io.ReadCloser
	var source string
	if file == "-" {
		reader = io.NopCloser(os.Stdin)
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

	rows, parseErrs := parseRemitFile(reader)

	s, err := loadStore(dir)
	if err != nil {
		return err
	}

	// 先校验：任何问题都在此阶段收集，全部通过后才整体原子保存，
	// 保证解析、校验或保存失败时全部业务状态不变。
	var problems []string
	problems = append(problems, parseErrs...)
	if len(rows) == 0 && len(problems) == 0 {
		problems = append(problems, "文件只有表头：收款导入至少需要一条记录")
	}

	// batchNew 记录本批先前已接受的新增收款（它们同时挂在内存的 s.Payments
	// 上以参与后续行的余额计算，但尚未落盘）；判重时优先于存档识别，使本批
	// 重复行标注为“本批先前出现”，存档已有记录标注为“存档已有”。
	batchNew := make(map[string]*payment)
	var accepted []*acceptedRemit
	var results []remitRowResult

	for _, r := range rows {
		// 格式问题已在解析阶段收集；此处仍对必填项做防御性检查。
		if r.payID == "" || r.customerID == "" || strings.TrimSpace(r.note) == "" || r.total <= 0 {
			problems = append(problems, fmt.Sprintf("第 %d 行：字段不合法（标识、客户、非空备注与正整数金额均必需）", r.line))
			continue
		}
		key := remitKey{customerID: r.customerID, total: r.total, note: r.note}

		// 判重先于余额分配：本批先前出现或存档已有的相同自动登记记录计为
		// 重复跳过；重复行不重新分配、不占序号，后来新增账单、更正、退款或
		// 撤销都不阻止相同重放。
		if prev, ok := batchNew[r.payID]; ok {
			prevKey := remitKey{customerID: prev.CustomerID, total: prev.Total, note: prev.Note}
			if prevKey != key {
				problems = append(problems, fmt.Sprintf("第 %d 行：收款标识 %q 在本批先前已接受但内容不同（先前：客户=%s 总额=%d 备注=%q），拒绝复用",
					r.line, r.payID, prev.CustomerID, prev.Total, prev.Note))
				continue
			}
			results = append(results, remitRowResult{row: r, isNew: false, dupOf: "本批先前出现", p: prev})
			continue
		}
		if existing, ok := s.Payments[r.payID]; ok {
			if !existing.Auto {
				problems = append(problems, fmt.Sprintf("第 %d 行：收款标识 %q 已由显式分配登记占用（已有：客户=%s 总额=%d 备注=%q 分配=%s），自动与显式分配登记之间不得复用",
					r.line, r.payID, existing.CustomerID, existing.Total, existing.Note, formatAllocations(existing.Allocations)))
				continue
			}
			existKey := remitKey{customerID: existing.CustomerID, total: existing.Total, note: existing.Note}
			if existKey != key {
				problems = append(problems, fmt.Sprintf("第 %d 行：收款标识 %q 已存在但内容不同（已有：客户=%s 总额=%d 备注=%q），拒绝复用",
					r.line, r.payID, existing.CustomerID, existing.Total, existing.Note))
				continue
			}
			results = append(results, remitRowResult{row: r, isNew: false, dupOf: "存档已有", p: existing})
			continue
		}

		if _, ok := s.Customers[r.customerID]; !ok {
			problems = append(problems, fmt.Sprintf("第 %d 行：客户标识 %q 不存在", r.line, r.customerID))
			continue
		}

		// 全新收款：余额基于原存档及本批前面已接受的新收款（它们已挂到
		// 内存中的 s.Payments，paymentReceived 会计入），按最早欠款账期
		// 自动分配；无欠款或金额超出欠款则本行非法，整批拒绝。
		allocs, err := autoAllocate(s, r.customerID, r.total)
		if err != nil {
			problems = append(problems, fmt.Sprintf("第 %d 行：%v", r.line, err))
			continue
		}

		p := &payment{
			ID:          r.payID,
			CustomerID:  r.customerID,
			Total:       r.total,
			Note:        r.note,
			Allocations: allocs,
			Auto:        true,
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		}
		// 按首次出现顺序各占一个连续递增的账后全局序号；重复行不占序号。
		s.NextSeq++
		p.Seq = s.NextSeq
		s.Payments[r.payID] = p
		batchNew[r.payID] = p
		accepted = append(accepted, &acceptedRemit{row: r, p: p, allocs: allocs})
		results = append(results, remitRowResult{row: r, isNew: true, p: p})
	}

	if len(problems) > 0 {
		// 内存中挂接的新收款不落盘即随进程退出丢弃；磁盘存档保持原样，
		// 不占任何新标识或序号，可排除故障后原样重试。
		return fmt.Errorf("收款整批导入失败，整批未生效（共 %d 个问题）：\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}

	newCount := len(accepted)
	if newCount > 0 {
		// 整批新增记录、分配与序号一次原子保存；保存失败全部业务状态不变。
		if err := s.save(); err != nil {
			for _, a := range accepted {
				delete(s.Payments, a.p.ID)
			}
			s.NextSeq -= int64(newCount)
			return fmt.Errorf("收款整批导入保存失败，整批未生效: %w", err)
		}
	}

	// 完整保存成功（或全为重复不写盘）后才按输入顺序逐行报告。
	if newCount == 0 {
		fmt.Fprintf(stdout, "整批自动分配导入完成（%s）：新增 0 笔，重复跳过 %d 行（全部为重复记录，未改写存档）\n",
			source, len(results))
	} else {
		fmt.Fprintf(stdout, "整批自动分配导入完成（%s）：新增 %d 笔，重复跳过 %d 行\n",
			source, newCount, len(results)-newCount)
	}
	for i, res := range results {
		r := res.row
		current := currentAllocations(s, res.p)
		first := res.p.Allocations
		if res.isNew {
			fmt.Fprintf(stdout, "  %d. 第 %d 行 新增：收款标识 %s，客户 %s，总额 %d 分（%s），备注：%s\n",
				i+1, r.line, r.payID, r.customerID, r.total, moneyFen(r.total), r.note)
		} else {
			fmt.Fprintf(stdout, "  %d. 第 %d 行 重复跳过（%s，不重新分配、不占序号）：收款标识 %s，客户 %s，总额 %d 分（%s），备注：%s\n",
				i+1, r.line, res.dupOf, r.payID, r.customerID, r.total, moneyFen(r.total), r.note)
		}
		fmt.Fprintf(stdout, "     首次分配：%s\n", formatAllocations(first))
		if sameAllocations(current, first) {
			fmt.Fprintf(stdout, "     最新分配：%s（与首次分配相同）\n", formatAllocations(current))
		} else {
			fmt.Fprintf(stdout, "     最新分配（经更正，以最新为准）：%s\n", formatAllocations(current))
		}
	}
	return nil
}

// parseRemitFile 完整读取收款 CSV，做格式层面的解析；业务层面的校验在调用方
// 完成。规则与用量导入一致：固定表头、恰好四个字段、整数字段按 64 位解析。
func parseRemitFile(r io.Reader) ([]*parsedRemitRow, []string) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = 4
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err == io.EOF {
		return nil, []string{"文件为空：首行必须是表头 " + remitHeader}
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("无法读取表头：%v（首行必须是 %s）", err, remitHeader)}
	}
	for i := range header {
		header[i] = strings.TrimSpace(header[i])
	}
	if len(header) != 4 || header[0] != "payment_id" || header[1] != "customer_id" ||
		header[2] != "total_fen" || header[3] != "note" {
		return nil, []string{fmt.Sprintf("表头必须是 %s，实际为 %s", remitHeader, strings.Join(header, ","))}
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
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}

		payID, custID, totalText, note := rec[0], rec[1], rec[2], rec[3]
		if payID == "" {
			problems = append(problems, fmt.Sprintf("第 %d 行：收款标识不能为空", line))
			continue
		}
		if custID == "" {
			problems = append(problems, fmt.Sprintf("第 %d 行：客户标识不能为空", line))
			continue
		}
		total, perr := strconv.ParseInt(totalText, 10, 64)
		if perr != nil {
			problems = append(problems, fmt.Sprintf("第 %d 行：总金额 %q 不是有符号 64 位整数范围内的整数", line, totalText))
			continue
		}
		if total <= 0 {
			problems = append(problems, fmt.Sprintf("第 %d 行：总金额必须是正整数分，收到 %d", line, total))
			continue
		}
		if strings.TrimSpace(note) == "" {
			problems = append(problems, fmt.Sprintf("第 %d 行：备注不能为空", line))
			continue
		}

		rows = append(rows, &parsedRemitRow{
			line:       line,
			customerID: custID,
			payID:      payID,
			total:      total,
			note:       note,
		})
	}
	return rows, problems
}

// autoAllocate 是 bill remit-auto 与整批导入共用的自动分配核心：只选择该
// 客户当前已有账单，按 UTC 账期月升序依次偿还当前未收余额（调整、最新收款
// 分配、撤销及退款后的净额），跳过余额为 0 的月份，前一月份还清后才分配
// 下一月，最后一月可部分偿还。不补结算、不跨客户、不留未分配款项；无欠款
// 或总金额超过全部欠款时返回错误。
//
// 每月未收余额 = 当前应付 − 实收，均在 [0, 有符号 64 位最大值] 内；
// remaining 从总额（正整数分）起只减不增，各月余额与分配全程整数计算——
// 即使全部账单欠款合计超过 64 位上限，分配过程也只涉及不超过总额的中间值，
// 不会误拒合法金额。
func autoAllocate(s *state, customerID string, total int64) ([]paymentAllocation, error) {
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
