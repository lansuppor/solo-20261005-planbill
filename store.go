package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 数据目录布局：
//
//	<dir>/state.json      全部业务数据（客户、用量、账单、封账状态）
//	<dir>/state.json.tmp  原子写入临时文件（保存后 rename 覆盖）
//
// 所有命令顺序运行，单次调用内完成“读入—修改—整体原子落盘”，
// 因此账单与封账状态在同一次保存中持久化，不会出现半写状态。

const stateVersion = 1

type customer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int64  `json:"price_fen"` // 固定单价，非负整数分，创建后不可修改
}

type usageRecord struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Time       string `json:"time"` // RFC3339，保留原始输入用于去重判定
	Quantity   int64  `json:"quantity"`
}

type billLine struct {
	UsageID  string `json:"usage_id"`
	Time     string `json:"time"`
	Quantity int64  `json:"quantity"`
	LineFee  int64  `json:"line_fee_fen"`
}

type bill struct {
	ID         string     `json:"id"`
	CustomerID string     `json:"customer_id"`
	Month      string     `json:"month"` // YYYY-MM（UTC）
	TotalQty   int64      `json:"total_quantity"`
	UnitPrice  int64      `json:"unit_price_fen"`
	TotalFee   int64      `json:"total_fee_fen"`
	Lines      []billLine `json:"lines"`
	CreatedAt  string     `json:"created_at"`
}

type state struct {
	Version   int                     `json:"version"`
	Customers map[string]*customer    `json:"customers"`
	Usage     map[string]*usageRecord `json:"usage"` // 全局唯一用量标识 -> 记录
	Bills     map[string]*bill        `json:"bills"` // 客户 + "|" + 月份 -> 账单
	path      string                  `json:"-"`
}

// loadStore 读取数据目录；目录不存在时按需创建并视为空库。
// 已存在但损坏或不可读取的数据一律报错，绝不当作空库覆盖。
func loadStore(dir string) (*state, error) {
	p := filepath.Join(dir, "state.json")
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return nil, fmt.Errorf("无法创建数据目录 %s: %w", dir, mkErr)
			}
			return newState(p), nil
		}
		return nil, fmt.Errorf("数据文件不可读取 %s: %w", p, err)
	}

	var s state
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("数据文件已损坏（不是有效的 JSON）%s: %w", p, err)
	}
	// 拒绝尾部多余内容，避免静默吞掉异常数据。
	if dec.More() {
		return nil, fmt.Errorf("数据文件已损坏（JSON 之后存在多余内容）: %s", p)
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("数据文件已损坏: %w", err)
	}
	s.path = p
	return &s, nil
}

func newState(p string) *state {
	return &state{
		Version:   stateVersion,
		Customers: map[string]*customer{},
		Usage:     map[string]*usageRecord{},
		Bills:     map[string]*bill{},
		path:      p,
	}
}

