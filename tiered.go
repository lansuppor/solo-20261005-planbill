package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 本文件实现按月累计用量的阶梯计费：方案登记与查询、绑定方案客户的
// 创建、阶梯计价引擎与阶梯账单结算。固定单价计费路径不受影响。

// tieredLine 是一条用量记录的阶梯计价结果：跨档分段与本条小计。
type tieredLine struct {
	segments []lineSegment
	fee      int64
}

// tieredResult 是一组用量记录按阶梯规则计价后的完整结果。
type tieredResult struct {
	lines    []tieredLine // 与输入记录一一对应
	tierQty  []int64      // 各档实际数量（与方案档位顺序对齐）
	tierFee  []int64      // 各档实际金额（分）
	totalQty int64
	totalFee int64
}

// tieredPrice 按阶梯规则对一组已按（解析后的时间点升序、同一时间按标识
// 字典序）排列的用量记录从月累计零开始计价：每条记录按当时累计量落入的
// 档位分段，各档仅对落入本档的数量收费，不把最高档价格套用全月，也不对
// 每条用量重置阶梯。全程整数运算；月累计数量、分档金额或总额溢出有符号
// 64 位范围时返回错误。
func tieredPrice(tiers []tier, recs []*usageRecord) (*tieredResult, error) {
	res := &tieredResult{
		lines:   make([]tieredLine, len(recs)),
		tierQty: make([]int64, len(tiers)),
		tierFee: make([]int64, len(tiers)),
	}
	var cum int64 // 月累计数量
	tierIdx := 0  // 当前档位（单调前进，不回退）
	for i, u := range recs {
		remaining := u.Quantity
		var lineFee int64
		var segs []lineSegment
		for remaining > 0 {
			// 累计量越过当前档上限时前进到下一档；上限严格递增，
			// 因此前进后当前档剩余容量必为正。
			for tierIdx < len(tiers)-1 && cum >= tiers[tierIdx].Limit {
				tierIdx++
			}
			t := tiers[tierIdx]
			take := remaining
			if tierIdx < len(tiers)-1 && take > t.Limit-cum {
				take = t.Limit - cum
			}
			fee, err := mul64(take, t.Price)
			if err != nil {
				return nil, fmt.Errorf("用量 %s：第 %d 档数量 %d × 单价 %d 金额溢出", u.ID, tierIdx+1, take, t.Price)
			}
			if lineFee, err = add64(lineFee, fee); err != nil {
				return nil, fmt.Errorf("用量 %s：分段小计汇总溢出", u.ID)
			}
			if cum, err = add64(cum, take); err != nil {
				return nil, fmt.Errorf("月累计数量溢出")
			}
			if res.tierQty[tierIdx], err = add64(res.tierQty[tierIdx], take); err != nil {
				return nil, fmt.Errorf("第 %d 档累计数量溢出", tierIdx+1)
			}
			if res.tierFee[tierIdx], err = add64(res.tierFee[tierIdx], fee); err != nil {
				return nil, fmt.Errorf("第 %d 档累计金额溢出", tierIdx+1)
			}
			segs = append(segs, lineSegment{Tier: tierIdx, Quantity: take, Fee: fee})
			remaining -= take
		}
		res.lines[i] = tieredLine{segments: segs, fee: lineFee}
		var err error
		if res.totalQty, err = add64(res.totalQty, u.Quantity); err != nil {
			return nil, fmt.Errorf("汇总数量溢出")
		}
		if res.totalFee, err = add64(res.totalFee, lineFee); err != nil {
			return nil, fmt.Errorf("汇总金额溢出")
		}
	}
	return res, nil
}

// parseTierSpecs 解析阶梯参数：每档为 <上限>:<单价分>，上限为正整数且
// 严格递增；最后一档用 - 表示无上限（-:<单价分>）。至少一档。
func parseTierSpecs(args []string) ([]tier, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("阶梯方案至少需要一档，格式：<上限>:<单价分>，最后一档用 -:<单价分> 表示无上限")
	}
	tiers := make([]tier, 0, len(args))
	prev := int64(0)
	for i, arg := range args {
		parts := strings.Split(arg, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("阶梯档 %q 格式非法，应为 <上限>:<单价分>（如 100:10），最后一档用 -:<单价分> 表示无上限", arg)
		}
		last := i == len(args)-1
		limitText := strings.TrimSpace(parts[0])
		var limit int64
		if limitText == "-" {
			if !last {
				return nil, fmt.Errorf("阶梯档 %q：只有最后一档可以无上限（-:<单价分>）", arg)
			}
		} else {
			v, err := strconv.ParseInt(limitText, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("阶梯档 %q 的上限 %q 不是有符号 64 位整数: %w", arg, parts[0], err)
			}
			if v <= 0 {
				return nil, fmt.Errorf("阶梯档 %q 的上限必须是正整数，收到 %d", arg, v)
			}
			if v <= prev {
				return nil, fmt.Errorf("阶梯档 %q 的上限 %d 未严格大于上一档上限 %d", arg, v, prev)
			}
			if last {
				return nil, fmt.Errorf("最后一档必须无上限，请用 -:<单价分> 表示（如 -:%s）", parts[1])
			}
			prev = v
			limit = v
		}
		price, err := parsePrice(parts[1])
		if err != nil {
			return nil, fmt.Errorf("阶梯档 %q 的单价无效: %w", arg, err)
		}
		tiers = append(tiers, tier{Limit: limit, Price: price})
	}
	return tiers, nil
}

