package main

import (
	"fmt"
	"strings"
	"time"
)

// 本文件实现单客户单账期的结算草案：创建（bill draft）只做只读计价核对并
// 永久保存完整计费输入与计费快照，不生成账单、不封账、不占账后序号，也不
// 限制后续用量、方案或生命周期操作；只读查询（bill draft-show）展示原快照、
// 待确认或已确认状态及关联账单；确认（bill draft-confirm）按确认当时状态
// 逐项（而非仅总额）核对快照，全部一致且月份仍未封账、仍可结算时，才把与
// 快照一致的正式账单与封账状态、草案确认状态一次原子保存。
//
// 草案以全局标识判重：同客户同月份重放返回原快照与当前确认状态，不重新
// 计算、不写盘，后来费用变化或封账也不阻止；客户或月份不同拒绝。不同草案
// 可指向同一客户月份。快照永久保留、不可改写；待确认草案因后续操作过时是
// 合法状态，确认时按逐项核对拒绝即可。

// buildDraftSnapshot 按当前状态为某客户某 UTC 自然月构造完整计费快照（不落
// 盘）：调用与正式结算完全相同的计价路径（固定单价或当月有效阶梯方案、
// 月费、暂停与终止、无用量与溢出规则），再把账单内容与参与计价的用量输入
// 一并保存。recs 为 monthUsage 的归集结果（计价顺序）。
func buildDraftSnapshot(s *state, cust *customer, month string, recs []*usageRecord) (*settlementDraft, error) {
	b, err := buildBill(s, cust, month)
	if err != nil {
		return nil, err
	}
	inputs := make([]draftUsageInput, len(recs))
	for i, u := range recs {
		inputs[i] = draftUsageInput{UsageID: u.ID, Time: u.Time, Quantity: u.Quantity}
	}
	lines := make([]billLine, len(b.Lines))
	copy(lines, b.Lines)
	pricing := b.Pricing
	if pricing == "" {
		pricing = "fixed"
	}
	snap := draftBillSnapshot{
		Pricing:    pricing,
		UnitPrice:  b.UnitPrice,
		PlanID:     b.PlanID,
		PlanName:   b.PlanName,
		PlanTiers:  b.PlanTiers,
		TierTotals: b.TierTotals,
		MonthlyFee: b.MonthlyFee,
		TotalQty:   b.TotalQty,
		UsageFee:   b.TotalFee - b.MonthlyFee, // 总金额与月费均非负，相减不溢出
		TotalFee:   b.TotalFee,
		Lines:      lines,
	}
	return &settlementDraft{
		CustomerID: cust.ID,
		Month:      month,
		Inputs:     inputs,
		Snapshot:   snap,
	}, nil
}