// validate 对读入的库做一致性检查，任何异常都按损坏处理。
func (s *state) validate() error {
	if s.Version != stateVersion {
		return fmt.Errorf("不支持的数据版本 %d（仅支持 %d）", s.Version, stateVersion)
	}
	if s.Customers == nil || s.Usage == nil || s.Bills == nil {
		return errors.New("缺少必要的数据节")
	}
	for id, c := range s.Customers {
		if c == nil {
			return fmt.Errorf("客户 %q 的数据为空", id)
		}
		if c.ID != id {
			return fmt.Errorf("客户标识不一致: 键 %q / 记录 %q", id, c.ID)
		}
		if c.ID == "" || c.Name == "" {
			return fmt.Errorf("客户 %q 的标识或名称为空", id)
		}
		if c.Price < 0 {
			return fmt.Errorf("客户 %q 单价为负", id)
		}
	}
	for id, u := range s.Usage {
		if u == nil {
			return fmt.Errorf("用量 %q 的数据为空", id)
		}
		if u.ID != id {
			return fmt.Errorf("用量标识不一致: 键 %q / 记录 %q", id, u.ID)
		}
		if u.ID == "" {
			return errors.New("存在空的用量标识")
		}
		if _, ok := s.Customers[u.CustomerID]; !ok {
			return fmt.Errorf("用量 %q 引用了不存在的客户 %q", u.ID, u.CustomerID)
		}
		if u.Quantity <= 0 {
			return fmt.Errorf("用量 %q 的数量非正", u.ID)
		}
		if _, err := time.Parse(time.RFC3339, u.Time); err != nil {
			return fmt.Errorf("用量 %q 的时间不是 RFC3339: %w", u.ID, err)
		}
	}
	for key, b := range s.Bills {
		if b == nil {
			return fmt.Errorf("账单 %q 的数据为空", key)
		}
		if billKey(b.CustomerID, b.Month) != key {
			return fmt.Errorf("账单键不一致: 键 %q / 客户月份 %s|%s", key, b.CustomerID, b.Month)
		}
		if _, ok := s.Customers[b.CustomerID]; !ok {
			return fmt.Errorf("账单 %q 引用了不存在的客户 %q", key, b.CustomerID)
		}
		if !validMonth(b.Month) {
			return fmt.Errorf("账单 %q 的月份无效", key)
		}
		if len(b.Lines) == 0 {
			return fmt.Errorf("账单 %q 没有明细", key)
		}
		var qty, fee int64
		for _, ln := range b.Lines {
			u, ok := s.Usage[ln.UsageID]
			if !ok {
				return fmt.Errorf("账单 %q 引用了不存在的用量 %q", key, ln.UsageID)
			}
			if u.CustomerID != b.CustomerID || u.Time != ln.Time || u.Quantity != ln.Quantity {
				return fmt.Errorf("账单 %q 的明细 %q 与用量记录不一致", key, ln.UsageID)
			}
			if !inMonth(u.Time, b.Month) {
				return fmt.Errorf("账单 %q 的明细 %q 不在账期内", key, ln.UsageID)
			}
			var err error
			qty, err = add64(qty, ln.Quantity)
			if err != nil {
				return fmt.Errorf("账单 %q 汇总数量溢出", key)
			}
			fee, err = add64(fee, ln.LineFee)
			if err != nil {
				return fmt.Errorf("账单 %q 汇总金额溢出", key)
			}
		}
		if qty != b.TotalQty {
			return fmt.Errorf("账单 %q 总数量与明细不符", key)
		}
		if fee != b.TotalFee {
			return fmt.Errorf("账单 %q 总金额与明细不符", key)
		}
		c := s.Customers[b.CustomerID]
		for _, ln := range b.Lines {
			lf, err := mul64(ln.Quantity, c.Price)
			if err != nil {
				return fmt.Errorf("账单 %q 明细 %q 小计溢出", key, ln.UsageID)
			}
			if lf != ln.LineFee {
				return fmt.Errorf("账单 %q 明细 %q 小计与数量×单价不符", key, ln.UsageID)
			}
		}
		if b.UnitPrice != c.Price {
			return fmt.Errorf("账单 %q 单价与客户固定单价不符", key)
		}
		// 封账期内不得存在游离于账单之外的用量（封账后新增被禁止，
		// 而已入账记录理应全部在明细中）。
		for _, u := range s.Usage {
			if u.CustomerID == b.CustomerID && inMonth(u.Time, b.Month) {
				found := false
				for _, ln := range b.Lines {
					if ln.UsageID == u.ID {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("客户 %s 的 %s 已封账，但用量 %q 未计入账单", b.CustomerID, b.Month, u.ID)
				}
			}
		}
	}
	return nil
}

// save 整体原子落盘：先写临时文件并同步，再 rename 覆盖。
// 只有完整保存成功后调用方才会向用户报告成功。
func (s *state) save() error {
	if err := s.validate(); err != nil {
		return fmt.Errorf("拒绝写入不一致的数据: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化数据失败: %w", err)
	}
	data = append(data, '\n')

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("写入临时数据文件失败: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("写入临时数据文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("同步数据文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("关闭数据文件失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换数据文件失败: %w", err)
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

func billKey(customerID, month string) string {
	return customerID + "|" + month
}

// sealed 报告某客户的指定月份是否已封账。
func (s *state) sealed(customerID, month string) bool {
	_, ok := s.Bills[billKey(customerID, month)]
	return ok
}
