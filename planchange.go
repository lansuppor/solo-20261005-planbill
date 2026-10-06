package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现阶梯客户按月生效的方案变更：登记变更与查询客户方案安排。
// 用于续期时采用新价格而不重算历史账单：创建时绑定的方案永久保留为
// 初始方案，生效月（UTC 自然月）起改用目标方案，直到下一次变更，之前
// 月份不受影响。变更只能按生效月递增追加，已有变更不可改写；不产生
// 账后流水事件，也不占用全局操作序号。

// cmdCustomerChangePlan 登记一项方案变更：customer change-plan
// <客户标识> <YYYY-MM> <目标方案标识> <原因>。仅限已绑定阶梯方案的客户；
// 生效月须晚于该客户所有已封账月份，且晚于该客户已有变更的生效月。
func cmdCustomerChangePlan(dir, customerID, month, planID, reason string) error {
	if !validMonth(month) {
		return fmt.Errorf("生效月份 %q 无效，必须是 YYYY-MM 形式（如 2027-01）", month)
	}
	if strings.TrimSpace(planID) == "" {
		return fmt.Errorf("方案标识不能为空")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("变更原因不能为空")
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	cust, ok := s.Customers[customerID]
	if !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}
	if cust.PlanID == "" {
		return fmt.Errorf("客户 %s 是固定单价客户，未绑定阶梯方案，不能登记方案变更", customerID)
	}
	p, ok := s.Plans[planID]
	if !ok {
		return fmt.Errorf("方案标识 %q 不存在", planID)
	}

	key := changeKey(customerID, month)
	if existing, ok := s.PlanChanges[key]; ok {
		// 以客户与生效月共同标识一项变更：相同目标方案和原因的重放返回
		// 原记录，即使后来追加了变更或月份已封账也不重复生效；同一客户
		// 同一生效月内容不同拒绝，已有变更不可改写。
		if existing.PlanID == planID && existing.Reason == reason {
			fmt.Fprintf(stdout, "客户 %s 的 %s 方案变更已存在且内容相同，返回原记录（幂等，不重复生效）：\n\n", customerID, month)
			printPlanChange(existing, s)
			return nil
		}
		return fmt.Errorf("客户 %s 的 %s 已登记方案变更（目标方案 %q，原因 %q），内容不同，拒绝改写；已有变更不可修改",
			customerID, month, existing.PlanID, existing.Reason)
	}

	// 新变更只能按生效月递增追加。
	var maxChange string
	for _, ch := range s.PlanChanges {
		if ch.CustomerID == customerID && ch.Month > maxChange {
			maxChange = ch.Month
		}
	}
	if maxChange != "" && month <= maxChange {
		return fmt.Errorf("客户 %s 已存在生效月 %s 的方案变更，新变更生效月 %s 必须更晚（只能按生效月递增追加）",
			customerID, maxChange, month)
	}

	// 生效月须晚于该客户所有已封账月份，历史账单不因变更重算。
	var maxSealed string
	for _, b := range s.Bills {
		if b.CustomerID == customerID && b.Month > maxSealed {
			maxSealed = b.Month
		}
	}
	if maxSealed != "" && month <= maxSealed {
		return fmt.Errorf("客户 %s 的 %s 已封账，新变更生效月 %s 必须晚于所有已封账月份",
			customerID, maxSealed, month)
	}

	// 登记前预检：对生效月起已导入的未封账用量（生效月晚于所有已封账
	// 月份，故生效月及之后的用量必然未封账），按新方案逐条从零计价检查
	// 金额；任一条溢出则整项拒绝并指出用量，不修改用量。只逐条检查，
	// 月累计溢出仍由结算拒绝且不封账。
	var ids []string
	for id, u := range s.Usage {
		if u.CustomerID != customerID {
			continue
		}
		if usageMonth(u) >= month {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec := *s.Usage[id]
		if _, err := tieredPrice(p.Tiers, []*usageRecord{&rec}); err != nil {
			return fmt.Errorf("按新方案 %q 对用量 %q 从零计价失败：%v；整项变更被拒绝，未修改任何用量", planID, id, err)
		}
	}

	ch := &planChange{
		CustomerID: customerID,
		Month:      month,
		PlanID:     planID,
		Reason:     reason,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.PlanChanges[key] = ch

	// 变更整体原子落盘后才报告成功；保存失败则一切不生效，该客户月份
	// 不被占用，可重试。
	if err := s.save(); err != nil {
		delete(s.PlanChanges, key)
		return err
	}
	fmt.Fprintf(stdout, "已登记方案变更：客户 %s 自 %s（UTC 自然月）起改用方案 %q（%s），%s 之前的月份不受影响：\n\n",
		customerID, month, planID, p.Name, month)
	printPlanChange(ch, s)
	return nil
}

// usageMonth 返回用量记录时间换算 UTC 后的自然月（YYYY-MM）。
// 时间已在载入/导入时校验为 RFC3339。
func usageMonth(u *usageRecord) string {
	t, _ := time.Parse(time.RFC3339, u.Time)
	return t.UTC().Format("2006-01")
}

// printPlanChange 输出一项方案变更记录。
func printPlanChange(ch *planChange, s *state) {
	p := s.Plans[ch.PlanID] // 载入时已校验存在
	fmt.Fprintf(stdout, "客户：%s\n", ch.CustomerID)
	fmt.Fprintf(stdout, "生效月：%s（UTC 自然月，自该月起生效，直到下一次变更）\n", ch.Month)
	fmt.Fprintf(stdout, "目标方案：%s（%s）：%s\n", p.ID, p.Name, formatTiers(p.Tiers))
	fmt.Fprintf(stdout, "原因：%s\n", ch.Reason)
}

// cmdCustomerPlanSchedule 查询客户方案安排（只读）：customer plan-schedule
// <客户标识> [YYYY-MM]。展示初始方案、按生效月排列的变更及原因；给定
// 月份时说明该月的有效方案。
func cmdCustomerPlanSchedule(dir, customerID, month string, hasMonth bool) error {
	if hasMonth && !validMonth(month) {
		return fmt.Errorf("月份 %q 无效，必须是 YYYY-MM 形式（如 2027-01）", month)
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	cust, ok := s.Customers[customerID]
	if !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}
	if cust.PlanID == "" {
		fmt.Fprintf(stdout, "客户 %s（%s）是固定单价客户（单价 %d 分，%s），无阶梯方案安排\n",
			cust.ID, cust.Name, cust.Price, moneyFen(cust.Price))
		return nil
	}

	initial := s.Plans[cust.PlanID] // 载入时已校验存在
	fmt.Fprintf(stdout, "客户 %s（%s）的方案安排：\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "初始方案：%s（%s）：%s\n", initial.ID, initial.Name, formatTiers(initial.Tiers))

	changes := changesFor(s, customerID)
	if len(changes) == 0 {
		fmt.Fprintln(stdout, "变更（按生效月排列）：无")
	} else {
		fmt.Fprintln(stdout, "变更（按生效月排列）：")
		for i, ch := range changes {
			p := s.Plans[ch.PlanID] // 载入时已校验存在
			fmt.Fprintf(stdout, "  %d. 自 %s 起改用 %s（%s）：%s；原因：%s\n",
				i+1, ch.Month, p.ID, p.Name, formatTiers(p.Tiers), ch.Reason)
		}
	}
	if hasMonth {
		p := s.Plans[s.planForMonth(customerID, month)] // 载入时已校验存在
		fmt.Fprintf(stdout, "%s 的有效方案：%s（%s）：%s\n", month, p.ID, p.Name, formatTiers(p.Tiers))
	}
	return nil
}

// changesFor 返回某客户的全部方案变更，按生效月升序。
func changesFor(s *state, customerID string) []*planChange {
	var list []*planChange
	for _, ch := range s.PlanChanges {
		if ch.CustomerID == customerID {
			list = append(list, ch)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Month < list[j].Month })
	return list
}
