package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现阶梯客户的按月暂停区间：登记（customer suspend）、查询
// （customer suspensions）以及暂停的按月提前恢复（customer resume）。
// 区间按 UTC 自然月解释为 [起月, 结束月)，包含起月、不包含结束月；暂停月
// 不接收用量、不产生月费账单。区间可相接（相接视为连续暂停）但不得重叠，
// 不在任何当前有效区间内的月份正常服务。
//
// 提前恢复只缩短目标暂停的当前有效区间：原暂停区间与原因永久保留，当前有效
// 区间改为 [原起月, 恢复月)，恢复月起不再受该项暂停限制；恢复记录永久保留、
// 不可改写或撤销，每项暂停只能登记一次。暂停与恢复都不产生账后流水事件，
// 也不占用全局操作序号。固定单价客户不适用暂停。

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
		// 一项暂停由客户与起月共同标识：仍按原结束月与原原因判重。相同结束
		// 月与原因的重放返回原记录且不写盘，不能借重放延长有效暂停——即使
		// 该项暂停后来已提前恢复（当前有效区间已缩短）；结束月或原因不同
		// 则拒绝，记录不可改写。
		if existing.EndMonth == endMonth && existing.Reason == reason {
			fmt.Fprintf(stdout, "客户 %s 自 %s 起的暂停区间已存在且内容相同，返回原记录（不写盘）：\n\n", customerID, startMonth)
			printSuspension(existing)
			return nil
		}
		return fmt.Errorf("客户 %s 在 %s 已登记暂停区间（原结束月 %s，原因 %q），内容不同，暂停记录不可改写",
			customerID, startMonth, existing.EndMonth, existing.Reason)
	}

	// 起月须晚于该客户所有已封账月份：已封账账单不因暂停改变。
	for _, b := range s.Bills {
		if b.CustomerID == customerID && b.Month >= startMonth {
			return fmt.Errorf("客户 %s 的 %s 已封账，暂停起月 %s 须晚于该客户所有已封账月份",
				customerID, b.Month, startMonth)
		}
	}
	// 同一客户的当前有效区间不得重叠（可以相接，相接视为连续暂停）：提前
	// 恢复只缩短其他暂停的有效区间，故重叠判断使用各项的当前有效结束月；
	// 其他客户互不影响。
	for _, su := range s.suspensionsFor(customerID) {
		effEnd := s.suspensionEffectiveEnd(su)
		if startMonth < effEnd && endMonth > su.StartMonth {
			return fmt.Errorf("新区间 %s..%s（不含结束月）与客户 %s 已登记区间 %s..%s（当前有效，不含结束月）重叠；区间可以相接但不得重叠",
				startMonth, endMonth, customerID, su.StartMonth, effEnd)
		}
	}
	// 登记前检查新区间内已导入的有效（未撤回）用量：存在任一条就拒绝并指出
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

