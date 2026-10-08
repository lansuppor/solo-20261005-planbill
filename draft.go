package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现单客户单 UTC 自然月账期的结算草案：
//
//   - bill draft <草案标识> <客户标识> <YYYY-MM>：按创建时的固定单价或当月
//     有效阶梯方案（含月费）与全部有效用量计价，保存完整计费输入、方案规则、
//     逐条及跨档明细、数量和金额快照。草案不生成账单、不封账、不占账后序号，
//     也不限制后续用量、方案或生命周期操作；不同草案可指向同一客户月份。
//   - bill draft-show <草案标识>：只读查询原快照、待确认/已确认状态及关联账单。
//   - bill draft-confirm <草案标识>：以确认时状态重新计价并与快照逐项比对，
//     全部一致且月份仍未封账、仍可结算时，原子生成正式账单、封账并永久关联。
//
// 以全局草案标识判重：同客户同月份的相同重放返回原快照与当前确认状态，不
// 重新计算、不写盘，后来费用变化或封账也不阻止；客户或月份不同拒绝。快照
// 永久保留、不可改写。待确认草案因后续操作过时是合法状态，确认时拒绝即可。

// cmdBillDraft 创建结算草案。
func cmdBillDraft(dir, draftID, customerID, month string) error {
	if strings.TrimSpace(draftID) == "" {
		return fmt.Errorf("草案标识不能为空")
	}
	if !validMonth(month) {
		return fmt.Errorf("月份 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", month)
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}

	// 以草案标识判重，优先于一切首次校验：同客户同月份的重放即使在后来费用
	// 变化、方案变更或封账之后仍返回原快照与当前确认状态，不重新计算、不写盘；
	// 客户或月份不同（标识复用）拒绝。
	if existing, ok := s.Drafts[draftID]; ok {
		if existing.CustomerID == customerID && existing.Month == month {
			fmt.Fprintf(stdout, "草案 %q 已存在（客户 %s 月份 %s），返回原快照与当前确认状态（不重新计算、不写盘）：\n\n",
				draftID, customerID, month)
			printDraft(s, existing)
			return nil
		}
		return fmt.Errorf("草案标识 %q 已存在但指向不同客户月份（已有：客户=%s 月份=%s），拒绝复用；快照永久保留、不可改写",
			draftID, existing.CustomerID, existing.Month)
	}

	cust, ok := s.Customers[customerID]
	if !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}
	// 创建仅限未封账且可结算的 UTC 自然月：暂停月、终止月（含）起拒绝；
	// 无用量且不因正月费可出账的月份同样拒绝。
	if s.sealed(customerID, month) {
		return fmt.Errorf("客户 %s 的 %s 已封账，不能再创建结算草案", customerID, month)
	}
	if s.isSuspendedMonth(customerID, month) {
		return fmt.Errorf("客户 %s 的 %s 处于暂停区间（%s），暂停服务期间不产生月费账单，不能创建结算草案",
			customerID, month, describeSuspension(s, customerID, month))
	}
	if s.isTerminatedMonth(customerID, month) {
		return fmt.Errorf("客户 %s 的 %s 不早于终止月 %s，订阅已终止：不结算、不封账、不收月费，不能创建结算草案",
			customerID, month, s.Terminations[customerID].Month)
	}

	d, err := buildDraftSnapshot(s, cust, month)
	if err != nil {
		return fmt.Errorf("客户 %s 的 %s 草案计价失败，未创建草案、不占用草案标识: %w", customerID, month, err)
	}
	d.ID = draftID
	d.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	s.Drafts[draftID] = d

	// 快照原子落盘；保存失败则一切不生效，失败创建不占用草案标识。
	if err := s.save(); err != nil {
		delete(s.Drafts, draftID)
		return err
	}
	fmt.Fprintf(stdout, "已创建结算草案 %q（客户 %s 月份 %s）；草案不生成账单、不封账、不占账后序号，可用 bill draft-confirm 确认封账：\n\n",
		draftID, customerID, month)
	printDraft(s, d)
	return nil
}

