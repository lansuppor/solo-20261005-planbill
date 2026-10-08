package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现方案变更的撤销（plan revoke）：取消误登记的价格安排并保留追溯。
//
// 撤销以客户与目标变更的生效月共同标识，仅限阶梯客户且目标变更必须存在，
// 撤销原因非空。目标变更的原内容（目标方案、原因与登记时间）永久保留，撤销
// 只追加一条不可撤销的撤销记录；每项变更只能撤销一次，撤销不产生账后流水
// 事件，也不占用全局操作序号。
//
// 有效方案只考虑未撤销变更：撤销目标后，自其生效月起沿用此前最后一项未
// 撤销变更的方案，无则用初始方案，直到其后下一项未撤销变更的生效月前；没有
// 后一项则覆盖之后所有月份。连续撤销按当时安排确定受影响区间，其他变更与
// 暂停、提前恢复记录不改动。
//
// 首次撤销时，受影响区间内有已封账账单则拒绝（区间外已封账不妨碍登记）；
// 区间内有效（未撤回）用量按撤销后方案逐条从零预检用量费，任一条溢出有符号
// 64 位范围则整项拒绝并指出记录，已撤回用量不参与；月累计及月费加用量费
// 溢出仍由结算拒绝且不封账。用量内容、已有账单快照与账后余额保持不变。
//
// 相同客户与目标生效月、相同原因的重放返回原撤销和当前安排、不写盘；后来
// 封账、追加变更或撤销其他项均不影响，换原因拒绝。撤销记录整体原子保存后
// 才报告成功。

