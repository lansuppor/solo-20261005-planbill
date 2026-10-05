package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// storeFile 是数据目录中的数据文件名。
const storeFile = "data.json"

// dbVersion 是当前数据格式版本。
const dbVersion = 1

// Customer 是已登记客户。单价（分/计量单位）创建后不可修改。
type Customer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int64  `json:"price"` // 非负整数分
}

// Usage 是一条已生效的用量记录。时间以 RFC3339 文本存储，
// 统一规范化到 UTC，便于结算归集与去重比较。
type Usage struct {
	RecID      string `json:"recId"`
	CustomerID string `json:"customerId"`
	Time       string `json:"time"` // 规范化后的 UTC RFC3339 时间
	Quantity   int64  `json:"quantity"`
}

// Line 是账单中的一条用量明细。
type Line struct {
	RecID    string `json:"recId"`
	Time     string `json:"time"`
	Quantity int64  `json:"quantity"`
	Subtotal int64  `json:"subtotal"` // 数量 × 单价（分）
}

// Bill 是一次月结产物。同一客户同一 UTC 自然月只有一张账单。
type Bill struct {
	ID         string `json:"id"`
	CustomerID string `json:"customerId"`
	Month      string `json:"month"` // YYYY-MM（UTC）
	TotalQty   int64  `json:"totalQty"`
	UnitPrice  int64  `json:"unitPrice"`
	TotalAmt   int64  `json:"totalAmt"`
	Lines      []Line `json:"lines"`
}

// dbJSON 是磁盘上的完整数据快照。账单与封账状态保存在同一快照内原子落盘。
type dbJSON struct {
	Version     int             `json:"version"`
	NextBillSeq int64           `json:"nextBillSeq"`
	Customers   []Customer      `json:"customers"`
	Usages      []Usage         `json:"usages"`
	Bills       map[string]Bill `json:"bills"` // billKey(客户标识, 月份) -> 账单
}

// Store 通过单个 JSON 文件持久化全部业务数据，跨进程保存，不依赖外部服务。
type Store struct {
	dir  string
	data dbJSON
}

// NewStore 创建存储并从数据目录加载已有数据。
// 数据文件存在但损坏或不可读取时返回错误，绝不当作空库覆盖。
func NewStore(dir string) (*Store, error) {
	s := &Store{dir: dir}
	path := filepath.Join(dir, storeFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.data.Version = dbVersion
			s.data.Bills = map[string]Bill{}
			return s, nil
		}
		return nil, fmt.Errorf("读取数据文件 %s 失败: %w", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, fmt.Errorf("数据文件 %s 已损坏: 文件为空", path)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&s.data); err != nil {
		return nil, fmt.Errorf("数据文件 %s 已损坏: %v", path, err)
	}
	// 不允许文件尾部还有多余内容，避免静默吞掉损坏数据。
	if dec.More() {
		return nil, fmt.Errorf("数据文件 %s 已损坏: JSON 结束后仍有多余数据", path)
	}
	if s.data.Version != dbVersion {
		return nil, fmt.Errorf("数据文件 %s 已损坏: 不支持的数据版本 %d（期望 %d）", path, s.data.Version, dbVersion)
	}
	if s.data.Bills == nil {
		s.data.Bills = map[string]Bill{}
	}
	if err := s.data.validateLoaded(); err != nil {
		return nil, fmt.Errorf("数据文件 %s 已损坏: %v", path, err)
	}
	return s, nil
}

// validateLoaded 对已加载数据做完整性检查，发现结构异常即拒绝。
func (d *dbJSON) validateLoaded() error {
	cust := map[string]bool{}
	for _, c := range d.Customers {
		if c.ID == "" {
			return errors.New("存在空客户标识")
		}
		if cust[c.ID] {
			return fmt.Errorf("客户标识 %q 重复", c.ID)
		}
		if c.Price < 0 {
			return fmt.Errorf("客户 %q 单价为负", c.ID)
		}
		cust[c.ID] = true
	}
	rec := map[string]bool{}
	for _, u := range d.Usages {
		if u.RecID == "" {
			return errors.New("存在空用量标识")
		}
		if rec[u.RecID] {
			return fmt.Errorf("用量标识 %q 重复", u.RecID)
		}
		if !cust[u.CustomerID] {
			return fmt.Errorf("用量 %q 引用了不存在的客户 %q", u.RecID, u.CustomerID)
		}
		if u.Quantity <= 0 {
			return fmt.Errorf("用量 %q 数量非正", u.RecID)
		}
		if _, err := parseUsageTime(u.Time); err != nil {
			return fmt.Errorf("用量 %q 时间无效: %v", u.RecID, err)
		}
		rec[u.RecID] = true
	}
	for k, b := range d.Bills {
		if !cust[b.CustomerID] {
			return fmt.Errorf("账单 %q 引用了不存在的客户", b.ID)
		}
		if billKey(b.CustomerID, b.Month) != k {
			return fmt.Errorf("账单索引 %q 与内容不一致", k)
		}
	}
	return nil
}

// save 将完整快照原子写入：先写临时文件并 fsync，再重命名替换，
// 并对目录 fsync；崩溃不会留下半写状态，业务调用成功只在完整保存后报告。
func (s *Store) save() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录 %s 失败: %w", s.dir, err)
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化数据失败: %w", err)
	}
	b = append(b, '\n')
	path := filepath.Join(s.dir, storeFile)
	tmp, err := os.CreateTemp(s.dir, ".data.json.*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时数据文件失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时数据文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("同步临时数据文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时数据文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("替换数据文件失败: %w", err)
	}
	cleanup = false
	if d, err := os.Open(s.dir); err == nil {
		if err := d.Sync(); err != nil {
			d.Close()
			return fmt.Errorf("同步数据目录失败: %w", err)
		}
		d.Close()
	}
	return nil
}

// ---- 查询辅助 ----

func billKey(customerID, month string) string { return customerID + "\x00" + month }

func (s *Store) findCustomer(id string) *Customer {
	for i := range s.data.Customers {
		if s.data.Customers[i].ID == id {
			return &s.data.Customers[i]
		}
	}
	return nil
}

func (s *Store) findUsage(recID string) *Usage {
	for i := range s.data.Usages {
		if s.data.Usages[i].RecID == recID {
			return &s.data.Usages[i]
		}
	}
	return nil
}
