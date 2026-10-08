package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现收款客户归属更正（bill reassign）：把登记给错误客户的汇款
// 整笔转移给正确客户——撤去更正时当前归属客户最新分配的全部实收，按完整
// 新分配计入目标客户已有账单，总额不变、不重复收钱。归属更正与 bill
// correct 的分配更正共用同一个更正标识命名空间，同一标识不得跨类型复用；
// 每次归属更正占用一个递增的账后全局序号，记录永久保留、不可撤销，可连续
// 更正。原登记客户、总额、备注、首次分配及自动/显式登记身份不改写。

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
	// 完整月份金额分配与收款分配同格式：至少一项，月份 YYYY-MM 不重复，
	// 各项金额为正整数分。
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
		return fmt.Errorf("收款标识 %q 不存在，无法更正客户归属", payID)
	}

	// 更正标识与 bill correct 共用且不可跨类型复用：标识已被另一类更正
	// 占用时一律拒绝。
	if other, cross := s.Corrections[corrID]; cross {
		return fmt.Errorf("更正标识 %q 已由分配更正占用（关联收款 %s，新分配=%s），两类更正标识不得跨类型复用，拒绝",
			corrID, other.PaymentID, formatAllocations(other.Allocations))
	}
	if existing, ok := s.OwnershipTransfers[corrID]; ok {
		// 相同标识按目标收款、目标客户、原因及新月份-金额对应关系判重
		// （列表顺序无关）：内容相同返回原更正与当前状态，不写盘、不增
		// 序号；后续更正、退款或整笔撤销不影响重放，也不恢复旧归属或
		// 款项。任何一项不同（含跨收款复用）均拒绝。
		if existing.PaymentID == payID && existing.ToCustomerID == targetCustomer &&
			existing.Reason == reason && sameAllocations(existing.Allocations, allocs) {
			fmt.Fprintf(stdout, "归属更正 %q 已存在且内容相同，返回原更正与当前状态（不写盘、不增序号）：\n\n", corrID)
			printOwnershipTransfer(existing, s)
			return nil
		}
		return fmt.Errorf("更正标识 %q 已存在但内容不同（已有：收款=%s 目标客户=%s 原因=%q 新分配=%s），拒绝复用",
			corrID, existing.PaymentID, existing.ToCustomerID, existing.Reason, formatAllocations(existing.Allocations))
	}

	// 目标客户须存在且异于当前归属客户；固定单价与阶梯客户均适用。
	if _, ok := s.Customers[targetCustomer]; !ok {
		return fmt.Errorf("目标客户标识 %q 不存在", targetCustomer)
	}
	currentOwner := paymentCurrentCustomer(s, p)
	if targetCustomer == currentOwner {
		return fmt.Errorf("目标客户 %q 与收款 %q 的当前归属客户相同，归属更正必须转移给另一客户", targetCustomer, payID)
	}

	// 首次登记仅限未撤销且未退款收款：已撤销收款没有可转移的实收；首次
	// 退款后最新分配固定，两类更正与整笔撤销都禁止新增（已有请求的相同
	// 重放已在上面按原记录返回，不受此限）。
	if p.Revoked {
		return fmt.Errorf("收款 %q 已撤销（撤销原因 %q），不能新增归属更正", payID, p.RevokeReason)
	}
	if paymentHasRefunds(s, payID) {
		return fmt.Errorf("收款 %q 已发生退款，最新分配已固定，拒绝新增归属更正", payID)
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
			return fmt.Errorf("目标客户 %s 的 %s 尚无账单（未结算），归属更正只能分配到已存在账单", targetCustomer, al.Month)
		}
	}

	// 整笔转移预检：撤去当前归属客户最新分配（转出月份实收下降，必然
	// 非负），把新分配计入目标客户（转入月份实收上升，不得溢出且不得超过
	// 当前应付）；其他收款、账单与应付不变，不补结算。全部合法才整笔生效。
	current := currentAllocations(s, p)
	for _, al := range current {
		received, err := paymentReceived(s, currentOwner, al.Month)
		if err != nil {
			return fmt.Errorf("客户 %s 的 %s 实收累计异常，拒绝归属更正: %w", currentOwner, al.Month, err)
		}
		if received < al.Amount {
			return fmt.Errorf("客户 %s 的 %s 转出后实收将为 %d 分（小于 0），拒绝归属更正",
				currentOwner, al.Month, received-al.Amount)
		}
	}
	for _, al := range allocs {
		b := s.Bills[billKey(targetCustomer, al.Month)]
		_, payable, err := billTotals(b, adjustmentsFor(s, targetCustomer, al.Month))
		if err != nil {
			return fmt.Errorf("目标客户 %s 的 %s 当前应付异常，拒绝归属更正: %w", targetCustomer, al.Month, err)
		}
		received, err := paymentReceived(s, targetCustomer, al.Month)
		if err != nil {
			return fmt.Errorf("目标客户 %s 的 %s 实收累计异常，拒绝归属更正: %w", targetCustomer, al.Month, err)
		}
		newReceived, err := add64(received, al.Amount)
		if err != nil {
			return fmt.Errorf("归属更正后客户 %s 的 %s 实收溢出有符号 64 位整数范围，拒绝归属更正", targetCustomer, al.Month)
		}
		if newReceived > payable {
			return fmt.Errorf("转入后客户 %s 的 %s 实收 %d 分将超过当前应付 %d 分，拒绝归属更正；全部状态保持不变",
				targetCustomer, al.Month, newReceived, payable)
		}
	}

	t := &ownershipTransfer{
		ID:             corrID,
		PaymentID:      payID,
		FromCustomerID: currentOwner,
		ToCustomerID:   targetCustomer,
		Reason:         reason,
		Allocations:    allocs,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	s.NextSeq++
	t.Seq = s.NextSeq
	s.OwnershipTransfers[corrID] = t

	// 归属更正记录、序号与余额状态整体原子保存后才报告成功；保存失败则
	// 一切不生效，该更正标识与序号不被占用，可原样重试。
	if err := s.save(); err != nil {
		delete(s.OwnershipTransfers, corrID)
		s.NextSeq--
		return err
	}
	fmt.Fprintf(stdout, "已登记收款归属更正 %q（关联收款 %q，客户 %s → %s，占用一个账后序号 %d）：\n\n",
		corrID, payID, currentOwner, targetCustomer, t.Seq)
	printOwnershipTransfer(t, s)
	return nil
}