// cmdBillDraftShow 按草案标识只读查询原快照、确认状态与关联账单；成功失败
// 均不写盘。
func cmdBillDraftShow(dir, draftID string) error {
	if strings.TrimSpace(draftID) == "" {
		return fmt.Errorf("草案标识不能为空")
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	d, ok := s.Drafts[draftID]
	if !ok {
		return fmt.Errorf("草案标识 %q 不存在", draftID)
	}
	printDraft(s, d)
	return nil
}

// cmdBillDraftConfirm 确认草案：首次确认要求目标月份仍未封账、仍可结算，
// 并以确认时状态重新计价，与快照逐项比对方案、月费及规则、有效用量的
// 标识、解析后的时间点和数量、明细与金额；全部一致才生成与快照一致的
// 正式账单并封账，账单、封账与确认状态一次原子保存。月份已由其他草案或
// 原结算命令出账时拒绝，不认领已有账单。已确认草案的相同重放返回关联
// 账单及当前账后状态，不写盘、不重新收费、不占序号。
func cmdBillDraftConfirm(dir, draftID string) error {
	if strings.TrimSpace(draftID) == "" {
		return fmt.Errorf("草案标识不能为空")
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	d, ok := s.Drafts[draftID]
	if !ok {
		return fmt.Errorf("草案标识 %q 不存在", draftID)
	}
	cust, ok := s.Customers[d.CustomerID]
	if !ok {
		// 载入校验已保证客户存在，此处仅作防御。
		return fmt.Errorf("草案 %q 引用的客户 %q 不存在", draftID, d.CustomerID)
	}

	if d.Confirmed {
		// 已确认重放：返回关联账单及当前账后状态，不写盘、不重新收费、
		// 不占序号。
		b := s.Bills[billKey(d.CustomerID, d.Month)] // 载入校验已保证存在
		fmt.Fprintf(stdout, "草案 %q 已确认（确认时间 %s），返回关联账单及当前账后状态（不写盘、不重新收费、不占序号）：\n\n",
			draftID, d.ConfirmedAt)
		printBill(b, cust, s)
		return nil
	}

	month := d.Month
	// 月份已由其他草案确认或原结算命令（含批量结算）出账时拒绝：不认领
	// 已有账单，即使其费用恰好与快照一致。
	if s.sealed(d.CustomerID, month) {
		other := s.Bills[billKey(d.CustomerID, month)]
		return fmt.Errorf("客户 %s 的 %s 已由其他结算出账（账单 %s），草案 %q 确认被拒绝；不认领已有账单，草案保留为待确认",
			d.CustomerID, month, other.ID, draftID)
	}
	if s.isSuspendedMonth(d.CustomerID, month) {
		return fmt.Errorf("客户 %s 的 %s 当前处于暂停区间（%s），月份不再可结算，草案 %q 已过时；草案保留，可排障后核对",
			d.CustomerID, month, describeSuspension(s, d.CustomerID, month), draftID)
	}
	if s.isTerminatedMonth(d.CustomerID, month) {
		return fmt.Errorf("客户 %s 的 %s 当前不早于终止月 %s，订阅已终止、月份不再可结算，草案 %q 已过时；草案保留，可排障后核对",
			d.CustomerID, month, s.Terminations[d.CustomerID].Month, draftID)
	}

	// 以确认时状态重新计价并与快照逐项比对；不能仅比较总额。
	if diffs := draftConfirmDiffs(s, d); len(diffs) > 0 {
		return fmt.Errorf("草案 %q 的计费快照与客户 %s 的 %s 当前状态不一致，确认被拒绝（待确认草案因后续操作过时是合法状态，草案保留不修改）：\n  %s",
			draftID, d.CustomerID, month, strings.Join(diffs, "\n  "))
	}

	// 比对通过：按当前状态生成正式账单，其计费内容与快照一致。
	recs := monthUsage(s, d.CustomerID, month)
	var b *bill
	if d.isTiered() {
		b, err = buildTieredBill(s, cust, month, recs)
	} else {
		b, err = buildFixedBill(cust, month, recs)
	}
	if err != nil {
		return fmt.Errorf("草案 %q 确认时生成账单失败，未确认、未封账: %w", draftID, err)
	}
	if !billMatchesDraft(b, d) {
		// 载入与比对已保证自洽，命中仅说明程序内部不一致，拒绝写入以防万一。
		return fmt.Errorf("草案 %q 重新生成的账单与快照不一致，确认被拒绝；草案保留为待确认", draftID)
	}

	key := billKey(d.CustomerID, month)
	now := time.Now().UTC().Format(time.RFC3339)
	s.Bills[key] = b
	d.Confirmed = true
	d.ConfirmedAt = now
	d.BillID = b.ID

	// 账单、封账与确认状态在同一次原子保存中持久化后才报告成功；保存失败
	// 则不新增账单或封账，草案保留为待确认，可排障后重试。
	if err := s.save(); err != nil {
		delete(s.Bills, key)
		d.Confirmed = false
		d.ConfirmedAt = ""
		d.BillID = ""
		return err
	}
	fmt.Fprintf(stdout, "草案 %q 已确认：客户 %s 的 %s 生成正式账单 %s 并封账（账单、封账与确认状态一次原子保存；不占账后序号）：\n\n",
		draftID, d.CustomerID, month, b.ID)
	printBill(b, cust, s)
	return nil
}

// buildDraftSnapshot 按当前库状态为指定客户月份构造不落盘的草案快照：
// 归集该 UTC 自然月的全部有效（未撤回）用量，固定单价客户逐条 数量×单价，
// 阶梯客户按当月有效方案从零累计分档（含月费与跨档明细）。无用量且不因
// 正月费可出账，或计价溢出时返回错误。
func buildDraftSnapshot(s *state, cust *customer, month string) (*settlementDraft, error) {
	recs := monthUsage(s, cust.ID, month)
	if len(recs) == 0 && !monthlyFeeBillable(s, cust, month) {
		return nil, fmt.Errorf("该 UTC 自然月没有用量")
	}
	d := &settlementDraft{
		CustomerID: cust.ID,
		Month:      month,
		UsageRefs:  make([]draftUsageRef, 0, len(recs)),
		Lines:      make([]billLine, 0, len(recs)),
	}
	for _, u := range recs {
		d.UsageRefs = append(d.UsageRefs, draftUsageRef{UsageID: u.ID, Time: u.Time, Quantity: u.Quantity})
	}
	if cust.PlanID != "" {
		b, err := buildTieredBill(s, cust, month, recs)
		if err != nil {
			return nil, err
		}
		d.Pricing = "tiered"
		d.PlanID = b.PlanID
		d.PlanName = b.PlanName
		d.PlanTiers = b.PlanTiers
		d.MonthlyFee = b.MonthlyFee
		d.TierTotals = b.TierTotals
		d.Lines = b.Lines
		d.TotalQty = b.TotalQty
		d.UnitPrice = 0
		d.TotalFee = b.TotalFee
		return d, nil
	}
	b, err := buildFixedBill(cust, month, recs)
	if err != nil {
		return nil, err
	}
	d.Pricing = "fixed"
	d.UnitPrice = b.UnitPrice
	d.Lines = b.Lines
	d.TotalQty = b.TotalQty
	d.TotalFee = b.TotalFee
	return d, nil
}

func (d *settlementDraft) isTiered() bool {
	return d.Pricing == "tiered"
}

// draftConfirmDiffs 以确认时状态与快照逐项比对，返回所有不一致的人类可读
// 说明；空切片表示完全一致。比较范围仅限计费内容：当月有效方案、月费及
// 规则、有效用量的标识、解析后的时间点和数量、重新计价的明细与金额；
// 不比较操作时间或存档版本，其他月份与纯账后变化不在此列。
func draftConfirmDiffs(s *state, d *settlementDraft) []string {
	var diffs []string
	cust := s.Customers[d.CustomerID] // 调用前已确认存在
	recs := monthUsage(s, d.CustomerID, d.Month)

	// 有效用量的标识、解析后的时间点和数量必须逐条一致（含计价顺序与条数）。
	if len(recs) != len(d.UsageRefs) {
		diffs = append(diffs, fmt.Sprintf("有效用量条数变化：快照 %d 条，当前 %d 条", len(d.UsageRefs), len(recs)))
	} else {
		for i, ref := range d.UsageRefs {
			u := recs[i]
			if u.ID != ref.UsageID {
				diffs = append(diffs, fmt.Sprintf("第 %d 条用量标识变化：快照 %q，当前 %q", i+1, ref.UsageID, u.ID))
				continue
			}
			if u.Quantity != ref.Quantity {
				diffs = append(diffs, fmt.Sprintf("用量 %q 数量变化：快照 %d，当前 %d", ref.UsageID, ref.Quantity, u.Quantity))
			}
			st, err1 := time.Parse(time.RFC3339, ref.Time)
			ct, err2 := time.Parse(time.RFC3339, u.Time)
			if err1 != nil || err2 != nil || !ct.Equal(st) {
				diffs = append(diffs, fmt.Sprintf("用量 %q 解析后的时间点变化：快照 %s，当前 %s", ref.UsageID, ref.Time, u.Time))
			}
		}
	}

	// 当月有效方案、月费及规则（固定单价客户比较固定单价）必须一致。
	if d.isTiered() {
		if cust.PlanID == "" {
			diffs = append(diffs, "客户当前为固定单价客户，快照为阶梯计费草案")
		} else {
			curPlanID := s.effectivePlanID(cust, d.Month)
			if curPlanID != d.PlanID {
				diffs = append(diffs, fmt.Sprintf("%s 月有效方案变化：快照 %q，当前 %q", d.Month, d.PlanID, curPlanID))
			}
			if p, ok := s.Plans[curPlanID]; ok {
				if p.Name != d.PlanName {
					diffs = append(diffs, fmt.Sprintf("方案 %q 名称变化：快照 %q，当前 %q", d.PlanID, d.PlanName, p.Name))
				}
				if !tiersEqual(p.Tiers, d.PlanTiers) {
					diffs = append(diffs, fmt.Sprintf("方案 %q 的阶梯规则变化：快照 %s，当前 %s", d.PlanID, formatTiers(d.PlanTiers), formatTiers(p.Tiers)))
				}
				if p.MonthlyFee != d.MonthlyFee {
					diffs = append(diffs, fmt.Sprintf("方案 %q 的月费变化：快照 %d 分，当前 %d 分", d.PlanID, d.MonthlyFee, p.MonthlyFee))
				}
			}
		}
	} else {
		if cust.PlanID != "" {
			diffs = append(diffs, "客户当前为阶梯计费客户，快照为固定单价草案")
		} else if cust.Price != d.UnitPrice {
			diffs = append(diffs, fmt.Sprintf("固定单价变化：快照 %d 分，当前 %d 分", d.UnitPrice, cust.Price))
		}
	}

	// 以确认时状态重新计价，明细与金额必须与快照完全一致，不能仅比较总额。
	var b *bill
	var err error
	if d.isTiered() {
		b, err = buildTieredBill(s, cust, d.Month, recs)
	} else {
		b, err = buildFixedBill(cust, d.Month, recs)
	}
	if err != nil {
		diffs = append(diffs, fmt.Sprintf("按当前状态重新计价失败：%v", err))
		return diffs
	}
	for _, msg := range draftBillDiffs(d, b) {
		diffs = append(diffs, msg)
	}
	return diffs
}

// draftBillDiffs 比较重新计价的账单与快照的数量、逐条及跨档明细和金额。
func draftBillDiffs(d *settlementDraft, b *bill) []string {
	var diffs []string
	if b.TotalQty != d.TotalQty {
		diffs = append(diffs, fmt.Sprintf("总数量变化：快照 %d，重新计价 %d", d.TotalQty, b.TotalQty))
	}
	if b.TotalFee != d.TotalFee {
		diffs = append(diffs, fmt.Sprintf("原总金额变化：快照 %d 分，重新计价 %d 分", d.TotalFee, b.TotalFee))
	}
	if d.isTiered() {
		if b.MonthlyFee != d.MonthlyFee {
			diffs = append(diffs, fmt.Sprintf("月费变化：快照 %d 分，重新计价 %d 分", d.MonthlyFee, b.MonthlyFee))
		}
		if len(b.TierTotals) != len(d.TierTotals) {
			diffs = append(diffs, "分档合计档数变化")
		} else {
			for i := range d.TierTotals {
				if b.TierTotals[i] != d.TierTotals[i] {
					diffs = append(diffs, fmt.Sprintf("第 %d 档合计变化：快照 数量=%d 金额=%d 分，重新计价 数量=%d 金额=%d 分",
						i+1, d.TierTotals[i].Quantity, d.TierTotals[i].Fee, b.TierTotals[i].Quantity, b.TierTotals[i].Fee))
				}
			}
		}
	}
	if len(b.Lines) != len(d.Lines) {
		diffs = append(diffs, fmt.Sprintf("逐条明细条数变化：快照 %d 条，重新计价 %d 条", len(d.Lines), len(b.Lines)))
		return diffs
	}
	for i := range d.Lines {
		want, got := d.Lines[i], b.Lines[i]
		if want.UsageID != got.UsageID || want.Time != got.Time || want.Quantity != got.Quantity ||
			want.LineFee != got.LineFee || !segmentsEqual(want.Segments, got.Segments) {
			diffs = append(diffs, fmt.Sprintf("第 %d 条明细变化：快照 标识=%s 时间=%s 数量=%d 小计=%d 分（%d 段），重新计价 标识=%s 时间=%s 数量=%d 小计=%d 分（%d 段）",
				i+1, want.UsageID, want.Time, want.Quantity, want.LineFee, len(want.Segments),
				got.UsageID, got.Time, got.Quantity, got.LineFee, len(got.Segments)))
		}
	}
	return diffs
}

// billMatchesDraft 判定正式账单的计费快照与草案一致。
func billMatchesDraft(b *bill, d *settlementDraft) bool {
	return b.CustomerID == d.CustomerID && b.Month == d.Month &&
		b.TotalQty == d.TotalQty && b.TotalFee == d.TotalFee &&
		b.UnitPrice == d.UnitPrice && b.MonthlyFee == d.MonthlyFee &&
		b.PlanID == d.PlanID && b.PlanName == d.PlanName &&
		tiersEqual(b.PlanTiers, d.PlanTiers) &&
		tierTotalsEqual(b.TierTotals, d.TierTotals) &&
		billLinesEqual(b.Lines, d.Lines)
}

func tierTotalsEqual(a, b []tierTotal) bool {
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

func billLinesEqual(a, b []billLine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].UsageID != b[i].UsageID || a[i].Time != b[i].Time ||
			a[i].Quantity != b[i].Quantity || a[i].LineFee != b[i].LineFee ||
			!segmentsEqual(a[i].Segments, b[i].Segments) {
			return false
		}
	}
	return true
}

// validateDrafts 是载入校验的一部分：草案引用不得缺失（客户、方案、用量），
// 快照计价必须按保存的规则自洽，已确认草案关联账单的客户、月份及计费
// 快照必须与正式账单一致。待确认草案因后续用量、方案或生命周期操作过时
// 是合法状态（包括快照方案不再是当月有效方案、快照用量已撤回或月份已由
// 其他途径封账），不在此处判错。
func (s *state) validateDrafts() error {
	ids := make([]string, 0, len(s.Drafts))
	for id := range s.Drafts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := s.Drafts[id]
		if d == nil {
			return fmt.Errorf("结算草案 %q 的数据为空", id)
		}
		if d.ID != id {
			return fmt.Errorf("结算草案标识不一致: 键 %q / 记录 %q", id, d.ID)
		}
		if strings.TrimSpace(d.ID) == "" {
			return errors.New("存在空的结算草案标识")
		}
		cust, ok := s.Customers[d.CustomerID]
		if !ok {
			return fmt.Errorf("结算草案 %q 引用了不存在的客户 %q（草案引用缺失）", id, d.CustomerID)
		}
		if !validMonth(d.Month) {
			return fmt.Errorf("结算草案 %q 的月份无效（必须是 YYYY-MM）", id)
		}
		if _, err := time.Parse(time.RFC3339, d.CreatedAt); err != nil {
			return fmt.Errorf("结算草案 %q 的创建时间不是 RFC3339: %w", id, err)
		}

		// 计费输入快照：每条引用的用量必须存在、归属本客户、在账期内，
		// 且内容（解析后的时间点与数量）与记录一致；用量记录不可改写，
		// 不一致即存档损坏。引用顺序必须仍是计价顺序。
		refRecs := make([]*usageRecord, len(d.UsageRefs))
		for i, ref := range d.UsageRefs {
			if ref.UsageID == "" {
				return fmt.Errorf("结算草案 %q 的第 %d 条计费输入缺少用量标识", id, i+1)
			}
			u, ok := s.Usage[ref.UsageID]
			if !ok {
				return fmt.Errorf("结算草案 %q 引用了不存在的用量 %q（草案引用缺失）", id, ref.UsageID)
			}
			if u.CustomerID != d.CustomerID {
				return fmt.Errorf("结算草案 %q 的用量 %q 不属于客户 %s", id, ref.UsageID, d.CustomerID)
			}
			if !inMonth(u.Time, d.Month) {
				return fmt.Errorf("结算草案 %q 的用量 %q 不在账期 %s 内", id, ref.UsageID, d.Month)
			}
			if u.Quantity != ref.Quantity {
				return fmt.Errorf("结算草案 %q 的用量 %q 数量快照与记录不一致（快照 %d，记录 %d）",
					id, ref.UsageID, ref.Quantity, u.Quantity)
			}
			st, err1 := time.Parse(time.RFC3339, ref.Time)
			if err1 != nil {
				return fmt.Errorf("结算草案 %q 的用量 %q 快照时间不是 RFC3339: %w", id, ref.UsageID, err1)
			}
			ct, _ := time.Parse(time.RFC3339, u.Time)
			if !ct.Equal(st) {
				return fmt.Errorf("结算草案 %q 的用量 %q 时间快照与记录不一致（快照 %s，记录 %s）",
					id, ref.UsageID, ref.Time, u.Time)
			}
			refRecs[i] = u
		}
		if !sortedByInstant(refRecs) {
			return fmt.Errorf("结算草案 %q 的计费输入未按计价顺序（时间点升序、同一时间按标识字典序）排列", id)
		}

		// 快照计价自洽性：按快照保存的方案规则/固定单价与计费输入重新计价，
		// 逐条分段、小计、各档合计、数量与金额必须全部复现。
		switch d.Pricing {
		case "", "fixed":
			if d.PlanID != "" || d.PlanName != "" || len(d.PlanTiers) > 0 || len(d.TierTotals) > 0 {
				return fmt.Errorf("结算草案 %q 是固定单价草案但携带阶梯方案信息", id)
			}
			if d.MonthlyFee != 0 {
				return fmt.Errorf("结算草案 %q 是固定单价草案但携带月费", id)
			}
			if cust.PlanID != "" {
				return fmt.Errorf("结算草案 %q 是固定单价草案但客户 %q 绑定了阶梯方案", id, d.CustomerID)
			}
			if d.UnitPrice != cust.Price {
				return fmt.Errorf("结算草案 %q 的固定单价快照 %d 分与客户单价 %d 分不符", id, d.UnitPrice, cust.Price)
			}
			if len(refRecs) == 0 {
				return fmt.Errorf("结算草案 %q 是固定单价草案但没有计费输入（无用量的固定单价月份不可创建草案）", id)
			}
			b, err := buildFixedBill(cust, d.Month, refRecs)
			if err != nil {
				return fmt.Errorf("结算草案 %q 按快照计价不自洽: %w", id, err)
			}
			if b.UnitPrice != d.UnitPrice || b.TotalQty != d.TotalQty || b.TotalFee != d.TotalFee ||
				!billLinesEqual(b.Lines, d.Lines) {
				return fmt.Errorf("结算草案 %q 的固定单价明细、数量或金额快照不自洽", id)
			}
		case "tiered":
			if cust.PlanID == "" {
				return fmt.Errorf("结算草案 %q 是阶梯草案但客户 %q 未绑定阶梯方案", id, d.CustomerID)
			}
			p, ok := s.Plans[d.PlanID]
			if !ok {
				return fmt.Errorf("结算草案 %q 引用了不存在的方案 %q（草案引用缺失）", id, d.PlanID)
			}
			if d.PlanName != p.Name || !tiersEqual(d.PlanTiers, p.Tiers) || d.MonthlyFee != p.MonthlyFee {
				return fmt.Errorf("结算草案 %q 的方案规则快照与方案 %q 不符", id, d.PlanID)
			}
			// 无计费输入只允许出现在正月费方案（仅月费草案）；待确认草案过时
			// 合法，但快照自身必须可由其保存的规则与输入复算。
			if len(refRecs) == 0 && d.MonthlyFee == 0 {
				return fmt.Errorf("结算草案 %q 没有计费输入且月费为 0（无用量的零月费月份不可创建草案）", id)
			}
			priced, err := tieredPrice(d.PlanTiers, refRecs)
			if err != nil {
				return fmt.Errorf("结算草案 %q 按快照规则计价失败: %w", id, err)
			}
			wantTotal, err := add64(priced.totalFee, d.MonthlyFee)
			if err != nil {
				return fmt.Errorf("结算草案 %q 月费加用量费溢出，快照不自洽", id)
			}
			if d.TotalQty != priced.totalQty || d.TotalFee != wantTotal {
				return fmt.Errorf("结算草案 %q 的数量或总金额快照不自洽", id)
			}
			if len(d.TierTotals) != len(priced.tierQty) {
				return fmt.Errorf("结算草案 %q 的分档合计档数不自洽", id)
			}
			for i := range priced.tierQty {
				if d.TierTotals[i].Quantity != priced.tierQty[i] || d.TierTotals[i].Fee != priced.tierFee[i] {
					return fmt.Errorf("结算草案 %q 的第 %d 档合计快照不自洽", id, i+1)
				}
			}
			if len(d.Lines) != len(priced.lines) {
				return fmt.Errorf("结算草案 %q 的逐条明细条数不自洽", id)
			}
			for i, pl := range priced.lines {
				if d.Lines[i].UsageID != d.UsageRefs[i].UsageID || d.Lines[i].Time != d.UsageRefs[i].Time ||
					d.Lines[i].Quantity != d.UsageRefs[i].Quantity || d.Lines[i].LineFee != pl.fee ||
					!segmentsEqual(d.Lines[i].Segments, pl.segments) {
					return fmt.Errorf("结算草案 %q 的第 %d 条明细快照不自洽", id, i+1)
				}
			}
		default:
			return fmt.Errorf("结算草案 %q 的计价类型 %q 未知", id, d.Pricing)
		}

		// 确认状态与关联账单：待确认不得携带关联；已确认必须关联到客户月份
		// 一致、标识相同且计费快照一致的正式账单。
		if !d.Confirmed {
			if d.BillID != "" || d.ConfirmedAt != "" {
				return fmt.Errorf("结算草案 %q 未确认但携带账单关联或确认时间", id)
			}
			continue
		}
		if d.BillID == "" {
			return fmt.Errorf("结算草案 %q 已确认但缺少关联账单标识", id)
		}
		if _, err := time.Parse(time.RFC3339, d.ConfirmedAt); err != nil {
			return fmt.Errorf("结算草案 %q 的确认时间不是 RFC3339: %w", id, err)
		}
		b, ok := s.Bills[billKey(d.CustomerID, d.Month)]
		if !ok {
			return fmt.Errorf("已确认结算草案 %q 关联的账单（客户 %s 月份 %s）不存在（草案引用缺失）",
				id, d.CustomerID, d.Month)
		}
		if b.ID != d.BillID {
			return fmt.Errorf("已确认结算草案 %q 关联账单标识 %q 与客户 %s 月份 %s 的账单 %q 不符",
				id, d.BillID, d.CustomerID, d.Month, b.ID)
		}
		if !billMatchesDraft(b, d) {
			return fmt.Errorf("已确认结算草案 %q 的客户、月份及计费快照与关联账单 %q 不一致", id, b.ID)
		}
	}
	return nil
}

