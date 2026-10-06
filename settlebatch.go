package main

import (
	"fmt"
	"strings"
)

// 本文件实现按客户账期清单的整批结算（bill settle-batch）：一次确认的
// 结算范围同时生成账单并封账。清单是非空的 客户标识:YYYY-MM 配对列表，
// 可包含多个客户及同一客户的不同月份，同一客户月份不得重复。整份清单
// 先确认可结算性：任一项非法、无用量或计价溢出都拒绝整批，不留下任何
// 新账单，也不封闭任何原本未封账的月份。已结算项返回原账单，不重新计费；
// 全部新增账单与封账状态在一次原子保存中整体落盘，不产生账后事件，
// 也不占用全局操作序号。

// settleItem 是批量结算清单中解析后的一项：客户标识与账期月份的配对，
// 以及结算性确认后确定的账单（已有的原账单或新计算的账单）。
type settleItem struct {
	pos    int    // 清单中的位置（1 起），用于报错定位与结果展示
	custID string // 客户标识
	month  string // 账期月份 YYYY-MM（UTC 自然月）
	bill   *bill  // 已有账单或新计算的账单
	isNew  bool   // 是否本次新生成
}

func cmdBillSettleBatch(dir string, pairArgs []string) error {
	if len(pairArgs) == 0 {
		return usageError("用法：bill settle-batch <客户标识:YYYY-MM> [更多 客户:月份 ...]")
	}

	// 解析阶段：逐项拆分 客户标识:YYYY-MM（按最后一个冒号拆分），校验
	// 客户标识非空、月份合法，并检查同一客户月份在清单中不重复。
	items := make([]*settleItem, 0, len(pairArgs))
	var problems []string
	seen := make(map[string]int) // 客户|月份 -> 首次出现的清单位置
	for i, arg := range pairArgs {
		pos := i + 1
		idx := strings.LastIndex(arg, ":")
		if idx < 0 {
			problems = append(problems, fmt.Sprintf("第 %d 项 %q：格式非法，应为 客户标识:YYYY-MM（如 acme:2026-09）", pos, arg))
			continue
		}
		custID, month := arg[:idx], arg[idx+1:]
		if strings.TrimSpace(custID) == "" {
			problems = append(problems, fmt.Sprintf("第 %d 项 %q：客户标识不能为空", pos, arg))
			continue
		}
		if !validMonth(month) {
			problems = append(problems, fmt.Sprintf("第 %d 项（客户 %s）：月份 %q 无效，必须是 YYYY-MM 形式（如 2026-09）", pos, custID, month))
			continue
		}
		key := billKey(custID, month)
		if first, dup := seen[key]; dup {
			problems = append(problems, fmt.Sprintf("第 %d 项（客户 %s 月份 %s）：与第 %d 项重复，同一客户月份在清单中不得重复", pos, custID, month, first))
			continue
		}
		seen[key] = pos
		items = append(items, &settleItem{pos: pos, custID: custID, month: month})
	}

	s, err := loadStore(dir)
	if err != nil {
		return err
	}

	// 结算性确认：每个客户必须存在；未结算项必须有该 UTC 自然月的用量
	// 且按账期计费规则计价不溢出；已结算项直接采用原账单，不重新计费。
	// 全部问题收集后一次性报告，整批不生效。
	for _, it := range items {
		cust, ok := s.Customers[it.custID]
		if !ok {
			problems = append(problems, fmt.Sprintf("第 %d 项（客户 %s 月份 %s）：客户标识 %q 不存在", it.pos, it.custID, it.month, it.custID))
			continue
		}
		if existing, ok := s.Bills[billKey(it.custID, it.month)]; ok {
			it.bill = existing
			continue
		}
		b, err := computeBill(s, cust, it.month)
		if err != nil {
			problems = append(problems, fmt.Sprintf("第 %d 项（客户 %s 月份 %s）：%v", it.pos, it.custID, it.month, err))
			continue
		}
		it.bill = b
		it.isNew = true
	}

	if len(problems) > 0 {
		return fmt.Errorf("批量结算被拒绝，整批未生效，未生成任何新账单、未封闭任何原本未封账的月份（共 %d 个问题）：\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}

	// 全部新增账单与封账状态整体原子保存，完整保存后才报告成功；
	// 保存失败整批不生效，原有账单、用量和其他状态不变，排除故障后
	// 原清单可重试。全部项目已结算时不改写存档。
	newCount := 0
	for _, it := range items {
		if it.isNew {
			s.Bills[billKey(it.custID, it.month)] = it.bill
			newCount++
		}
	}
	if newCount > 0 {
		if err := s.save(); err != nil {
			for _, it := range items {
				if it.isNew {
					delete(s.Bills, billKey(it.custID, it.month))
				}
			}
			return err
		}
	}

	fmt.Fprintf(stdout, "批量结算完成：新增 %d 项，已有 %d 项（共 %d 项）：\n", newCount, len(items)-newCount, len(items))
	for _, it := range items {
		status := "已有（返回原账单，未重新计费）"
		if it.isNew {
			status = "新增（已封账）"
		}
		fmt.Fprintf(stdout, "  第 %d 项：客户 %s 月份 %s 账单 %s 原总金额 %d 分（%s）：%s\n",
			it.pos, it.custID, it.month, it.bill.ID, it.bill.TotalFee, moneyFen(it.bill.TotalFee), status)
	}
	fmt.Fprintln(stdout, "完整明细可用 bill show 或重复 bill settle 读回")
	return nil
}
