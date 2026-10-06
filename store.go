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
	Price  int64  `json:"price_fen"`         // 固定单价，非负整数分，创建后不可修改
	PlanID string `json:"plan_id,omitempty"` // 绑定的阶梯计费方案标识；空表示固定单价客户，创建后不可变更
}

// tier 是阶梯计费方案中的一档：Limit 为月累计数量上限（正整数），
// 仅最后一档无上限（Limit 为 0）；Price 为该档每单位价格（非负整数分）。
type tier struct {
	Limit int64 `json:"limit"`     // 累计数量上限；0 表示无上限（仅最后一档）
	Price int64 `json:"price_fen"` // 每单位价格，非负整数分，可升可降
}

// plan 是按月累计用量的阶梯计费方案：标识唯一非空、名称非空、固定月费
// （非负整数分，省略视为 0）与至少一档有序阶梯；月费与规则创建后不可修改。
type plan struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	MonthlyFee int64  `json:"monthly_fee_fen,omitempty"` // 固定月费，非负整数分；每账期收取一次整月，不按天折算
	Tiers      []tier `json:"tiers"`                     // 至少一档；有限上限严格递增，最后一档无上限
}

type usageRecord struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Time       string `json:"time"` // RFC3339，保留原始输入用于去重判定
	Quantity   int64  `json:"quantity"`
}

// lineSegment 是阶梯计费下一条用量记录落入某一档的分段：数量与小计；
// 该档单价由账单保存的方案规则按档位序号取得。
type lineSegment struct {
	Tier     int   `json:"tier"`     // 档位序号（0 起，对应账单保存的方案规则）
	Quantity int64 `json:"quantity"` // 落入本档的数量
	Fee      int64 `json:"fee_fen"`  // 本分段小计（数量 × 该档单价）
}

// tierTotal 是阶梯账单中某一档的合计数量与金额（与 PlanTiers 顺序对齐）。
type tierTotal struct {
	Quantity int64 `json:"quantity"`
	Fee      int64 `json:"fee_fen"`
}

type billLine struct {
	UsageID  string        `json:"usage_id"`
	Time     string        `json:"time"`
	Quantity int64         `json:"quantity"`
	LineFee  int64         `json:"line_fee_fen"`
	Segments []lineSegment `json:"segments,omitempty"` // 阶梯账单：本条用量的跨档分段
}

