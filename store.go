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
	ID          string              `json:"id"`
	CustomerID  string              `json:"customer_id"`
	Total       int64               `json:"total_fen"` // 收款总额，正整数分，等于分配合计
	Note        string              `json:"note"`
	Allocations []paymentAllocation `json:"allocations"` // 至少一项，月份不重复，按月份升序保存
	// Auto 为 true 表示该收款由 bill remit-auto 自动分配登记：首次分配由工具
	// 按最早欠款账期确定，判重只看客户、总额与备注；false（含旧存档缺字段）
	// 表示 bill pay / bill remit 的显式分配登记，仍按月份-金额对应关系判重。
	// 自动与显式分配登记之间不得复用同一收款标识。
	Auto         bool   `json:"auto,omitempty"`
	Seq          int64  `json:"seq"` // 全局递增操作序号，决定历史展示顺序
	CreatedAt    string `json:"created_at"`
	Revoked      bool   `json:"revoked"`
	RevokeReason string `json:"revoke_reason,omitempty"`
	RevokeSeq    int64  `json:"revoke_seq,omitempty"`
	RevokedAt    string `json:"revoked_at,omitempty"`
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

// correction 是对一笔未撤销收款的一次更正登记，分两类：
//
//   - 分配更正（Reassign 为 false）：客户、总额、备注、原账单及应付不变，
//     仅把该笔收款当前生效的分配整体替换为完整新分配。记录一旦写入永不
//     删除；首次登记内容永久保留，bill pay / bill remit 仍按原始分配判重，
//     更正不影响收款身份。可连续更正，每次以当时最新分配为起点。
//   - 收款客户归属更正（Reassign 为 true，bill reassign）：把整笔实收从
//     当前归属客户转移到另一个已存在客户，不重复收钱。当前客户最新分配
//     整笔撤去，计入目标客户的新分配（Allocations）；原登记客户、总额、
//     备注、首次分配及自动或显式登记身份不改写，收款 CustomerID 仍为首次
//     登记客户。记录永久保留、不可撤销，可连续更正（包括再次归属更正）。
//
// 两类更正共用全局唯一更正标识：同一标识不可跨类型复用。
type correction struct {
	ID          string              `json:"id"`
	PaymentID   string              `json:"payment_id"` // 目标收款标识
	Reason      string              `json:"reason"`
	Allocations []paymentAllocation `json:"allocations"` // 完整新分配，至少一项，月份不重复，按月份升序保存
	// Reassign 为 true 表示这是一笔收款客户归属更正：Allocations 的月份
	// 属于目标客户 TargetCustomerID（异于更正发生时的当前归属客户）；
	// false（含旧存档缺字段）表示同客户分配更正，月份属于收款当前归属客户。
	Reassign         bool   `json:"reassign,omitempty"`
	TargetCustomerID string `json:"target_customer_id,omitempty"` // 归属更正的目标客户标识；仅 Reassign 为 true 时非空
	Seq              int64  `json:"seq"`                          // 全局递增操作序号，与调整/收款及其撤销共用
	CreatedAt        string `json:"created_at"`
}

// refund 是对一笔已登记收款的部分退款登记：从该收款当前最新分配涉及的
// 月份中实际退回资金，各月退款金额为正整数分，累计不得超过该笔在该月的
// 分配。记录一旦写入永不删除、不可撤销；首次退款后该收款的最新分配固定，
// 不得再新增分配更正或整笔撤销。退款标识与收款、调整、更正标识命名空间
// 相互独立，可同名。
type refund struct {
	ID          string              `json:"id"`
	PaymentID   string              `json:"payment_id"` // 目标收款标识
	Reason      string              `json:"reason"`
	Allocations []paymentAllocation `json:"allocations"` // 各月退款金额，至少一项，月份不重复，按月份升序保存
	Seq         int64               `json:"seq"`         // 全局递增操作序号，与调整/收款/更正及其撤销共用
	CreatedAt   string              `json:"created_at"`
}

// planChange 是阶梯客户的一次按月生效的方案变更：自生效月（UTC 自然月，
// 含）起改用目标方案，直到下一次变更；生效月之前的月份不受影响。记录一旦
// 写入永不修改或删除；变更不产生账后流水事件，也不占用全局操作序号。
// 变更可被 plan revoke 撤销：撤销只追加撤销记录（PlanChangeRevocations），
// 原变更内容与原原因永久保留；有效方案只考虑未撤销变更，已撤销变更的月份
// 槽位仍永久占用（新变更不得复用该客户月份）。
type planChange struct {
	CustomerID string `json:"customer_id"`
	Month      string `json:"month"`   // YYYY-MM（UTC），生效月（含）
	PlanID     string `json:"plan_id"` // 目标阶梯计费方案标识
	Reason     string `json:"reason"`
	CreatedAt  string `json:"created_at"`
}

// planChangeRevocation 是一项方案变更的撤销登记：以客户与目标变更生效月
// 共同识别目标变更。原变更内容（目标方案与原因）永久保留，撤销只追加本
// 记录；撤销记录永久保留、不可撤销，每项变更只能撤销一次。撤销不产生账后
// 流水事件，也不占用全局操作序号。撤销后目标变更不再参与有效方案安排：
// 自其生效月起沿用此前最后一项未撤销变更的方案，无则用初始方案。
type planChangeRevocation struct {
	CustomerID string `json:"customer_id"`
	Month      string `json:"month"`  // 目标变更的生效月（YYYY-MM，UTC），与客户共同标识撤销目标
	Reason     string `json:"reason"` // 撤销原因，非空
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
	EndMonth   string `json:"end_month"`   // YYYY-MM（UTC），原结束月（不含），严格晚于起月；登记提前恢复后仍永久保留原值
	Reason     string `json:"reason"`      // 原暂停原因，永久保留
	CreatedAt  string `json:"created_at"`
}

