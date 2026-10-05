package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// importRecord 是用量导入文件中的一行（JSON 对象）。
type importRecord struct {
	ID       string      `json:"id"`       // 全局唯一用量标识
	Customer string      `json:"customer"` // 客户标识
	Time     string      `json:"time"`     // RFC3339 时间
	Quantity json.Number `json:"quantity"` // 正整数数量
}

// ImportResult 是一次成功导入的计数。
type ImportResult struct {
	Added      int
	Duplicated int
}

// pendingImport 是通过校验、待生效的一条记录。
type pendingImport struct {
	customer string
	time     string // 规范化后的 UTC RFC3339 时间
	qty      int64
}

// ImportUsage 从 JSONL 文件整批导入用量。
//
// 规则：先完整校验、再生效；任何一条记录有问题则整批失败、不留下新用量。
// 已存在的用量标识按（客户、解析后的 UTC 时间点、数量）判定：
// 内容相同视为重复并跳过，内容不同拒绝整批；文件内重复遵循同样规则。
// 若该记录所属客户的 UTC 自然月已封账（已结算），新用量不得进入；
// 已入账记录的相同重放仍算重复、可成功跳过。
func (s *Store) ImportUsage(path string) (*ImportResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("无法打开用量文件 %q: %w", path, err)
	}
	defer f.Close()

	// seen 记录本文件内已出现的用量标识及其规范化内容，处理文件内重复。
	seen := map[string]pendingImport{}
	var toAdd []Usage
	res := &ImportResult{}

	scanner := bufio.NewScanner(f)
	// 单行上限放宽到 8 MiB，避免正常长行被误判。
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	lineNo := 0
	recordLines := 0
	for scanner.Scan() {
		lineNo++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue // 空行不是记录
		}
		recordLines++
		rec, ut, qty, perr := parseImportLine(raw)
		if perr != nil {
			return nil, fmt.Errorf("用量文件 %q 第 %d 行格式错误: %v", path, lineNo, perr)
		}

		c := s.findCustomer(rec.Customer)
		if c == nil {
			return nil, fmt.Errorf("用量文件 %q 第 %d 行: 客户不存在: %q（用量标识 %q）", path, lineNo, rec.Customer, rec.ID)
		}

		cur := pendingImport{customer: rec.Customer, time: ut, qty: qty}

		// 与库内既有记录同标识：内容一致即重复跳过，不一致整批拒绝。
		if ex := s.findUsage(rec.ID); ex != nil {
			if ex.CustomerID != rec.Customer || ex.Time != ut || ex.Quantity != qty {
				return nil, fmt.Errorf(
					"用量文件 %q 第 %d 行: 用量标识 %q 与既有记录内容冲突（既有: 客户=%s 时间=%s 数量=%d；文件: 客户=%s 时间=%s 数量=%d）",
					path, lineNo, rec.ID,
					ex.CustomerID, ex.Time, ex.Quantity,
					rec.Customer, ut, qty)
			}
			seen[rec.ID] = cur
			res.Duplicated++
			continue
		}

		// 与本文件内先前记录同标识：同样规则。
		if p, dup := seen[rec.ID]; dup {
			if p != cur {
				return nil, fmt.Errorf("用量文件 %q 第 %d 行: 文件内用量标识 %q 重复但内容不一致（首见: 客户=%s 时间=%s 数量=%d）",
					path, lineNo, rec.ID, p.customer, p.time, p.qty)
			}
			res.Duplicated++
			continue
		}

		// 金额预校验：数量 × 固定单价不得溢出 int64（整数运算，不用浮点数）。
		if _, perr := mulInt64(qty, c.Price); perr != nil {
			return nil, fmt.Errorf("用量文件 %q 第 %d 行: 用量 %q 金额（数量 %d × 单价 %d）%v", path, lineNo, rec.ID, qty, c.Price, perr)
		}

		// 新用量不得进入已封账月份。
		var t time.Time
		if t, perr = parseUsageTime(ut); perr != nil {
			return nil, fmt.Errorf("用量文件 %q 第 %d 行: %v", path, lineNo, perr)
		}
		month := utcMonth(t)
		if _, sealed := s.data.Bills[billKey(rec.Customer, month)]; sealed {
			return nil, fmt.Errorf("用量文件 %q 第 %d 行: 客户 %q 的 %s 月已封账，新用量不得进入（用量标识 %q）",
				path, lineNo, rec.Customer, month, rec.ID)
		}

		seen[rec.ID] = cur
		toAdd = append(toAdd, Usage{
			RecID:      rec.ID,
			CustomerID: rec.Customer,
			Time:       ut,
			Quantity:   qty,
		})
		res.Added++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取用量文件 %q 失败: %w", path, err)
	}
	if recordLines == 0 {
		return nil, fmt.Errorf("用量文件 %q 为空或不包含任何记录", path)
	}

	// 全部校验通过后才一次性追加并原子保存；此前任何失败都不改变原业务状态。
	s.data.Usages = append(s.data.Usages, toAdd...)
	if err := s.save(); err != nil {
		return nil, err
	}
	return res, nil
}

// parseImportLine 解析并校验一行 JSON 用量记录，返回规范化 UTC 时间与数量。
func parseImportLine(line []byte) (importRecord, string, int64, error) {
	var rec importRecord
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields() // 字段名拼错（如 customerId）视为格式错误
	if err := dec.Decode(&rec); err != nil {
		return rec, "", 0, fmt.Errorf("不是合法 JSON 对象或含未知字段: %v", err)
	}
	if dec.More() {
		return rec, "", 0, fmt.Errorf("单行在 JSON 对象后还有多余内容")
	}
	if rec.ID == "" {
		return rec, "", 0, fmt.Errorf("用量标识 id 不能为空")
	}
	if rec.Customer == "" {
		return rec, "", 0, fmt.Errorf("客户标识 customer 不能为空（用量标识 %q）", rec.ID)
	}
	if rec.Time == "" {
		return rec, "", 0, fmt.Errorf("时间 time 不能为空（用量标识 %q）", rec.ID)
	}
	if rec.Quantity == "" {
		return rec, "", 0, fmt.Errorf("数量 quantity 缺失（用量标识 %q）", rec.ID)
	}
	qty, err := rec.Quantity.Int64()
	if err != nil {
		return rec, "", 0, fmt.Errorf("数量 quantity 必须是 64 位有符号整数（用量标识 %q）: %v", rec.ID, err)
	}
	if qty <= 0 {
		return rec, "", 0, fmt.Errorf("数量 quantity 必须为正整数，得到 %d（用量标识 %q）", qty, rec.ID)
	}
	t, err := parseUsageTime(rec.Time)
	if err != nil {
		return rec, "", 0, err
	}
	return rec, normalizeUTC(t), qty, nil
}
