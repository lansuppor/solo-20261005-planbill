package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现阶梯客户的按月订阅暂停：登记（pause add）与只读查询
// （pause list）。暂停区间为 [起月, 结束月)，按 UTC 自然月解释，包含起月、
// 不包含结束月，与操作当天无关；暂停月不接收新用量、不产生月费账单，区间
// 之外正常服务，其他客户互不影响。一项暂停由客户与起月共同识别，记录一旦
// 写入永不修改；暂停不产生账后流水事件，也不占用全局操作序号。固定单价
// 客户不适用暂停。

func cmdPauseAdd(dir, customerID, startMonth, endMonth, reason string) error {
	if !validMonth(startMonth) {
		return fmt.Errorf("暂停起月 %q 无效，必须是 YYYY-MM 形式（如 2026-11）", startMonth)
	}
	if !validMonth(endMonth) {
		return fmt.Errorf("暂停结束月 %q 无效，必须是 YYYY-MM 形式（如 2027-02）", endMonth)
	}
	if endMonth <= startMonth {
		return fmt.Errorf("暂停结束月 %s 必须晚于起月 %s（区间含起月、不含结束月）", endMonth, startMonth)
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
		return fmt.Errorf("客户 %s 是固定单价客户，未绑定阶梯方案，不适用订阅暂停", customerID)
	}

	key := pauseKey(customerID, startMonth)
	if existing, ok := s.Pauses[key]; ok {
		// 以客户与起月共同识别一项暂停：相同结束月与原因的重放返回原记录，
		// 不写盘——即使后来结算了恢复后的月份也须成功；同一项内容不同则
		// 拒绝，已有暂停不可改写。
		if existing.EndMonth == endMonth && existing.Reason == reason {
			fmt.Fprintf(stdout, "客户 %s 自 %s 起的暂停已存在且内容相同，返回原记录（不重复生效）：\n\n", customerID, startMonth)
			printPause(existing)
			return nil
		}
		return fmt.Errorf("客户 %s 自 %s 起已登记暂停（结束月 %s，原因 %q），内容不同，已有暂停不可改写",
			customerID, startMonth, existing.EndMonth, existing.Reason)
	}

	// 新登记的起月须晚于该客户所有已封账月份：已封账账单不受影响。
	for _, b := range s.Bills {
		if b.CustomerID == customerID && b.Month >= startMonth {
			return fmt.Errorf("客户 %s 的 %s 已封账，暂停起月 %s 须晚于该客户所有已封账月份", customerID, b.Month, startMonth)
		}
	}
	// 同一客户的区间不得重叠；可以相接（相接视为连续暂停）。
	for _, p := range s.Pauses {
		if p.CustomerID == customerID && startMonth < p.EndMonth && p.StartMonth < endMonth {
			return fmt.Errorf("新区间 [%s, %s) 与客户 %s 已有暂停区间 [%s, %s) 重叠（区间可以相接，但不得重叠）",
				startMonth, endMonth, customerID, p.StartMonth, p.EndMonth)
		}
	}
	// 登记前检查区间内已导入用量：存在任一条即拒绝并指出冲突记录，
	// 不删除或改写用量。
	var conflict []string
	for _, u := range s.Usage {
		if u.CustomerID == customerID {
			if m := utcMonth(u.Time); m >= startMonth && m < endMonth {
				conflict = append(conflict, fmt.Sprintf("用量 %s（%s，数量 %d）", u.ID, m, u.Quantity))
			}
		}
	}
	if len(conflict) > 0 {
		sort.Strings(conflict)
		return fmt.Errorf("客户 %s 在暂停区间 [%s, %s) 内已存在 %d 条用量，拒绝登记（用量保持不变）：\n  %s",
			customerID, startMonth, endMonth, len(conflict), strings.Join(conflict, "\n  "))
	}

	p := &pause{
		CustomerID: customerID,
		StartMonth: startMonth,
		EndMonth:   endMonth,
		Reason:     reason,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	s.Pauses[key] = p

	// 暂停区间整体原子落盘后才报告成功；保存失败则一切不生效，
	// 该客户该起月不被占用，可原样重试。
	if err := s.save(); err != nil {
		delete(s.Pauses, key)
		return err
	}
	fmt.Fprintf(stdout, "已登记订阅暂停：客户 %s 在 [%s, %s)（UTC 自然月，含起月、不含结束月）暂停服务，暂停月不接收用量、不产生月费账单：\n\n",
		customerID, startMonth, endMonth)
	printPause(p)
	return nil
}

// printPause 输出一条暂停区间记录。
func printPause(p *pause) {
	fmt.Fprintf(stdout, "客户：%s\n", p.CustomerID)
	fmt.Fprintf(stdout, "暂停区间：[%s, %s)（UTC 自然月，含起月、不含结束月）\n", p.StartMonth, p.EndMonth)
	fmt.Fprintf(stdout, "原因：%s\n", p.Reason)
}

func cmdPauseList(dir, customerID, month string, hasMonth bool) error {
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

	// 只读查询：不改写数据、不占用序号、不新增记录。
	if cust.PlanID == "" {
		fmt.Fprintf(stdout, "客户 %s（%s）是固定单价客户（单价 %d 分），不适用订阅暂停\n",
			cust.ID, cust.Name, cust.Price)
		return nil
	}
	fmt.Fprintf(stdout, "客户：%s（%s）\n", cust.ID, cust.Name)
	pauses := s.pausesFor(customerID)
	if len(pauses) == 0 {
		fmt.Fprintln(stdout, "暂停区间：无（全部月份正常服务）")
	} else {
		fmt.Fprintln(stdout, "暂停区间（按起月升序，含起月、不含结束月）：")
		for i, p := range pauses {
			fmt.Fprintf(stdout, "  %d. [%s, %s)，原因：%s\n", i+1, p.StartMonth, p.EndMonth, p.Reason)
		}
	}
	if hasMonth {
		if p := s.pauseCovering(customerID, month); p != nil {
			fmt.Fprintf(stdout, "月份 %s：暂停中（处于区间 [%s, %s)，原因：%s）\n", month, p.StartMonth, p.EndMonth, p.Reason)
		} else {
			fmt.Fprintf(stdout, "月份 %s：正常服务（不在任何暂停区间内）\n", month)
		}
	}
	return nil
}
