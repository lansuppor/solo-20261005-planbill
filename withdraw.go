package main

import (
	"fmt"
	"strings"
	"time"
)

// 本文件实现未封账用量的撤回（usage withdraw）与按用量标识的只读查询
// （usage show），用于纠正误导入的记录并保留追溯信息。撤回适用于固定单价
// 与阶梯客户：首次撤回须找到原记录，且按原时间换算的 UTC 自然月尚未封账；
// 已封账用量拒绝撤回，不改变账单或账后余额。成功后原客户、时间、数量与标识
// 永久保留，仅追加撤回状态与原因；撤回不可恢复，标识不可复用，相同内容的
// 导入重放仍按原记录判重跳过、不会恢复记录。撤回只作用于该条记录，不产生
// 账后流水事件，也不占用全局操作序号。

func cmdUsageWithdraw(dir, usageID, reason string) error {
	if strings.TrimSpace(usageID) == "" {
		return fmt.Errorf("用量标识不能为空")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("撤回原因不能为空")
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	u, ok := s.Usage[usageID]
	if !ok {
		return fmt.Errorf("用量标识 %q 不存在，无法撤回", usageID)
	}

	if w, ok := s.Withdrawals[usageID]; ok {
		// 同一用量相同原因的重复撤回：返回原记录且不写盘——即使后来该月
		// 已封账或暂停仍成功；改用其他原因则拒绝，撤回不可恢复。
		if w.Reason == reason {
			fmt.Fprintf(stdout, "用量 %q 已撤回且撤回原因相同，返回原记录（不写盘）：\n\n", usageID)
			printUsageRecord(s, u)
			return nil
		}
		return fmt.Errorf("用量 %q 已撤回（撤回原因 %q），改用其他原因重复撤回被拒绝；撤回不可恢复", usageID, w.Reason)
	}

	// 首次撤回：按原记录时间换算的 UTC 自然月尚未封账才允许；已封账用量
	// 拒绝撤回，不改变账单或账后余额。
	month := utcMonth(u.Time)
	if s.sealed(u.CustomerID, month) {
		return fmt.Errorf("用量 %q 属于客户 %s 的 %s（UTC 自然月），该月已封账，拒绝撤回；账单与账后余额不变",
			usageID, u.CustomerID, month)
	}

	w := &withdrawal{
		UsageID:   usageID,
		Reason:    reason,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.Withdrawals[usageID] = w

	// 撤回状态与原因整体原子保存后才报告成功；保存失败则一切不生效，
	// 不留下撤回标记，可原样重试。
	if err := s.save(); err != nil {
		delete(s.Withdrawals, usageID)
		return err
	}
	fmt.Fprintf(stdout, "已撤回用量 %q（客户 %s，%s，数量 %d），撤回不可恢复、标识不可复用；该记录不再参与结算与冲突检查：\n\n",
		usageID, u.CustomerID, month, u.Quantity)
	printUsageRecord(s, u)
	return nil
}

func cmdUsageShow(dir, usageID string) error {
	if strings.TrimSpace(usageID) == "" {
		return fmt.Errorf("用量标识不能为空")
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	u, ok := s.Usage[usageID]
	if !ok {
		return fmt.Errorf("用量标识 %q 不存在", usageID)
	}
	// 只读查询：不改写存档、不占用序号、不新增记录；成功或失败均不落盘。
	printUsageRecord(s, u)
	return nil
}

// printUsageRecord 输出一条用量记录的原始内容（标识、客户、时间、数量）、
// 换算后的 UTC 自然月，以及是否撤回与撤回原因；此外展示该记录在用量更正链
// 上的直接前身、后继及对应更正原因，无任何更正关联时明确说明。
func printUsageRecord(s *state, u *usageRecord) {
	cust := s.Customers[u.CustomerID] // 载入时已校验存在
	fmt.Fprintf(stdout, "用量标识：%s\n", u.ID)
	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	fmt.Fprintf(stdout, "时间：%s\n", u.Time)
	fmt.Fprintf(stdout, "UTC 月份：%s（UTC 自然月，左闭右开）\n", utcMonth(u.Time))
	fmt.Fprintf(stdout, "数量：%d\n", u.Quantity)
	if w, ok := s.Withdrawals[u.ID]; ok {
		fmt.Fprintf(stdout, "当前状态：已撤回（撤回原因：%s；不参与结算与冲突检查，不可恢复）\n", w.Reason)
	} else {
		fmt.Fprintln(stdout, "当前状态：有效（参与结算与冲突检查）")
	}
	// 直接前身：本记录是哪条原用量的更正替代。
	if pred := predecessorCorrection(s, u.ID); pred != nil {
		fmt.Fprintf(stdout, "直接前身：%s（由用量更正替代而来，更正原因：%s）\n", pred.UsageID, pred.Reason)
	} else {
		fmt.Fprintln(stdout, "直接前身：无（不是任何用量更正的替代记录）")
	}
	// 直接后继：本记录被更正后产生的替代记录。
	if succ := successorCorrection(s, u.ID); succ != nil {
		fmt.Fprintf(stdout, "直接后继：%s（本记录已被用量更正替代，更正原因：%s；更正不可撤销）\n", succ.ReplacementID, succ.Reason)
	} else {
		fmt.Fprintln(stdout, "直接后继：无（本记录未曾被更正）")
	}
}
