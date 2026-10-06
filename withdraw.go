package main

import (
	"fmt"
	"strings"
	"time"
)

// 本文件实现未封账用量的撤回（usage withdraw）与按用量标识的只读查询
// （usage show）。撤回用于纠正误导入的记录：原客户、时间、数量与标识永久
// 保留，只追加撤回状态与原因；撤回不可恢复，标识不可复用。已撤回用量不
// 参与单笔及批量结算、方案变更预检与暂停区间冲突检查，但相同内容的重放
// 仍按原内容判重跳过。撤回只作用于该条记录，不产生账后流水事件，也不
// 占用全局操作序号。固定单价客户与阶梯客户的用量均可撤回。

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
		// 相同标识与相同原因重复撤回：幂等返回原记录，不写盘——即使后来
		// 该月已封账或暂停仍成功；改用其他原因则拒绝，撤回不可恢复。
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
		return fmt.Errorf("客户 %s 的 %s 已封账，已封账用量 %q 不得撤回；账单与账后余额保持不变",
			u.CustomerID, month, usageID)
	}

	s.Withdrawals[usageID] = &usageWithdrawal{
		UsageID:   usageID,
		Reason:    reason,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// 撤回状态与原因整体原子保存后才报告成功；保存失败则一切不生效，
	// 不留下撤回标记，可原样重试。
	if err := s.save(); err != nil {
		delete(s.Withdrawals, usageID)
		return err
	}
	fmt.Fprintf(stdout, "已撤回用量 %q（客户 %s 的 %s，数量 %d），该记录不再参与计费，原内容永久保留：\n\n",
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
// 换算后的 UTC 自然月，以及是否已撤回与撤回原因。
func printUsageRecord(s *state, u *usageRecord) {
	fmt.Fprintf(stdout, "用量标识：%s\n", u.ID)
	fmt.Fprintf(stdout, "客户：%s\n", u.CustomerID)
	fmt.Fprintf(stdout, "时间：%s（原始输入）\n", u.Time)
	fmt.Fprintf(stdout, "UTC 月份：%s（UTC 自然月，左闭右开）\n", utcMonth(u.Time))
	fmt.Fprintf(stdout, "数量：%d\n", u.Quantity)
	if w, ok := s.Withdrawals[u.ID]; ok {
		fmt.Fprintf(stdout, "状态：已撤回（撤回原因：%s）；不再参与计费，原内容永久保留，撤回不可恢复\n", w.Reason)
	} else {
		fmt.Fprintln(stdout, "状态：有效（参与计费）")
	}
}
