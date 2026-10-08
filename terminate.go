package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现阶梯客户的按月订阅终止：登记（customer terminate）与只读查询
// （customer termination）。终止月按 UTC 自然月解释，与操作当天无关：自终止月
// （含）起永久结束后续服务——不接收新用量、不结算、不封账、不收月费、不生成
// 零金额账单；终止前月份仍按当月有效方案、暂停、月费及封账规则补结算，历史
// 账务（账单、调整、收款、更正、退款、撤销与对账）全部保留且照常可用。
//
// 每客户只能登记一次，记录永久保留、不可修改或撤销，客户标识不能复用；以
// 客户识别终止登记，同月同原因重放返回原记录、不写盘，不同月份或原因拒绝。
// 终止不产生账后流水事件，也不占用全局操作序号。固定单价客户不适用终止。

func cmdCustomerTerminate(dir, customerID, month, reason string) error {
	if !validMonth(month) {
		return fmt.Errorf("终止月 %q 无效，必须是 YYYY-MM 形式（如 2027-03）", month)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("终止原因不能为空")
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
		return fmt.Errorf("客户 %s 是固定单价客户（单价 %d 分），未绑定阶梯方案，不适用按月订阅终止", customerID, cust.Price)
	}

	if existing, ok := s.Terminations[customerID]; ok {
		// 以客户识别终止登记：相同终止月与原因的重放返回原记录、不写盘——
		// 即使后来补结算了较早月份或发生了账后操作也成功；月份或原因不同
		// 则拒绝，记录永久保留、不可修改或撤销。
		if existing.Month == month && existing.Reason == reason {
			fmt.Fprintf(stdout, "客户 %s 的按月订阅终止已存在且内容相同，返回原记录（不写盘）：\n\n", customerID)
			printTermination(existing)
			return nil
		}
		return fmt.Errorf("客户 %s 已登记按月订阅终止（终止月 %s，原因 %q），终止月或原因不同即拒绝；每客户只能登记一次，记录永久保留、不可修改或撤销",
			customerID, existing.Month, existing.Reason)
	}

	// 首次登记要求终止月晚于该客户全部已封账月份：终止月及之后不得存在
	// 账单（已封账月份不受终止影响，保持原样）。
	var sealed []string
	for _, b := range s.Bills {
		if b.CustomerID == customerID && b.Month >= month {
			sealed = append(sealed, b.Month)
		}
	}
	if len(sealed) > 0 {
		sort.Strings(sealed)
		return fmt.Errorf("客户 %s 的 %s 已封账，终止月 %s 须晚于该客户全部已封账月份，拒绝登记；已有账单与账后余额保持不变",
			customerID, strings.Join(sealed, "、"), month)
	}
	// 终止月及之后不得存在有效（未撤回）用量：存在任一条即整项拒绝并指出
	// 冲突记录，不删除或改写任何用量；已撤回记录不阻塞，可存在于终止月及
	// 之后。
	var conflicts []string
	for _, u := range s.Usage {
		if s.isWithdrawn(u.ID) {
			continue
		}
		if u.CustomerID != customerID {
			continue
		}
		if m := utcMonth(u.Time); m >= month {
			conflicts = append(conflicts, fmt.Sprintf("用量 %s（%s，数量 %d）", u.ID, m, u.Quantity))
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return fmt.Errorf("终止月 %s（含）起已存在 %d 条有效用量，拒绝登记且不改写用量：%s；可先按原规则撤回或更正这些记录",
			month, len(conflicts), strings.Join(conflicts, "、"))
	}

	tm := &termination{
		CustomerID: customerID,
		Month:      month,
		Reason:     reason,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.Terminations[customerID] = tm

	// 终止登记整体原子落盘后才报告成功；保存失败则一切不生效，不占用该客户
	// 的终止登记，可原样重试。方案变更、暂停及提前恢复记录全部保留。
	if err := s.save(); err != nil {
		delete(s.Terminations, customerID)
		return err
	}
	fmt.Fprintf(stdout, "已登记按月订阅终止：客户 %s 自 %s（UTC 自然月，含）起永久结束后续服务——不接收新用量、不结算、不封账、不收月费、不生成零金额账单；终止前月份仍可按原规则补结算，历史账务全部保留：\n\n",
		customerID, month)
	printTermination(tm)
	return nil
}

// printTermination 输出一条按月订阅终止登记：客户、终止月与原因（登记与
// 相同重放使用）。记录永久保留、不可修改或撤销。
func printTermination(tm *termination) {
	fmt.Fprintf(stdout, "客户：%s\n", tm.CustomerID)
	fmt.Fprintf(stdout, "终止月：%s（UTC 自然月，含；自该月起永久结束后续服务）\n", tm.Month)
	fmt.Fprintf(stdout, "原因：%s\n", tm.Reason)
}

func cmdCustomerTermination(dir, customerID, month string, hasMonth bool) error {
	if hasMonth && !validMonth(month) {
		return fmt.Errorf("月份 %q 无效，必须是 YYYY-MM 形式（如 2027-03）", month)
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
		fmt.Fprintf(stdout, "客户 %s（%s）是固定单价客户（单价 %d 分），不适用按月订阅终止，全部月份正常服务\n",
			cust.ID, cust.Name, cust.Price)
		if hasMonth {
			fmt.Fprintf(stdout, "月份 %s：不适用终止（固定单价客户正常服务）\n", month)
		}
		return nil
	}

	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	tm := s.terminationOf(customerID)
	if tm == nil {
		fmt.Fprintln(stdout, "按月订阅终止：未登记（全部 UTC 自然月按原规则服务）")
		if hasMonth {
			fmt.Fprintf(stdout, "月份 %s：终止限制未生效（未登记终止，正常服务）\n", month)
		}
		return nil
	}
	fmt.Fprintf(stdout, "按月订阅终止：已登记（记录永久保留、不可修改或撤销）\n")
	fmt.Fprintf(stdout, "  终止月：%s（UTC 自然月，含）\n", tm.Month)
	fmt.Fprintf(stdout, "  原因：%s\n", tm.Reason)
	fmt.Fprintf(stdout, "  效果：自终止月（含）起不接收新用量、不结算、不封账、不收月费、不生成零金额账单；终止前月份仍按原规则补结算，历史账务保留\n")
	if hasMonth {
		if month >= tm.Month {
			fmt.Fprintf(stdout, "月份 %s：终止限制生效（不早于终止月 %s，不接收新用量、不结算、不封账、不收月费）\n", month, tm.Month)
		} else {
			fmt.Fprintf(stdout, "月份 %s：终止限制未生效（早于终止月 %s，按当月有效方案、暂停、月费及封账规则正常处理）\n", month, tm.Month)
		}
	}
	return nil
}
