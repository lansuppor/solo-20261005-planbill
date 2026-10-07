package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 本文件实现未封账用量的原子更正（usage correct）：用一条全新记录替代错误
// 的客户、时间或数量，并永久保留来源。固定单价与阶梯客户均适用。
//
// 首次更正要求原记录存在且未撤回、按原时间换算的 UTC 自然月未封账；新标识
// 非空、不同于原标识且从未使用（已有同内容记录也不能充当替代记录）；新客户
// 须存在，新时间对应的 UTC 自然月未封账且未暂停。数量按新客户固定单价或
// 该月有效阶梯方案做单条从零计价预检，全程整数计算并检查溢出；月累计溢出
// 仍由结算把关。
//
// 成功时原记录按更正原因标记撤回、登记替代记录并永久保存关联，三项在同一
// 次原子保存中落盘后才报告成功；原内容不可改写，更正不可撤销，不占用账后
// 操作序号。任一步失败都非零退出并说明原因，全部业务状态不变，不占新标识，
// 可原样重试。
//
// 每条原用量只能更正一次，以原标识识别该项更正。相同新标识、客户、解析后
// 的时间点、数量与原因重放，返回原更正及两条记录当前状态，不写盘；任一项
// 不同拒绝。替代记录可继续更正或按原规则撤回；后来封账、暂停、方案变化或
// 替代记录已撤回，都不能阻止相同重放，也不能恢复任何记录。

func cmdUsageCorrect(dir, origID, newID, newCustomerID, newTimeText, qtyText, reason string) error {
	if strings.TrimSpace(origID) == "" {
		return fmt.Errorf("原用量标识不能为空")
	}
	if strings.TrimSpace(newID) == "" {
		return fmt.Errorf("新用量标识不能为空")
	}
	if strings.TrimSpace(newCustomerID) == "" {
		return fmt.Errorf("新客户标识不能为空")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("更正原因不能为空")
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(newTimeText))
	if err != nil {
		return fmt.Errorf("新时间 %q 不是 RFC3339 格式：%w", newTimeText, err)
	}
	qty, err := strconv.ParseInt(strings.TrimSpace(qtyText), 10, 64)
	if err != nil {
		return fmt.Errorf("新数量 %q 不是有符号 64 位整数: %w", qtyText, err)
	}
	if qty <= 0 {
		return fmt.Errorf("新数量必须是正整数，收到 %d", qty)
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}

	// 幂等重放优先于一切首次校验：以原标识识别该项更正。后来封账、暂停、
	// 方案变化或替代记录已撤回，都不能阻止相同重放，也不能恢复任何记录。
	if uc, ok := s.UsageCorrections[origID]; ok {
		if uc.ReplacementID == newID && uc.NewCustomerID == newCustomerID &&
			uc.NewQuantity == qty && uc.Reason == reason {
			savedT, perr := time.Parse(time.RFC3339, uc.NewTime)
			if perr == nil && savedT.Equal(t) {
				fmt.Fprintf(stdout, "用量 %q 的更正已存在且内容相同，返回原更正及两条记录当前状态（不写盘）：\n\n", origID)
				printUsageCorrection(s, uc)
				return nil
			}
		}
		return fmt.Errorf("用量 %q 已更正（替代记录 %q，客户 %s，时间 %s，数量 %d，原因 %q），"+
			"新标识、客户、解析后的时间点、数量或原因任一项不同即拒绝；每条原用量只能更正一次，更正不可撤销",
			origID, uc.ReplacementID, uc.NewCustomerID, uc.NewTime, uc.NewQuantity, uc.Reason)
	}

	orig, ok := s.Usage[origID]
	if !ok {
		return fmt.Errorf("原用量标识 %q 不存在，无法更正", origID)
	}
	// 已撤回（含被其他更正替代，该项已在上面按重放处理）的记录不能再更正；
	// 更正只能作用于一条仍有效的原记录。
	if s.isWithdrawn(origID) {
		w := s.Withdrawals[origID]
		return fmt.Errorf("原用量 %q 已撤回（撤回原因 %q），不能更正；撤回不可恢复", origID, w.Reason)
	}
	origMonth := utcMonth(orig.Time)
	if s.sealed(orig.CustomerID, origMonth) {
		return fmt.Errorf("原用量 %q 属于客户 %s 的 %s（UTC 自然月），该月已封账，拒绝更正；账单与账后余额不变",
			origID, orig.CustomerID, origMonth)
	}

	if newID == origID {
		return fmt.Errorf("新用量标识 %q 与原用量标识相同，替代记录必须使用全新标识", newID)
	}
	if _, used := s.Usage[newID]; used {
		// 标识从未使用：即使已有一条客户、时间、数量完全相同的记录，也不能
		// 让它充当替代记录。
		return fmt.Errorf("新用量标识 %q 已被使用，替代记录必须使用从未使用的全新标识（已有同内容记录也不能充当替代记录）", newID)
	}
	newCust, ok := s.Customers[newCustomerID]
	if !ok {
		return fmt.Errorf("新客户标识 %q 不存在", newCustomerID)
	}
	newMonth := t.UTC().Format("2006-01")
	if s.sealed(newCustomerID, newMonth) {
		return fmt.Errorf("新用量属于客户 %s 的 %s（UTC 自然月），该月已封账，拒绝更正", newCustomerID, newMonth)
	}
	if s.isSuspendedMonth(newCustomerID, newMonth) {
		return fmt.Errorf("新用量属于客户 %s 的 %s（UTC 自然月），该月处于暂停区间（%s），暂停服务期间不接收用量，拒绝更正",
			newCustomerID, newMonth, describeSuspension(s, newCustomerID, newMonth))
	}

	// 单条从零计价预检：固定单价检查 数量×单价；阶梯客户按新时间换算的
	// UTC 月份的有效方案计价，检查分段金额不溢出。月累计溢出仍由结算把关。
	repl := &usageRecord{ID: newID, CustomerID: newCustomerID, Time: strings.TrimSpace(newTimeText), Quantity: qty}
	if newCust.PlanID == "" {
		if _, err := mul64(qty, newCust.Price); err != nil {
			return fmt.Errorf("新数量 %d × 客户 %s 单价 %d 金额溢出有符号 64 位整数范围，拒绝更正",
				qty, newCustomerID, newCust.Price)
		}
	} else {
		planID := s.effectivePlanID(newCust, newMonth)
		if _, err := tieredPrice(s.Plans[planID].Tiers, []*usageRecord{repl}); err != nil {
			return fmt.Errorf("按客户 %s 的 %s 月有效方案 %q 对新数量单条从零计价失败：%v；拒绝更正",
				newCustomerID, newMonth, planID, err)
		}
	}

	// 三项变更先在内存完成，再整体原子保存：原记录撤回标记、替代记录、
	// 更正关联。原内容不改写，不占用账后操作序号。
	now := time.Now().UTC().Format(time.RFC3339)
	s.Withdrawals[origID] = &withdrawal{UsageID: origID, Reason: reason, CreatedAt: now}
	s.Usage[newID] = repl
	uc := &usageCorrection{
		UsageID:       origID,
		ReplacementID: newID,
		NewCustomerID: newCustomerID,
		NewTime:       repl.Time,
		NewQuantity:   qty,
		Reason:        reason,
		CreatedAt:     now,
	}
	s.UsageCorrections[origID] = uc

	if err := s.save(); err != nil {
		// 保存失败：三项全部回滚，全部业务状态不变，失败不占用新标识，
		// 原记录不留下撤回标记或关联，可原样重试。
		delete(s.Withdrawals, origID)
		delete(s.Usage, newID)
		delete(s.UsageCorrections, origID)
		return err
	}

	fmt.Fprintf(stdout, "已更正用量 %q：原记录按更正原因标记撤回，替代记录 %q 已登记，关联永久保存；原内容不可改写、更正不可撤销、不占用账后操作序号：\n\n",
		origID, newID)
	printUsageCorrection(s, uc)
	return nil
}

