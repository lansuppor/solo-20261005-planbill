package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现方案变更撤销（plan revoke）：取消误登记的价格安排并保留追溯。
//
// 撤销接受客户、目标变更的 YYYY-MM 生效月和非空原因，仅限阶梯客户且目标
// 变更必须存在。原变更内容（目标方案与原因）永久保留，只追加不可撤销的
// 撤销记录；每项变更只能撤销一次，撤销不占用账后操作序号。
//
// 有效方案只考虑未撤销变更：撤销目标后，自其生效月起沿用此前最后一项未
// 撤销变更的方案，无则用初始方案，直到其后下一项未撤销变更的生效月前；
// 没有后一项则覆盖之后所有月份。连续撤销按当时安排确定受影响区间；其他
// 变更和暂停、提前恢复记录不改动。
//
// 首次撤销时，受影响区间内有已封账账单则拒绝，区间外已封账不妨碍登记。
// 区间内有效用量按撤销后方案逐条从零预检用量费，任一条溢出有符号 64 位
// 范围则整项拒绝并指出记录，已撤回用量不参与；月累计及月费加用量费溢出
// 仍由结算拒绝且不封账。用量内容、已有账单快照与账后余额保持不变。

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
		return fmt.Errorf("客户 %s 在 %s 的方案变更不存在，无法撤销（目标必须是已登记的方案变更）", customerID, month)
	}

	if rv, ok := s.PlanChangeRevocations[key]; ok {
		// 以客户与目标生效月识别撤销：相同原因的重放返回原撤销与当前安排、
		// 不写盘——即使后来相关月份已封账、追加了新变更或撤销了其他项也
		// 成功；换原因拒绝，撤销记录不可改写、不可撤销。
		if rv.Reason == reason {
			fmt.Fprintf(stdout, "客户 %s 在 %s 的方案变更撤销已存在且原因相同，返回原撤销与当前安排（不写盘）：\n\n",
				customerID, month)
			printPlanRevocation(s, rv, ch)
			return nil
		}
		return fmt.Errorf("客户 %s 在 %s 的方案变更已撤销（撤销原因 %q），改用其他原因重复撤销被拒绝；撤销记录不可改写、不可撤销",
			customerID, month, rv.Reason)
	}

	// 受影响区间 [month, end)：自目标生效月（含）起，直到其后下一项未撤销
	// 变更的生效月前；没有后一项则覆盖之后所有月份（end 为空）。连续撤销时
	// 该区间按当时安排（目标本身仍有效）确定。
	end := s.nextActiveChangeMonth(customerID, month)

	// 首次撤销时，受影响区间内有已封账账单则拒绝：已封账账单快照不因撤销
	// 改变，区间外已封账不妨碍登记。
	var sealed []string
	for _, b := range s.Bills {
		if b.CustomerID == customerID && b.Month >= month && (end == "" || b.Month < end) {
			sealed = append(sealed, b.Month)
		}
	}
	if len(sealed) > 0 {
		sort.Strings(sealed)
		return fmt.Errorf("客户 %s 的方案变更 %s 受影响区间 %s 内已有封账账单（月份 %s），拒绝撤销；账单快照与账后余额保持不变",
			customerID, month, describeMonthRange(month, end), strings.Join(sealed, "、"))
	}

	// 区间内有效（未撤回）用量按撤销后方案逐条从零预检用量费：任一条按其
	// UTC 月份撤销后有效方案计价溢出有符号 64 位范围则整项拒绝并指出记录；
	// 已撤回用量不参与。月累计及月费加用量费溢出仍由结算把关。
	var ids []string
	for _, u := range s.Usage {
		if s.isWithdrawn(u.ID) {
			continue
		}
		if u.CustomerID == customerID {
			m := utcMonth(u.Time)
			if m >= month && (end == "" || m < end) {
				ids = append(ids, u.ID)
			}
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		u := s.Usage[id]
		m := utcMonth(u.Time)
		planID := s.effectivePlanIDIgnoring(cust, m, key)
		p := s.Plans[planID] // 载入时已校验存在
		rec := *u
		if _, err := tieredPrice(p.Tiers, []*usageRecord{&rec}); err != nil {
			return fmt.Errorf("用量 %s（%s，数量 %d）按撤销后方案 %q 单条从零计价失败：%v；整项撤销拒绝，用量、账单与账后余额均不修改",
				u.ID, m, u.Quantity, planID, err)
		}
	}

	rv := &planChangeRevocation{
		CustomerID: customerID,
		Month:      month,
		Reason:     reason,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.PlanChangeRevocations[key] = rv

	// 撤销记录整体原子落盘后才报告成功；保存失败则一切不生效，不占用该项
	// 变更的撤销登记，可原样重试。撤销只追加记录，不改动用量内容、已有账单
	// 快照、账后余额、其他变更、暂停与提前恢复记录。
	if err := s.save(); err != nil {
		delete(s.PlanChangeRevocations, key)
		return err
	}
	fmt.Fprintf(stdout, "已撤销方案变更：客户 %s 自 %s（UTC 自然月，含）起的目标安排取消，原变更内容永久保留，撤销记录不可撤销、不占用账后操作序号：\n\n",
		customerID, month)
	printPlanRevocation(s, rv, ch)
	return nil
}

// printPlanRevocation 输出一项方案变更撤销：原目标变更内容与原原因、撤销
// 原因、受影响区间及该区间当前实际有效方案。撤销后安排为：自目标生效月起
// 沿用此前最后一项未撤销变更的方案，无则用初始方案。
func printPlanRevocation(s *state, rv *planChangeRevocation, ch *planChange) {
	cust := s.Customers[rv.CustomerID] // 载入时已校验存在
	tp := s.Plans[ch.PlanID]           // 载入时已校验存在
	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "目标变更生效月：%s（UTC 自然月，含）\n", ch.Month)
	fmt.Fprintf(stdout, "原变更内容（永久保留）：改用方案 %s（%s）：月费 %d 分，规则 %s，原原因：%s\n",
		tp.ID, tp.Name, tp.MonthlyFee, formatTiers(tp.Tiers), ch.Reason)
	fmt.Fprintf(stdout, "当前状态：已撤销（撤销原因：%s；撤销记录不可撤销，原客户月份不可复用）\n", rv.Reason)
	end := s.nextActiveChangeMonth(rv.CustomerID, rv.Month)
	fmt.Fprintf(stdout, "受影响区间：%s\n", describeMonthRange(rv.Month, end))
	// 撤销后该区间的实际有效方案：目标变更已撤销，effectivePlanID 只考虑
	// 未撤销变更，故目标月本身的有效方案即回退后的方案。
	p := s.Plans[s.effectivePlanID(cust, rv.Month)]
	fmt.Fprintf(stdout, "撤销后有效方案：%s（%s）：月费 %d 分，规则 %s（自 %s 起沿用）\n",
		p.ID, p.Name, p.MonthlyFee, formatTiers(p.Tiers), rv.Month)
}