// cmdCustomerResume 为一项已登记暂停登记按月提前恢复：以客户与原起月识别
// 目标暂停，恢复月（UTC 自然月，含）起重新提供服务。原起月 < 恢复月 <
// 原结束月；原暂停区间与原因永久保留，当前有效区间缩短为 [原起月, 恢复月)。
// 每项暂停只能登记一次提前恢复；相同恢复月与原因的重放返回保存信息与当前
// 有效区间、不写盘，内容不同拒绝。其他月份已封账不妨碍登记，已有账单、
// 用量、方案安排和账后余额不变，不占用账后操作序号。
func cmdCustomerResume(dir, customerID, startMonth, resumeMonth, reason string) error {
	if !validMonth(startMonth) {
		return fmt.Errorf("目标暂停原起月 %q 无效，必须是 YYYY-MM 形式（如 2026-11）", startMonth)
	}
	if !validMonth(resumeMonth) {
		return fmt.Errorf("恢复月 %q 无效，必须是 YYYY-MM 形式（如 2026-12）", resumeMonth)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("提前恢复原因不能为空")
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
		return fmt.Errorf("客户 %s 是固定单价客户（单价 %d 分），未绑定阶梯方案，不适用暂停与提前恢复", customerID, cust.Price)
	}

	key := suspensionKey(customerID, startMonth)
	su, ok := s.Suspensions[key]
	if !ok {
		return fmt.Errorf("客户 %s 自 %s 起的暂停不存在，无法登记提前恢复（目标暂停必须先以 customer suspend 登记）",
			customerID, startMonth)
	}
	if existing, ok := s.SuspensionResumes[key]; ok {
		// 以客户和原起月识别该项恢复：相同恢复月与原因的重放返回保存信息
		// 和当前有效区间、不写盘——即使恢复月后来已结算、又登记了新的暂停
		// 或发生方案变更也须成功，且不撤销后来的暂停；内容不同拒绝，恢复
		// 记录不可改写或撤销。
		if existing.ResumeMonth == resumeMonth && existing.Reason == reason {
			fmt.Fprintf(stdout, "客户 %s 自 %s 起暂停的提前恢复已存在且内容相同，返回保存信息与当前有效区间（不写盘）：\n\n",
				customerID, startMonth)
			printSuspensionDetail(s, su)
			return nil
		}
		return fmt.Errorf("客户 %s 自 %s 起的暂停已登记提前恢复（恢复月 %s，原因 %q），恢复月或原因不同即拒绝；每项暂停只能登记一次提前恢复，恢复记录不可改写或撤销",
			customerID, startMonth, existing.ResumeMonth, existing.Reason)
	}

	// 原起月 < 恢复月 < 原结束月：恢复月必须仍在目标暂停原区间内部，
	// 月份按 UTC 自然月解释，与操作当天无关。
	if resumeMonth <= startMonth {
		return fmt.Errorf("恢复月 %s 必须晚于目标暂停原起月 %s", resumeMonth, startMonth)
	}
	if resumeMonth >= su.EndMonth {
		return fmt.Errorf("恢复月 %s 必须早于目标暂停原结束月 %s（原结束月本就恢复服务，无需提前恢复）",
			resumeMonth, su.EndMonth)
	}

	r := &suspensionResume{
		CustomerID:  customerID,
		StartMonth:  startMonth,
		ResumeMonth: resumeMonth,
		Reason:      reason,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	s.SuspensionResumes[key] = r

	// 恢复记录整体原子保存后才报告成功；保存失败则一切不生效，不占用该项
	// 暂停的恢复登记，可原样重试。提前恢复只缩短目标区间，不改动用量、账单、
	// 方案安排、账后余额或其他暂停。
	if err := s.save(); err != nil {
		delete(s.SuspensionResumes, key)
		return err
	}
	fmt.Fprintf(stdout, "已登记暂停提前恢复：客户 %s 的暂停自 %s（UTC 自然月，含）起恢复服务，无需等到原结束月 %s；原暂停区间与原因永久保留，恢复记录不可改写或撤销：\n\n",
		customerID, resumeMonth, su.EndMonth)
	printSuspensionDetail(s, su)
	return nil
}

// printSuspension 输出一条暂停区间的原区间与原因（customer suspend 登记
// 与相同重放使用）。提前恢复信息由 printSuspensionDetail 展示。
func printSuspension(su *suspension) {
	fmt.Fprintf(stdout, "客户：%s\n", su.CustomerID)
	fmt.Fprintf(stdout, "暂停区间：%s（含）至 %s（不含，UTC 自然月）\n", su.StartMonth, su.EndMonth)
	fmt.Fprintf(stdout, "原因：%s\n", su.Reason)
}

// printSuspensionDetail 输出一项暂停的完整信息：原暂停区间与原因、提前恢复
// 信息（若有）以及当前有效区间。原区间与原因永久保留；当前有效区间在提前
// 恢复后缩短为 [原起月, 恢复月)。
func printSuspensionDetail(s *state, su *suspension) {
	fmt.Fprintf(stdout, "客户：%s\n", su.CustomerID)
	fmt.Fprintf(stdout, "原暂停区间：%s（含）至 %s（不含，UTC 自然月，永久保留）\n", su.StartMonth, su.EndMonth)
	fmt.Fprintf(stdout, "原暂停原因：%s（永久保留）\n", su.Reason)
	effEnd := s.suspensionEffectiveEnd(su)
	if r := s.suspensionResumeOf(su); r != nil {
		fmt.Fprintf(stdout, "提前恢复：自 %s（含，UTC 自然月）起恢复服务，恢复原因：%s（恢复记录永久保留、不可改写或撤销）\n",
			r.ResumeMonth, r.Reason)
		fmt.Fprintf(stdout, "当前有效区间：%s（含）至 %s（不含，恢复月起不再受该项暂停限制；仍暂停的月份不补收月费）\n",
			su.StartMonth, effEnd)
	} else {
		fmt.Fprintln(stdout, "提前恢复：无")
		fmt.Fprintf(stdout, "当前有效区间：%s（含）至 %s（不含，与原暂停区间相同）\n", su.StartMonth, effEnd)
	}
}

// describeSuspension 返回覆盖指定月份的暂停当前有效区间的简短描述，用于
// 错误信息。
func describeSuspension(s *state, customerID, month string) string {
	for _, su := range s.suspensionsFor(customerID) {
		effEnd := s.suspensionEffectiveEnd(su)
		if monthInRange(month, su.StartMonth, effEnd) {
			return fmt.Sprintf("有效区间 %s（含）至 %s（不含），原因：%s", su.StartMonth, effEnd, su.Reason)
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
		fmt.Fprintln(stdout, "暂停区间（按起月升序；原区间与原因永久保留，另展示提前恢复信息与当前有效区间）：")
		for i, su := range list {
			effEnd := s.suspensionEffectiveEnd(su)
			fmt.Fprintf(stdout, "  %d. %s（含）至 %s（不含），原因：%s\n",
				i+1, su.StartMonth, su.EndMonth, su.Reason)
			if r := s.suspensionResumeOf(su); r != nil {
				fmt.Fprintf(stdout, "     提前恢复：自 %s（含，UTC 自然月）起恢复服务，恢复原因：%s（永久保留、不可改写或撤销）\n",
					r.ResumeMonth, r.Reason)
				fmt.Fprintf(stdout, "     当前有效区间：%s（含）至 %s（不含，恢复月起正常服务）\n", su.StartMonth, effEnd)
			} else {
				fmt.Fprintf(stdout, "     提前恢复：无；当前有效区间：%s（含）至 %s（不含，与原区间相同）\n",
					su.StartMonth, effEnd)
			}
		}
	}
	if hasMonth {
		var hit *suspension
		for _, su := range list {
			if monthInRange(month, su.StartMonth, s.suspensionEffectiveEnd(su)) {
				hit = su
				break
			}
		}
		if hit != nil {
			r := s.suspensionResumeOf(hit)
			fmt.Fprintf(stdout, "月份 %s：已暂停（处于当前有效区间 %s（含）至 %s（不含）），原暂停原因：%s",
				month, hit.StartMonth, s.suspensionEffectiveEnd(hit), hit.Reason)
			if r != nil {
				// 当前有效区间内的月份不可能晚于恢复月；保留恢复信息以便核对。
				fmt.Fprintf(stdout, "；该暂停已登记自 %s（含）起提前恢复", r.ResumeMonth)
			}
			fmt.Fprintln(stdout)
		} else {
			// 实际未暂停时区分：原本在暂停原区间内但因提前恢复而释放的月份，
			// 与从未被任何暂停覆盖的月份。
			var released *suspension
			for _, su := range list {
				if r := s.suspensionResumeOf(su); r != nil &&
					monthInRange(month, su.StartMonth, su.EndMonth) && month >= r.ResumeMonth {
					released = su
					break
				}
			}
			if released != nil {
				r := s.suspensionResumeOf(released)
				fmt.Fprintf(stdout, "月份 %s：未暂停（原暂停 %s（含）至 %s（不含）已登记提前恢复，自 %s（含）起恢复服务，恢复原因：%s，正常服务）\n",
					month, released.StartMonth, released.EndMonth, r.ResumeMonth, r.Reason)
			} else {
				fmt.Fprintf(stdout, "月份 %s：未暂停（不在任何暂停当前有效区间内，正常服务）\n", month)
			}
		}
	}
	return nil
}