// printOwnershipTransfer 输出单笔归属更正：更正标识、关联收款（总额、备注、
// 自动/显式身份）、原因、前后归属客户与完整分配，以及两侧涉及月份更正后
// 的当前余额。相同重放时当前归属可能已因后续更正再次变化，归属客户与月份
// 余额均取自当前库状态。
func printOwnershipTransfer(t *ownershipTransfer, s *state) {
	p := s.Payments[t.PaymentID] // 载入时已校验存在
	autoKind := "显式分配登记"
	if p.Auto {
		autoKind = "自动分配登记"
	}
	fmt.Fprintf(stdout, "归属更正标识：%s\n", t.ID)
	fmt.Fprintf(stdout, "关联收款：%s（总额 %d 分（%s），备注：%s，%s；序号 %d）\n",
		p.ID, p.Total, moneyFen(p.Total), p.Note, autoKind, p.Seq)
	fmt.Fprintf(stdout, "原因：%s\n", t.Reason)
	fmt.Fprintf(stdout, "更正前归属客户：%s\n", t.FromCustomerID)
	fmt.Fprintf(stdout, "更正后归属客户：%s\n", t.ToCustomerID)
	before := allocationBeforeTransfer(s, t)
	fmt.Fprintf(stdout, "转出分配（撤去客户 %s 的最新分配）：%s\n", t.FromCustomerID, formatAllocations(before))
	fmt.Fprintf(stdout, "转入新分配（计入客户 %s）：%s\n", t.ToCustomerID, formatAllocations(t.Allocations))
	curOwner := paymentCurrentCustomer(s, p)
	current := currentAllocations(s, p)
	fmt.Fprintf(stdout, "当前归属客户：%s；当前最新分配：%s\n", curOwner, formatAllocations(current))
	fmt.Fprintln(stdout, "两侧涉及月份当前余额：")
	printSideBalances := func(customerID string, allocs []paymentAllocation, label string) {
		for _, al := range allocs {
			b, ok := s.Bills[billKey(customerID, al.Month)]
			if !ok {
				fmt.Fprintf(stdout, "  [%s] 月份 %s：本侧分配 %d 分；该账单当前不在客户 %s 名下（历史记录）\n",
					label, al.Month, al.Amount, customerID)
				continue
			}
			_, payable, _ := billTotals(b, adjustmentsFor(s, customerID, al.Month))
			received, _ := paymentReceived(s, customerID, al.Month)
			fmt.Fprintf(stdout, "  [%s] 客户 %s 月份 %s：本侧分配 %d 分；当前应付 %d 分（%s），实收 %d 分（%s），未收余额 %d 分（%s）\n",
				label, customerID, al.Month, al.Amount,
				payable, moneyFen(payable), received, moneyFen(received), payable-received, moneyFen(payable-received))
		}
	}
	printSideBalances(t.FromCustomerID, allocationBeforeTransfer(s, t), "转出")
	printSideBalances(t.ToCustomerID, t.Allocations, "转入")
}

// allocationBeforeTransfer 返回一次归属更正发生前该收款生效的分配（即被
// 整笔撤去的当前客户最新分配）：沿该收款时间线找到序号恰为本更正序号的
// 事件，取其 before。
func allocationBeforeTransfer(s *state, t *ownershipTransfer) []paymentAllocation {
	p := s.Payments[t.PaymentID] // 载入时已校验存在
	evs, _ := buildPaymentTimeline(s, p)
	for _, ev := range evs {
		if ev.kind == ptTransfer && ev.seq == t.Seq {
			return ev.before
		}
	}
	return p.Allocations
}