// describeMonthRange 以人类可读形式展示左闭右开月份区间：end 为空表示覆盖
// 之后所有月份。
func describeMonthRange(start, end string) string {
	if end == "" {
		return fmt.Sprintf("%s（含）起覆盖之后所有月份", start)
	}
	return fmt.Sprintf("%s（含）至 %s（不含，UTC 自然月）", start, end)
}

// nextActiveChangeMonth 返回某客户严格晚于 afterMonth 的最早一项未撤销
// 变更的生效月；不存在时返回空串（受影响区间覆盖之后所有月份）。
func (s *state) nextActiveChangeMonth(customerID, afterMonth string) string {
	end := ""
	for _, ch := range s.activePlanChanges(customerID) {
		if ch.Month > afterMonth && (end == "" || ch.Month < end) {
			end = ch.Month
		}
	}
	return end
}

// effectivePlanIDIgnoring 与 effectivePlanID 相同，但额外忽略一项变更
// （以键给出），用于撤销登记前按“撤销后安排”对用量做计价预检：目标变更
// 尚未写入撤销标记，需要显式排除。
func (s *state) effectivePlanIDIgnoring(cust *customer, month, ignoreKey string) string {
	planID := cust.PlanID
	latest := ""
	for _, ch := range s.PlanChanges {
		if ch.CustomerID != cust.ID {
			continue
		}
		if planChangeKey(ch.CustomerID, ch.Month) == ignoreKey {
			continue
		}
		if s.planChangeRevoked(ch.CustomerID, ch.Month) {
			continue
		}
		if ch.Month <= month && ch.Month >= latest {
			latest = ch.Month
			planID = ch.PlanID
		}
	}
	return planID
}