func cmdBillDraft(dir, draftID, customerID, month string) error {
	if strings.TrimSpace(draftID) == "" {
		return fmt.Errorf("草案标识不能为空")
	}
	if strings.TrimSpace(customerID) == "" {
		return fmt.Errorf("客户标识不能为空")
	}
	if !validMonth(month) {
		return fmt.Errorf("月份 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", month)
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}

	// 以草案标识判重：同客户同月份重放返回原快照和当前确认状态，不重新
	// 计算、不写盘——即使后来费用变化或月份已封账也成功；客户或月份不同
	// 拒绝。快照永久保留、不可改写。
	if d, exists := s.Drafts[draftID]; exists {
		if d.CustomerID != customerID || d.Month != month {
			return fmt.Errorf("草案标识 %q 已存在但内容不同（已有客户 %s 月份 %s），草案快照不可改写，收到客户 %s 月份 %s",
				draftID, d.CustomerID, d.Month, customerID, month)
		}
		fmt.Fprintf(stdout, "草案 %q 已存在，返回原计费快照（幂等，不重新计算、不写盘）：\n\n", draftID)
		printDraft(d, s)
		return nil
	}

	cust, ok := s.Customers[customerID]
	if !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}
	// 草案仅限未封账月份：已由原结算命令或其他草案确认出账的月份不得再建
	// 草案（草案本身不封账，待确认草案不阻止其他草案创建，但账单存在即
	// 已封账）。
	if s.sealed(customerID, month) {
		return fmt.Errorf("客户 %s 的 %s 已封账（账单 %s），不能为已封账月份创建结算草案",
			customerID, month, s.Bills[billKey(customerID, month)].ID)
	}
	// 可结算性与正式结算完全一致：暂停月不出账，终止月（含）起不出账。
	if s.isSuspendedMonth(customerID, month) {
		return fmt.Errorf("客户 %s 的 %s 处于暂停区间，暂停服务期间不产生月费账单，不能创建结算草案", customerID, month)
	}
	if s.isTerminatedMonth(customerID, month) {
		return fmt.Errorf("客户 %s 的 %s 不早于终止月 %s，订阅已终止：不结算、不封账、不收月费，不能创建结算草案",
			customerID, month, s.Terminations[customerID].Month)
	}

	recs := monthUsage(s, customerID, month)
	if len(recs) == 0 && !monthlyFeeBillable(s, cust, month) {
		return fmt.Errorf("客户 %s 在 %s 没有用量且当月有效方案无月费，不可结算，不能创建结算草案", customerID, month)
	}
	d, err := buildDraftSnapshot(s, cust, month, recs)
	if err != nil {
		return fmt.Errorf("客户 %s 的 %s 计价失败，不能创建结算草案: %w", customerID, month, err)
	}
	d.ID = draftID
	d.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	s.Drafts[draftID] = d

	// 草案整体原子落盘；保存失败不占用草案标识，全部业务状态不变。
	if err := s.save(); err != nil {
		delete(s.Drafts, draftID)
		return err
	}
	fmt.Fprintf(stdout, "已创建结算草案 %q（客户 %s，账期 %s）：草案不生成账单、不封账、不占账后序号，也不限制后续用量、方案或生命周期操作；核对无误后可用 bill draft-confirm %s 确认封账。\n\n",
		draftID, customerID, month, draftID)
	printDraft(d, s)
	return nil
}

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
		return fmt.Errorf("结算草案 %q 不存在", draftID)
	}
	// 只读查询：成功或失败均不写盘。
	printDraft(d, s)
	return nil
}

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
		return fmt.Errorf("结算草案 %q 不存在", draftID)
	}

	// 已确认草案的确认重放：返回关联账单及当前账后状态，不写盘、不重新
	// 收费、不占序号。载入时已校验关联账单存在且与快照一致。
	if d.Confirmed {
		b := s.Bills[billKey(d.CustomerID, d.Month)]
		cust := s.Customers[d.CustomerID]
		fmt.Fprintf(stdout, "草案 %q 已确认，返回关联账单（幂等，不重新收费、不写盘、不占序号）：\n\n", draftID)
		printBill(b, cust, s)
		return nil
	}

	cust := s.Customers[d.CustomerID] // 载入时已校验客户存在
	key := billKey(d.CustomerID, d.Month)
	// 月份已由其他草案确认或原结算命令（含批量结算）出账时拒绝，不认领
	// 已有账单。
	if existing, sealed := s.Bills[key]; sealed {
		return fmt.Errorf("草案 %q 的目标月份 %s 的 %s 已由其他结算出账封账（账单 %s），拒绝确认且不认领已有账单；原草案保留为待确认，可排障后处理",
			draftID, d.CustomerID, d.Month, existing.ID)
	}
	// 首次确认要求目标月份仍可结算：暂停或终止后的月份拒绝（草案保留）。
	if s.isSuspendedMonth(d.CustomerID, d.Month) {
		return fmt.Errorf("草案 %q 的目标月份 %s 的 %s 现已处于暂停区间，与快照时状态不一致，拒绝确认；待确认草案保留，可排障后重试",
			draftID, d.CustomerID, d.Month)
	}
	if s.isTerminatedMonth(d.CustomerID, d.Month) {
		return fmt.Errorf("草案 %q 的目标月份 %s 的 %s 现已不早于终止月，与快照时状态不一致，拒绝确认；待确认草案保留，可排障后重试",
			draftID, d.CustomerID, d.Month)
	}

	// 以确认时状态重新归集用量并重新计价：有效用量的标识、解析后的时间点
	// 和数量，当月有效方案、月费及规则，重新计价的逐条、跨档明细与金额，
	// 必须全部与快照一致，不能仅比较总额。只比较这些计费内容，不比较操作
	// 时间或存档整体版本；其他月份和纯账后变化不阻塞。
	recs := monthUsage(s, d.CustomerID, d.Month)
	if len(recs) == 0 && !monthlyFeeBillable(s, cust, d.Month) {
		return fmt.Errorf("草案 %q 已过时：客户 %s 的 %s 当前没有用量且当月有效方案无月费，不再可结算，拒绝确认；待确认草案保留",
			draftID, d.CustomerID, d.Month)
	}
	current, err := buildBill(s, cust, d.Month)
	if err != nil {
		return fmt.Errorf("草案 %q 按确认时状态重新计价失败，拒绝确认；待确认草案保留: %w", draftID, err)
	}
	var mismatches []string
	mismatches = append(mismatches, compareDraftInputs(d, recs)...)
	mismatches = append(mismatches, compareDraftBilling(d, current)...)
	if len(mismatches) > 0 {
		return fmt.Errorf("草案 %q 已过时：确认时计费内容与创建快照不一致（待确认草案保留，不新增账单或封账，可排障后重试）：\n  %s",
			draftID, strings.Join(mismatches, "\n  "))
	}

	// 全部一致：正式账单、封账状态与草案确认状态在同一次保存中原子持久化，
	// 完整保存成功后才报告成功。
	d.Confirmed = true
	d.BillID = current.ID
	d.ConfirmedAt = time.Now().UTC().Format(time.RFC3339)
	s.Bills[key] = current
	if err := s.save(); err != nil {
		delete(s.Bills, key)
		d.Confirmed = false
		d.BillID = ""
		d.ConfirmedAt = ""
		return fmt.Errorf("草案确认保存失败，账单与封账均未生效，待确认草案保留: %w", err)
	}
	fmt.Fprintf(stdout, "草案 %q 已确认：客户 %s 的 %s 正式出账并封账，账单与封账状态已整体原子保存（不占账后序号）。\n\n",
		draftID, d.CustomerID, d.Month)
	printBill(current, cust, s)
	return nil
}

