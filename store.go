package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// adjustment 是对已结算账单的一次费用调整（正数补收、负数减免）。
// 记录一旦写入永不删除；撤销只是追加撤销信息，保留原记录。
type adjustment struct {
	ID           string `json:"id"`
	CustomerID   string `json:"customer_id"`
	Month        string `json:"month"` // YYYY-MM（UTC），与所属账单一致
	Amount       int64  `json:"amount_fen"`
	Reason       string `json:"reason"`
	Seq          int64  `json:"seq"` // 全局递增操作序号，决定历史展示顺序
	CreatedAt    string `json:"created_at"`
	Revoked      bool   `json:"revoked"`
	RevokeReason string `json:"revoke_reason,omitempty"`
	RevokeSeq    int64  `json:"revoke_seq,omitempty"`
	RevokedAt    string `json:"revoked_at,omitempty"`
}

// paymentAllocation 是一笔收款在某个已结算月份上的分配（正整数分）。
type paymentAllocation struct {
	Month  string `json:"month"`      // YYYY-MM（UTC），须已有账单
	Amount int64  `json:"amount_fen"` // 正整数分
}

// payment 是一笔实收登记：同一客户的一笔汇款（总额 Total 分）按分配列表
// 计入一个或多个已结算月份；单账单收款视为只有一项分配的汇款。
// 记录一旦写入永不删除；撤销只是追加撤销信息，整笔撤销全部分配并保留
// 原记录，撤销后该笔不再计入实收，也不会因重放而恢复。
type payment struct {
	ID           string              `json:"id"`
	CustomerID   string              `json:"customer_id"`
	Total        int64               `json:"total_fen"` // 收款总额，正整数分，等于分配合计
	Note         string              `json:"note"`
	Allocations  []paymentAllocation `json:"allocations"` // 至少一项，月份不重复，按月份升序保存
	Seq          int64               `json:"seq"`         // 全局递增操作序号，决定历史展示顺序
	CreatedAt    string              `json:"created_at"`
	Revoked      bool                `json:"revoked"`
	RevokeReason string              `json:"revoke_reason,omitempty"`
	RevokeSeq    int64               `json:"revoke_seq,omitempty"`
	RevokedAt    string              `json:"revoked_at,omitempty"`
}