// predecessorCorrection 返回以指定用量为替代记录的更正（即该记录的直接
// 前身关联）；无前身时返回 nil。
func predecessorCorrection(s *state, usageID string) *usageCorrection {
	for _, uc := range s.UsageCorrections {
		if uc.ReplacementID == usageID {
			return uc
		}
	}
	return nil
}

// successorCorrection 返回指定用量作为原记录的更正（即该记录的直接后继
// 关联）；无后继时返回 nil。
func successorCorrection(s *state, usageID string) *usageCorrection {
	return s.UsageCorrections[usageID]
}

// printUsageCorrection 输出一项用量更正：更正原因、原记录与替代记录的完整
// 内容、各自 UTC 月份与当前状态，以及二者的替代关联。记录状态取当前库状态：
// 替代记录可能已被继续更正或撤回，原记录始终保持已撤回。
func printUsageCorrection(s *state, uc *usageCorrection) {
	fmt.Fprintf(stdout, "更正原因：%s\n", uc.Reason)
	orig := s.Usage[uc.UsageID]       // 载入时已校验存在
	repl := s.Usage[uc.ReplacementID] // 载入时已校验存在
	fmt.Fprintln(stdout, "原记录（已按更正原因标记撤回，原内容永久保留、不可改写）：")
	printUsageRecord(s, orig)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "替代记录（全新标识，承载更正后的客户、时间与数量）：")
	printUsageRecord(s, repl)
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "关联：%s（%s，UTC 月份 %s）→ %s（%s，UTC 月份 %s），原因：%s；更正不可撤销\n",
		orig.ID, orig.CustomerID, utcMonth(orig.Time),
		repl.ID, repl.CustomerID, utcMonth(repl.Time), uc.Reason)
}
