package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现阶梯客户的按月暂停区间：登记（customer suspend）与查询
// （customer suspensions）。区间按 UTC 自然月解释为 [起月, 结束月)，
// 包含起月、不包含结束月；暂停月不接收用量、不产生月费账单。区间可相接
// （相接视为连续暂停）但不得重叠，不在任何区间内的月份正常服务。记录一旦
// 写入永不修改或删除；暂停不产生账后流水事件，也不占用全局操作序号。
// 固定单价客户不适用暂停。

func cmdCustomerSuspend(dir, customerID, startMonth, endMonth, reason string) error {
	if !validMonth(startMonth) {
		return fmt.Errorf("暂停起月 %q 无效，必须是 YYYY-MM 形式（如 2026-11）", startMonth)
	}
	if !validMonth(endMonth) {
		return fmt.Errorf("暂停结束月 %q 无效，必须是 YYYY-MM 形式（如 2027-02）", endMonth)
	}
	if endMonth <= startMonth {
		return fmt.Errorf("暂停结束月 %s 必须晚于起月 %s（区间包含起月、不包含结束月）", endMonth, startMonth)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("暂停原因不能为空")
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
		return fmt.Errorf("客户 %s 是固定单价客户（单价 %d 分），未绑定阶梯方案，不适用暂停", customerID, cust.Price)
	}

	key := suspensionKey(customerID, startMonth)
	if existing, ok := s.Suspensions[key]; ok {
		// 一项暂停由客户与起月共同标识：相同结束月与原因的重放返回原记录，
		// 不写盘——即使后来结算了恢复后的月份也须成功；结束月或原因不同
		// 则拒绝，记录不可改写。
		if existing.EndMonth == endMonth && existing.Reason == reason {
			fmt.Fprintf(stdout, "客户 %s 自 %s 起的暂停区间已存在且内容相同，返回原记录（不写盘）：\n\n", customerID, startMonth)
			printSuspension(existing)
			return nil
		}
		return fmt.Errorf("客户 %s 在 %s 已登记暂停区间（结束月 %s，原因 %q），内容不同，暂停记录不可改写",
			customerID, startMonth, existing.EndMonth, existing.Reason)
	}

	// 起月须晚于该客户所有已封账月份：已封账账单不因暂停改变。
	for _, b := range s.Bills {
		if b.CustomerID == customerID && b.Month >= startMonth {
			return fmt.Errorf("客户 %s 的 %s 已封账，暂停起月 %s 须晚于该客户所有已封账月份",
				customerID, b.Month, startMonth)
		}
	}
	// 同一客户的区间不得重叠（可以相接，相接视为连续暂停）；其他客户互不影响。
	for _, su := range s.suspensionsFor(customerID) {
		if startMonth < su.EndMonth && endMonth > su.StartMonth {
			return fmt.Errorf("新区间 %s..%s（不含结束月）与客户 %s 已登记区间 %s..%s（不含结束月）重叠；区间可以相接但不得重叠",
				startMonth, endMonth, customerID, su.StartMonth, su.EndMonth)
		}
	}
	// 登记前检查区间内已导入的有效（未撤回）用量：存在任一条就拒绝并指出
	// 冲突记录，不删除或改写任何用量；已撤回记录不参与冲突判断，可存在于
	// 随后暂停的月份。
	var conflicts []string
	conflictMonths := make(map[string]bool)
	for _, u := range s.Usage {
		if s.isWithdrawn(u.ID) {
			continue
		}
		if u.CustomerID != customerID {
			continue
		}
		m := utcMonth(u.Time)
		if monthInRange(m, startMonth, endMonth) {
			conflicts = append(conflicts, fmt.Sprintf("用量 %s（%s，数量 %d）", u.ID, m, u.Quantity))
			conflictMonths[m] = true
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		months := make([]string, 0, len(conflictMonths))
		for m := range conflictMonths {
			months = append(months, m)
		}
		sort.Strings(months)
		return fmt.Errorf("暂停区间 %s..%s（不含结束月）内已存在 %d 条用量（月份 %s），拒绝登记且不改写用量：%s",
			startMonth, endMonth, len(conflicts), strings.Join(months, "、"), strings.Join(conflicts, "、"))
	}

	su := &suspension{
		CustomerID: customerID,
		StartMonth: startMonth,
		EndMonth:   endMonth,
		Reason:     reason,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.Suspensions[key] = su

	// 暂停记录整体原子落盘后才报告成功；保存失败则一切不生效，
	// 该客户该起月不被占用，可原样重试。
	if err := s.save(); err != nil {
		delete(s.Suspensions, key)
		return err
	}
	fmt.Fprintf(stdout, "已登记暂停区间：客户 %s 自 %s（UTC 自然月，含）至 %s（不含）暂停服务，期间不接收用量、不产生月费账单：\n\n",
		customerID, startMonth, endMonth)
	printSuspension(su)
	return nil
}

// printSuspension 输出一条暂停区间记录。
func printSuspension(su *suspension) {
	fmt.Fprintf(stdout, "客户：%s\n", su.CustomerID)
	fmt.Fprintf(stdout, "暂停区间：%s（含）至 %s（不含，UTC 自然月）\n", su.StartMonth, su.EndMonth)
	fmt.Fprintf(stdout, "原因：%s\n", su.Reason)
}

// describeSuspension 返回覆盖指定月份的暂停区间的简短描述，用于错误信息。
func describeSuspension(s *state, customerID, month string) string {
	for _, su := range s.suspensionsFor(customerID) {
		if monthInRange(month, su.StartMonth, su.EndMonth) {
			return fmt.Sprintf("区间 %s（含）至 %s（不含），原因：%s", su.StartMonth, su.EndMonth, su.Reason)
		}
	}
	return "暂停区间"
}

func cmdCustomerSuspensions(dir, customerID, month string, hasMonth bool) error {
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

	// 只读查询：不改写存档、不占用序号、不新增记录；成功或失败均不落盘。
	if cust.PlanID == "" {
		fmt.Fprintf(stdout, "客户 %s（%s）是固定单价客户（单价 %d 分），不适用按月暂停，全部月份正常服务\n",
			cust.ID, cust.Name, cust.Price)
		if hasMonth {
			fmt.Fprintf(stdout, "月份 %s：不适用暂停（固定单价客户正常服务）\n", month)
		}
		return nil
	}

	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	list := s.suspensionsFor(customerID)
	if len(list) == 0 {
		fmt.Fprintln(stdout, "暂停区间：无（全部 UTC 自然月正常服务）")
	} else {
		fmt.Fprintln(stdout, "暂停区间（按起月升序；包含起月、不包含结束月）：")
		for i, su := range list {
			fmt.Fprintf(stdout, "  %d. %s（含）至 %s（不含），原因：%s\n",
				i+1, su.StartMonth, su.EndMonth, su.Reason)
		}
	}
	if hasMonth {
		var hit *suspension
		for _, su := range list {
			if monthInRange(month, su.StartMonth, su.EndMonth) {
				hit = su
				break
			}
		}
		if hit != nil {
			fmt.Fprintf(stdout, "月份 %s：已暂停（处于区间 %s（含）至 %s（不含）），原因：%s\n",
				month, hit.StartMonth, hit.EndMonth, hit.Reason)
		} else {
			fmt.Fprintf(stdout, "月份 %s：未暂停（不在任何暂停区间内，正常服务）\n", month)
		}
	}
	return nil
}