// formatTiers 以 "100:10 500:8 -:5" 形式紧凑展示阶梯规则。
func formatTiers(tiers []tier) string {
	parts := make([]string, len(tiers))
	for i, t := range tiers {
		limit := "-"
		if t.Limit != 0 {
			limit = strconv.FormatInt(t.Limit, 10)
		}
		parts[i] = fmt.Sprintf("%s:%d", limit, t.Price)
	}
	return strings.Join(parts, " ")
}

// printPlan 输出一个阶梯计费方案的完整规则（含固定月费）。
func printPlan(p *plan) {
	fmt.Fprintf(stdout, "方案标识：%s\n", p.ID)
	fmt.Fprintf(stdout, "方案名称：%s\n", p.Name)
	fmt.Fprintf(stdout, "月费：%d 分（%s，每账期按有效方案收取一次整月，不按天折算）\n", p.MonthlyFee, moneyFen(p.MonthlyFee))
	fmt.Fprintln(stdout, "阶梯规则（按 UTC 自然月累计用量分档计价，每月从零累计）：")
	for i, t := range p.Tiers {
		if t.Limit == 0 {
			fmt.Fprintf(stdout, "  第 %d 档：累计数量无上限，单价 %d 分（%s）\n", i+1, t.Price, moneyFen(t.Price))
		} else {
			fmt.Fprintf(stdout, "  第 %d 档：累计上限 %d，单价 %d 分（%s）\n", i+1, t.Limit, t.Price, moneyFen(t.Price))
		}
	}
}

func cmdPlanAdd(dir, id, name string, tierArgs []string) error {
	// 省略月费视为 0。
	return registerPlan(dir, id, name, 0, tierArgs)
}

// cmdPlanAddFee 登记含固定月费的阶梯计费方案；月费为非负整数人民币分。
func cmdPlanAddFee(dir, id, name, feeText string, tierArgs []string) error {
	fee, err := parseMonthlyFee(feeText)
	if err != nil {
		return err
	}
	return registerPlan(dir, id, name, fee, tierArgs)
}

// parseMonthlyFee 解析月费：非负有符号 64 位整数人民币分。
func parseMonthlyFee(text string) (int64, error) {
	text = strings.TrimSpace(text)
	fee, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("月费 %q 不是有符号 64 位整数范围内的整数: %w", text, err)
	}
	if fee < 0 {
		return 0, fmt.Errorf("月费必须是非负整数分，收到 %d", fee)
	}
	return fee, nil
}

func registerPlan(dir, id, name string, monthlyFee int64, tierArgs []string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("方案标识不能为空")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("方案名称不能为空")
	}
	tiers, err := parseTierSpecs(tierArgs)
	if err != nil {
		return err
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	if _, exists := s.Plans[id]; exists {
		return fmt.Errorf("方案标识 %q 已存在，方案创建后不可修改", id)
	}
	p := &plan{ID: id, Name: name, MonthlyFee: monthlyFee, Tiers: tiers}
	s.Plans[id] = p

	// 方案整体原子落盘；保存失败则一切不生效，该方案标识不被占用。
	if err := s.save(); err != nil {
		delete(s.Plans, id)
		return err
	}
	fmt.Fprintf(stdout, "已登记阶梯计费方案 %q（%s）：\n\n", id, name)
	printPlan(p)
	return nil
}

func cmdPlanShow(dir, id string) error {
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	p, ok := s.Plans[id]
	if !ok {
		return fmt.Errorf("方案标识 %q 不存在", id)
	}
	printPlan(p)
	return nil
}