// UnmarshalJSON 兼容旧版单账单收款格式（month + amount_fen，无 allocations）：
// 读入时归一化为一项分配，之后一律按新格式写回。
func (p *payment) UnmarshalJSON(data []byte) error {
	type plain payment // 去掉方法，避免递归
	var raw struct {
		plain
		Month  string `json:"month"`      // 旧格式：所属账单月份
		Amount int64  `json:"amount_fen"` // 旧格式：收款金额
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = payment(raw.plain)
	if len(p.Allocations) == 0 && raw.Month != "" {
		p.Allocations = []paymentAllocation{{Month: raw.Month, Amount: raw.Amount}}
		p.Total = raw.Amount
	}
	return nil
}

// amountFor 返回该收款在指定月份的首次登记分配金额；无分配时为 0。
// 当前生效分配（经更正后）请用 currentAmountFor。
func (p *payment) amountFor(month string) int64 {
	for _, al := range p.Allocations {
		if al.Month == month {
			return al.Amount
		}
	}
	return 0
}

// correction 是对一笔未撤销收款的分配更正：以更正时生效的分配（From）为
// 起点，整笔替换为完整新分配（To）。收款的客户、总额、备注与首次登记分配
// 永远不变；记录一旦写入永不删除，可连续更正（后一次以前一次的结果为起点）。
type correction struct {
	ID        string              `json:"id"`
	PaymentID string              `json:"payment_id"` // 目标收款标识
	Reason    string              `json:"reason"`
	From      []paymentAllocation `json:"from"` // 更正前生效分配（快照，按月份升序）
	To        []paymentAllocation `json:"to"`   // 更正后完整新分配（按月份升序）
	Seq       int64               `json:"seq"`  // 全局递增操作序号（与调整/收款及其撤销共用）
	CreatedAt string              `json:"created_at"`
}

type state struct {
	Version     int                     `json:"version"`
	Customers   map[string]*customer    `json:"customers"`
	Usage       map[string]*usageRecord `json:"usage"`       // 全局唯一用量标识 -> 记录
	Bills       map[string]*bill        `json:"bills"`       // 客户 + "|" + 月份 -> 账单
	Adjustments map[string]*adjustment  `json:"adjustments"` // 全局唯一调整标识 -> 记录
	Payments    map[string]*payment     `json:"payments"`    // 全局唯一收款标识 -> 记录（与调整标识相互独立，可同名）
	Corrections map[string]*correction  `json:"corrections"` // 全局唯一更正标识 -> 记录（与收款、调整标识相互独立，可同名）
	NextSeq     int64                   `json:"next_seq"`    // 已分配的最大操作序号（调整/收款/更正及其撤销共用）
	path        string                  `json:"-"`
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
	// 旧版本数据文件没有 adjustments/payments/corrections/next_seq 字段：
	// 视为零调整、零实收、零更正。
	if s.Adjustments == nil {
		s.Adjustments = map[string]*adjustment{}
	}
	if s.Payments == nil {
		s.Payments = map[string]*payment{}
	}
	if s.Corrections == nil {
		s.Corrections = map[string]*correction{}
	}
	s.path = p
	return &s, nil
}

func newState(p string) *state {
	return &state{
		Version:     stateVersion,
		Customers:   map[string]*customer{},
		Usage:       map[string]*usageRecord{},
		Bills:       map[string]*bill{},
		Adjustments: map[string]*adjustment{},
		Payments:    map[string]*payment{},
		Corrections: map[string]*correction{},
		path:        p,
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

	// 调整与收款记录：标识、内容、引用与序号都必须自洽。
	// 调整与收款共用同一个全局操作序号池；两者标识命名空间相互独立，
	// 允许同名，各自判重与撤销。
	if s.NextSeq < 0 {
		return fmt.Errorf("操作序号计数器为负: %d", s.NextSeq)
	}
	seenSeq := make(map[int64]string) // 全局操作序号（登记与撤销共用同一池）-> 来源描述
	for id, a := range s.Adjustments {
		if a == nil {
			return fmt.Errorf("调整 %q 的数据为空", id)
		}
		if a.ID != id {
			return fmt.Errorf("调整标识不一致: 键 %q / 记录 %q", id, a.ID)
		}
		if a.ID == "" {
			return errors.New("存在空的调整标识")
		}
		if a.Amount == 0 {
			return fmt.Errorf("调整 %q 的金额为零", id)
		}
		if strings.TrimSpace(a.Reason) == "" {
			return fmt.Errorf("调整 %q 的原因为空", id)
		}
		if !validMonth(a.Month) {
			return fmt.Errorf("调整 %q 的月份无效", id)
		}
		if _, ok := s.Customers[a.CustomerID]; !ok {
			return fmt.Errorf("调整 %q 引用了不存在的客户 %q", id, a.CustomerID)
		}
		if _, ok := s.Bills[billKey(a.CustomerID, a.Month)]; !ok {
			return fmt.Errorf("调整 %q 引用了不存在的账单（客户 %s 月份 %s）", id, a.CustomerID, a.Month)
		}
		if a.Seq < 1 || a.Seq > s.NextSeq {
			return fmt.Errorf("调整 %q 的操作序号越界", id)
		}
		if prev, dup := seenSeq[a.Seq]; dup {
			return fmt.Errorf("调整 %q 与 %q 的操作序号重复", id, prev)
		}
		seenSeq[a.Seq] = "调整 " + id
		if a.Revoked {
			if strings.TrimSpace(a.RevokeReason) == "" {
				return fmt.Errorf("调整 %q 已撤销但撤销原因为空", id)
			}
			if a.RevokeSeq <= a.Seq || a.RevokeSeq > s.NextSeq {
				return fmt.Errorf("调整 %q 的撤销序号越界", id)
			}
			if prev, dup := seenSeq[a.RevokeSeq]; dup {
				return fmt.Errorf("撤销调整 %q 与 %q 的操作序号重复", id, prev)
			}
			seenSeq[a.RevokeSeq] = "撤销调整 " + id
		} else if a.RevokeReason != "" || a.RevokeSeq != 0 {
			return fmt.Errorf("调整 %q 未撤销但存在撤销信息", id)
		}
	}
	for id, p := range s.Payments {
		if p == nil {
			return fmt.Errorf("收款 %q 的数据为空", id)
		}
		if p.ID != id {
			return fmt.Errorf("收款标识不一致: 键 %q / 记录 %q", id, p.ID)
		}
		if p.ID == "" {
			return errors.New("存在空的收款标识")
		}
		if p.Total <= 0 {
			return fmt.Errorf("收款 %q 的总额不是正整数", id)
		}
		if strings.TrimSpace(p.Note) == "" {
			return fmt.Errorf("收款 %q 的备注为空", id)
		}
		if _, ok := s.Customers[p.CustomerID]; !ok {
			return fmt.Errorf("收款 %q 引用了不存在的客户 %q", id, p.CustomerID)
		}
		if len(p.Allocations) == 0 {
			return fmt.Errorf("收款 %q 没有分配", id)
		}
		seenMonths := make(map[string]bool)
		var sum int64
		for _, al := range p.Allocations {
			if !validMonth(al.Month) {
				return fmt.Errorf("收款 %q 的分配月份 %q 无效", id, al.Month)
			}
			if seenMonths[al.Month] {
				return fmt.Errorf("收款 %q 的分配月份 %s 重复", id, al.Month)
			}
			seenMonths[al.Month] = true
			if al.Amount <= 0 {
				return fmt.Errorf("收款 %q 在 %s 的分配金额不是正整数", id, al.Month)
			}
			if _, ok := s.Bills[billKey(p.CustomerID, al.Month)]; !ok {
				return fmt.Errorf("收款 %q 的分配引用了不存在的账单（客户 %s 月份 %s）", id, p.CustomerID, al.Month)
			}
			var err error
			sum, err = add64(sum, al.Amount)
			if err != nil {
				return fmt.Errorf("收款 %q 的分配金额汇总溢出: %w", id, err)
			}
		}
		if sum != p.Total {
			return fmt.Errorf("收款 %q 的分配合计 %d 分与总额 %d 分不一致", id, sum, p.Total)
		}
		if p.Seq < 1 || p.Seq > s.NextSeq {
			return fmt.Errorf("收款 %q 的操作序号越界", id)
		}
		if prev, dup := seenSeq[p.Seq]; dup {
			return fmt.Errorf("收款 %q 与 %q 的操作序号重复", id, prev)
		}
		seenSeq[p.Seq] = "收款 " + id
		if p.Revoked {
			if strings.TrimSpace(p.RevokeReason) == "" {
				return fmt.Errorf("收款 %q 已撤销但撤销原因为空", id)
			}
			if p.RevokeSeq <= p.Seq || p.RevokeSeq > s.NextSeq {
				return fmt.Errorf("收款 %q 的撤销序号越界", id)
			}
			if prev, dup := seenSeq[p.RevokeSeq]; dup {
				return fmt.Errorf("撤销收款 %q 与 %q 的操作序号重复", id, prev)
			}
			seenSeq[p.RevokeSeq] = "撤销收款 " + id
		} else if p.RevokeReason != "" || p.RevokeSeq != 0 {
			return fmt.Errorf("收款 %q 未撤销但存在撤销信息", id)
		}
	}
	// 更正记录：标识、原因、目标收款、前后分配与序号都必须自洽。
	// 更正与收款、调整标识命名空间相互独立，允许同名，但共用同一全局序号池。
	for id, c := range s.Corrections {
		if c == nil {
			return fmt.Errorf("更正 %q 的数据为空", id)
		}
		if c.ID != id {
			return fmt.Errorf("更正标识不一致: 键 %q / 记录 %q", id, c.ID)
		}
		if c.ID == "" {
			return errors.New("存在空的更正标识")
		}
		if strings.TrimSpace(c.Reason) == "" {
			return fmt.Errorf("更正 %q 的原因为空", id)
		}
		p, ok := s.Payments[c.PaymentID]
		if !ok {
			return fmt.Errorf("更正 %q 引用了不存在的收款 %q", id, c.PaymentID)
		}
		if err := checkCorrectionAllocations(s, p, c.From, "更正前"); err != nil {
			return fmt.Errorf("更正 %q: %w", id, err)
		}
		if err := checkCorrectionAllocations(s, p, c.To, "更正后"); err != nil {
			return fmt.Errorf("更正 %q: %w", id, err)
		}
		if c.Seq <= p.Seq || c.Seq > s.NextSeq {
			return fmt.Errorf("更正 %q 的操作序号越界", id)
		}
		if prev, dup := seenSeq[c.Seq]; dup {
			return fmt.Errorf("更正 %q 与 %q 的操作序号重复", id, prev)
		}
		seenSeq[c.Seq] = "更正 " + id
		// 新增更正只允许未撤销收款：收款一旦撤销，其后的更正不可能合法登记。
		if p.Revoked && c.Seq >= p.RevokeSeq {
			return fmt.Errorf("更正 %q 的序号不早于收款 %q 的撤销序号", id, p.ID)
		}
	}
	// 链式一致性：以首次登记分配为起点按序号回放，每次更正的“更正前分配”
	// 必须等于当时生效的分配，更正后生效分配即其“更正后分配”。
	for pid, p := range s.Payments {
		cur := normalizeAllocations(p.Allocations)
		for _, c := range correctionsForPayment(s, pid) {
			if !allocationsEqual(cur, c.From) {
				return fmt.Errorf("更正 %q 的更正前分配与收款 %q 当时生效的分配不一致", c.ID, pid)
			}
			cur = normalizeAllocations(c.To)
		}
	}
	// 每张账单：当前应付（原总金额 + 全部未撤销调整净额）必须介于
	// 0 与有符号 64 位最大值之间；实收（全部未撤销收款在该月的当前生效
	// 分配之和）必须满足 0 ≤ 实收 ≤ 当前应付。越界说明金额与记录不一致。
	for key, b := range s.Bills {
		_, payable, err := billTotals(b, adjustmentsFor(s, b.CustomerID, b.Month))
		if err != nil {
			return fmt.Errorf("账单 %q 的调整金额与记录不一致: %w", key, err)
		}
		received, err := paymentReceived(s, b.CustomerID, b.Month)
		if err != nil {
			return fmt.Errorf("账单 %q 的收款金额与记录不一致: %w", key, err)
		}
		if received > payable {
			return fmt.Errorf("账单 %q 的实收 %d 分超过当前应付 %d 分", key, received, payable)
		}
	}
	return nil
}

// checkCorrectionAllocations 校验一条更正的前/后分配：非空、月份合法不重复、
// 各项为正整数分、引用原客户已结算账单、合计等于收款总额。
func checkCorrectionAllocations(s *state, p *payment, allocs []paymentAllocation, label string) error {
	if len(allocs) == 0 {
		return fmt.Errorf("%s分配为空", label)
	}
	seenMonths := make(map[string]bool)
	var sum int64
	for _, al := range allocs {
		if !validMonth(al.Month) {
			return fmt.Errorf("%s分配的月份 %q 无效", label, al.Month)
		}
		if seenMonths[al.Month] {
			return fmt.Errorf("%s分配的月份 %s 重复", label, al.Month)
		}
		seenMonths[al.Month] = true
		if al.Amount <= 0 {
			return fmt.Errorf("%s分配在 %s 的金额不是正整数", label, al.Month)
		}
		if _, ok := s.Bills[billKey(p.CustomerID, al.Month)]; !ok {
			return fmt.Errorf("%s分配引用了不存在的账单（客户 %s 月份 %s）", label, p.CustomerID, al.Month)
		}
		var err error
		sum, err = add64(sum, al.Amount)
		if err != nil {
			return fmt.Errorf("%s分配金额汇总溢出: %w", label, err)
		}
	}
	if sum != p.Total {
		return fmt.Errorf("%s分配合计 %d 分与收款总额 %d 分不一致", label, sum, p.Total)
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

// adjustmentsFor 返回某客户某月账单的全部调整，按操作序号升序。
func adjustmentsFor(s *state, customerID, month string) []*adjustment {
	var list []*adjustment
	for _, a := range s.Adjustments {
		if a.CustomerID == customerID && a.Month == month {
			list = append(list, a)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	return list
}

// paymentsFor 返回曾涉及某客户某月账单的全部收款（首次登记分配或任一更正的
// 前后分配涉及该月，含已被更正移出该月的收款），按操作序号升序。
// 用于历史展示；实收计算请用 paymentReceived。
func paymentsFor(s *state, customerID, month string) []*payment {
	var list []*payment
	for _, p := range s.Payments {
		if p.CustomerID == customerID && paymentInvolves(s, p, month) {
			list = append(list, p)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	return list
}

// paymentInvolves 报告该收款是否曾涉及指定月份：首次登记分配或任一更正的
// 前/后分配包含该月。
func paymentInvolves(s *state, p *payment, month string) bool {
	if p.amountFor(month) > 0 {
		return true
	}
	for _, c := range correctionsForPayment(s, p.ID) {
		if amountIn(c.From, month) > 0 || amountIn(c.To, month) > 0 {
			return true
		}
	}
	return false
}

// correctionsForPayment 返回针对某笔收款的全部更正，按操作序号升序。
func correctionsForPayment(s *state, payID string) []*correction {
	var list []*correction
	for _, c := range s.Corrections {
		if c.PaymentID == payID {
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	return list
}

// currentAllocations 以首次登记分配为起点，按操作序号依次应用全部更正
// （每次更正整笔替换分配），返回该收款当前生效的分配（按月份升序）。
func currentAllocations(s *state, p *payment) []paymentAllocation {
	cur := p.Allocations
	for _, c := range correctionsForPayment(s, p.ID) {
		cur = c.To
	}
	return cur
}

// currentAmountFor 返回该收款当前生效分配中指定月份的金额；无分配时为 0。
func currentAmountFor(s *state, p *payment, month string) int64 {
	return amountIn(currentAllocations(s, p), month)
}

// amountIn 返回分配列表中指定月份的金额；无该月时为 0。
func amountIn(allocs []paymentAllocation, month string) int64 {
	for _, al := range allocs {
		if al.Month == month {
			return al.Amount
		}
	}
	return 0
}

// normalizeAllocations 返回按月份升序排序的分配副本（分配顺序不影响身份）。
func normalizeAllocations(allocs []paymentAllocation) []paymentAllocation {
	out := make([]paymentAllocation, len(allocs))
	copy(out, allocs)
	sort.Slice(out, func(i, j int) bool { return out[i].Month < out[j].Month })
	return out
}

// allocationsEqual 按月份-金额对应关系判定两条分配相同，顺序无关。
func allocationsEqual(a, b []paymentAllocation) bool {
	if len(a) != len(b) {
		return false
	}
	na, nb := normalizeAllocations(a), normalizeAllocations(b)
	for i := range na {
		if na[i] != nb[i] {
			return false
		}
	}
	return true
}

// paymentReceived 返回全部未撤销收款在指定月份的当前生效分配之和。各笔分配
// 均为正整数，用非负 64 位加法累加，溢出时返回错误。
func paymentReceived(s *state, customerID, month string) (int64, error) {
	var received int64
	for _, p := range s.Payments {
		if p.CustomerID != customerID || p.Revoked {
			continue
		}
		amt := currentAmountFor(s, p, month)
		if amt == 0 {
			continue
		}
		var err error
		received, err = add64(received, amt)
		if err != nil {
			return 0, err
		}
	}
	return received, nil
}

// errPayableOutOfRange 表示当前应付越出 [0, 有符号 64 位最大值]。
var errPayableOutOfRange = errors.New("当前应付越界（须介于 0 与有符号 64 位最大值之间）")

// billTotals 计算全部未撤销调整的净额与当前应付（原总金额 + 净额）。
// 净额按 128 位精确求和（与顺序无关），再校验应付落在
// [0, 有符号 64 位最大值] 内，全程整数运算。
func billTotals(b *bill, adjs []*adjustment) (net, payable int64, err error) {
	var amounts []int64
	for _, a := range adjs {
		if !a.Revoked {
			amounts = append(amounts, a.Amount)
		}
	}
	net, err = sumSigned64(amounts...)
	if err != nil {
		return 0, 0, err
	}
	payable, err = addSigned64(b.TotalFee, net)
	if err != nil {
		return 0, 0, err
	}
	if payable < 0 {
		return 0, 0, errPayableOutOfRange
	}
	return net, payable, nil
}
