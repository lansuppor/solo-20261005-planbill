package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 本文件实现未封账用量的原子更正（usage correct）：用一条全新的替代记录
// 替换错误的客户、时间或数量，并永久保留来源。固定单价与阶梯客户均适用。
//
// 首次更正要求原记录存在且未撤回、原时间对应的 UTC 月份未封账；新标识非空、
// 不同于原标识且从未使用（已有同内容记录也不能充当替代记录）；新客户须存在，
// 新时间对应的 UTC 月份未封账且未暂停；替代记录按新客户固定单价或该月有效
// 阶梯方案做单条从零计价预检，全程整数计算并检查溢出（月累计溢出仍由结算
// 把关）。
//
// 成功后三项（原记录撤回标记、替代记录、关联登记）在同一次原子保存中落盘：
// 原内容不可改写，更正不可撤销，不占用账后操作序号。每条原用量只能更正一次，
// 以原标识识别；相同新标识、客户、解析后的时间点、数量与原因重放，返回原更正
// 及两条记录当前状态，不写盘，任一项不同拒绝。替代记录可继续更正或按原规则
// 撤回；后来封账、暂停、方案变化或替代记录已撤回，都不能阻止相同重放，也不能
// 恢复任何记录。

func cmdUsageCorrect(dir, oldID, newID, newCustomerID, newTimeText, qtyText, reason string) error {
	if strings.TrimSpace(oldID) == "" {
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
		return fmt.Errorf("新时间 %q 不是 RFC3339 格式：%v", newTimeText, err)
	}
	qty, err := strconv.ParseInt(strings.TrimSpace(qtyText), 10, 64)
	if err != nil {
		return fmt.Errorf("数量 %q 不是有符号 64 位整数范围内的整数: %w", qtyText, err)
	}
	if qty <= 0 {
		return fmt.Errorf("数量必须是正整数，收到 %d", qty)
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}

	// 幂等重放：每条原用量只能更正一次，以原标识识别该项更正。相同新标识、
	// 客户、解析后的时间点、数量与原因的重放返回原更正及两条记录当前状态，
	// 不写盘；任一项不同拒绝。后来封账、暂停、方案变化或替代记录已撤回都
	// 不阻止相同重放。
	if uc, ok := s.UsageCorrections[oldID]; ok {
		return replayUsageCorrection(s, uc, newID, newCustomerID, t, qty, reason)
	}

	// 首次更正：原记录必须存在且未撤回。
	old, ok := s.Usage[oldID]
	if !ok {
		return fmt.Errorf("原用量标识 %q 不存在，无法更正", oldID)
	}
	if s.isWithdrawn(oldID) {
		// 无关联的撤回是普通撤回；有关联则该记录已被更正，两种情况都不可
		// 再作为“首次更正”的原记录（已更正记录只能按相同内容重放）。
		if s.correctionByReplacement(oldID) != nil || s.correctionOfUsage(oldID) != nil {
			return fmt.Errorf("原用量 %q 已更正，每条原用量只能更正一次；相同内容的重放返回原更正", oldID)
		}
		return fmt.Errorf("原用量 %q 已撤回（撤回原因 %q），不能更正；如需修正请重新导入正确用量",
			oldID, s.Withdrawals[oldID].Reason)
	}
	// 原时间对应的 UTC 月份未封账。
	oldMonth := utcMonth(old.Time)
	if s.sealed(old.CustomerID, oldMonth) {
		return fmt.Errorf("原用量 %q 属于客户 %s 的 %s（UTC 自然月），该月已封账，拒绝更正；已有账单及账后余额不变",
			oldID, old.CustomerID, oldMonth)
	}

	// 新标识：不同于原标识且从未使用（标识一旦使用即永久保留，已撤回记录
	// 的标识同样不可再用）；已有同内容记录也不能充当替代记录。
	if newID == oldID {
		return fmt.Errorf("新用量标识不能与原用量标识 %q 相同", oldID)
	}
	if _, used := s.Usage[newID]; used {
		return fmt.Errorf("新用量标识 %q 已使用过，替代记录必须使用从未使用的全新标识（已有同内容记录也不能充当替代记录）", newID)
	}

	// 新客户须存在。
	newCust, ok := s.Customers[newCustomerID]
	if !ok {
		return fmt.Errorf("新客户标识 %q 不存在", newCustomerID)
	}

	// 新时间对应的 UTC 月份：未封账且未暂停（按新客户判断）。
	newMonth := t.UTC().Format("2006-01")
	if s.sealed(newCustomerID, newMonth) {
		return fmt.Errorf("客户 %s 的 %s 已封账，替代用量 %q 不得进入", newCustomerID, newMonth, newID)
	}
	if s.isSuspendedMonth(newCustomerID, newMonth) {
		return fmt.Errorf("客户 %s 的 %s 处于暂停区间（%s），暂停服务期间不接收用量，替代用量 %q 被拒绝",
			newCustomerID, newMonth, describeSuspension(s, newCustomerID, newMonth), newID)
	}

	// 单条从零计价预检：固定单价检查 数量×单价；阶梯客户按新时间对应 UTC
	// 月份的有效方案对单条数量从零计价。全程整数计算并检查溢出；月累计
	// 溢出仍由结算把关。
	if newCust.PlanID == "" {
		if _, err := mul64(qty, newCust.Price); err != nil {
			return fmt.Errorf("替代用量按新客户固定单价计价溢出：数量 %d × 单价 %d 超出有符号 64 位整数范围，拒绝更正",
				qty, newCust.Price)
		}
	} else {
		planID := s.effectivePlanID(newCust, newMonth)
		rec := &usageRecord{ID: newID, CustomerID: newCustomerID, Time: newTimeText, Quantity: qty}
		if _, err := tieredPrice(s.Plans[planID].Tiers, []*usageRecord{rec}); err != nil {
			return fmt.Errorf("替代用量按客户 %s 的 %s 月有效方案 %q 单条从零计价失败：%v；拒绝更正",
				newCustomerID, newMonth, planID, err)
		}
	}

	// 三项变更：原记录按更正原因撤回、替代记录入账、关联永久保存。原内容
	// 不做任何改写。
	newRec := &usageRecord{ID: newID, CustomerID: newCustomerID, Time: newTimeText, Quantity: qty}
	uc := &usageCorrection{
		OldUsageID: oldID,
		NewUsageID: newID,
		NewTime:    newTimeText,
		Reason:     reason,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.Usage[newID] = newRec
	s.Withdrawals[oldID] = &withdrawal{
		UsageID:   oldID,
		Reason:    reason,
		CreatedAt: uc.CreatedAt,
	}
	s.UsageCorrections[oldID] = uc

	// 三项整体原子保存后才报告成功；保存失败则全部不生效——不留下撤回
	// 标记、替代记录或关联，不占用新标识，可原样重试。
	if err := s.save(); err != nil {
		delete(s.Usage, newID)
		delete(s.Withdrawals, oldID)
		delete(s.UsageCorrections, oldID)
		return err
	}
	fmt.Fprintf(stdout, "已原子更正用量：原记录 %q 按更正原因标记撤回，替代记录 %q 已入账，关联永久保存；更正不可撤销、不占用账后操作序号：\n\n",
		oldID, newID)
	printUsageCorrection(s, uc)
	return nil
}

// replayUsageCorrection 处理同一原标识的更正重放：五项内容（新标识、新客户、
// 解析后的时间点、数量、原因）完全相同则返回原更正及两条记录当前状态且不写盘；
// 任一项不同拒绝。
func replayUsageCorrection(s *state, uc *usageCorrection, newID, newCustomerID string, newT time.Time, qty int64, reason string) error {
	savedT, err := time.Parse(time.RFC3339, uc.NewTime)
	if err != nil {
		// 载入时已校验为 RFC3339，此处不应发生。
		return fmt.Errorf("存档中的更正 %q 时间无法解析：%w", uc.OldUsageID, err)
	}
	same := uc.NewUsageID == newID &&
		s.Usage[uc.NewUsageID].CustomerID == newCustomerID &&
		savedT.Equal(newT.UTC()) &&
		s.Usage[uc.NewUsageID].Quantity == qty &&
		uc.Reason == reason
	// 时间比较的是瞬间：Z 与 +00:00 等表示同一时刻的写法视为相同。
	if !same {
		return fmt.Errorf("原用量 %q 已更正（新标识=%s 客户=%s 时间=%s 数量=%d 原因=%q），重放内容任一项不同即拒绝；更正不可撤销",
			uc.OldUsageID, uc.NewUsageID, s.Usage[uc.NewUsageID].CustomerID, uc.NewTime,
			s.Usage[uc.NewUsageID].Quantity, uc.Reason)
	}
	fmt.Fprintf(stdout, "原用量 %q 的更正已存在且内容相同，返回原更正及两条记录当前状态（不写盘）：\n\n", uc.OldUsageID)
	printUsageCorrection(s, uc)
	return nil
}

// printUsageCorrection 展示一次用量更正：原因，以及原记录与替代记录的完整
// 内容、UTC 月份与当前状态。两条记录均通过 printUsageRecord 展示，其直接
// 前身、后继与更正原因一并呈现。
func printUsageCorrection(s *state, uc *usageCorrection) {
	old := s.Usage[uc.OldUsageID] // 载入时已校验存在
	nu := s.Usage[uc.NewUsageID]
	fmt.Fprintf(stdout, "更正原因：%s\n", uc.Reason)
	fmt.Fprintln(stdout, "原记录（已按更正原因撤回，原内容永久保留、不可改写）：")
	printUsageRecordIndented(s, old, "  ")
	fmt.Fprintln(stdout, "替代记录（全新标识入账，参与结算与冲突检查）：")
	printUsageRecordIndented(s, nu, "  ")
}