type bill struct {
	ID         string      `json:"id"`
	CustomerID string      `json:"customer_id"`
	Month      string      `json:"month"`                 // YYYY-MM（UTC）
	Pricing    string      `json:"pricing,omitempty"`     // 计价类型：空或 fixed 为固定单价；tiered 为阶梯计费
	PlanID     string      `json:"plan_id,omitempty"`     // 阶梯账单：方案标识
	PlanName   string      `json:"plan_name,omitempty"`   // 阶梯账单：方案名称（快照）
	PlanTiers  []tier      `json:"plan_tiers,omitempty"`  // 阶梯账单：完整方案规则（快照）
	TierTotals []tierTotal `json:"tier_totals,omitempty"` // 阶梯账单：各档实际数量与金额合计
	// 阶梯账单：实际收取的固定月费快照（分），非负；按账期有效方案收取一次
	// 整月，不计入总数量、各档金额或逐条小计。固定单价账单恒为 0。
	MonthlyFee int64      `json:"monthly_fee_fen,omitempty"`
	TotalQty   int64      `json:"total_quantity"`
	UnitPrice  int64      `json:"unit_price_fen"` // 固定单价账单的单价；阶梯账单恒为 0（不伪造统一单价）
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

// planChange 是阶梯客户的一次按月生效的方案变更：自生效月（UTC 自然月，
// 含）起改用目标方案，直到下一次变更；生效月之前的月份不受影响。记录一旦
// 写入永不修改或删除；变更不产生账后流水事件，也不占用全局操作序号。
type planChange struct {
	CustomerID string `json:"customer_id"`
	Month      string `json:"month"`   // YYYY-MM（UTC），生效月（含）
	PlanID     string `json:"plan_id"` // 目标阶梯计费方案标识
	Reason     string `json:"reason"`
	CreatedAt  string `json:"created_at"`
}

// suspension 是阶梯客户的一段按月暂停区间：自起月（UTC 自然月，含）起
// 暂停服务，直到结束月（不含），即 [StartMonth, EndMonth) 覆盖的自然月。
// 暂停月不接收用量、不产生月费账单；区间可相接（相接视为连续暂停）但不得
// 重叠。记录一旦写入永不修改或删除；暂停不产生账后流水事件，也不占用全局
// 操作序号。固定单价客户不适用暂停。
type suspension struct {
	CustomerID string `json:"customer_id"`
	StartMonth string `json:"start_month"` // YYYY-MM（UTC），暂停起月（含）
	EndMonth   string `json:"end_month"`   // YYYY-MM（UTC），结束月（不含），严格晚于起月
	Reason     string `json:"reason"`
	CreatedAt  string `json:"created_at"`
}

// withdrawal 是一条用量记录的撤回标记：原记录（客户、时间、数量、标识）永久
// 保留在 Usage 中，撤回只追加状态与原因，用于纠正误导入的记录。撤回不可恢复、
// 标识不可复用；已撤回用量不参与结算、方案变更预检与暂停区间冲突检查，但相同
// 内容的重放仍按原记录判重跳过。撤回只作用于该条记录，不占用全局操作序号。
type withdrawal struct {
	UsageID   string `json:"usage_id"` // 撤回的用量标识（与键一致）
	Reason    string `json:"reason"`   // 撤回原因，非空
	CreatedAt string `json:"created_at"`
}

type state struct {
	Version     int                     `json:"version"`
	Customers   map[string]*customer    `json:"customers"`
	Plans       map[string]*plan        `json:"plans"`                 // 全局唯一方案标识 -> 阶梯计费方案
	Usage       map[string]*usageRecord `json:"usage"`                 // 全局唯一用量标识 -> 记录
	Bills       map[string]*bill        `json:"bills"`                 // 客户 + "|" + 月份 -> 账单
	Adjustments map[string]*adjustment  `json:"adjustments"`           // 全局唯一调整标识 -> 记录
	Payments    map[string]*payment     `json:"payments"`              // 全局唯一收款标识 -> 记录（与调整标识相互独立，可同名）
	Corrections map[string]*correction  `json:"corrections"`           // 全局唯一更正标识 -> 记录（与收款、调整标识相互独立，可同名）
	PlanChanges map[string]*planChange  `json:"plan_changes"`          // 客户 + "|" + 生效月 -> 方案变更（按生效月递增追加，不可改写）
	Suspensions map[string]*suspension  `json:"suspensions,omitempty"` // 客户 + "|" + 起月 -> 暂停区间（不可改写）
	Withdrawals map[string]*withdrawal  `json:"withdrawals,omitempty"` // 用量标识 -> 撤回标记（不可恢复，标识不可复用）
	NextSeq     int64                   `json:"next_seq"`              // 已分配的最大操作序号（调整/收款/更正及其撤销共用；方案变更、暂停与用量撤回不占用）
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
	// 拒绝完整 JSON 之后的任何非空白内容（包括单独的 ] 或 }），避免静默
	// 吞掉异常数据；合法尾随空白仍可读取。
	if off := dec.InputOffset(); off < int64(len(data)) {
		if strings.TrimSpace(string(data[off:])) != "" {
			return nil, fmt.Errorf("数据文件已损坏（JSON 之后存在多余内容）: %s", p)
		}
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("数据文件已损坏: %w", err)
	}
	// 旧版本数据文件没有 plans/adjustments/payments/corrections/plan_changes/
	// suspensions/withdrawals/next_seq 字段：视为零方案、零调整、零实收、零更正、
	// 零方案变更、零暂停、零撤回（全部用量有效）。
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
	if s.PlanChanges == nil {
		s.PlanChanges = map[string]*planChange{}
	}
	if s.Suspensions == nil {
		s.Suspensions = map[string]*suspension{}
	}
	if s.Withdrawals == nil {
		s.Withdrawals = map[string]*withdrawal{}
	}
	s.path = p
	return &s, nil
}

func newState(p string) *state {
	return &state{
		Version:     stateVersion,
		Customers:   map[string]*customer{},
		Plans:       map[string]*plan{},
		Usage:       map[string]*usageRecord{},
		Bills:       map[string]*bill{},
		Adjustments: map[string]*adjustment{},
		Payments:    map[string]*payment{},
		Corrections: map[string]*correction{},
		PlanChanges: map[string]*planChange{},
		Suspensions: map[string]*suspension{},
		Withdrawals: map[string]*withdrawal{},
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
		if err := validatePlanRules(p); err != nil {
			return err
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
	// 方案变更：键、客户（须为绑定阶梯方案的客户）、目标方案、生效月与
	// 原因都必须自洽；同一客户的变更生效月由键保证唯一。
	for key, ch := range s.PlanChanges {
		if ch == nil {
			return fmt.Errorf("方案变更 %q 的数据为空", key)
		}
		if planChangeKey(ch.CustomerID, ch.Month) != key {
			return fmt.Errorf("方案变更键不一致: 键 %q / 客户月份 %s|%s", key, ch.CustomerID, ch.Month)
		}
		c, ok := s.Customers[ch.CustomerID]
		if !ok {
			return fmt.Errorf("方案变更 %q 引用了不存在的客户 %q", key, ch.CustomerID)
		}
		if c.PlanID == "" {
			return fmt.Errorf("方案变更 %q 的客户 %q 未绑定阶梯方案", key, ch.CustomerID)
		}
		if !validMonth(ch.Month) {
			return fmt.Errorf("方案变更 %q 的生效月无效", key)
		}
		if _, ok := s.Plans[ch.PlanID]; !ok {
			return fmt.Errorf("方案变更 %q 引用了不存在的方案 %q", key, ch.PlanID)
		}
		if strings.TrimSpace(ch.Reason) == "" {
			return fmt.Errorf("方案变更 %q 的原因为空", key)
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
	// 撤回标记：键与记录一致、目标用量存在（撤回目标失效即损坏）、原因非空。
	// 原用量记录仍按上面的规则完整校验（格式、正数量、客户存在性），撤回不
	// 删除或改写原记录。旧文件缺少撤回记录视为全部用量有效（Withdrawals 已
	// 在载入时补为空表）。
	for id, w := range s.Withdrawals {
		if w == nil {
			return fmt.Errorf("用量 %q 的撤回记录为空", id)
		}
		if w.UsageID != id {
			return fmt.Errorf("撤回记录键不一致: 键 %q / 用量标识 %q", id, w.UsageID)
		}
		if w.UsageID == "" {
			return errors.New("存在空用量标识的撤回记录")
		}
		if _, ok := s.Usage[w.UsageID]; !ok {
			return fmt.Errorf("撤回记录引用了不存在的用量 %q（撤回目标失效）", w.UsageID)
		}
		if strings.TrimSpace(w.Reason) == "" {
			return fmt.Errorf("用量 %q 的撤回原因为空", w.UsageID)
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
		if b.MonthlyFee < 0 {
			return fmt.Errorf("账单 %q 的月费为负", key)
		}
		// 仅月费大于 0 的账单允许空用量明细（无用量也须出账收取月费）。
		if len(b.Lines) == 0 && b.MonthlyFee == 0 {
			return fmt.Errorf("账单 %q 没有明细", key)
		}
		var qty, fee int64
		for _, ln := range b.Lines {
			u, ok := s.Usage[ln.UsageID]
			if !ok {
				return fmt.Errorf("账单 %q 引用了不存在的用量 %q", key, ln.UsageID)
			}
			// 已撤回用量不参与结算，账单不得把已撤回记录视为待计费用量。
			if _, withdrawn := s.Withdrawals[ln.UsageID]; withdrawn {
				return fmt.Errorf("账单 %q 引用了已撤回的用量 %q", key, ln.UsageID)
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
		// 原总金额 = 月费 + 全月用量费；月费不计入逐条用量小计，故明细
		// 小计之和等于用量费（总金额 − 月费）。月费与总金额均非负，相减
		// 不会溢出；总金额为负或小于月费时此处必然不符。
		if usageFee := b.TotalFee - b.MonthlyFee; fee != usageFee {
			return fmt.Errorf("账单 %q 总金额不等于月费加全月用量费（明细小计合计 %d 分，月费 %d 分，总金额 %d 分）",
				key, fee, b.MonthlyFee, b.TotalFee)
		}
		c := s.Customers[b.CustomerID]
		switch b.Pricing {
		case "", "fixed":
			// 固定单价账单：每条小计必须等于 数量×客户固定单价，且不得
			// 携带阶梯方案信息、分段或月费。
			if b.PlanID != "" || b.PlanName != "" || len(b.PlanTiers) > 0 || len(b.TierTotals) > 0 {
				return fmt.Errorf("账单 %q 是固定单价账单但携带阶梯方案信息", key)
			}
			if b.MonthlyFee != 0 {
				return fmt.Errorf("账单 %q 是固定单价账单但携带月费", key)
			}
			for _, ln := range b.Lines {
				if len(ln.Segments) > 0 {
					return fmt.Errorf("账单 %q 的明细 %q 携带阶梯分段", key, ln.UsageID)
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
		case "tiered":
			if err := s.validateTieredBill(key, b, c); err != nil {
				return err
			}
		default:
			return fmt.Errorf("账单 %q 的计价类型 %q 未知", key, b.Pricing)
		}
		// 封账期内不得存在游离于账单之外的有效用量（封账后新增被禁止，
		// 而已入账记录理应全部在明细中）；已撤回用量不计费用，允许存在
		// 于已封账月份而不进入账单。
		for _, u := range s.Usage {
			if _, withdrawn := s.Withdrawals[u.ID]; withdrawn {
				continue
			}
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

	// 暂停区间：键、客户（须为绑定阶梯方案的客户）、区间与原因都必须自洽；
	// 同一客户的区间不得重叠（可以相接）；暂停月内不得存在有效（未撤回）
	// 用量或账单。
	// 旧文件缺少暂停记录视为正常服务（Suspensions 已在载入时补为空表）。
	for key, su := range s.Suspensions {
		if su == nil {
			return fmt.Errorf("暂停区间 %q 的数据为空", key)
		}
		if suspensionKey(su.CustomerID, su.StartMonth) != key {
			return fmt.Errorf("暂停区间键不一致: 键 %q / 客户起月 %s|%s", key, su.CustomerID, su.StartMonth)
		}
		c, ok := s.Customers[su.CustomerID]
		if !ok {
			return fmt.Errorf("暂停区间 %q 引用了不存在的客户 %q", key, su.CustomerID)
		}
		if c.PlanID == "" {
			return fmt.Errorf("暂停区间 %q 的客户 %q 未绑定阶梯方案（固定单价客户不适用暂停）", key, su.CustomerID)
		}
		if !validMonth(su.StartMonth) {
			return fmt.Errorf("暂停区间 %q 的起月无效", key)
		}
		if !validMonth(su.EndMonth) || su.EndMonth <= su.StartMonth {
			return fmt.Errorf("暂停区间 %q 的结束月无效（结束月必须是晚于起月 %s 的 YYYY-MM）", key, su.StartMonth)
		}
		if strings.TrimSpace(su.Reason) == "" {
			return fmt.Errorf("暂停区间 %q 的原因为空", key)
		}
	}
	for customerID := range s.Customers {
		list := s.suspensionsFor(customerID)
		for i := 1; i < len(list); i++ {
			if list[i].StartMonth < list[i-1].EndMonth {
				return fmt.Errorf("客户 %s 的暂停区间 %s..%s 与 %s..%s 重叠（区间可以相接但不得重叠）",
					customerID, list[i-1].StartMonth, list[i-1].EndMonth, list[i].StartMonth, list[i].EndMonth)
			}
		}
		for _, su := range list {
			for _, u := range s.Usage {
				// 已撤回用量不参与用量冲突判断，可存在于随后暂停的月份。
				if _, withdrawn := s.Withdrawals[u.ID]; withdrawn {
					continue
				}
				if u.CustomerID == customerID && monthInRange(utcMonth(u.Time), su.StartMonth, su.EndMonth) {
					return fmt.Errorf("客户 %s 在暂停区间 %s..%s（不含结束月）内存在用量 %q（%s）",
						customerID, su.StartMonth, su.EndMonth, u.ID, utcMonth(u.Time))
				}
			}
			for _, b := range s.Bills {
				if b.CustomerID == customerID && monthInRange(b.Month, su.StartMonth, su.EndMonth) {
					return fmt.Errorf("客户 %s 在暂停区间 %s..%s（不含结束月）内存在账单（月份 %s），暂停月不得封账",
						customerID, su.StartMonth, su.EndMonth, b.Month)
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

// validatePlanRules 校验阶梯方案的标识、名称、月费与阶梯规则本身自洽：
// 月费为非负整数分，至少一档，有限上限为严格递增的正整数，最后一档无
// 上限，单价非负。
func validatePlanRules(p *plan) error {
	if p.ID == "" || p.Name == "" {
		return fmt.Errorf("方案 %q 的标识或名称为空", p.ID)
	}
	if p.MonthlyFee < 0 {
		return fmt.Errorf("方案 %q 的月费为负", p.ID)
	}
	if len(p.Tiers) == 0 {
		return fmt.Errorf("方案 %q 至少需要一档阶梯", p.ID)
	}
	prev := int64(0)
	for i, t := range p.Tiers {
		if t.Price < 0 {
			return fmt.Errorf("方案 %q 第 %d 档单价为负", p.ID, i+1)
		}
		if i == len(p.Tiers)-1 {
			if t.Limit != 0 {
				return fmt.Errorf("方案 %q 最后一档必须无上限", p.ID)
			}
		} else {
			if t.Limit <= 0 {
				return fmt.Errorf("方案 %q 第 %d 档上限必须是正整数", p.ID, i+1)
			}
			if t.Limit <= prev {
				return fmt.Errorf("方案 %q 的有限上限必须严格递增", p.ID)
			}
			prev = t.Limit
		}
	}
	return nil
}

// tiersEqual 判定两份阶梯规则完全相同。
func tiersEqual(a, b []tier) bool {
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

// segmentsEqual 判定两份分段列表完全相同。
func segmentsEqual(a, b []lineSegment) bool {
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

// sortedByInstant 判定用量记录是否按（解析后的时间点升序、同一时间按
// 标识字典序）排列，即阶梯计价的计价顺序。
func sortedByInstant(recs []*usageRecord) bool {
	for i := 1; i < len(recs); i++ {
		ti, _ := time.Parse(time.RFC3339, recs[i-1].Time)
		tj, _ := time.Parse(time.RFC3339, recs[i].Time)
		ui, uj := ti.UTC(), tj.UTC()
		if uj.Before(ui) || (uj.Equal(ui) && recs[i].ID <= recs[i-1].ID) {
			return false
		}
	}
	return true
}

// validateTieredBill 校验阶梯账单：方案引用有效、快照与方案一致、账单方案
// 符合账期月的方案安排，并按保存的方案规则从零累计重放计价，逐条比对分段、
// 小计、各档合计与总金额；任何不符都视为数据损坏。
func (s *state) validateTieredBill(key string, b *bill, c *customer) error {
	if b.PlanID == "" {
		return fmt.Errorf("账单 %q 缺少方案标识", key)
	}
	p, ok := s.Plans[b.PlanID]
	if !ok {
		return fmt.Errorf("账单 %q 引用了不存在的方案 %q", key, b.PlanID)
	}
	if b.PlanName != p.Name {
		return fmt.Errorf("账单 %q 的方案名称快照与方案 %q 不符", key, b.PlanID)
	}
	if !tiersEqual(b.PlanTiers, p.Tiers) {
		return fmt.Errorf("账单 %q 的方案规则快照与方案 %q 不符", key, b.PlanID)
	}
	if b.MonthlyFee != p.MonthlyFee {
		return fmt.Errorf("账单 %q 的月费快照与方案 %q 不符", key, b.PlanID)
	}
	// 账单方案必须符合账期安排：创建时绑定的初始方案被生效月不晚于
	// 账期月的最后一次变更替换（无变更时即初始绑定，兼容旧存档）。
	if want := s.effectivePlanID(c, b.Month); want != b.PlanID {
		return fmt.Errorf("账单 %q 的方案 %q 与账期 %s 安排的方案 %q 不一致", key, b.PlanID, b.Month, want)
	}
	if b.UnitPrice != 0 {
		return fmt.Errorf("账单 %q 是阶梯账单但保存了统一单价", key)
	}
	if len(b.TierTotals) != len(p.Tiers) {
		return fmt.Errorf("账单 %q 的分档合计档数与方案规则不符", key)
	}
	// 明细必须按计价顺序排列（前面已逐条核验与用量记录一致）。
	recs := make([]*usageRecord, len(b.Lines))
	for i, ln := range b.Lines {
		recs[i] = s.Usage[ln.UsageID]
	}
	if !sortedByInstant(recs) {
		return fmt.Errorf("账单 %q 的明细未按计价顺序（时间点升序、同一时间按标识字典序）排列", key)
	}
	priced, err := tieredPrice(p.Tiers, recs)
	if err != nil {
		return fmt.Errorf("账单 %q 按方案规则计价失败: %w", key, err)
	}
	for i, ln := range b.Lines {
		if !segmentsEqual(ln.Segments, priced.lines[i].segments) {
			return fmt.Errorf("账单 %q 的明细 %q 分段与计价规则不符", key, ln.UsageID)
		}
		if ln.LineFee != priced.lines[i].fee {
			return fmt.Errorf("账单 %q 的明细 %q 小计与计价规则不符", key, ln.UsageID)
		}
	}
	for i, tt := range b.TierTotals {
		if tt.Quantity != priced.tierQty[i] || tt.Fee != priced.tierFee[i] {
			return fmt.Errorf("账单 %q 的第 %d 档合计与计价规则不符", key, i+1)
		}
	}
	// 原总金额 = 月费 + 全月用量费；月费不进入分档合计与逐条小计。
	wantTotal, err := add64(priced.totalFee, b.MonthlyFee)
	if err != nil || wantTotal != b.TotalFee {
		return fmt.Errorf("账单 %q 总金额不等于月费加全月用量费", key)
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

func planChangeKey(customerID, month string) string {
	return customerID + "|" + month
}

func suspensionKey(customerID, startMonth string) string {
	return customerID + "|" + startMonth
}

// monthInRange 判定月份 m 是否落在左闭右开区间 [start, end) 内。
// 月份均为已校验的 YYYY-MM，字典序即时间序。
func monthInRange(m, start, end string) bool {
	return m >= start && m < end
}

// suspensionsFor 返回某客户的全部暂停区间，按起月升序。
func (s *state) suspensionsFor(customerID string) []*suspension {
	var list []*suspension
	for _, su := range s.Suspensions {
		if su.CustomerID == customerID {
			list = append(list, su)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].StartMonth < list[j].StartMonth })
	return list
}

// isSuspendedMonth 报告某客户的指定 UTC 自然月是否处于任一暂停区间内
// （区间包含起月、不包含结束月；相接区间视为连续暂停）。
func (s *state) isSuspendedMonth(customerID, month string) bool {
	for _, su := range s.Suspensions {
		if su.CustomerID == customerID && monthInRange(month, su.StartMonth, su.EndMonth) {
			return true
		}
	}
	return false
}

// planChangesFor 返回某客户的全部方案变更，按生效月升序。
func (s *state) planChangesFor(customerID string) []*planChange {
	var list []*planChange
	for _, ch := range s.PlanChanges {
		if ch.CustomerID == customerID {
			list = append(list, ch)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Month < list[j].Month })
	return list
}

// effectivePlanID 返回客户在某 UTC 自然月实际适用的阶梯方案标识：
// 创建时绑定的初始方案，被生效月不晚于该月的最后一次变更替换。
func (s *state) effectivePlanID(cust *customer, month string) string {
	planID := cust.PlanID
	latest := ""
	for _, ch := range s.PlanChanges {
		if ch.CustomerID == cust.ID && ch.Month <= month && ch.Month >= latest {
			latest = ch.Month
			planID = ch.PlanID
		}
	}
	return planID
}

// sealed 报告某客户的指定月份是否已封账。
func (s *state) sealed(customerID, month string) bool {
	_, ok := s.Bills[billKey(customerID, month)]
	return ok
}

// isWithdrawn 报告指定用量标识是否已撤回。已撤回用量不参与结算、方案变更
// 预检与暂停区间冲突检查，但原记录永久保留并继续参与导入判重。
func (s *state) isWithdrawn(usageID string) bool {
	_, ok := s.Withdrawals[usageID]
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