// compareDraftInputs 比较快照计费输入与确认时归集的有效用量：标识、解析后
// 的时间点（同一瞬间的不同写法视为相同）与数量，且条数与计价顺序必须一致。
func compareDraftInputs(d *settlementDraft, recs []*usageRecord) []string {
	var problems []string
	if len(recs) != len(d.Inputs) {
		problems = append(problems, fmt.Sprintf("有效用量条数变化：快照 %d 条，确认时 %d 条", len(d.Inputs), len(recs)))
	}
	n := len(recs)
	if len(d.Inputs) < n {
		n = len(d.Inputs)
	}
	for i := 0; i < n; i++ {
		want, got := d.Inputs[i], recs[i]
		if want.UsageID != got.ID {
			problems = append(problems, fmt.Sprintf("第 %d 条用量标识变化：快照 %q，确认时 %q", i+1, want.UsageID, got.ID))
		}
		if want.Quantity != got.Quantity {
			problems = append(problems, fmt.Sprintf("用量 %q 数量变化：快照 %d，确认时 %d", want.UsageID, want.Quantity, got.Quantity))
		}
		wt, errW := time.Parse(time.RFC3339, want.Time)
		gt, errG := time.Parse(time.RFC3339, got.Time)
		if errW != nil || errG != nil || !wt.Equal(gt) {
			problems = append(problems, fmt.Sprintf("用量 %q 的时间点变化：快照 %s，确认时 %s", want.UsageID, want.Time, got.Time))
		}
	}
	return problems
}