func cmdPlanRevoke(dir, customerID, month, reason string) error {
	if !validMonth(month) {
		return fmt.Errorf("目标变更生效月 %q 无效，必须是 YYYY-MM 形式（如 2026-11）", month)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("撤销原因不能为空")
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
		return fmt.Errorf("客户 %s 是固定单价客户，未绑定阶梯方案，不能撤销方案变更", customerID)
	}

	key := planChangeKey(customerID, month)
	ch, ok := s.PlanChanges[key]
	if !ok {
		return fmt.Errorf("客户 %s 在 %s 的方案变更不存在，撤销目标必须先以 plan change 登记", customerID, month)
	}

	if existing, ok := s.PlanChangeRevokes[key]; ok {
		// 以客户与目标生效月识别撤销：相同原因重放返回原撤销和当前安排，
		// 不写盘——即使后来区间内已封账、追加了变更或撤销了其他项也成功；
		// 换原因拒绝，撤销记录不可改写或撤销。
		if existing.Reason == reason {
			fmt.Fprintf(stdout, "客户 %s 在 %s 的方案变更已撤销且撤销原因相同，返回原撤销和当前安排（不写盘）：\n\n", customerID, month)
			printPlanRevoke(s, ch, existing)
			return nil
		}
		return fmt.Errorf("客户 %s 在 %s 的方案变更已撤销（撤销原因 %q），改用其他原因重复撤销被拒绝；撤销不可撤销",
			customerID, month, existing.Reason)
	}

	// 受影响区间按撤销时的当前安排确定：[目标生效月, 其后下一项未撤销变更
	// 的生效月)，没有后一项则开放至之后所有月份。区间内沿用的方案为此前
	// 最后一项未撤销变更的方案，无则用初始方案。
	restoredPlanID, endMonth := s.revokeImpact(cust, month)

	// 首次撤销时区间内有已封账账单则拒绝：已封账账单的方案快照与金额不因
	// 撤销重算。区间外（区间结束月起）已封账不妨碍登记。
	for _, b := range s.Bills {
		if b.CustomerID != customerID {
			continue
		}
		if b.Month >= month && (endMonth == "" || b.Month < endMonth) {
			return fmt.Errorf("客户 %s 的 %s 已封账，落在撤销受影响区间 %s 内，拒绝撤销；已封账账单不重算，用量、账单快照与账后余额不变",
				customerID, b.Month, describeRevokeRange(month, endMonth))
		}
	}

	// 区间内有效（未撤回）用量按撤销后方案逐条从零预检用量费，任一条溢出
	// 有符号 64 位范围则整项拒绝并指出记录；已撤回用量不参与。月累计及月费
	// 加用量费溢出仍由结算把关（拒绝且不封账）。
	p := s.Plans[restoredPlanID]
	var ids []string
	for _, u := range s.Usage {
		if s.isWithdrawn(u.ID) {
			continue
		}
		if u.CustomerID == customerID {
			m := utcMonth(u.Time)
			if m >= month && (endMonth == "" || m < endMonth) {
				ids = append(ids, u.ID)
			}
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		u := s.Usage[id]
		rec := *u
		if _, err := tieredPrice(p.Tiers, []*usageRecord{&rec}); err != nil {
			return fmt.Errorf("用量 %s（%s，数量 %d）按撤销后方案 %q 单条从零计价失败：%v；整项撤销拒绝，用量、已有账单快照与账后余额不变",
				u.ID, utcMonth(u.Time), u.Quantity, restoredPlanID, err)
		}
	}

	rv := &planChangeRevoke{
		CustomerID: customerID,
		Month:      month,
		Reason:     reason,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.PlanChangeRevokes[key] = rv

	// 撤销记录整体原子保存后才报告成功；保存失败则一切不生效，不留下撤销
	// 标记，不占用该项变更的撤销登记，可原样重试。
	if err := s.save(); err != nil {
		delete(s.PlanChangeRevokes, key)
		return err
	}
	fmt.Fprintf(stdout, "已撤销方案变更：客户 %s 自 %s（UTC 自然月，含）起误登记的价格安排取消，原变更内容永久保留，撤销不可撤销；其他变更、暂停与提前恢复记录不改动：\n\n",
		customerID, month)
	printPlanRevoke(s, ch, rv)
	return nil
}

// revokeImpact 返回撤销客户在 targetMonth 的变更后的受影响区间与沿用方案：
// restoredPlanID 为此前最后一项未撤销变更的方案（无则为初始方案），endMonth
// 为其后下一项未撤销变更的生效月（区间不含该月），空字符串表示没有后一项、
// 覆盖之后所有月份。目标变更本身（无论是否已在调用前撤销）都跳过不计。
func (s *state) revokeImpact(cust *customer, targetMonth string) (restoredPlanID, endMonth string) {
	restoredPlanID = cust.PlanID
	for _, ch := range s.activePlanChangesFor(cust.ID) {
		if ch.Month == targetMonth {
			continue
		}
		if ch.Month < targetMonth {
			restoredPlanID = ch.PlanID
		} else if ch.Month > targetMonth {
			endMonth = ch.Month
			break
		}
	}
	return restoredPlanID, endMonth
}

// describeRevokeRange 以简短中文描述撤销受影响区间。
func describeRevokeRange(startMonth, endMonth string) string {
	if endMonth == "" {
		return fmt.Sprintf("%s（含）起及之后所有月份", startMonth)
	}
	return fmt.Sprintf("%s（含）至 %s（不含）", startMonth, endMonth)
}

// printPlanRevoke 输出一项方案变更撤销：目标变更原内容（永久保留）、撤销
// 原因与撤销后当前安排（受影响区间及区间内沿用的方案）。当前安排取当前库
// 状态，因此相同重放在后来追加变更或撤销其他项后反映最新区间。
func printPlanRevoke(s *state, ch *planChange, rv *planChangeRevoke) {
	tp := s.Plans[ch.PlanID] // 载入时已校验存在
	cust := s.Customers[ch.CustomerID]
	restoredPlanID, endMonth := s.revokeImpact(cust, ch.Month)
	rp := s.Plans[restoredPlanID]
	fmt.Fprintln(stdout, "目标变更（原内容永久保留，不可改写）：")
	fmt.Fprintf(stdout, "  客户：%s\n", ch.CustomerID)
	fmt.Fprintf(stdout, "  生效月：%s（UTC 自然月，含）\n", ch.Month)
	fmt.Fprintf(stdout, "  目标方案：%s（%s）：月费 %d 分，规则 %s\n", tp.ID, tp.Name, tp.MonthlyFee, formatTiers(tp.Tiers))
	fmt.Fprintf(stdout, "  原变更原因：%s\n", ch.Reason)
	fmt.Fprintf(stdout, "撤销原因：%s（撤销记录永久保留、不可撤销；每项变更只能撤销一次）\n", rv.Reason)
	fmt.Fprintf(stdout, "受影响区间：%s\n", describeRevokeRange(ch.Month, endMonth))
	fmt.Fprintf(stdout, "区间内有效方案：%s（%s）：月费 %d 分，规则 %s（撤销后沿用上一安排，无前置变更则为初始方案）\n",
		rp.ID, rp.Name, rp.MonthlyFee, formatTiers(rp.Tiers))
	if endMonth == "" {
		fmt.Fprintf(stdout, "其后无未撤销变更，%s 起之后所有月份均按方案 %s 计价\n", ch.Month, restoredPlanID)
	} else {
		next := s.Plans[s.effectivePlanID(cust, endMonth)]
		fmt.Fprintf(stdout, "自 %s（含）起恢复下一项未撤销变更的安排：%s（%s）\n", endMonth, next.ID, next.Name)
	}
}