// suspensionResume 是一段暂停的按月提前恢复登记：目标暂停由客户与原起月
// 共同标识，恢复月（UTC 自然月，含）起重新提供服务，该月不再受该项暂停
// 限制；目标暂停的当前有效区间由 [原起月, 原结束月) 缩短为 [原起月, 恢复月)，
// 原区间与原原因永久保留。恢复记录一旦写入永不修改、不可撤销，每项暂停只能
// 登记一次提前恢复；恢复不产生账后流水事件，也不占用全局操作序号。
type suspensionResume struct {
	CustomerID  string `json:"customer_id"`
	StartMonth  string `json:"start_month"`  // 目标暂停的原起月（YYYY-MM，UTC），与客户共同标识目标暂停
	ResumeMonth string `json:"resume_month"` // YYYY-MM（UTC），恢复月（含）；严格满足 原起月 < 恢复月 < 原结束月
	Reason      string `json:"reason"`       // 恢复原因，非空
	CreatedAt   string `json:"created_at"`
}

// termination 是阶梯客户的按月订阅终止登记：自终止月（UTC 自然月，含）起
// 永久结束后续服务——不接收新用量、不结算、不封账、不收月费、不生成零金额
// 账单，终止前月份仍按原规则补结算，历史账务全部保留。每客户只能登记一次，
// 记录永久保留、不可修改或撤销，客户标识不能复用；终止不产生账后流水事件，
// 也不占用全局操作序号。固定单价客户不适用终止。
type termination struct {
	CustomerID string `json:"customer_id"`
	Month      string `json:"month"` // YYYY-MM（UTC），终止月（含），与操作当天无关
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

// usageCorrection 是一条未封账用量的原子更正：以原用量标识为键，把错误的
// 客户、时间或数量替换为一条全新替代记录。更正成功时原记录按更正原因登记
// 撤回（Withdrawals 中保存同因撤回标记），替代记录登记到 Usage 并在此永久
// 保存关联；原内容不可改写，更正不可撤销，不占用账后操作序号。每条原用量
// 只能更正一次；替代记录本身仍可继续更正（形成前身→后继链）或按原规则撤回。
// 字段保存首次更正请求的完整内容，用于相同请求的幂等重放判定（时间按解析后
// 的瞬间比较）。
type usageCorrection struct {
	UsageID       string `json:"usage_id"`       // 原用量标识（与键一致）
	ReplacementID string `json:"replacement_id"` // 替代用量标识（全局唯一，首次更正时全新）
	NewCustomerID string `json:"new_customer_id"`
	NewTime       string `json:"new_time"` // RFC3339，保留首次更正的原始输入
	NewQuantity   int64  `json:"new_quantity"`
	Reason        string `json:"reason"` // 更正原因，同时是原记录的撤回原因，非空
	CreatedAt     string `json:"created_at"`
}

type state struct {
	Version     int                     `json:"version"`
	Customers   map[string]*customer    `json:"customers"`
	Plans       map[string]*plan        `json:"plans"`             // 全局唯一方案标识 -> 阶梯计费方案
	Usage       map[string]*usageRecord `json:"usage"`             // 全局唯一用量标识 -> 记录
	Bills       map[string]*bill        `json:"bills"`             // 客户 + "|" + 月份 -> 账单
	Adjustments map[string]*adjustment  `json:"adjustments"`       // 全局唯一调整标识 -> 记录
	Payments    map[string]*payment     `json:"payments"`          // 全局唯一收款标识 -> 记录（与调整标识相互独立，可同名）
	Corrections map[string]*correction  `json:"corrections"`       // 全局唯一更正标识 -> 记录（与收款、调整标识相互独立，可同名）
	Refunds     map[string]*refund      `json:"refunds,omitempty"` // 全局唯一退款标识 -> 记录（与收款、调整、更正标识相互独立，可同名；不可撤销）
	PlanChanges map[string]*planChange  `json:"plan_changes"`      // 客户 + "|" + 生效月 -> 方案变更（按生效月递增追加，不可改写；撤销后原记录仍保留）
	// PlanChangeRevocations 以客户与目标变更生效月为键保存方案变更撤销登记；
	// 原变更仍保留在 PlanChanges 中并永久可追溯，有效方案只考虑未撤销变更。
	// 旧存档缺少本字段时全部变更视为有效。撤销记录永久保留、不可撤销。
	PlanChangeRevocations map[string]*planChangeRevocation `json:"plan_change_revocations,omitempty"`
	Suspensions           map[string]*suspension           `json:"suspensions,omitempty"` // 客户 + "|" + 起月 -> 暂停区间（原区间与原因不可改写）
	// SuspensionResumes 以客户与原起月为键保存暂停的提前恢复登记；旧存档
	// 缺少本字段时暂停沿用原区间。恢复记录永久保留、不可改写或撤销。
	SuspensionResumes map[string]*suspensionResume `json:"suspension_resumes,omitempty"`
	Withdrawals       map[string]*withdrawal       `json:"withdrawals,omitempty"` // 用量标识 -> 撤回标记（不可恢复，标识不可复用）
	// UsageCorrections 以原用量标识为键保存用量更正关联；原记录同时在
	// Withdrawals 中按更正原因标记撤回。旧存档缺少本字段视为无更正。
	UsageCorrections map[string]*usageCorrection `json:"usage_corrections,omitempty"`
	// Terminations 以客户标识为键保存按月订阅终止登记；每客户至多一条，
	// 永久保留、不可修改或撤销。旧存档缺少本字段视为未终止。
	Terminations map[string]*termination `json:"terminations,omitempty"`
	NextSeq      int64                   `json:"next_seq"` // 已分配的最大操作序号（调整/收款/更正/退款及其撤销共用；方案变更、暂停、用量撤回与用量更正不占用）
	path         string                  `json:"-"`
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
	// 旧版本数据文件没有 plans/adjustments/payments/corrections/refunds/
	// plan_changes/suspensions/withdrawals/usage_corrections/terminations/
	// next_seq 字段：视为零方案、零调整、零实收、零更正、零退款、零方案变更、
	// 零暂停、零撤回、零用量更正（全部用量有效）、未终止。
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
	if s.Refunds == nil {
		s.Refunds = map[string]*refund{}
	}
	if s.PlanChanges == nil {
		s.PlanChanges = map[string]*planChange{}
	}
	if s.PlanChangeRevocations == nil {
		s.PlanChangeRevocations = map[string]*planChangeRevocation{}
	}
	if s.Suspensions == nil {
		s.Suspensions = map[string]*suspension{}
	}
	if s.SuspensionResumes == nil {
		s.SuspensionResumes = map[string]*suspensionResume{}
	}
	if s.Withdrawals == nil {
		s.Withdrawals = map[string]*withdrawal{}
	}
	if s.UsageCorrections == nil {
		s.UsageCorrections = map[string]*usageCorrection{}
	}
	if s.Terminations == nil {
		s.Terminations = map[string]*termination{}
	}
	s.path = p
	return &s, nil
}

func newState(p string) *state {
	return &state{
		Version:               stateVersion,
		Customers:             map[string]*customer{},
		Plans:                 map[string]*plan{},
		Usage:                 map[string]*usageRecord{},
		Bills:                 map[string]*bill{},
		Adjustments:           map[string]*adjustment{},
		Payments:              map[string]*payment{},
		Corrections:           map[string]*correction{},
		Refunds:               map[string]*refund{},
		PlanChanges:           map[string]*planChange{},
		PlanChangeRevocations: map[string]*planChangeRevocation{},
		Suspensions:           map[string]*suspension{},
		SuspensionResumes:     map[string]*suspensionResume{},
		Withdrawals:           map[string]*withdrawal{},
		UsageCorrections:      map[string]*usageCorrection{},
		Terminations:          map[string]*termination{},
		path:                  p,
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
	// 方案变更撤销登记：键与客户/目标生效月一致；引用的目标变更必须存在
	// （失效撤销引用即损坏）；撤销原因非空。原变更内容、原原因与撤销记录都
	// 永久保留，撤销不可撤销。旧文件缺少撤销记录时全部变更视为有效
	// （PlanChangeRevocations 已在载入时补为空表）。账单方案与撤销后有效
	// 安排的一致性由阶梯账单校验（validateTieredBill）把关。
	for key, rv := range s.PlanChangeRevocations {
		if rv == nil {
			return fmt.Errorf("方案变更撤销 %q 的数据为空", key)
		}
		if planChangeKey(rv.CustomerID, rv.Month) != key {
			return fmt.Errorf("方案变更撤销键不一致: 键 %q / 客户月份 %s|%s", key, rv.CustomerID, rv.Month)
		}
		if _, ok := s.PlanChanges[key]; !ok {
			return fmt.Errorf("方案变更撤销 %q 引用了不存在的方案变更（客户 %q 生效月 %q，撤销目标失效）",
				key, rv.CustomerID, rv.Month)
		}
		if !validMonth(rv.Month) {
			return fmt.Errorf("方案变更撤销 %q 的目标生效月无效", key)
		}
		if strings.TrimSpace(rv.Reason) == "" {
			return fmt.Errorf("方案变更撤销 %q 的原因为空", key)
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
	// 用量更正关联：键与原标识一致；原记录与替代记录都必须存在于 Usage，
	// 替代记录内容（客户、解析后的时间点、数量）须与更正保存的新内容一致；
	// 原记录必须已按更正原因撤回（撤回标记的原因与更正原因不符即损坏）；
	// 同一替代记录只能有一个来源；关联不得成环。替代记录后来被合法撤回或
	// 继续更正（即它本身又作为某条更正的原记录）属正常状态，不判为损坏。
	// 旧文件缺少更正记录视为无更正（UsageCorrections 已在载入时补为空表）。
	replacementSources := make(map[string]string) // 替代用量标识 -> 原用量标识
	for id, uc := range s.UsageCorrections {
		if uc == nil {
			return fmt.Errorf("用量更正 %q 的数据为空", id)
		}
		if uc.UsageID != id {
			return fmt.Errorf("用量更正键不一致: 键 %q / 原用量标识 %q", id, uc.UsageID)
		}
		if uc.UsageID == "" {
			return errors.New("存在空原用量标识的更正记录")
		}
		if strings.TrimSpace(uc.Reason) == "" {
			return fmt.Errorf("用量更正 %q 的原因为空", id)
		}
		orig, ok := s.Usage[uc.UsageID]
		if !ok {
			return fmt.Errorf("用量更正 %q 引用了不存在的原用量 %q（更正来源失效）", id, uc.UsageID)
		}
		// 原记录内容不可改写：原用量记录仍须是正常的正数量有效格式（已在
		// 上面的用量校验中核验），此处仅确认其存在。
		if orig.ID != uc.UsageID {
			return fmt.Errorf("用量更正 %q 的原记录标识不一致", id)
		}
		if uc.ReplacementID == "" {
			return fmt.Errorf("用量更正 %q 缺少替代用量标识", id)
		}
		if uc.ReplacementID == uc.UsageID {
			return fmt.Errorf("用量更正 %q 的替代标识与原标识相同", id)
		}
		repl, ok := s.Usage[uc.ReplacementID]
		if !ok {
			return fmt.Errorf("用量更正 %q 引用了不存在的替代用量 %q（更正目标失效）", id, uc.ReplacementID)
		}
		if src, dup := replacementSources[uc.ReplacementID]; dup {
			return fmt.Errorf("替代用量 %q 同时是用量 %q 与 %q 的更正替代（同一替代记录不得有多个来源）",
				uc.ReplacementID, src, uc.UsageID)
		}
		replacementSources[uc.ReplacementID] = uc.UsageID
		if _, ok := s.Customers[uc.NewCustomerID]; !ok {
			return fmt.Errorf("用量更正 %q 引用了不存在的新客户 %q", id, uc.NewCustomerID)
		}
		nt, err := time.Parse(time.RFC3339, uc.NewTime)
		if err != nil {
			return fmt.Errorf("用量更正 %q 的新时间不是 RFC3339: %w", id, err)
		}
		if uc.NewQuantity <= 0 {
			return fmt.Errorf("用量更正 %q 的新数量非正", id)
		}
		rt, err := time.Parse(time.RFC3339, repl.Time)
		if err != nil {
			return fmt.Errorf("用量更正 %q 的替代记录 %q 时间不是 RFC3339: %w", id, uc.ReplacementID, err)
		}
		// 替代记录内容（客户、解析后的时间点、数量）必须与更正保存的新内容一致；
		// 时间按瞬间比较，Z 与 +08:00 表示同一时刻视为一致。
		if repl.CustomerID != uc.NewCustomerID || repl.Quantity != uc.NewQuantity || !rt.Equal(nt) {
			return fmt.Errorf("用量更正 %q 的替代记录 %q 内容与保存的新内容不一致（更正关联损坏）",
				id, uc.ReplacementID)
		}
		// 原记录必须已撤回，且撤回原因就是更正原因。
		w, ok := s.Withdrawals[uc.UsageID]
		if !ok {
			return fmt.Errorf("用量更正 %q 的原用量 %q 未撤回（更正须把原记录按更正原因标记撤回）", id, uc.UsageID)
		}
		if w.Reason != uc.Reason {
			return fmt.Errorf("用量更正 %q 的原用量 %q 撤回原因 %q 与更正原因 %q 不符",
				id, uc.UsageID, w.Reason, uc.Reason)
		}
	}
	// 关联成环检测：沿“原记录 -> 替代记录”的更正链前进，重复访问即成环。
	// 合法链必然终止于一条未再被更正的记录。
	for id := range s.UsageCorrections {
		seen := map[string]bool{id: true}
		cur := id
		for {
			uc, ok := s.UsageCorrections[cur]
			if !ok {
				break // 链终止于未再被更正的记录
			}
			if seen[uc.ReplacementID] {
				return fmt.Errorf("用量更正关联在 %q 处成环（更正链不得成环）", uc.ReplacementID)
			}
			seen[uc.ReplacementID] = true
			cur = uc.ReplacementID
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
	// 同一客户的当前有效区间不得重叠（可以相接）；有效暂停月内不得存在有效
	// （未撤回）用量或账单。登记提前恢复后原区间与原因仍永久保留，仅当前
	// 有效区间缩短为 [原起月, 恢复月)。
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
	// 暂停提前恢复记录：键与客户/原起月一致；引用的目标暂停必须存在（恢复
	// 引用缺失即损坏）；恢复月必须是严格介于原起月与原结束月之间的 YYYY-MM；
	// 原因非空。原区间、原原因与恢复记录都永久保留，不可改写或撤销。旧文件
	// 缺少恢复记录时暂停沿用原区间（SuspensionResumes 已在载入时补为空表）。
	for key, r := range s.SuspensionResumes {
		if r == nil {
			return fmt.Errorf("暂停提前恢复 %q 的数据为空", key)
		}
		if suspensionKey(r.CustomerID, r.StartMonth) != key {
			return fmt.Errorf("暂停提前恢复键不一致: 键 %q / 客户原起月 %s|%s", key, r.CustomerID, r.StartMonth)
		}
		su, ok := s.Suspensions[key]
		if !ok {
			return fmt.Errorf("暂停提前恢复 %q 引用了不存在的暂停（客户 %q 原起月 %q，恢复目标缺失）", key, r.CustomerID, r.StartMonth)
		}
		if !validMonth(r.ResumeMonth) {
			return fmt.Errorf("暂停提前恢复 %q 的恢复月无效（必须是 YYYY-MM）", key)
		}
		if r.ResumeMonth <= su.StartMonth || r.ResumeMonth >= su.EndMonth {
			return fmt.Errorf("暂停提前恢复 %q 的恢复月 %s 越界（必须严格满足 原起月 %s < 恢复月 < 原结束月 %s）",
				key, r.ResumeMonth, su.StartMonth, su.EndMonth)
		}
		if strings.TrimSpace(r.Reason) == "" {
			return fmt.Errorf("暂停提前恢复 %q 的原因为空", key)
		}
	}
	for customerID := range s.Customers {
		list := s.suspensionsFor(customerID)
		for i := 1; i < len(list); i++ {
			// 区间重叠按当前有效区间判断：提前恢复只缩短目标区间，其他暂停
			// 独立生效；区间仍可相接（相接视为连续暂停）。
			if list[i].StartMonth < s.suspensionEffectiveEnd(list[i-1]) {
				return fmt.Errorf("客户 %s 的暂停有效区间 %s..%s 与 %s..%s 重叠（区间可以相接但不得重叠）",
					customerID,
					list[i-1].StartMonth, s.suspensionEffectiveEnd(list[i-1]),
					list[i].StartMonth, s.suspensionEffectiveEnd(list[i]))
			}
		}
		for _, su := range list {
			// 有效暂停月为 [原起月, 当前有效结束月)：提前恢复释放的月份正常
			// 服务，允许存在用量与账单。
			effEnd := s.suspensionEffectiveEnd(su)
			for _, u := range s.Usage {
				// 已撤回用量不参与用量冲突判断，可存在于随后暂停的月份。
				if _, withdrawn := s.Withdrawals[u.ID]; withdrawn {
					continue
				}
				if u.CustomerID == customerID && monthInRange(utcMonth(u.Time), su.StartMonth, effEnd) {
					return fmt.Errorf("客户 %s 在暂停有效区间 %s..%s（不含结束月）内存在用量 %q（%s）",
						customerID, su.StartMonth, effEnd, u.ID, utcMonth(u.Time))
				}
			}
			for _, b := range s.Bills {
				if b.CustomerID == customerID && monthInRange(b.Month, su.StartMonth, effEnd) {
					return fmt.Errorf("客户 %s 在暂停有效区间 %s..%s（不含结束月）内存在账单（月份 %s），暂停月不得封账",
						customerID, su.StartMonth, effEnd, b.Month)
				}
			}
		}
	}

	// 按月订阅终止登记：键即客户标识；客户必须存在且为绑定阶梯方案的客户
	// （固定单价客户不适用终止）；终止月必须是合法的 YYYY-MM；原因非空。
	// 终止月（含）起不得存在有效（未撤回）用量或账单——已撤回用量与保留的
	// 方案变更、暂停及提前恢复安排正常读取，不参与本检查。旧文件缺少终止
	// 记录视为未终止（Terminations 已在载入时补为空表）。
	for key, tm := range s.Terminations {
		if tm == nil {
			return fmt.Errorf("订阅终止 %q 的数据为空", key)
		}
		if tm.CustomerID != key {
			return fmt.Errorf("订阅终止键不一致: 键 %q / 客户 %q", key, tm.CustomerID)
		}
		c, ok := s.Customers[tm.CustomerID]
		if !ok {
			return fmt.Errorf("订阅终止 %q 引用了不存在的客户 %q", key, tm.CustomerID)
		}
		if c.PlanID == "" {
			return fmt.Errorf("订阅终止 %q 的客户 %q 未绑定阶梯方案（固定单价客户不适用终止）", key, tm.CustomerID)
		}
		if !validMonth(tm.Month) {
			return fmt.Errorf("订阅终止 %q 的终止月无效（必须是 YYYY-MM）", key)
		}
		if strings.TrimSpace(tm.Reason) == "" {
			return fmt.Errorf("订阅终止 %q 的原因为空", key)
		}
		for _, u := range s.Usage {
			if _, withdrawn := s.Withdrawals[u.ID]; withdrawn {
				continue
			}
			if u.CustomerID == tm.CustomerID && utcMonth(u.Time) >= tm.Month {
				return fmt.Errorf("客户 %s 的终止月 %s 及之后存在有效用量 %q（%s），终止月起不接收新用量",
					tm.CustomerID, tm.Month, u.ID, utcMonth(u.Time))
			}
		}
		for _, b := range s.Bills {
			if b.CustomerID == tm.CustomerID && b.Month >= tm.Month {
				return fmt.Errorf("客户 %s 的终止月 %s 及之后存在账单（月份 %s），终止月起不结算、不封账",
					tm.CustomerID, tm.Month, b.Month)
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
	// 分配更正与归属更正记录：标识、目标收款、原因、新分配与序号都必须自洽。
	// 两类更正共用全局唯一更正标识（map 键保证），不可跨类型复用；允许与收款、
	// 调整标识同名。分配更正的月份须属于更正发生时（序号前一状态）当前归属
	// 客户的已存在账单；归属更正的目标客户须存在且异于当前归属客户，月份须
	// 属于目标客户的已存在账单。归属时间线与逐步余额在本循环之后统一回放核验。
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
		// 归属更正必须带目标客户；分配更正不得带目标客户。
		if c.Reassign {
			if c.TargetCustomerID == "" {
				return fmt.Errorf("归属更正 %q 缺少目标客户", id)
			}
			if _, ok := s.Customers[c.TargetCustomerID]; !ok {
				return fmt.Errorf("归属更正 %q 引用了不存在的目标客户 %q", id, c.TargetCustomerID)
			}
		} else if c.TargetCustomerID != "" {
			return fmt.Errorf("分配更正 %q 携带了目标客户 %q（仅归属更正允许转移客户）", id, c.TargetCustomerID)
		}
		// 更正发生时（序号前一状态）收款的归属客户；归属更正的目标必须异于
		// 该客户，分配月份按更正类型归属到对应客户的已存在账单。
		ownerBefore := paymentOwnerAt(s, p, c.Seq-1)
		allocCustomer := ownerBefore
		if c.Reassign {
			if c.TargetCustomerID == ownerBefore {
				return fmt.Errorf("归属更正 %q 的目标客户 %q 与收款 %q 当前归属客户相同，不得自我归属更正", id, c.TargetCustomerID, c.PaymentID)
			}
			allocCustomer = c.TargetCustomerID
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
			if _, ok := s.Bills[billKey(allocCustomer, al.Month)]; !ok {
				return fmt.Errorf("更正 %q 的分配引用了不存在的账单（客户 %s 月份 %s）", id, allocCustomer, al.Month)
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
	// 退款记录：标识、目标收款、原因、各月退款与序号都必须自洽。退款标识与
	// 收款、调整、更正标识命名空间相互独立，允许同名。退款只作用于未撤销
	// 收款的最新分配涉及的月份；首次退款后该收款不得再更正或整笔撤销，
	// 因此全部退款都以当前最新分配为准核验，累计不得超退。旧文件缺少退款
	// 记录视为无退款（Refunds 已在载入时补为空表）。
	for id, r := range s.Refunds {
		if r == nil {
			return fmt.Errorf("退款 %q 的数据为空", id)
		}
		if r.ID != id {
			return fmt.Errorf("退款标识不一致: 键 %q / 记录 %q", id, r.ID)
		}
		if r.ID == "" {
			return errors.New("存在空的退款标识")
		}
		p, ok := s.Payments[r.PaymentID]
		if !ok {
			return fmt.Errorf("退款 %q 引用了不存在的收款 %q（退款目标失效）", id, r.PaymentID)
		}
		if strings.TrimSpace(r.Reason) == "" {
			return fmt.Errorf("退款 %q 的原因为空", id)
		}
		if len(r.Allocations) == 0 {
			return fmt.Errorf("退款 %q 没有月份金额清单", id)
		}
		current := currentAllocations(s, p)
		seenMonths := make(map[string]bool)
		for _, al := range r.Allocations {
			if !validMonth(al.Month) {
				return fmt.Errorf("退款 %q 的月份 %q 无效", id, al.Month)
			}
			if seenMonths[al.Month] {
				return fmt.Errorf("退款 %q 的月份 %s 重复", id, al.Month)
			}
			seenMonths[al.Month] = true
			if al.Amount <= 0 {
				return fmt.Errorf("退款 %q 在 %s 的金额不是正整数", id, al.Month)
			}
			if allocAmountFor(current, al.Month) <= 0 {
				return fmt.Errorf("退款 %q 的月份 %s 不在收款 %q 的最新分配中（仅允许退最新分配涉及的月份）", id, al.Month, r.PaymentID)
			}
		}
		if r.Seq <= p.Seq || r.Seq > s.NextSeq {
			return fmt.Errorf("退款 %q 的操作序号越界", id)
		}
		if prev, dup := seenSeq[r.Seq]; dup {
			return fmt.Errorf("退款 %q 与 %q 的操作序号重复", id, prev)
		}
		seenSeq[r.Seq] = "退款 " + id
	}
	// 逐收款核验退款相关约束：已退款的收款不得被整笔撤销；首次退款后不得
	// 再出现任何更正（分配更正与归属更正都禁止，最新分配已固定）；各月累计
	// 退款不得超过该笔在该月的最新分配（累计超退即余额异常）。
	for pid, p := range s.Payments {
		refs := refundsFor(s, pid)
		if len(refs) == 0 {
			continue
		}
		firstRefundSeq := refs[0].Seq // refundsFor 按序号升序
		if p.Revoked {
			return fmt.Errorf("收款 %q 已发生退款（首次退款序号 %d）却又被整笔撤销（序号 %d），操作先后非法", pid, firstRefundSeq, p.RevokeSeq)
		}
		for _, c := range s.Corrections {
			if c.PaymentID == pid && c.Seq > firstRefundSeq {
				kind := "分配更正"
				if c.Reassign {
					kind = "归属更正"
				}
				return fmt.Errorf("%s %q 的序号 %d 晚于收款 %q 的首次退款序号 %d（首次退款后最新分配已固定，两类更正均禁止新增），操作先后非法",
					kind, c.ID, c.Seq, pid, firstRefundSeq)
			}
		}
		current := currentAllocations(s, p)
		totals := make(map[string]int64)
		for _, r := range refs {
			for _, al := range r.Allocations {
				sum, err := add64(totals[al.Month], al.Amount)
				if err != nil {
					return fmt.Errorf("收款 %q 在 %s 的累计退款溢出有符号 64 位整数范围，余额异常", pid, al.Month)
				}
				totals[al.Month] = sum
			}
		}
		for month, refunded := range totals {
			if alloc := allocAmountFor(current, month); refunded > alloc {
				return fmt.Errorf("收款 %q 在 %s 的累计退款 %d 分超过该月分配 %d 分（累计超退），余额异常", pid, month, refunded, alloc)
			}
		}
	}
	// 逐步回放全部账后操作，核验每张被触及账单在每一步之后都满足
	// 0 ≤ 实收 ≤ 应付 ≤ 有符号 64 位最大值——归属更正把实收从原客户账单整笔
	// 转出、计入目标客户账单，两侧都必须在迁移发生序号即合法；只核验最终余额
	// 会放过中间越界的存档。截止后的迁移不提前影响历史余额也在此体现：每个
	// 序号只回放该序号以前（含）的操作。
	if err := s.validatePostbillTimeline(); err != nil {
		return err
	}
	// 每张账单：当前应付（原总金额 + 全部未撤销调整净额）必须介于
	// 0 与有符号 64 位最大值之间；实收（当前归属本客户的全部未撤销收款按
	// 最新分配在该月计入并扣除退款后的合计）必须满足 0 ≤ 实收 ≤ 当前应付。
	// 越界说明金额与记录不一致。
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

// tvDelta 是一次账后操作对一张账单的应付/实收增量（分，带符号）。
type tvDelta struct {
	payable  int64
	received int64
}

// validatePostbillTimeline 按全局操作序号逐步回放全部账后操作（调整及其撤销、
// 收款登记、两类更正、收款撤销与退款），核验每张被触及账单在每一步之后都
// 满足 0 ≤ 实收 ≤ 应付 ≤ 有符号 64 位最大值。归属更正在其发生序号把实收
// 从原归属客户账单整笔转出（负增量）并计入目标客户账单（正增量），两侧在
// 同一序号都必须合法；截止后的迁移因此不会提前影响较早序号的余额。任一
// 中间步骤越界、溢出或回放终态与当前推导不一致，都按数据损坏拒绝。
func (s *state) validatePostbillTimeline() error {
	type tvEvent struct {
		seq    int64
		desc   string
		deltas map[string]tvDelta // billKey -> 增量
	}
	var events []tvEvent
	addDelta := func(ev *tvEvent, key string, dpay, drecv int64) error {
		cur := ev.deltas[key]
		if dpay != 0 {
			v, err := addSigned64(cur.payable, dpay)
			if err != nil {
				return fmt.Errorf("操作序号 %d（%s）之后应付越出有符号 64 位整数范围，余额异常", ev.seq, ev.desc)
			}
			cur.payable = v
		}
		if drecv != 0 {
			v, err := addSigned64(cur.received, drecv)
			if err != nil {
				return fmt.Errorf("操作序号 %d（%s）之后实收越出有符号 64 位整数范围，余额异常", ev.seq, ev.desc)
			}
			cur.received = v
		}
		ev.deltas[key] = cur
		return nil
	}

	// 调整及其撤销只改所属账期应付。
	for _, a := range s.Adjustments {
		ev := tvEvent{seq: a.Seq, desc: "调整 " + a.ID, deltas: map[string]tvDelta{}}
		if err := addDelta(&ev, billKey(a.CustomerID, a.Month), a.Amount, 0); err != nil {
			return err
		}
		events = append(events, ev)
		if a.Revoked {
			neg, err := neg64(a.Amount)
			if err != nil {
				return fmt.Errorf("调整 %q 的金额 %d 分无法抵消（越界），余额异常", a.ID, a.Amount)
			}
			rev := tvEvent{seq: a.RevokeSeq, desc: "撤销调整 " + a.ID, deltas: map[string]tvDelta{}}
			if err := addDelta(&rev, billKey(a.CustomerID, a.Month), neg, 0); err != nil {
				return err
			}
			events = append(events, rev)
		}
	}

	for _, p := range s.Payments {
		// 收款登记：按首次登记客户与首次分配增加对应账期实收。
		reg := tvEvent{seq: p.Seq, desc: "收款 " + p.ID, deltas: map[string]tvDelta{}}
		for _, al := range p.Allocations {
			if err := addDelta(&reg, billKey(p.CustomerID, al.Month), 0, al.Amount); err != nil {
				return err
			}
		}
		events = append(events, reg)

		// 沿更正链回放：分配更正只在同客户账单间替换；归属更正在同一序号
		// 从原归属客户账单整笔转出、计入目标客户账单。
		owner := p.CustomerID
		running := p.Allocations
		for _, c := range correctionsFor(s, p.ID) {
			ev := tvEvent{seq: c.Seq, deltas: map[string]tvDelta{}}
			if c.Reassign {
				ev.desc = "归属更正 " + c.ID
			} else {
				ev.desc = "更正 " + c.ID
			}
			for _, m := range unionMonths(running, c.Allocations) {
				oldAmt, newAmt := allocAmountFor(running, m), allocAmountFor(c.Allocations, m)
				if oldAmt > 0 {
					if err := addDelta(&ev, billKey(owner, m), 0, -oldAmt); err != nil {
						return err
					}
				}
				if newAmt > 0 {
					target := owner
					if c.Reassign {
						target = c.TargetCustomerID
					}
					if err := addDelta(&ev, billKey(target, m), 0, newAmt); err != nil {
						return err
					}
				}
			}
			events = append(events, ev)
			if c.Reassign {
				owner = c.TargetCustomerID
			}
			running = c.Allocations
		}

		// 整笔撤销取消撤销发生时最新归属客户的最新分配。
		if p.Revoked {
			rev := tvEvent{seq: p.RevokeSeq, desc: "撤销收款 " + p.ID, deltas: map[string]tvDelta{}}
			for _, al := range running {
				if err := addDelta(&rev, billKey(owner, al.Month), 0, -al.Amount); err != nil {
					return err
				}
			}
			events = append(events, rev)
		}

		// 退款只减少退款发生时归属客户的对应月实收。
		for _, r := range refundsFor(s, p.ID) {
			ownerAt := paymentOwnerAt(s, p, r.Seq)
			ev := tvEvent{seq: r.Seq, desc: "退款 " + r.ID, deltas: map[string]tvDelta{}}
			for _, al := range r.Allocations {
				if err := addDelta(&ev, billKey(ownerAt, al.Month), 0, -al.Amount); err != nil {
					return err
				}
			}
			events = append(events, ev)
		}
	}

	sort.Slice(events, func(i, j int) bool { return events[i].seq < events[j].seq })

	payable := make(map[string]int64, len(s.Bills))
	received := make(map[string]int64, len(s.Bills))
	for key, b := range s.Bills {
		payable[key] = b.TotalFee
		received[key] = 0
	}
	for _, ev := range events {
		// 同一事件先累加应付再累加实收，并在每张被触及账单上校验边界。
		for key, d := range ev.deltas {
			if _, ok := payable[key]; !ok {
				return fmt.Errorf("操作序号 %d（%s）引用了不存在的账单 %s（引用缺失）", ev.seq, ev.desc, key)
			}
			if d.payable != 0 {
				v, err := addSigned64(payable[key], d.payable)
				if err != nil {
					return fmt.Errorf("操作序号 %d（%s）之后应付越出有符号 64 位整数范围，余额异常", ev.seq, ev.desc)
				}
				payable[key] = v
			}
		}
		for key, d := range ev.deltas {
			if d.received != 0 {
				v, err := addSigned64(received[key], d.received)
				if err != nil {
					return fmt.Errorf("操作序号 %d（%s）之后实收越出有符号 64 位整数范围，余额异常", ev.seq, ev.desc)
				}
				received[key] = v
			}
			if payable[key] < 0 {
				return fmt.Errorf("操作序号 %d（%s）之后账单 %s 应付为 %d 分（小于 0），逐步余额越界", ev.seq, ev.desc, key, payable[key])
			}
			if received[key] < 0 {
				return fmt.Errorf("操作序号 %d（%s）之后账单 %s 实收为 %d 分（小于 0），逐步余额越界", ev.seq, ev.desc, key, received[key])
			}
			if received[key] > payable[key] {
				return fmt.Errorf("操作序号 %d（%s）之后账单 %s 实收 %d 分超过应付 %d 分，逐步余额越界",
					ev.seq, ev.desc, key, received[key], payable[key])
			}
		}
	}

	// 回放终态必须与当前推导一致，确保两套计算口径相同。
	for key, b := range s.Bills {
		_, wantPayable, err := billTotals(b, adjustmentsFor(s, b.CustomerID, b.Month))
		if err != nil {
			return fmt.Errorf("账单 %q 的调整金额与记录不一致: %w", key, err)
		}
		wantReceived, err := paymentReceived(s, b.CustomerID, b.Month)
		if err != nil {
			return fmt.Errorf("账单 %q 的收款金额与记录不一致: %w", key, err)
		}
		if payable[key] != wantPayable || received[key] != wantReceived {
			return fmt.Errorf("账单 %q 的账后回放终态（应付 %d、实收 %d）与当前余额（应付 %d、实收 %d）不一致，数据异常",
				key, payable[key], received[key], wantPayable, wantReceived)
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

// suspensionResumeKey 与 suspensionKey 相同：提前恢复以客户与目标暂停的原
// 起月共同标识，一项目暂停至多一条恢复记录。
func suspensionResumeKey(customerID, startMonth string) string {
	return customerID + "|" + startMonth
}

// suspensionResumeOf 返回某项暂停的提前恢复登记；未登记时返回 nil。
func (s *state) suspensionResumeOf(su *suspension) *suspensionResume {
	return s.SuspensionResumes[suspensionResumeKey(su.CustomerID, su.StartMonth)]
}

// suspensionEffectiveEnd 返回某项暂停的当前有效结束月（不含）：登记提前
// 恢复后为恢复月，否则为原结束月。原暂停区间与原因永久保留，本函数只决定
// 当前仍受暂停限制的月份范围 [原起月, 有效结束月)。
func (s *state) suspensionEffectiveEnd(su *suspension) string {
	if r := s.suspensionResumeOf(su); r != nil {
		return r.ResumeMonth
	}
	return su.EndMonth
}

// isSuspendedMonth 报告某客户的指定 UTC 自然月是否处于任一暂停的当前有效
// 区间内（区间包含原起月、不包含当前有效结束月；相接区间视为连续暂停）。
// 提前恢复释放的月份不再受该项暂停限制。
func (s *state) isSuspendedMonth(customerID, month string) bool {
	for _, su := range s.Suspensions {
		if su.CustomerID == customerID && monthInRange(month, su.StartMonth, s.suspensionEffectiveEnd(su)) {
			return true
		}
	}
	return false
}

// terminationOf 返回某客户的按月订阅终止登记；未登记时返回 nil。
// 终止以客户标识识别，每客户至多一条。
func (s *state) terminationOf(customerID string) *termination {
	return s.Terminations[customerID]
}

// isTerminatedMonth 报告某客户的指定 UTC 自然月是否不早于其终止月：自终止月
// （含）起永久结束后续服务，不接收新用量、不结算、不封账、不收月费。未登记
// 终止时全部月份正常服务。终止限制与暂停、提前恢复相互独立：任何安排都不能
// 越过终止边界恢复服务。
func (s *state) isTerminatedMonth(customerID, month string) bool {
	tm := s.Terminations[customerID]
	return tm != nil && month >= tm.Month
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

// planChangeRevoked 报告一项目标方案变更（以客户与生效月识别）是否已撤销。
// 撤销只追加撤销记录，原变更永久保留；已撤销变更不参与有效方案安排。
func (s *state) planChangeRevoked(customerID, month string) bool {
	_, ok := s.PlanChangeRevocations[planChangeKey(customerID, month)]
	return ok
}

// activePlanChanges 返回某客户全部未撤销的方案变更，按生效月升序。有效
// 方案安排只考虑这些变更；已撤销变更仍保留在 planChangesFor 的完整列表中
// 供追溯展示，且其生效月槽位仍永久占用。
func (s *state) activePlanChanges(customerID string) []*planChange {
	var list []*planChange
	for _, ch := range s.PlanChanges {
		if ch.CustomerID == customerID && !s.planChangeRevoked(ch.CustomerID, ch.Month) {
			list = append(list, ch)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Month < list[j].Month })
	return list
}

// effectivePlanID 返回客户在某 UTC 自然月实际适用的阶梯方案标识：
// 创建时绑定的初始方案，被生效月不晚于该月的最后一次**未撤销**变更替换；
// 已撤销变更不参与安排——其生效月起沿用此前最后一项未撤销变更的方案，无
// 则用初始方案，直到其后下一项未撤销变更的生效月前。
func (s *state) effectivePlanID(cust *customer, month string) string {
	planID := cust.PlanID
	latest := ""
	for _, ch := range s.PlanChanges {
		if ch.CustomerID != cust.ID || s.planChangeRevoked(ch.CustomerID, ch.Month) {
			continue
		}
		if ch.Month <= month && ch.Month >= latest {
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

// correctionsFor 返回某收款的全部更正（分配更正与归属更正），按操作序号升序。
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

// paymentOwnerAt 返回某收款在指定操作序号完成后的归属客户标识：以首次登记
// 客户为起点，按序号回放全部归属更正（Reassign），序号不晚于 at 的归属更正
// 生效。分配更正不改变归属。载入时已校验归属更正目标客户存在且互不相同。
func paymentOwnerAt(s *state, p *payment, at int64) string {
	owner := p.CustomerID
	for _, c := range correctionsFor(s, p.ID) {
		if c.Reassign && c.Seq <= at {
			owner = c.TargetCustomerID
		}
	}
	return owner
}

// paymentOwner 返回某收款的当前归属客户标识（全部归属更正生效后）。
func paymentOwner(s *state, p *payment) string {
	return paymentOwnerAt(s, p, s.NextSeq)
}

// allocationsAt 返回某收款在指定操作序号完成后生效的分配：以首次登记分配为
// 起点，按序号回放全部更正（分配更正替换同客户月份，归属更正整笔替换为目标
// 客户的新分配），取序号不晚于 at 的最后一次更正结果。
func allocationsAt(s *state, p *payment, at int64) []paymentAllocation {
	current := p.Allocations
	for _, c := range correctionsFor(s, p.ID) {
		if c.Seq <= at {
			current = c.Allocations
		}
	}
	return current
}

// currentAllocations 返回一笔收款当前生效的分配：无更正时为首次登记的
// 原始分配，否则为序号最大（最新）的更正（分配更正或归属更正）的分配。
// 最新分配所属客户用 paymentOwner 取得，二者必然一致。
func currentAllocations(s *state, p *payment) []paymentAllocation {
	return allocationsAt(s, p, s.NextSeq)
}

// reassignsFor 返回某收款的全部归属更正，按操作序号升序。
func reassignsFor(s *state, paymentID string) []*correction {
	var list []*correction
	for _, c := range s.Corrections {
		if c.PaymentID == paymentID && c.Reassign {
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	return list
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

// paymentsEverFor 返回历史上曾以某客户为归属、且分配曾涉及该客户某月账单的
// 全部收款（首次登记分配或任一次更正——含归属更正转入——涉及该月，即使后来
// 被更正移出或整笔转出到其他客户），按操作序号升序。收款的首次登记客户
// 永久保留在 payment.CustomerID，归属经归属更正变化，故不能只按该字段过滤。
func paymentsEverFor(s *state, customerID, month string) []*payment {
	var list []*payment
	for _, p := range s.Payments {
		// 该收款是否曾归属于该客户：首次登记客户或某次归属更正的目标客户。
		everOwned := p.CustomerID == customerID
		if !everOwned {
			for _, c := range reassignsFor(s, p.ID) {
				if c.TargetCustomerID == customerID {
					everOwned = true
					break
				}
			}
		}
		if !everOwned {
			continue
		}
		involved := false
		// 沿更正链只在归属为该客户期间检查分配是否涉及该月：首次分配（首次
		// 登记客户）或某次以该客户为目标的更正的新分配涉及该月即可。
		if p.CustomerID == customerID && p.amountFor(month) > 0 {
			involved = true
		}
		if !involved {
			for _, c := range s.Corrections {
				if c.PaymentID == p.ID && allocAmountFor(c.Allocations, month) > 0 {
					owner := c.TargetCustomerID
					if !c.Reassign {
						owner = paymentOwnerAt(s, p, c.Seq-1)
					}
					if owner == customerID {
						involved = true
						break
					}
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

// paymentReceived 返回当前归属于指定客户、未撤销的全部收款，按最新分配在
// 指定月份计入并扣除退款后的实收之和。收款可能经归属更正来自其他客户，
// 归属按归属更正时间线取当前值；已整笔转出（当前归属为其他客户）或已撤销
// 的收款不计入。各笔净额（分配减累计退款）均为非负整数，用非负 64 位加法
// 累加，溢出时返回错误。
func paymentReceived(s *state, customerID, month string) (int64, error) {
	var received int64
	for _, p := range s.Payments {
		if p.Revoked || paymentOwner(s, p) != customerID {
			continue
		}
		amt := allocAmountFor(currentAllocations(s, p), month) - refundedForMonth(s, p.ID, month)
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

// refundsFor 返回某收款的全部退款，按操作序号升序。
func refundsFor(s *state, paymentID string) []*refund {
	var list []*refund
	for _, r := range s.Refunds {
		if r.PaymentID == paymentID {
			list = append(list, r)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	return list
}

// refundedForMonth 返回某收款在指定月份的累计退款金额（分）；无退款时为 0。
// 数据在载入时已校验累计不超退，合计不超过该月分配，不会溢出。
func refundedForMonth(s *state, paymentID, month string) int64 {
	var total int64
	for _, r := range s.Refunds {
		if r.PaymentID == paymentID {
			total += allocAmountFor(r.Allocations, month)
		}
	}
	return total
}

// paymentHasRefunds 报告某收款是否已发生退款。首次退款后该收款的最新分配
// 固定，不得再新增分配更正或整笔撤销。
func paymentHasRefunds(s *state, paymentID string) bool {
	for _, r := range s.Refunds {
		if r.PaymentID == paymentID {
			return true
		}
	}
	return false
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
