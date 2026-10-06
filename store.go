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
	ID     string `json:"id"`
	Name   string `json:"name"`
	Price  int64  `json:"price_fen"`         // 固定单价，非负整数分，创建后不可修改（阶梯客户恒为 0，不参与计价）
	PlanID string `json:"plan_id,omitempty"` // 绑定的阶梯计费方案标识；空表示固定单价客户，创建后不可变更
}

// planTier 是阶梯计费方案的一档：累计数量上限（最后一档为 0，表示无上限）
// 与落入本档数量的每单位价格分。
type planTier struct {
	Limit int64 `json:"limit"`     // 累计数量上限；有限上限为严格递增的正整数，最后一档为 0（无上限）
	Price int64 `json:"price_fen"` // 本档每单位价格，非负整数分，可升可降
}

// plan 是按月累计用量的阶梯计费方案：唯一非空标识、非空名称与至少一档
// 有序阶梯。创建后不可修改，被客户绑定后不可解除。
type plan struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Tiers []planTier `json:"tiers"`
}

type usageRecord struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Time       string `json:"time"` // RFC3339，保留原始输入用于去重判定
	Quantity   int64  `json:"quantity"`
}

// billLineTier 是一条用量在某一档上的分段计价结果：跨档记录拆成多段，
// 各段数量之和等于本条数量，各段金额之和等于本条小计。
type billLineTier struct {
	Tier      int   `json:"tier"` // 档序号（从 0 开始的方案档下标）
	Quantity  int64 `json:"quantity"`
	UnitPrice int64 `json:"unit_price_fen"`
	Fee       int64 `json:"fee_fen"`
}

type billLine struct {
	UsageID  string         `json:"usage_id"`
	Time     string         `json:"time"`
	Quantity int64          `json:"quantity"`
	LineFee  int64          `json:"line_fee_fen"`
	Segments []billLineTier `json:"segments,omitempty"` // 阶梯账单的分段计价；固定单价账单为空
}

// tierTotal 是阶梯账单在某一档上的实际计价合计（与 PlanTiers 按下标对齐）。
type tierTotal struct {
	Quantity int64 `json:"quantity"`
	Fee      int64 `json:"fee_fen"`
}

