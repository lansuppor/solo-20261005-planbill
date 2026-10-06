package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现阶梯客户按月生效的方案变更：登记（plan change）与查询
// （plan schedule）。变更自生效月（UTC 自然月，含）起改用目标方案，
// 直到下一次变更；生效月之前的月份不受影响，用于续期时采用新价格而
// 不重算历史账单。变更记录一旦写入永不修改，不产生账后流水事件，
// 也不占用全局操作序号。固定单价客户与方案规则本身仍不可修改。

func cmdPlanChange(dir, customerID, month, planID, reason string) error {
	if !validMonth(month) {
		return fmt.Errorf("生效月份 %q 无效，必须是 YYYY-MM 形式（如 2026-11）", month)
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
		return fmt.Errorf("目标方案标识 %q 不存在", planID)
	}

	key := planChangeKey(customerID, month)
	if existing, ok := s.PlanChanges[key]; ok {
		// 以客户与生效月共同标识一项变更：相同目标方案与原因的重放返回
		// 原记录，不重复生效——即使后来追加了变更或相关月份已封账；
		// 同一项内容不同则拒绝，已有变更不可改写。
		if existing.PlanID == planID && existing.Reason == reason {
			fmt.Fprintf(stdout, "客户 %s 在 %s 的方案变更已存在且内容相同，返回原记录（不重复生效）：\n\n", customerID, month)
			printPlanChange(existing, s)
			return nil
		}
		return fmt.Errorf("客户 %s 在 %s 已登记方案变更（目标方案 %q，原因 %q），内容不同，已有变更不可改写",
			customerID, month, existing.PlanID, existing.Reason)
	}

	// 新变更只能按生效月递增追加。
	for _, ch := range s.PlanChanges {
		if ch.CustomerID == customerID && ch.Month >= month {
			return fmt.Errorf("客户 %s 已存在生效月 %s 的变更，新生效月 %s 必须更晚（新变更只能按生效月递增追加）",
				customerID, ch.Month, month)
		}
	}
	// 生效月须晚于该客户所有已封账月份：已封账账单的计费结果不受影响。
	for _, b := range s.Bills {
		if b.CustomerID == customerID && b.Month >= month {
			return fmt.Errorf("客户 %s 的 %s 已封账，生效月 %s 须晚于该客户所有已封账月份", customerID, b.Month, month)
		}
	}
	// 登记前预检：生效月起已导入的用量（上述检查已保证这些月份均未封账）
	// 按目标方案逐条从零计价，任一条金额溢出则整项拒绝并指出用量，
	// 不修改用量；月累计溢出仍由结算把关（拒绝且不封账）。
	var ids []string
	for _, u := range s.Usage {
		if u.CustomerID == customerID && utcMonth(u.Time) >= month {
			ids = append(ids, u.ID)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		u := s.Usage[id]
		rec := *u
		if _, err := tieredPrice(p.Tiers, []*usageRecord{&rec}); err != nil {
			return fmt.Errorf("用量 %s（%s，数量 %d）按目标方案 %q 单条从零计价失败：%v；整项变更拒绝，用量未修改",
				u.ID, utcMonth(u.Time), u.Quantity, planID, err)
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

	// 变更整体原子落盘后才报告成功；保存失败则一切不生效，
	// 该客户该月份不被占用，可重试。
	if err := s.save(); err != nil {
		delete(s.PlanChanges, key)
		return err
	}
	fmt.Fprintf(stdout, "已登记方案变更：客户 %s 自 %s（UTC 自然月，含）起改用方案 %q（%s），之前月份不受影响：\n\n",
		customerID, month, planID, p.Name)
	printPlanChange(ch, s)
	return nil
}

// printPlanChange 输出一条方案变更记录。
func printPlanChange(ch *planChange, s *state) {
	p := s.Plans[ch.PlanID] // 载入时已校验存在
	fmt.Fprintf(stdout, "客户：%s\n", ch.CustomerID)
	fmt.Fprintf(stdout, "生效月：%s（UTC 自然月，含；之前月份不受影响）\n", ch.Month)
	fmt.Fprintf(stdout, "目标方案：%s（%s）：规则 %s\n", p.ID, p.Name, formatTiers(p.Tiers))
	fmt.Fprintf(stdout, "原因：%s\n", ch.Reason)
}

func cmdPlanSchedule(dir, customerID, month string, hasMonth bool) error {
	if hasMonth && !validMonth(month) {
		return fmt.Errorf("月份 %q 无效，必须是 YYYY-MM 形式（如 2026-11）", month)
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	cust, ok := s.Customers[customerID]
	if !ok {
		return fmt.Errorf("客户标识 %q 不存在", customerID)
	}

	// 只读查询：不改写数据、不占用序号、不新增记录。
	if cust.PlanID == "" {
		fmt.Fprintf(stdout, "客户 %s（%s）是固定单价客户（单价 %d 分），无阶梯方案安排\n",
			cust.ID, cust.Name, cust.Price)
		return nil
	}
	initial := s.Plans[cust.PlanID] // 载入时已校验存在
	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "初始方案：%s（%s）：规则 %s（创建时绑定，永久保留）\n",
		initial.ID, initial.Name, formatTiers(initial.Tiers))
	changes := s.planChangesFor(customerID)
	if len(changes) == 0 {
		fmt.Fprintln(stdout, "方案变更：无")
	} else {
		fmt.Fprintln(stdout, "方案变更（按生效月升序）：")
		for i, ch := range changes {
			p := s.Plans[ch.PlanID] // 载入时已校验存在
			fmt.Fprintf(stdout, "  %d. 自 %s 起改用 %s（%s）：规则 %s，原因：%s\n",
				i+1, ch.Month, p.ID, p.Name, formatTiers(p.Tiers), ch.Reason)
		}
	}
	if hasMonth {
		p := s.Plans[s.effectivePlanID(cust, month)] // 载入时已校验存在
		fmt.Fprintf(stdout, "月份 %s 的有效方案：%s（%s）：规则 %s\n", month, p.ID, p.Name, formatTiers(p.Tiers))
	}
	return nil
}
