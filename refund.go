package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 本文件实现已登记收款的部分退款登记（bill refund）：从一笔未撤销收款的
// 最新分配涉及的月份中实际退回资金。退款只减少对应月实收、增加未收余额，
// 不改变应付、其他收款或费用调整；原收款总额、备注、原始分配与账单永久
// 保留。退款记录永久保留且不可撤销；首次退款后该收款的最新分配固定，不得
// 再新增分配更正或整笔撤销（无退款的收款仍按原规则处理）。每笔退款占用
// 一个递增的账后全局序号。

func cmdBillRefund(dir, payID, refundID, reason string, allocArgs []string) error {
	if strings.TrimSpace(payID) == "" {
		return fmt.Errorf("收款标识不能为空")
	}
	if strings.TrimSpace(refundID) == "" {
		return fmt.Errorf("退款标识不能为空")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("退款原因不能为空")
	}
	// 月份金额清单与收款分配同格式：至少一项，月份 YYYY-MM 不得重复，
	// 每项金额为正整数分。
	allocs, err := parseAllocations(allocArgs)
	if err != nil {
		return err
	}
	// 清单顺序不影响退款身份：统一按月份升序保存与比较。
	sort.Slice(allocs, func(i, j int) bool { return allocs[i].Month < allocs[j].Month })

	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	p, ok := s.Payments[payID]
	if !ok {
		return fmt.Errorf("收款标识 %q 不存在，无法登记退款", payID)
	}

	if existing, ok := s.Refunds[refundID]; ok {
		// 相同标识按目标收款、原因及月份-金额对应关系判重（清单顺序无关）：
		// 内容相同返回原记录且不写盘——即使后来该收款已退尽；任何一项不同
		// （含跨收款复用）均拒绝。
		if existing.PaymentID == payID && existing.Reason == reason && sameAllocations(existing.Allocations, allocs) {
			fmt.Fprintf(stdout, "退款 %q 已存在且内容相同，返回原记录（不重复退款、不写盘）：\n\n", refundID)
			printRefund(existing, s)
			return nil
		}
		return fmt.Errorf("退款标识 %q 已存在但内容不同（已有：收款=%s 原因=%q 月份金额=%s），拒绝复用",
			refundID, existing.PaymentID, existing.Reason, formatAllocations(existing.Allocations))
	}

	// 仅允许退未撤销收款；已撤销收款的实收已整笔取消，无可退余额。
	if p.Revoked {
		return fmt.Errorf("收款 %q 已撤销（撤销原因 %q），仅允许退未撤销收款，拒绝登记退款", payID, p.RevokeReason)
	}

	// 整笔校验：每项月份须属于该收款当前最新分配，各月金额不超过该笔在
	// 该月分配减该月累计退款（只动本收款余额，不借用其他收款）；允许多次
	// 部分退款及退尽余额，任一项非法整笔拒绝。
	current := currentAllocations(s, p)
	for _, al := range allocs {
		alloc := allocAmountFor(current, al.Month)
		if alloc <= 0 {
			return fmt.Errorf("收款 %q 的最新分配不涉及月份 %s（仅允许退最新分配涉及的月份），拒绝登记退款", payID, al.Month)
		}
		refunded := refundedForMonth(s, payID, al.Month)
		remaining := alloc - refunded
		if al.Amount > remaining {
			return fmt.Errorf("月份 %s 退款 %d 分超过剩余可退额 %d 分（收款 %q 在该月分配 %d 分，累计已退 %d 分；不能借用其他收款余额），整笔拒绝",
				al.Month, al.Amount, remaining, payID, alloc, refunded)
		}
	}

	r := &refund{
		ID:          refundID,
		PaymentID:   payID,
		Reason:      reason,
		Allocations: allocs,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	s.NextSeq++
	r.Seq = s.NextSeq
	s.Refunds[refundID] = r

	// 退款记录、序号与余额状态整体原子保存后才报告成功；保存失败则一切
	// 不生效，该退款标识与序号不被占用，可原样重试。
	if err := s.save(); err != nil {
		delete(s.Refunds, refundID)
		s.NextSeq--
		return err
	}
	fmt.Fprintf(stdout, "已登记退款 %q（关联收款 %q），该收款最新分配已固定（不得再更正或整笔撤销）：\n\n", refundID, payID)
	printRefund(r, s)
	return nil
}

// printRefund 输出单笔退款记录：退款标识、关联收款、原因、各月退款及
// 剩余可退额（该月分配减累计退款，取自当前库状态）。
func printRefund(r *refund, s *state) {
	p := s.Payments[r.PaymentID] // 载入时已校验存在
	current := currentAllocations(s, p)
	fmt.Fprintf(stdout, "退款标识：%s\n", r.ID)
	fmt.Fprintf(stdout, "关联收款：%s（客户 %s，总额 %d 分（%s），备注：%s）\n",
		p.ID, p.CustomerID, p.Total, moneyFen(p.Total), p.Note)
	fmt.Fprintf(stdout, "原因：%s\n", r.Reason)
	fmt.Fprintln(stdout, "各月退款与剩余可退额：")
	for _, al := range r.Allocations {
		alloc := allocAmountFor(current, al.Month)
		refunded := refundedForMonth(s, r.PaymentID, al.Month)
		fmt.Fprintf(stdout, "  月份 %s：本次退款 %d 分（%s）；该月分配 %d 分，累计已退 %d 分，剩余可退 %d 分（%s）\n",
			al.Month, al.Amount, moneyFen(al.Amount), alloc, refunded, alloc-refunded, moneyFen(alloc-refunded))
	}
	fmt.Fprintln(stdout, "当前状态：已生效（退款记录永久保留，不可撤销）")
}