// effectivePricing 归一化账单计价类型：空字符串与 fixed 均为固定单价。
func effectivePricing(p string) string {
	if p == "" {
		return "fixed"
	}
	return p
}

// compareDraftBilling 比较重新计价的账单与快照的全部计费内容：计价类型、
// 固定单价或当月有效方案（标识、名称、月费、完整规则）、逐条小计与跨档
// 分段、各档合计、总数量、用量费与原总金额。任何差异都逐条列出。
func compareDraftBilling(d *settlementDraft, b *bill) []string {
	snap := &d.Snapshot
	var problems []string
	if effectivePricing(b.Pricing) != snap.Pricing {
		problems = append(problems, fmt.Sprintf("计价类型变化：快照 %q，确认时 %q", snap.Pricing, effectivePricing(b.Pricing)))
	}
	if b.UnitPrice != snap.UnitPrice {
		problems = append(problems, fmt.Sprintf("固定单价变化：快照 %d 分，确认时 %d 分", snap.UnitPrice, b.UnitPrice))
	}
	if b.PlanID != snap.PlanID {
		problems = append(problems, fmt.Sprintf("当月有效方案变化：快照 %q，确认时 %q", snap.PlanID, b.PlanID))
	}
	if b.PlanName != snap.PlanName {
		problems = append(problems, fmt.Sprintf("方案名称变化：快照 %q，确认时 %q", snap.PlanName, b.PlanName))
	}
	if !tiersEqual(b.PlanTiers, snap.PlanTiers) {
		problems = append(problems, fmt.Sprintf("方案 %q 的阶梯规则变化：快照 %s，确认时 %s",
			snap.PlanID, formatTiers(snap.PlanTiers), formatTiers(b.PlanTiers)))
	}
	if b.MonthlyFee != snap.MonthlyFee {
		problems = append(problems, fmt.Sprintf("月费变化：快照 %d 分，确认时 %d 分", snap.MonthlyFee, b.MonthlyFee))
	}
	if b.TotalQty != snap.TotalQty {
		problems = append(problems, fmt.Sprintf("总数量变化：快照 %d，确认时 %d", snap.TotalQty, b.TotalQty))
	}
	if b.TotalFee != snap.TotalFee {
		problems = append(problems, fmt.Sprintf("原总金额变化：快照 %d 分，确认时 %d 分（不能仅比较总额，须全部计费内容一致）", snap.TotalFee, b.TotalFee))
	}
	if b.TotalFee-b.MonthlyFee != snap.UsageFee {
		problems = append(problems, fmt.Sprintf("用量费变化：快照 %d 分，确认时 %d 分", snap.UsageFee, b.TotalFee-b.MonthlyFee))
	}
	if len(b.Lines) != len(snap.Lines) {
		problems = append(problems, fmt.Sprintf("逐条明细条数变化：快照 %d 条，确认时 %d 条", len(snap.Lines), len(b.Lines)))
	}
	n := len(b.Lines)
	if len(snap.Lines) < n {
		n = len(snap.Lines)
	}
	for i := 0; i < n; i++ {
		want, got := snap.Lines[i], b.Lines[i]
		if want.UsageID != got.UsageID || want.Quantity != got.Quantity {
			problems = append(problems, fmt.Sprintf("第 %d 条明细输入变化：快照 %s %s %d，确认时 %s %s %d",
				i+1, want.UsageID, want.Time, want.Quantity, got.UsageID, got.Time, got.Quantity))
		}
		// 时间按解析后的瞬间比较（Z 与 +00:00 等同一时刻写法视为相同）。
		wt, errW := time.Parse(time.RFC3339, want.Time)
		gt, errG := time.Parse(time.RFC3339, got.Time)
		if errW != nil || errG != nil || !wt.Equal(gt) {
			problems = append(problems, fmt.Sprintf("用量 %q 的时间点变化：快照 %s，确认时 %s", want.UsageID, want.Time, got.Time))
		}
		if want.LineFee != got.LineFee {
			problems = append(problems, fmt.Sprintf("用量 %q 小计变化：快照 %d 分，确认时 %d 分", want.UsageID, want.LineFee, got.LineFee))
		}
		if !segmentsEqual(want.Segments, got.Segments) {
			problems = append(problems, fmt.Sprintf("用量 %q 的跨档分段变化：快照 %s，确认时 %s",
				want.UsageID, formatSegments(want.Segments), formatSegments(got.Segments)))
		}
	}
	if len(b.TierTotals) != len(snap.TierTotals) {
		problems = append(problems, fmt.Sprintf("分档合计档数变化：快照 %d 档，确认时 %d 档", len(snap.TierTotals), len(b.TierTotals)))
	}
	tn := len(b.TierTotals)
	if len(snap.TierTotals) < tn {
		tn = len(snap.TierTotals)
	}
	for i := 0; i < tn; i++ {
		if b.TierTotals[i] != snap.TierTotals[i] {
			problems = append(problems, fmt.Sprintf("第 %d 档合计变化：快照 数量 %d 金额 %d 分，确认时 数量 %d 金额 %d 分",
				i+1, snap.TierTotals[i].Quantity, snap.TierTotals[i].Fee, b.TierTotals[i].Quantity, b.TierTotals[i].Fee))
		}
	}
	return problems
}

