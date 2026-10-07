package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现阶梯客户的按月暂停区间与按月提前恢复：登记（customer suspend）、
// 提前恢复（customer resume）与查询（customer suspensions）。区间按 UTC
// 自然月解释为 [起月, 结束月)，包含起月、不包含结束月；暂停月不接收用量、
// 不产生月费账单。区间可相接（相接视为连续暂停）但不得重叠，不在任何有效
// 区间内的月份正常服务。记录一旦写入永不修改或删除；暂停与提前恢复都不产生
// 账后流水事件，也不占用全局操作序号。固定单价客户不适用暂停与提前恢复。
//
// 提前恢复以客户与原起月识别目标暂停，把该项暂停的当前有效区间缩短为
// [原起月, 恢复月)：原暂停区间、原结束月与原因永久保留，恢复月（含）起
// 不再受该项暂停限制；每项暂停只能登记一次提前恢复，恢复记录不可改写或
// 撤销。用量导入与更正、单笔和批量结算、新暂停登记及载入校验统一使用当前
// 有效区间。

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
			printSuspension(s, existing)
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
	// 同一客户的当前有效区间不得重叠（可以相接，相接视为连续暂停）；提前
	// 恢复已缩短为 [原起月, 恢复月) 的区间只按其有效区间比较，释放月份可
	// 另登记暂停。其他客户互不影响。
	for _, su := range s.suspensionsFor(customerID) {
		effEnd := s.effectiveEndMonth(su)
		if startMonth < effEnd && endMonth > su.StartMonth {
			return fmt.Errorf("新区间 %s..%s（不含结束月）与客户 %s 已登记有效区间 %s..%s（不含结束月）重叠；区间可以相接但不得重叠",
				startMonth, endMonth, customerID, su.StartMonth, effEnd)
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
	printSuspension(s, su)
	return nil
}

// cmdCustomerResume 为一项已登记暂停登记按月提前恢复：以客户与原起月识别
// 目标暂停，把该项暂停的当前有效区间缩短为 [原起月, 恢复月)。仅适用于
// 绑定阶梯方案的客户；月份为 YYYY-MM（UTC 自然月，与操作当天无关），须
// 满足原起月 < 恢复月 < 原结束月，原因非空。原暂停区间、原结束月与原因
// 永久保留；每项暂停只能登记一次提前恢复，恢复记录永久保留、不可改写或
// 撤销，不占用账后操作序号。相同恢复月与原因的重放返回保存信息与当前
// 有效区间且不写盘；恢复月或原因不同拒绝。其他月份已封账、已有账单/用量/
// 方案安排均不阻止登记，也不被改动。
func cmdCustomerResume(dir, customerID, startMonth, resumeMonth, reason string) error {
	if !validMonth(startMonth) {
		return fmt.Errorf("目标暂停的原起月 %q 无效，必须是 YYYY-MM 形式（如 2026-11）", startMonth)
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
	target, ok := s.Suspensions[key]
	if !ok {
		return fmt.Errorf("客户 %s 自 %s 起的暂停不存在，提前恢复必须针对一项已登记的暂停", customerID, startMonth)
	}

	// 一项暂停只能登记一次提前恢复，以客户与原起月识别：相同恢复月与原因
	// 的重放返回保存信息与当前有效区间，不写盘——即使后来封账、方案变更或
	// 新增暂停也须成功；恢复月或原因不同则拒绝，恢复记录不可改写或撤销。
	if existing, ok := s.SuspensionResumes[key]; ok {
		if existing.ResumeMonth == resumeMonth && existing.Reason == reason {
			fmt.Fprintf(stdout, "客户 %s 自 %s 起暂停的提前恢复已存在且内容相同，返回保存信息与当前有效区间（不写盘）：\n\n",
				customerID, startMonth)
			printSuspension(s, target)
			return nil
		}
		return fmt.Errorf("客户 %s 自 %s 起的暂停已登记提前恢复（恢复月 %s，原因 %q），恢复月或原因不同即拒绝；"+
			"每项暂停只能登记一次提前恢复，恢复记录不可改写或撤销",
			customerID, startMonth, existing.ResumeMonth, existing.Reason)
	}

	// 首次登记：原起月 < 恢复月 < 原结束月。恢复月即恢复服务之月（含），
	// 不再受该项暂停限制；原结束月与原区间永久保留。
	if resumeMonth <= startMonth || resumeMonth >= target.EndMonth {
		return fmt.Errorf("恢复月 %s 越界：必须满足原起月 %s < 恢复月 < 原结束月 %s（区间均按 UTC 自然月，含起月、不含结束月）",
			resumeMonth, startMonth, target.EndMonth)
	}

	rec := &suspensionResume{
		CustomerID:  customerID,
		StartMonth:  startMonth,
		ResumeMonth: resumeMonth,
		Reason:      reason,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	s.SuspensionResumes[key] = rec

	// 恢复记录整体原子落盘后才报告成功；保存失败则一切不生效，该客户该
	// 原起月的恢复登记不被占用，可原样重试。
	if err := s.save(); err != nil {
		delete(s.SuspensionResumes, key)
		return err
	}
	fmt.Fprintf(stdout, "已登记提前恢复：客户 %s 自 %s（UTC 自然月，含）起的暂停于 %s（含）起恢复服务，无需等到原结束月 %s；恢复月起可正常导入用量、按当月有效方案结算：\n\n",
		customerID, startMonth, resumeMonth, target.EndMonth)
	printSuspension(s, target)
	return nil
}

// printSuspension 输出一项暂停的原区间与原因；已提前恢复时一并输出提前
// 恢复信息（恢复月、原因）与当前有效区间 [原起月, 有效结束月)。
func printSuspension(s *state, su *suspension) {
	fmt.Fprintf(stdout, "客户：%s\n", su.CustomerID)
	fmt.Fprintf(stdout, "暂停区间：%s（含）至 %s（不含，UTC 自然月）\n", su.StartMonth, su.EndMonth)
	fmt.Fprintf(stdout, "原因：%s\n", su.Reason)
	if r := s.resumeFor(su.CustomerID, su.StartMonth); r != nil {
		fmt.Fprintf(stdout, "提前恢复：%s（含）起恢复服务（原结束月 %s 之前），原因：%s\n",
			r.ResumeMonth, su.EndMonth, r.Reason)
		fmt.Fprintf(stdout, "当前有效区间：%s（含）至 %s（不含，UTC 自然月；%s 起不再受该项暂停限制）\n",
			su.StartMonth, r.ResumeMonth, r.ResumeMonth)
	}
}

// describeSuspension 返回覆盖指定月份的有效暂停区间的简短描述，用于错误
// 信息。月份只有落在当前有效区间 [原起月, 有效结束月) 内才视为暂停。
func describeSuspension(s *state, customerID, month string) string {
	for _, su := range s.suspensionsFor(customerID) {
		end := s.effectiveEndMonth(su)
		if monthInRange(month, su.StartMonth, end) {
			return fmt.Sprintf("区间 %s（含）至 %s（不含），原因：%s", su.StartMonth, end, su.Reason)
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
		fmt.Fprintln(stdout, "暂停区间（按起月升序；原区间包含起月、不包含结束月）：")
		for i, su := range list {
			fmt.Fprintf(stdout, "  %d. %s（含）至 %s（不含），原因：%s\n",
				i+1, su.StartMonth, su.EndMonth, su.Reason)
			// 已提前恢复：原区间与原因在上一行永久展示，另列提前恢复信息与
			// 当前有效区间；恢复月（含）起不再受该项暂停限制。
			if r := s.resumeFor(customerID, su.StartMonth); r != nil {
				fmt.Fprintf(stdout, "     提前恢复：%s（含）起恢复服务（早于原结束月 %s），原因：%s；当前有效区间 %s（含）至 %s（不含）\n",
					r.ResumeMonth, su.EndMonth, r.Reason, su.StartMonth, r.ResumeMonth)
			}
		}
	}
	if hasMonth {
		var hit *suspension
		for _, su := range list {
			if monthInRange(month, su.StartMonth, s.effectiveEndMonth(su)) {
				hit = su
				break
			}
		}
		if hit != nil {
			end := s.effectiveEndMonth(hit)
			if r := s.resumeFor(customerID, hit.StartMonth); r != nil {
				fmt.Fprintf(stdout, "月份 %s：已暂停（处于当前有效区间 %s（含）至 %s（不含）；原区间至 %s（不含），%s 起提前恢复解封），原因：%s\n",
					month, hit.StartMonth, end, hit.EndMonth, r.ResumeMonth, hit.Reason)
			} else {
				fmt.Fprintf(stdout, "月份 %s：已暂停（处于区间 %s（含）至 %s（不含）），原因：%s\n",
					month, hit.StartMonth, end, hit.Reason)
			}
		} else {
			fmt.Fprintf(stdout, "月份 %s：未暂停（不在任何暂停的当前有效区间内，正常服务）\n", month)
		}
	}
	return nil
}