type bill struct {
	ID         string     `json:"id"`
	CustomerID string     `json:"customer_id"`
	Month      string     `json:"month"` // YYYY-MM（UTC）
	TotalQty   int64      `json:"total_quantity"`
	UnitPrice  int64      `json:"unit_price_fen"` // 固定单价账单的单价；阶梯账单恒为 0，不伪造统一单价
	TotalFee   int64      `json:"total_fee_fen"`
	Lines      []billLine `json:"lines"`
	// 阶梯账单保存结算时方案的完整快照：标识、名称与全部阶梯规则，
	// 以及各档实际计价的数量与金额；固定单价账单以下字段均为空。
	PlanID     string      `json:"plan_id,omitempty"`
	PlanName   string      `json:"plan_name,omitempty"`
	PlanTiers  []planTier  `json:"plan_tiers,omitempty"`
	TierTotals []tierTotal `json:"tier_totals,omitempty"`
	CreatedAt  string      `json:"created_at"`
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

// amountFor 返回该收款在指定月份的分配金额；无分配时为 0。
func (p *payment) amountFor(month string) int64 {
	for _, al := range p.Allocations {
		if al.Month == month {
			return al.Amount
		}
	}
	return 0
}

// correction 是对一笔未撤销收款的分配更正：客户、总额、备注、原账单及
// 应付不变，仅把该笔收款当前生效的分配整体替换为完整新分配。记录一旦写入
// 永不删除；首次登记内容永久保留，bill pay / bill remit 仍按原始分配判重，
// 更正不影响收款身份。可连续更正，每次以当时最新分配为起点。
type correction struct {
	ID          string              `json:"id"`
	PaymentID   string              `json:"payment_id"` // 目标收款标识
	Reason      string              `json:"reason"`
	Allocations []paymentAllocation `json:"allocations"` // 完整新分配，至少一项，月份不重复，按月份升序保存
	Seq         int64               `json:"seq"`         // 全局递增操作序号，与调整/收款及其撤销共用
	CreatedAt   string              `json:"created_at"`
}

type state struct {
	Version     int                     `json:"version"`
	Plans       map[string]*plan        `json:"plans"` // 全局唯一方案标识 -> 阶梯计费方案
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
	// 旧版本数据文件没有 plans/adjustments/payments/corrections/next_seq 字段：
	// 视为零方案、零调整、零实收、零更正。
	if s.Plans == nil {
		s.Plans = map[string]*plan{}
	}
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
		Plans:       map[string]*plan{},
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
	for id, p := range s.Plans {
		if p == nil {
			return fmt.Errorf("方案 %q 的数据为空", id)
		}
		if p.ID != id {
			return fmt.Errorf("方案标识不一致: 键 %q / 记录 %q", id, p.ID)
		}
		if p.ID == "" || p.Name == "" {
			return fmt.Errorf("方案 %q 的标识或名称为空", id)
		}
		if err := validateTiers(p.Tiers); err != nil {
			return fmt.Errorf("方案 %q 的阶梯规则非法: %w", id, err)
		}
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
		if c.PlanID != "" {
			if _, ok := s.Plans[c.PlanID]; !ok {
				return fmt.Errorf("客户 %q 绑定了不存在的方案 %q", id, c.PlanID)
			}
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
		if b.PlanID == "" {
			// 固定单价账单：不得携带阶梯数据，且客户不得绑定方案。
			if c.PlanID != "" {
				return fmt.Errorf("账单 %q 为固定单价账单，但客户绑定了方案 %q", key, c.PlanID)
			}
			if b.PlanName != "" || len(b.PlanTiers) != 0 || len(b.TierTotals) != 0 {
				return fmt.Errorf("账单 %q 不是阶梯账单但携带方案数据", key)
			}
			for _, ln := range b.Lines {
				if len(ln.Segments) != 0 {
					return fmt.Errorf("账单 %q 的明细 %q 不是阶梯计价但携带分段数据", key, ln.UsageID)
				}
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
		} else {
			// 阶梯账单：方案引用必须有效，快照规则必须合法，明细与金额
			// 按保存的阶梯规则从零累计重放核验。
			if c.PlanID != b.PlanID {
				return fmt.Errorf("账单 %q 的方案 %q 与客户绑定的方案 %q 不符", key, b.PlanID, c.PlanID)
			}
			if _, ok := s.Plans[b.PlanID]; !ok {
				return fmt.Errorf("账单 %q 引用了不存在的方案 %q", key, b.PlanID)
			}
			if b.PlanName == "" {
				return fmt.Errorf("账单 %q 的方案名称为空", key)
			}
			if err := validateTiers(b.PlanTiers); err != nil {
				return fmt.Errorf("账单 %q 保存的阶梯规则非法: %w", key, err)
			}
			if len(b.TierTotals) != len(b.PlanTiers) {
				return fmt.Errorf("账单 %q 的分档合计档数与阶梯规则不符", key)
			}
			var cum int64
			totals := make([]tierTotal, len(b.PlanTiers))
			for _, ln := range b.Lines {
				segs, newCum, fee, err := priceTiered(b.PlanTiers, cum, ln.Quantity)
				if err != nil {
					return fmt.Errorf("账单 %q 明细 %q 按阶梯规则重算失败: %w", key, ln.UsageID, err)
				}
				if fee != ln.LineFee {
					return fmt.Errorf("账单 %q 明细 %q 小计与阶梯规则不符", key, ln.UsageID)
				}
				if !sameSegments(segs, ln.Segments) {
					return fmt.Errorf("账单 %q 明细 %q 的分段与阶梯规则不符", key, ln.UsageID)
				}
				cum = newCum
				for _, sg := range segs {
					if totals[sg.Tier].Quantity, err = add64(totals[sg.Tier].Quantity, sg.Quantity); err != nil {
						return fmt.Errorf("账单 %q 分档数量汇总溢出", key)
					}
					if totals[sg.Tier].Fee, err = add64(totals[sg.Tier].Fee, sg.Fee); err != nil {
						return fmt.Errorf("账单 %q 分档金额汇总溢出", key)
					}
				}
			}
			for i, tt := range b.TierTotals {
				if tt != totals[i] {
					return fmt.Errorf("账单 %q 第 %d 档合计与明细不符", key, i+1)
				}
			}
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
	// 分配更正记录：标识、目标收款、原因、新分配与序号都必须自洽。
	// 更正标识与收款、调整标识命名空间相互独立，允许同名。
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
		p, ok := s.Payments[c.PaymentID]
		if !ok {
			return fmt.Errorf("更正 %q 引用了不存在的收款 %q", id, c.PaymentID)
		}
		if strings.TrimSpace(c.Reason) == "" {
			return fmt.Errorf("更正 %q 的原因为空", id)
		}
		if len(c.Allocations) == 0 {
			return fmt.Errorf("更正 %q 没有新分配", id)
		}
		seenMonths := make(map[string]bool)
		var sum int64
		for _, al := range c.Allocations {
			if !validMonth(al.Month) {
				return fmt.Errorf("更正 %q 的分配月份 %q 无效", id, al.Month)
			}
			if seenMonths[al.Month] {
				return fmt.Errorf("更正 %q 的分配月份 %s 重复", id, al.Month)
			}
			seenMonths[al.Month] = true
			if al.Amount <= 0 {
				return fmt.Errorf("更正 %q 在 %s 的分配金额不是正整数", id, al.Month)
			}
			if _, ok := s.Bills[billKey(p.CustomerID, al.Month)]; !ok {
				return fmt.Errorf("更正 %q 的分配引用了不存在的账单（客户 %s 月份 %s）", id, p.CustomerID, al.Month)
			}
			var err error
			sum, err = add64(sum, al.Amount)
			if err != nil {
				return fmt.Errorf("更正 %q 的分配金额汇总溢出: %w", id, err)
			}
		}
		if sum != p.Total {
			return fmt.Errorf("更正 %q 的分配合计 %d 分与关联收款 %q 的总额 %d 分不一致", id, sum, c.PaymentID, p.Total)
		}
		if c.Seq <= p.Seq || c.Seq > s.NextSeq {
			return fmt.Errorf("更正 %q 的操作序号越界", id)
		}
		if p.Revoked && c.Seq > p.RevokeSeq {
			return fmt.Errorf("更正 %q 的操作序号晚于关联收款 %q 的撤销序号", id, c.PaymentID)
		}
		if prev, dup := seenSeq[c.Seq]; dup {
			return fmt.Errorf("更正 %q 与 %q 的操作序号重复", id, prev)
		}
		seenSeq[c.Seq] = "更正 " + id
	}
	// 每张账单：当前应付（原总金额 + 全部未撤销调整净额）必须介于
	// 0 与有符号 64 位最大值之间；实收（全部未撤销收款按最新分配在该月
	// 计入之和）必须满足 0 ≤ 实收 ≤ 当前应付。越界说明金额与记录不一致。
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

// validateTiers 校验一组有序阶梯：至少一档；除最后一档外上限为严格递增
// 的正整数；最后一档上限为 0（无上限）；单价均为非负整数分，可升可降。
func validateTiers(tiers []planTier) error {
	if len(tiers) == 0 {
		return errors.New("至少需要一档阶梯")
	}
	for i, t := range tiers {
		if t.Price < 0 {
			return fmt.Errorf("第 %d 档单价 %d 为负，单价必须是非负整数分", i+1, t.Price)
		}
		if i < len(tiers)-1 {
			if t.Limit <= 0 {
				return fmt.Errorf("第 %d 档累计上限 %d 不是正整数", i+1, t.Limit)
			}
			if i > 0 && t.Limit <= tiers[i-1].Limit {
				return fmt.Errorf("第 %d 档累计上限 %d 未严格大于上一档 %d", i+1, t.Limit, tiers[i-1].Limit)
			}
		} else if t.Limit != 0 {
			return fmt.Errorf("最后一档必须无上限（上限须为 0），收到 %d", t.Limit)
		}
	}
	return nil
}

// priceTiered 从月累计量 cum 开始，对数量 qty 按阶梯计价：各档仅对落入
// 本档的数量收费，不把最高档价格套用全部数量，也不对每条用量重置阶梯。
// 返回分段结果（仅含实际计价的档）、新的月累计量与本次金额合计；
// 月累计数量或任一档金额溢出有符号 64 位整数范围时返回错误。
func priceTiered(tiers []planTier, cum, qty int64) ([]billLineTier, int64, int64, error) {
	if cum < 0 || qty < 0 {
		return nil, 0, 0, errOverflow
	}
	var segs []billLineTier
	var fee int64
	remaining := qty
	for i, t := range tiers {
		if remaining == 0 {
			break
		}
		var take int64
		if t.Limit == 0 { // 最后一档无上限
			take = remaining
		} else {
			if cum >= t.Limit {
				continue
			}
			take = t.Limit - cum
			if take > remaining {
				take = remaining
			}
		}
		segFee, err := mul64(take, t.Price)
		if err != nil {
			return nil, 0, 0, err
		}
		if fee, err = add64(fee, segFee); err != nil {
			return nil, 0, 0, err
		}
		if cum, err = add64(cum, take); err != nil {
			return nil, 0, 0, err
		}
		segs = append(segs, billLineTier{Tier: i, Quantity: take, UnitPrice: t.Price, Fee: segFee})
		remaining -= take
	}
	if remaining > 0 {
		// 规则经 validateTiers 校验后最后一档无上限，不会走到这里。
		return nil, 0, 0, errors.New("阶梯规则未覆盖全部数量")
	}
	return segs, cum, fee, nil
}

// sameSegments 判定两组分段计价结果完全一致。
func sameSegments(a, b []billLineTier) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// correctionsFor 返回某收款的全部分配更正，按操作序号升序。
func correctionsFor(s *state, paymentID string) []*correction {
	var list []*correction
	for _, c := range s.Corrections {
		if c.PaymentID == paymentID {
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	return list
}

// currentAllocations 返回一笔收款当前生效的分配：无更正时为首次登记的
// 原始分配，否则为序号最大（最新）的更正的分配。
func currentAllocations(s *state, p *payment) []paymentAllocation {
	var latest *correction
	for _, c := range s.Corrections {
		if c.PaymentID == p.ID && (latest == nil || c.Seq > latest.Seq) {
			latest = c
		}
	}
	if latest != nil {
		return latest.Allocations
	}
	return p.Allocations
}

// allocAmountFor 返回分配列表中指定月份的金额；无该月分配时为 0。
func allocAmountFor(allocs []paymentAllocation, month string) int64 {
	for _, al := range allocs {
		if al.Month == month {
			return al.Amount
		}
	}
	return 0
}

// paymentsEverFor 返回历史上曾涉及某客户某月账单的全部收款（首次登记
// 分配或任一次更正的新分配涉及该月，即使后来被更正移出），按操作序号升序。
func paymentsEverFor(s *state, customerID, month string) []*payment {
	var list []*payment
	for _, p := range s.Payments {
		if p.CustomerID != customerID {
			continue
		}
		involved := p.amountFor(month) > 0
		if !involved {
			for _, c := range s.Corrections {
				if c.PaymentID == p.ID && allocAmountFor(c.Allocations, month) > 0 {
					involved = true
					break
				}
			}
		}
		if involved {
			list = append(list, p)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	return list
}

// paymentReceived 返回全部未撤销收款按最新分配在指定月份计入的实收之和。
// 各笔分配均为正整数，用非负 64 位加法累加，溢出时返回错误。
func paymentReceived(s *state, customerID, month string) (int64, error) {
	var received int64
	for _, p := range s.Payments {
		if p.CustomerID != customerID || p.Revoked {
			continue
		}
		amt := allocAmountFor(currentAllocations(s, p), month)
		if amt <= 0 {
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