// printDraft 输出草案原快照、确认状态及关联账单（只读展示）。
func printDraft(s *state, d *settlementDraft) {
	cust := s.Customers[d.CustomerID]
	fmt.Fprintf(stdout, "草案标识：%s\n", d.ID)
	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "月份：%s（UTC 自然月，左闭右开）\n", d.Month)
	fmt.Fprintf(stdout, "创建时间：%s\n", d.CreatedAt)
	if d.Confirmed {
		fmt.Fprintf(stdout, "状态：已确认（确认时间 %s）\n", d.ConfirmedAt)
		fmt.Fprintf(stdout, "关联账单：%s\n", d.BillID)
	} else {
		fmt.Fprintln(stdout, "状态：待确认（快照永久保留、不可改写；草案不生成账单、不封账、不占账后序号）")
	}
	if d.isTiered() {
		fmt.Fprintln(stdout, "计价类型：阶梯计费（按 UTC 自然月累计用量分档计价，每月从零累计；快照为创建时当月有效方案）")
		fmt.Fprintf(stdout, "方案：%s（%s）\n", d.PlanID, d.PlanName)
		fmt.Fprintln(stdout, "阶梯规则：")
		for i, t := range d.PlanTiers {
			if t.Limit == 0 {
				fmt.Fprintf(stdout, "  第 %d 档：累计数量无上限，单价 %d 分（%s）\n", i+1, t.Price, moneyFen(t.Price))
			} else {
				fmt.Fprintf(stdout, "  第 %d 档：累计上限 %d，单价 %d 分（%s）\n", i+1, t.Limit, t.Price, moneyFen(t.Price))
			}
		}
		fmt.Fprintf(stdout, "总数量：%d\n", d.TotalQty)
		usageFee := d.TotalFee - d.MonthlyFee
		fmt.Fprintf(stdout, "月费：%d 分（%s，整月收取，不按天折算）\n", d.MonthlyFee, moneyFen(d.MonthlyFee))
		fmt.Fprintf(stdout, "用量费：%d 分（%s）\n", usageFee, moneyFen(usageFee))
		fmt.Fprintf(stdout, "原总金额：%d 分（%s，月费加全月用量费）\n", d.TotalFee, moneyFen(d.TotalFee))
		if len(d.TierTotals) > 0 {
			fmt.Fprintln(stdout, "各档合计：")
			for i, tt := range d.TierTotals {
				fmt.Fprintf(stdout, "  第 %d 档：数量 %d，单价 %d 分，金额 %d 分（%s）\n",
					i+1, tt.Quantity, d.PlanTiers[i].Price, tt.Fee, moneyFen(tt.Fee))
			}
		}
	} else {
		fmt.Fprintln(stdout, "计价类型：固定单价")
		fmt.Fprintf(stdout, "单价：%d 分（%s）\n", d.UnitPrice, moneyFen(d.UnitPrice))
		fmt.Fprintf(stdout, "总数量：%d\n", d.TotalQty)
		fmt.Fprintf(stdout, "总金额：%d 分（%s）\n", d.TotalFee, moneyFen(d.TotalFee))
	}
	if len(d.Lines) == 0 {
		fmt.Fprintln(stdout, "明细：无（本账期无用量，仅收取月费）")
	} else {
		fmt.Fprintln(stdout, "明细（创建时计费输入快照，按计价顺序）：")
	}
	for i, ln := range d.Lines {
		fmt.Fprintf(stdout, "  %d. 用量标识=%s 时间=%s 数量=%d 小计=%d 分（%s）\n",
			i+1, ln.UsageID, ln.Time, ln.Quantity, ln.LineFee, moneyFen(ln.LineFee))
		for j, seg := range ln.Segments {
			fmt.Fprintf(stdout, "     分段 %d：第 %d 档 数量=%d 单价=%d 分 小计=%d 分（%s）\n",
				j+1, seg.Tier+1, seg.Quantity, d.PlanTiers[seg.Tier].Price, seg.Fee, moneyFen(seg.Fee))
		}
	}
	if d.Confirmed {
		if b, ok := s.Bills[billKey(d.CustomerID, d.Month)]; ok {
			_, payable, _ := billTotals(b, adjustmentsFor(s, b.CustomerID, b.Month))
			received, _ := paymentReceived(s, b.CustomerID, b.Month)
			fmt.Fprintf(stdout, "关联账单当前账后状态：当前应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
				payable, moneyFen(payable), received, moneyFen(received), payable-received, moneyFen(payable-received))
		}
	} else {
		fmt.Fprintln(stdout, "提示：确认前若费用、方案、月费、规则或有效用量发生变化，确认将被拒绝；草案不会因过时而修改或删除。")
	}
}