func cmdPlanList(dir string) error {
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	if len(s.Plans) == 0 {
		fmt.Fprintln(stdout, "尚无已登记的阶梯计费方案")
		return nil
	}
	ids := make([]string, 0, len(s.Plans))
	for id := range s.Plans {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Fprintf(stdout, "已登记阶梯计费方案 %d 个：\n", len(ids))
	for i, id := range ids {
		p := s.Plans[id]
		fmt.Fprintf(stdout, "  %d. %s（%s）：月费 %d 分，%d 档，规则 %s\n", i+1, p.ID, p.Name, p.MonthlyFee, len(p.Tiers), formatTiers(p.Tiers))
	}
	return nil
}

func cmdCustomerAddPlan(dir, id, name, planID string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("客户标识不能为空")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("客户名称不能为空")
	}
	if strings.TrimSpace(planID) == "" {
		return fmt.Errorf("方案标识不能为空")
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	p, ok := s.Plans[planID]
	if !ok {
		return fmt.Errorf("方案标识 %q 不存在", planID)
	}
	if _, exists := s.Customers[id]; exists {
		return fmt.Errorf("客户标识 %q 已存在，客户创建后不可修改", id)
	}
	s.Customers[id] = &customer{ID: id, Name: name, PlanID: planID}

	// 客户整体原子落盘；保存失败则一切不生效，该客户标识不被占用。
	if err := s.save(); err != nil {
		delete(s.Customers, id)
		return err
	}
	fmt.Fprintf(stdout, "已登记客户 %q（%s），绑定阶梯计费方案 %q（%s），绑定不可变更\n", id, name, planID, p.Name)
	return nil
}

// settleTiered 对绑定阶梯方案的客户按 UTC 自然月累计用量分档计价并封账。
// recs 已按计价顺序（时间点升序、同一时间按标识字典序）排列；有效方案
// 月费大于 0 时 recs 可为空（无用量也须出账收取整月月费）。
// 月累计数量、分档金额或月费加全月用量费溢出时拒绝结算且不封账。
func settleTiered(s *state, cust *customer, month string, recs []*usageRecord) error {
	b, err := buildTieredBill(s, cust, month, recs)
	if err != nil {
		return fmt.Errorf("客户 %s 的 %s 阶梯计价失败，拒绝结算且不封账: %w", cust.ID, month, err)
	}
	key := billKey(cust.ID, month)
	s.Bills[key] = b

	// 账单与封账状态在同一次原子保存中一起持久化；保存失败则一切不生效。
	if err := s.save(); err != nil {
		delete(s.Bills, key)
		return err
	}
	fmt.Fprintf(stdout, "结算完成，客户 %s 的 %s 已封账（阶梯计费）：\n\n", cust.ID, month)
	printBill(b, cust, s)
	return nil
}

// buildTieredBill 为绑定阶梯方案的客户构造阶梯账单（不落盘）：采用账期月的
// 有效方案（初始绑定被生效月不晚于账期月的最后一次变更替换）从零累计分档
// 计价，账单保存方案标识、名称、完整规则与实际月费快照。recs 须已按计价
// 顺序排列；有效方案月费大于 0 时 recs 可为空。原总金额 = 月费 + 全月
// 用量费；月累计数量、分档金额或月费加全月用量费溢出时返回错误。
func buildTieredBill(s *state, cust *customer, month string, recs []*usageRecord) (*bill, error) {
	p := s.Plans[s.effectivePlanID(cust, month)] // 载入时已校验存在
	priced, err := tieredPrice(p.Tiers, recs)
	if err != nil {
		return nil, err
	}
	// 月费加全月用量费溢出时拒绝结算。
	totalFee, err := add64(priced.totalFee, p.MonthlyFee)
	if err != nil {
		return nil, fmt.Errorf("月费 %d 分加全月用量费 %d 分溢出有符号 64 位整数范围", p.MonthlyFee, priced.totalFee)
	}

	lines := make([]billLine, len(recs))
	for i, u := range recs {
		lines[i] = billLine{
			UsageID:  u.ID,
			Time:     u.Time,
			Quantity: u.Quantity,
			LineFee:  priced.lines[i].fee,
			Segments: priced.lines[i].segments,
		}
	}
	tierTotals := make([]tierTotal, len(p.Tiers))
	for i := range p.Tiers {
		tierTotals[i] = tierTotal{Quantity: priced.tierQty[i], Fee: priced.tierFee[i]}
	}
	// 账单保存方案标识、名称、完整规则与实际月费快照，之后计价与校验
	// 只依赖账单自身。
	tiersCopy := make([]tier, len(p.Tiers))
	copy(tiersCopy, p.Tiers)
	return &bill{
		ID:         stableBillID(cust.ID, month),
		CustomerID: cust.ID,
		Month:      month,
		Pricing:    "tiered",
		PlanID:     p.ID,
		PlanName:   p.Name,
		PlanTiers:  tiersCopy,
		TierTotals: tierTotals,
		MonthlyFee: p.MonthlyFee,
		TotalQty:   priced.totalQty,
		TotalFee:   totalFee,
		Lines:      lines,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// monthlyFeeBillable 报告客户在某账期是否因有效方案月费大于 0 而即使
// 无用量也须出账：仅绑定阶梯方案且该账期有效方案月费大于 0 时成立；
// 固定单价客户与月费为 0 的方案在无用量时仍拒绝结算。
func monthlyFeeBillable(s *state, cust *customer, month string) bool {
	if cust.PlanID == "" {
		return false
	}
	return s.Plans[s.effectivePlanID(cust, month)].MonthlyFee > 0
}