// formatSegments 紧凑展示一条用量的跨档分段。
func formatSegments(segs []lineSegment) string {
	if len(segs) == 0 {
		return "无"
	}
	parts := make([]string, len(segs))
	for i, seg := range segs {
		parts[i] = fmt.Sprintf("第%d档×%d=%d分", seg.Tier+1, seg.Quantity, seg.Fee)
	}
	return strings.Join(parts, ", ")
}

// printDraft 只读展示草案：标识、客户、月份、创建时间、待确认/已确认状态及
// 关联账单，以及创建时保存的完整计费快照（与 bill show 口径一致，但不含
// 账后信息——草案不生成账单，确认前不存在调整与收款）。
func printDraft(d *settlementDraft, s *state) {
	cust, ok := s.Customers[d.CustomerID]
	custName := ""
	if ok {
		custName = cust.Name
	}
	snap := &d.Snapshot
	fmt.Fprintf(stdout, "草案标识：%s\n", d.ID)
	fmt.Fprintf(stdout, "客户：%s（%s）\n", d.CustomerID, custName)
	fmt.Fprintf(stdout, "月份：%s（UTC 自然月，左闭右开）\n", d.Month)
	if d.Confirmed {
		fmt.Fprintf(stdout, "状态：已确认（关联账单 %s，确认时间 %s）\n", d.BillID, d.ConfirmedAt)
	} else {
		fmt.Fprintln(stdout, "状态：待确认（草案不生成账单、不封账、不占账后序号；快照永久保留、不可改写）")
	}
	tiered := snap.Pricing == "tiered"
	if tiered {
		fmt.Fprintln(stdout, "计价类型：阶梯计费（按 UTC 自然月累计用量分档计价，每月从零累计）")
		fmt.Fprintf(stdout, "方案：%s（%s）\n", snap.PlanID, snap.PlanName)
		fmt.Fprintln(stdout, "阶梯规则：")
		for i, t := range snap.PlanTiers {
			if t.Limit == 0 {
				fmt.Fprintf(stdout, "  第 %d 档：累计数量无上限，单价 %d 分（%s）\n", i+1, t.Price, moneyFen(t.Price))
			} else {
				fmt.Fprintf(stdout, "  第 %d 档：累计上限 %d，单价 %d 分（%s）\n", i+1, t.Limit, t.Price, moneyFen(t.Price))
			}
		}
	} else {
		fmt.Fprintf(stdout, "计价类型：固定单价\n单价：%d 分（%s）\n", snap.UnitPrice, moneyFen(snap.UnitPrice))
	}
	fmt.Fprintf(stdout, "总数量：%d\n", snap.TotalQty)
	if tiered {
		fmt.Fprintf(stdout, "月费：%d 分（%s，整月收取，不按天折算）\n", snap.MonthlyFee, moneyFen(snap.MonthlyFee))
		fmt.Fprintf(stdout, "用量费：%d 分（%s）\n", snap.UsageFee, moneyFen(snap.UsageFee))
		fmt.Fprintf(stdout, "原总金额：%d 分（%s，月费加全月用量费）\n", snap.TotalFee, moneyFen(snap.TotalFee))
	} else {
		fmt.Fprintf(stdout, "总金额：%d 分（%s）\n", snap.TotalFee, moneyFen(snap.TotalFee))
	}
	if tiered {
		fmt.Fprintln(stdout, "各档合计：")
		for i, tt := range snap.TierTotals {
			fmt.Fprintf(stdout, "  第 %d 档：数量 %d，单价 %d 分，金额 %d 分（%s）\n",
				i+1, tt.Quantity, snap.PlanTiers[i].Price, tt.Fee, moneyFen(tt.Fee))
		}
	}
	if len(d.Inputs) == 0 {
		fmt.Fprintln(stdout, "计费输入：无（本账期无用量，仅收取月费）")
	} else {
		fmt.Fprintf(stdout, "计费输入（创建时全部有效用量，共 %d 条，按时间点与标识排序）：\n", len(d.Inputs))
	}
	for i, in := range d.Inputs {
		fmt.Fprintf(stdout, "  %d. 用量标识=%s 时间=%s 数量=%d\n", i+1, in.UsageID, in.Time, in.Quantity)
	}
	if len(snap.Lines) == 0 {
		if tiered {
			fmt.Fprintln(stdout, "明细：无（本账期无用量，仅收取月费）")
		}
	} else {
		fmt.Fprintln(stdout, "逐条与跨档明细：")
		for i, ln := range snap.Lines {
			fmt.Fprintf(stdout, "  %d. 用量标识=%s 时间=%s 数量=%d 小计=%d 分（%s）\n",
				i+1, ln.UsageID, ln.Time, ln.Quantity, ln.LineFee, moneyFen(ln.LineFee))
			for j, seg := range ln.Segments {
				price := int64(0)
				if tiered && seg.Tier < len(snap.PlanTiers) {
					price = snap.PlanTiers[seg.Tier].Price
				}
				fmt.Fprintf(stdout, "     分段 %d：第 %d 档 数量=%d 单价=%d 分 小计=%d 分（%s）\n",
					j+1, seg.Tier+1, seg.Quantity, price, seg.Fee, moneyFen(seg.Fee))
			}
		}
	}
	if d.Confirmed {
		if b := s.Bills[billKey(d.CustomerID, d.Month)]; b != nil {
			_, payable, _ := billTotals(b, adjustmentsFor(s, b.CustomerID, b.Month))
			received, _ := paymentReceived(s, b.CustomerID, d.Month)
			fmt.Fprintf(stdout, "关联账单当前账后状态：当前应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
				payable, moneyFen(payable), received, moneyFen(received), payable-received, moneyFen(payable-received))
			fmt.Fprintln(stdout, "完整账单与账后历史可用 bill show 查询。")
		}
	} else {
		fmt.Fprintln(stdout, "关联账单：无（待确认）")
		// 待确认期间月份可能已由其他草案或原结算命令出账：只读提示，不认领。
		if b := s.Bills[billKey(d.CustomerID, d.Month)]; b != nil {
			fmt.Fprintf(stdout, "提示：客户 %s 的 %s 已由其他结算出账封账（账单 %s），本草案不会认领该账单，确认将被拒绝\n",
				d.CustomerID, d.Month, b.ID)
		}
	}
}
