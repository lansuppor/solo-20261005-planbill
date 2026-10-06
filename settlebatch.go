package main

import (
	"fmt"
	"strings"
)

// 本文件实现按客户账期清单的整批结算（bill settle-batch）：一次确认多项
// 客户与 UTC 自然月配对，同时生成账单并封账。整份清单先确认可结算性，
// 任一项非法、无用量或计价溢出都拒绝整批；所有新增账单与封账状态在同
// 一次原子保存中持久化，完整保存成功后才报告成功。批量结算不新增账后
// 事件，也不占用全局操作序号。

// buildBill 为未结算的客户月份构造账单（不落盘）：归集该 UTC 自然月的全部
// 用量，固定单价客户逐条 数量×单价，阶梯客户按账期月有效方案从零累计分档
// 计价（保留跨档明细与方案快照），原总金额含该方案的一次整月月费。没有用量
// 且有效方案月费为 0（或固定单价客户），或计价溢出时返回错误。
func buildBill(s *state, cust *customer, month string) (*bill, error) {
	recs := monthUsage(s, cust.ID, month)
	if len(recs) == 0 && !monthlyFeeBillable(s, cust, month) {
		return nil, fmt.Errorf("该 UTC 自然月没有用量")
	}
	if cust.PlanID != "" {
		return buildTieredBill(s, cust, month, recs)
	}
	return buildFixedBill(cust, month, recs)
}

// settleItem 是批量结算清单中一项的结算结果。
type settleItem struct {
	pos        int    // 清单中的位置（1 起）
	customerID string // 客户标识
	month      string // YYYY-MM（UTC）
	b          *bill  // 新增或已有的账单
	isNew      bool   // true 表示本次新增并封账；false 表示返回原账单
}

func cmdBillSettleBatch(dir string, args []string) error {
	if len(args) == 0 || len(args)%2 != 0 {
		return usageError("用法：bill settle-batch <客户标识> <YYYY-MM> [<客户标识> <YYYY-MM> ...]（至少一项配对，参数个数须为偶数）")
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}

	// 先校验整份清单：任何问题都在此阶段收集，全部通过后才一次性修改并
	// 保存，保证任一项非法都整批拒绝，不留下新账单，也不封闭任何月份。
	var items []settleItem
	var problems []string
	seen := make(map[string]int)       // 客户|月份 -> 首次出现的清单位置
	newBills := make(map[string]*bill) // 本次新增账单（保存失败时整体回滚）

	for i := 0; i < len(args); i += 2 {
		pos := i/2 + 1
		customerID, month := args[i], args[i+1]
		where := fmt.Sprintf("第 %d 项（客户 %q，月份 %q）", pos, customerID, month)
		if strings.TrimSpace(customerID) == "" {
			problems = append(problems, where+"：客户标识不能为空")
			continue
		}
		if !validMonth(month) {
			problems = append(problems, where+"：月份无效，必须是 YYYY-MM 形式（如 2026-09）")
			continue
		}
		cust, ok := s.Customers[customerID]
		if !ok {
			problems = append(problems, where+"：客户不存在")
			continue
		}
		key := billKey(customerID, month)
		if first, dup := seen[key]; dup {
			problems = append(problems, fmt.Sprintf("%s：与第 %d 项重复（同一客户同一月份不得在清单中重复出现）", where, first))
			continue
		}
		seen[key] = pos

		if existing, ok := s.Bills[key]; ok {
			// 已结算项返回原账单，不重新计费；已有账单的标识、快照、金额与
			// 账后历史均不改变。
			items = append(items, settleItem{pos: pos, customerID: customerID, month: month, b: existing})
			continue
		}
		// 未结算项按该账期的计费规则独立计价：不跨客户或月份共享累计量。
		b, err := buildBill(s, cust, month)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s：%v", where, err))
			continue
		}
		newBills[key] = b
		items = append(items, settleItem{pos: pos, customerID: customerID, month: month, b: b, isNew: true})
	}

	if len(problems) > 0 {
		return fmt.Errorf("批量结算失败，整批未生效（不新增账单、不封闭任何月份，共 %d 个问题）：\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}

	// 全部项目已结算时不改写存档；否则所有新账单与封账状态整体原子保存，
	// 保存失败则整批不生效，原有账单、用量和其他状态不变，可排除故障后
	// 按原清单重试。
	newCount := len(newBills)
	if newCount > 0 {
		for key, b := range newBills {
			s.Bills[key] = b
		}
		if err := s.save(); err != nil {
			for key := range newBills {
				delete(s.Bills, key)
			}
			return fmt.Errorf("批量结算保存失败，整批未生效: %w", err)
		}
	}

	// 完整保存成功后才报告成功，按清单顺序逐项列出结果。
	existingCount := len(items) - newCount
	if newCount == 0 {
		fmt.Fprintf(stdout, "批量结算完成：共 %d 项，新增账单 0 张，已有账单 %d 张（全部已结算，未改写存档）：\n",
			len(items), existingCount)
	} else {
		fmt.Fprintf(stdout, "批量结算完成：共 %d 项，新增账单 %d 张（对应月份已封账），已有账单 %d 张：\n",
			len(items), newCount, existingCount)
	}
	for _, it := range items {
		status := "已有（返回原账单，不重新计费）"
		if it.isNew {
			status = "新增（已封账）"
		}
		fmt.Fprintf(stdout, "  %d. 客户 %s 月份 %s 账单 %s 原总金额 %d 分（%s）[%s]\n",
			it.pos, it.customerID, it.month, it.b.ID, it.b.TotalFee, moneyFen(it.b.TotalFee), status)
	}
	fmt.Fprintln(stdout, "完整明细可用 bill show <客户标识> <YYYY-MM> 或重复 bill settle 查询。")
	return nil
}
