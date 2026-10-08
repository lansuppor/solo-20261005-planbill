package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现收款客户归属更正（bill reassign）：把一笔登记给错误客户的汇款
// 整笔转移给正确客户——撤去当前归属客户最新分配的全部实收，计入目标客户的
// 新分配实收，总额不变，因此不重复收钱。原登记客户、总额、备注、首次分配
// 及自动或显式登记身份都不改写（payment.CustomerID 仍为首次登记客户）；
// 归属更正记录永久保留、不可撤销，只占一个账后全局序号，可连续更正（再次
// 归属更正或同客户分配更正）。固定单价与阶梯客户同样适用。

// cmdBillReassign 登记一笔收款客户归属更正。参数顺序：收款标识、更正标识、
// 目标客户、非空原因、完整月份金额分配（目标客户已有账单）。
func cmdBillReassign(dir, payID, corrID, targetCustomer, reason string, allocArgs []string) error {
	if strings.TrimSpace(payID) == "" {
		return fmt.Errorf("收款标识不能为空")
	}
	if strings.TrimSpace(corrID) == "" {
		return fmt.Errorf("更正标识不能为空")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("归属更正原因不能为空")
	}
	allocs, err := parseAllocations(allocArgs)
	if err != nil {
		return err
	}
	// 分配顺序不影响更正身份：统一按月份升序保存与比较。
	sort.Slice(allocs, func(i, j int) bool { return allocs[i].Month < allocs[j].Month })

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	p, ok := s.Payments[payID]
	if !ok {
		return fmt.Errorf("收款标识 %q 不存在，无法登记客户归属更正", payID)
	}

	// 更正标识与 bill correct 共用且不可跨类型复用：相同标识按目标收款、
	// 目标客户、原因及月份-金额关系判重（顺序无关）；相同重放返回原更正与
	// 当前状态，不写盘、不增序号；内容不同（含跨类型、跨收款复用）拒绝。
	if existing, ok := s.Corrections[corrID]; ok {
		if !existing.Reassign {
			return fmt.Errorf("更正标识 %q 已由分配更正 %q 占用（关联收款 %s，新分配=%s），分配更正与归属更正之间不得复用标识",
				corrID, existing.ID, existing.PaymentID, formatAllocations(existing.Allocations))
		}
		if existing.PaymentID == payID && existing.TargetCustomerID == targetCustomer &&
			existing.Reason == reason && sameAllocations(existing.Allocations, allocs) {
			fmt.Fprintf(stdout, "归属更正 %q 已存在且内容相同，返回原更正与当前状态（不写盘、不增序号、不恢复旧归属或款项）：\n\n", corrID)
			printReassign(s, existing)
			return nil
		}
		return fmt.Errorf("更正标识 %q 已存在但内容不同（已有：收款=%s 目标客户=%s 原因=%q 新分配=%s），拒绝复用",
			corrID, existing.PaymentID, existing.TargetCustomerID, existing.Reason, formatAllocations(existing.Allocations))
	}

	if _, ok := s.Customers[targetCustomer]; !ok {
		return fmt.Errorf("目标客户标识 %q 不存在", targetCustomer)
	}
	// 首次登记仅限未撤销且未退款收款。
	if p.Revoked {
		return fmt.Errorf("收款 %q 已撤销（撤销原因 %q），不能登记客户归属更正", payID, p.RevokeReason)
	}
	if paymentHasRefunds(s, payID) {
		return fmt.Errorf("收款 %q 已发生退款，最新分配已固定，拒绝新增客户归属更正", payID)
	}

	owner := paymentOwner(s, p)
	if targetCustomer == owner {
		return fmt.Errorf("目标客户 %q 与收款 %q 当前归属客户相同，归属更正必须转往其他客户", targetCustomer, payID)
	}

	// 新分配合计必须等于原收款总额（整笔转移，不重复收钱），全程整数运算。
	var sum int64
	for _, al := range allocs {
		sum, err = add64(sum, al.Amount)
		if err != nil {
			return fmt.Errorf("新分配金额合计溢出有符号 64 位整数范围，拒绝归属更正")
		}
	}
	if sum != p.Total {
		return fmt.Errorf("新分配合计 %d 分与原收款 %q 的总额 %d 分不一致，拒绝归属更正", sum, payID, p.Total)
	}
	// 新分配月份须是目标客户的已结算账单。
	for _, al := range allocs {
		if _, ok := s.Bills[billKey(targetCustomer, al.Month)]; !ok {
			return fmt.Errorf("目标客户 %s 的 %s 尚无账单（未结算），归属更正只能分配到目标客户已存在账单",
				targetCustomer, al.Month)
		}
	}

	current := currentAllocations(s, p)

	// 两侧余额预检：
	//   - 转出侧（当前归属客户）：整笔撤去最新分配后各月实收不小于 0（该笔
	//     未撤销、未退款，其实收必然包含整笔当前分配，撤去不会变负）；
	//   - 转入侧（目标客户）：计入新分配后各月实收不得超过当前应付，加法
	//     不得溢出有符号 64 位整数。
	// 其他收款、账单与应付一律不变。
	for _, al := range current {
		b := s.Bills[billKey(owner, al.Month)] // 载入时已校验存在
		_, payable, err := billTotals(b, adjustmentsFor(s, owner, al.Month))
		if err != nil {
			return fmt.Errorf("客户 %s 的 %s 当前应付异常，拒绝归属更正: %w", owner, al.Month, err)
		}
		received, err := paymentReceived(s, owner, al.Month)
		if err != nil {
			return fmt.Errorf("客户 %s 的 %s 实收累计异常，拒绝归属更正: %w", owner, al.Month, err)
		}
		after := received - al.Amount // 未退款且本笔未撤销，必为非负
		if after < 0 || after > payable {
			return fmt.Errorf("转出后客户 %s 的 %s 实收 %d 分越界（当前应付 %d 分），拒绝归属更正；全部状态保持不变",
				owner, al.Month, after, payable)
		}
	}
	for _, al := range allocs {
		b := s.Bills[billKey(targetCustomer, al.Month)] // 上面已校验存在
		_, payable, err := billTotals(b, adjustmentsFor(s, targetCustomer, al.Month))
		if err != nil {
			return fmt.Errorf("目标客户 %s 的 %s 当前应付异常，拒绝归属更正: %w", targetCustomer, al.Month, err)
		}
		received, err := paymentReceived(s, targetCustomer, al.Month)
		if err != nil {
			return fmt.Errorf("目标客户 %s 的 %s 实收累计异常，拒绝归属更正: %w", targetCustomer, al.Month, err)
		}
		after, err := add64(received, al.Amount)
		if err != nil {
			return fmt.Errorf("转入后客户 %s 的 %s 实收溢出有符号 64 位整数范围，拒绝归属更正", targetCustomer, al.Month)
		}
		if after > payable {
			return fmt.Errorf("转入后目标客户 %s 的 %s 实收 %d 分将超过当前应付 %d 分，拒绝归属更正；全部状态保持不变",
				targetCustomer, al.Month, after, payable)
		}
	}

	c := &correction{
		ID:               corrID,
		PaymentID:        payID,
		Reason:           reason,
		Allocations:      allocs,
		Reassign:         true,
		TargetCustomerID: targetCustomer,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	s.NextSeq++
	c.Seq = s.NextSeq
	s.Corrections[corrID] = c

	// 归属更正记录、序号与两侧余额状态在同一次原子保存中持久化；保存失败
	// 则一切不生效，该更正标识与序号不被占用，可原样重试。
	if err := s.save(); err != nil {
		delete(s.Corrections, corrID)
		s.NextSeq--
		return err
	}
	fmt.Fprintf(stdout, "已登记收款客户归属更正 %q（关联收款 %q，客户 %s → %s，占用一个账后全局序号）：\n\n",
		corrID, payID, owner, targetCustomer)
	printReassign(s, c)
	return nil
}

// printReassign 输出单笔归属更正：更正标识、关联收款的永久身份、原因、前后
// 客户与完整分配、两侧涉及月份更正后的当前余额，以及收款的当前归属状态。
// 相同重放时同样使用本函数，因此“当前状态”始终取自存档最新状态（可能已被
// 后续更正或退款改变），不恢复旧归属或款项。
func printReassign(s *state, c *correction) {
	p := s.Payments[c.PaymentID] // 载入时已校验存在
	ownerBefore := paymentOwnerAt(s, p, c.Seq-1)
	before := allocationBefore(s, c)
	registerKind := "显式分配登记"
	if p.Auto {
		registerKind = "自动分配登记"
	}
	fmt.Fprintf(stdout, "更正标识：%s\n", c.ID)
	fmt.Fprintf(stdout, "更正类型：收款客户归属更正（整笔转移实收，不重复收钱；记录永久保留、不可撤销）\n")
	fmt.Fprintf(stdout, "关联收款：%s（原登记客户 %s，总额 %d 分（%s），备注：%s，%s 身份永久保留）\n",
		p.ID, p.CustomerID, p.Total, moneyFen(p.Total), p.Note, registerKind)
	fmt.Fprintf(stdout, "原因：%s\n", c.Reason)
	fmt.Fprintf(stdout, "归属更正：客户 %s → 客户 %s\n", ownerBefore, c.TargetCustomerID)
	fmt.Fprintf(stdout, "更正前分配（客户 %s）：%s\n", ownerBefore, formatAllocations(before))
	fmt.Fprintf(stdout, "更正后分配（客户 %s）：%s\n", c.TargetCustomerID, formatAllocations(c.Allocations))
	fmt.Fprintln(stdout, "涉及月份当前余额：")
	for _, al := range before {
		b := s.Bills[billKey(ownerBefore, al.Month)] // 载入时已校验存在
		_, payable, _ := billTotals(b, adjustmentsFor(s, ownerBefore, al.Month))
		received, _ := paymentReceived(s, ownerBefore, al.Month)
		fmt.Fprintf(stdout, "  [转出] 客户 %s 月份 %s：撤去分配 %d 分（%s）；当前应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
			ownerBefore, al.Month, al.Amount, moneyFen(al.Amount),
			payable, moneyFen(payable), received, moneyFen(received), payable-received, moneyFen(payable-received))
	}
	for _, al := range c.Allocations {
		b := s.Bills[billKey(c.TargetCustomerID, al.Month)] // 载入时已校验存在
		_, payable, _ := billTotals(b, adjustmentsFor(s, c.TargetCustomerID, al.Month))
		received, _ := paymentReceived(s, c.TargetCustomerID, al.Month)
		fmt.Fprintf(stdout, "  [转入] 客户 %s 月份 %s：计入分配 %d 分（%s）；当前应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
			c.TargetCustomerID, al.Month, al.Amount, moneyFen(al.Amount),
			payable, moneyFen(payable), received, moneyFen(received), payable-received, moneyFen(payable-received))
	}
	currentOwner := paymentOwner(s, p)
	fmt.Fprintf(stdout, "当前状态：收款 %s 当前归属客户 %s，最新分配（客户 %s）：%s\n",
		p.ID, currentOwner, currentOwner, formatAllocations(currentAllocations(s, p)))
}
