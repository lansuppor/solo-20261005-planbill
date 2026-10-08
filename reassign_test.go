package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// setupTwoCustomers 建两个固定单价客户（单价 100 分）并各自结算 2026-09
// 与 2026-10：c1 应付 500/300，c2 应付 800/800。
func setupTwoCustomers(t *testing.T, h *harness) {
	t.Helper()
	settleTwoMonths(h, "c1", "100", "5", "3")
	settleTwoMonths(h, "c2", "100", "8", "8")
}

func TestReassignHappyPathBalancesAndSeq(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	// 另一笔留在 c1 的收款，归属更正不得影响它。
	h.mustRun("bill", "pay", "c1", "2026-09", "r2", "100", "留底汇款")

	out := h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错客户", "2026-09:600", "2026-10:100")
	for _, want := range []string{
		"已登记收款归属更正", "客户 c1 → c2", "占用一个账后序号 3",
		"更正前归属客户：c1", "更正后归属客户：c2",
		"转出分配（撤去客户 c1 的最新分配）：2026-09:400,2026-10:300",
		"转入新分配（计入客户 c2）：2026-09:600,2026-10:100",
		"原因：登错客户",
		"[转出] 客户 c1 月份 2026-09", "[转入] 客户 c2 月份 2026-09",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("归属更正输出缺少 %q:\n%s", want, out)
		}
	}

	s := loadState(t, h)
	if s.NextSeq != 3 {
		t.Fatalf("整笔转移应只占一个序号，NextSeq=%d", s.NextSeq)
	}
	if s.OwnershipTransfers["t1"] == nil {
		t.Fatal("归属更正记录未保存")
	}
	if got := paymentCurrentCustomer(s, s.Payments["r1"]); got != "c2" {
		t.Fatalf("当前归属客户应为 c2，实际 %s", got)
	}
	if s.Payments["r1"].CustomerID != "c1" {
		t.Fatal("首次登记客户不得改写")
	}

	// 两侧余额：c1 两月实收归零（r2 的 100 保留），c2 按新分配计入。
	c1sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(c1sep, "实收：100 分") || !strings.Contains(c1sep, "未收余额：400 分") {
		t.Fatal(c1sep)
	}
	c1oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(c1oct, "实收：0 分") || !strings.Contains(c1oct, "未收余额：300 分") {
		t.Fatal(c1oct)
	}
	c2sep := h.mustRun("bill", "show", "c2", "2026-09")
	if !strings.Contains(c2sep, "实收：600 分") || !strings.Contains(c2sep, "未收余额：200 分") {
		t.Fatal(c2sep)
	}
	c2oct := h.mustRun("bill", "show", "c2", "2026-10")
	if !strings.Contains(c2oct, "实收：100 分") || !strings.Contains(c2oct, "未收余额：700 分") {
		t.Fatal(c2oct)
	}
}

func TestReassignShowKeepsHistoryBothSides(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错客户", "2026-09:600", "2026-10:100")

	c1 := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"收款 r1：总额 700 分", "本账单分配 400 分",
		"归属转出 t1：原因：登错客户（关联收款 r1，总额 700 分，本账单转出 400 分至客户 c2）",
	} {
		if !strings.Contains(c1, want) {
			t.Fatalf("c1 账单历史缺少 %q:\n%s", want, c1)
		}
	}
	c2 := h.mustRun("bill", "show", "c2", "2026-09")
	if !strings.Contains(c2, "归属转入 t1：原因：登错客户（关联收款 r1，自客户 c1 转入，总额 700 分，本账单转入 600 分）") {
		t.Fatal(c2)
	}
	// 重复结算保留曾涉及账单的历史。
	again := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(again, "归属转出 t1") || !strings.Contains(again, "收款 r1") {
		t.Fatal(again)
	}
}

func TestReassignLedgerReconcileBothSides(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")   // 序号 1
	h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错客户", "2026-09:600", "2026-10:100") // 序号 2

	// 转出侧 c1：收款后又整笔转出。
	led := h.mustRun("bill", "ledger", "c1", "2026-09")
	for _, want := range []string{
		"序号 1 收款 r1：实收 +400 分",
		"序号 2 归属转出 t1：实收 -400 分（-4.00 元，关联收款 r1，汇款总额 700 分，转出本账单 400 分至客户 c2）",
		"截止时余额：应付 500 分（5.00 元），实收 0 分（0.00 元），未收余额 500 分（5.00 元）",
	} {
		if !strings.Contains(led, want) {
			t.Fatalf("c1 流水缺少 %q:\n%s", want, led)
		}
	}
	// 转入侧 c2：同一序号列实际转入。
	led = h.mustRun("bill", "ledger", "c2", "2026-09")
	for _, want := range []string{
		"序号 2 归属转入 t1：实收 +600 分",
		"自客户 c1 转入本账单 600 分",
		"截止时余额：应付 800 分（8.00 元），实收 600 分（6.00 元），未收余额 200 分（2.00 元）",
	} {
		if !strings.Contains(led, want) {
			t.Fatalf("c2 流水缺少 %q:\n%s", want, led)
		}
	}
	// 截止在迁移之前：两侧都不提前受影响。
	if cut := h.mustRun("bill", "ledger", "c2", "2026-09", "1"); !strings.Contains(cut, "实收 0 分") ||
		strings.Contains(cut, "归属转入") {
		t.Fatal(cut)
	}
	if cut := h.mustRun("bill", "ledger", "c1", "2026-09", "1"); !strings.Contains(cut, "实收 400 分") {
		t.Fatal(cut)
	}
	// reconcile 两侧各只计本侧金额，同一序号。
	rc2 := h.mustRun("bill", "reconcile", "c2", "2026-09", "2026-10")
	if !strings.Contains(rc2, "序号 2 归属转入 t1（关联收款 r1，自客户 c1 转入）") ||
		!strings.Contains(rc2, "终点合计：应付 1600 分（16.00 元），实收 700 分（7.00 元），未收余额 900 分（9.00 元）") {
		t.Fatal(rc2)
	}
	rc1 := h.mustRun("bill", "reconcile", "c1", "2026-09", "2026-10")
	if !strings.Contains(rc1, "序号 2 归属转出 t1（关联收款 r1，转入客户 c2）") ||
		!strings.Contains(rc1, "终点合计：应付 800 分（8.00 元），实收 0 分（0.00 元），未收余额 800 分（8.00 元）") {
		t.Fatal(rc1)
	}
	// 序号区间截止在迁移前：不出现归属事件。
	before := h.mustRun("bill", "reconcile", "c2", "2026-09", "2026-10", "0", "1")
	if strings.Contains(before, "归属转入") {
		t.Fatal(before)
	}
}

func TestReassignIdempotentAndConflicts(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错客户", "2026-09:600", "2026-10:100")

	// 列表顺序不同的相同重放：返回原更正与当前状态，不写盘、不增序号。
	out := h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错客户", "2026-10:100", "2026-09:600")
	if !strings.Contains(out, "返回原更正与当前状态（不写盘、不增序号）") ||
		!strings.Contains(out, "转入新分配（计入客户 c2）：2026-09:600,2026-10:100") {
		t.Fatal(out)
	}
	if s := loadState(t, h); s.NextSeq != 2 {
		t.Fatalf("相同重放不应增加序号，NextSeq=%d", s.NextSeq)
	}
	// 目标客户、原因、月份金额、目标收款任一不同均拒绝。
	h.runExpectErr("bill", "reassign", "r1", "t1", "c1", "登错客户", "2026-09:400", "2026-10:300")
	h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "另一个原因", "2026-09:600", "2026-10:100")
	h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "登错客户", "2026-09:700")
	h.mustRun("bill", "pay", "c2", "2026-10", "r3", "100", "其他收款")
	h.runExpectErr("bill", "reassign", "r3", "t1", "c1", "登错客户", "2026-10:100")
	// 跨类型复用更正标识：先做同客户分配更正，再用同标识做归属更正当拒绝。
	h.mustRun("bill", "correct", "r1", "cc1", "同客户更正", "2026-09:600", "2026-10:100")
	h.runExpectErr("bill", "reassign", "r1", "cc1", "c1", "跨类型", "2026-09:400", "2026-10:300")
}

func TestReassignCrossTypeIDReuseBothDirections(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	// 归属更正先占用标识 shared。
	h.mustRun("bill", "reassign", "r1", "shared", "c2", "登错", "2026-09:600", "2026-10:100")
	msg := h.runExpectErr("bill", "correct", "r1", "shared", "再用", "2026-09:600", "2026-10:100")
	if !strings.Contains(msg, "不得跨类型复用") {
		t.Fatal(msg)
	}
	// 分配更正先占用标识 shared2，归属更正不得复用。
	h.mustRun("bill", "reassign", "r1", "t2", "c1", "转回来", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "correct", "r1", "shared2", "同客户", "2026-09:400", "2026-10:300")
	msg = h.runExpectErr("bill", "reassign", "r1", "shared2", "c2", "跨类型", "2026-09:600", "2026-10:100")
	if !strings.Contains(msg, "不得跨类型复用") {
		t.Fatal(msg)
	}
}

func TestReassignValidation(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")

	// 参数数量错误是用法错误（退出码 2）。
	for _, args := range [][]string{
		{"bill", "reassign"},
		{"bill", "reassign", "r1"},
		{"bill", "reassign", "r1", "t1"},
		{"bill", "reassign", "r1", "t1", "c2"},
		{"bill", "reassign", "r1", "t1", "c2", "原因"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误，得到 %v", args, err)
		}
	}
	// 业务校验：收款不存在、目标客户不存在、目标=当前归属、原因空、
	// 月份非法/重复、金额非正、合计不等、目标账单不存在。
	h.runExpectErr("bill", "reassign", "ghost", "t1", "c2", "原因", "2026-09:100")
	h.runExpectErr("bill", "reassign", "r1", "t1", "ghost", "原因", "2026-09:100")
	h.runExpectErr("bill", "reassign", "r1", "t1", "c1", "原因", "2026-09:400", "2026-10:300")
	h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "  ", "2026-09:600", "2026-10:100")
	h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "原因", "2026-9:100")
	h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "原因", "2026-09:300", "2026-09:400")
	h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "原因", "2026-09:0")
	h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "原因", "2026-09:600", "2026-10:99")
	h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "原因", "2026-09:600") // 合计 600 ≠ 总额 700
	h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "原因", "2026-11:700")
	// 合计溢出有符号 64 位。
	h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "原因",
		"2026-09:9223372036854775807", "2026-10:1")
	// 任何失败都不占标识与序号：同标识随后可成功使用。
	h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错客户", "2026-09:600", "2026-10:100")
	if s := loadState(t, h); s.NextSeq != 2 {
		t.Fatalf("失败尝试不应占序号，NextSeq=%d", s.NextSeq)
	}
}

func TestReassignRejectsRevokedAndRefunded(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")

	// 已撤销收款不能新增归属更正。
	h.mustRun("bill", "unpay", "r1", "登错撤销")
	if msg := h.runExpectErr("bill", "reassign", "r1", "t0", "c2", "原因", "2026-09:700"); !strings.Contains(msg, "已撤销") {
		t.Fatal(msg)
	}
	// 撤销后相同归属更正的重放场景：先撤销前登记过的归属更正，撤销后重放仍成功。
	setupTwoCustomersPaid := func() *harness {
		hh := newHarness(t)
		setupTwoCustomers(t, hh)
		hh.mustRun("bill", "remit", "c1", "p1", "700", "汇款", "2026-09:400", "2026-10:300")
		hh.mustRun("bill", "reassign", "p1", "t1", "c2", "登错", "2026-09:600", "2026-10:100")
		return hh
	}
	h2 := setupTwoCustomersPaid()
	h2.mustRun("bill", "unpay", "p1", "退汇")
	out := h2.mustRun("bill", "reassign", "p1", "t1", "c2", "登错", "2026-10:100", "2026-09:600")
	if !strings.Contains(out, "返回原更正与当前状态") {
		t.Fatal(out)
	}

	// 已退款收款不能新增归属更正；已有归属更重重放仍成功。
	h3 := setupTwoCustomersPaid()
	h3.mustRun("bill", "refund", "p1", "rf1", "退回", "2026-09:100")
	if msg := h3.runExpectErr("bill", "reassign", "p1", "t9", "c1", "退款后转", "2026-09:500", "2026-10:100"); !strings.Contains(msg, "已发生退款") {
		t.Fatal(msg)
	}
	out = h3.mustRun("bill", "reassign", "p1", "t1", "c2", "登错", "2026-10:100", "2026-09:600")
	if !strings.Contains(out, "返回原更正与当前状态") {
		t.Fatal(out)
	}
}

func TestReassignOverpayRejectsAtomically(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h) // c2 09 应付 800
	h.mustRun("bill", "pay", "c1", "2026-09", "r1", "500", "汇款")
	h.mustRun("bill", "pay", "c2", "2026-09", "r2", "600", "c2 已有收款")

	// 转入 500 会让 c2 09 实收 1100 > 应付 800：整笔拒绝。
	msg := h.runExpectErr("bill", "reassign", "r1", "t1", "c2", "登错", "2026-09:500")
	if !strings.Contains(msg, "超过当前应付") {
		t.Fatal(msg)
	}
	// 全部状态不变：序号不增、r1 仍归 c1、c2 实收仍为 600。
	s := loadState(t, h)
	if s.NextSeq != 2 || len(s.OwnershipTransfers) != 0 {
		t.Fatalf("失败不应写盘或占序号: NextSeq=%d transfers=%d", s.NextSeq, len(s.OwnershipTransfers))
	}
	if got := paymentCurrentCustomer(s, s.Payments["r1"]); got != "c1" {
		t.Fatalf("r1 仍应归属 c1，实际 %s", got)
	}
	c2 := h.mustRun("bill", "show", "c2", "2026-09")
	if !strings.Contains(c2, "实收：600 分") {
		t.Fatal(c2)
	}
}

func TestReassignChainAndIdentity(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "reassign", "r1", "t1", "c2", "第一次", "2026-09:600", "2026-10:100")
	h.mustRun("bill", "reassign", "r1", "t2", "c1", "第二次", "2026-09:400", "2026-10:300")

	s := loadState(t, h)
	if s.NextSeq != 3 {
		t.Fatalf("两次归属更正各占一个序号，NextSeq=%d", s.NextSeq)
	}
	if got := paymentCurrentCustomer(s, s.Payments["r1"]); got != "c1" {
		t.Fatalf("连续更正后应回到 c1，实际 %s", got)
	}
	// 原登记客户、总额、备注、首次分配不改写。
	p := s.Payments["r1"]
	if p.CustomerID != "c1" || p.Total != 700 || p.Note != "季度汇款" ||
		formatAllocations(p.Allocations) != "2026-09:400,2026-10:300" {
		t.Fatalf("首次登记身份被改写: %+v", p)
	}
	// 原收款登记入口仍按首次登记身份判重，相同重放不恢复旧归属或款项
	// （当前归属 c1 与首次相同，再用 c2 视角验证：转走后重放原 remit）。
	h.mustRun("bill", "reassign", "r1", "t3", "c2", "第三次", "2026-09:600", "2026-10:100")
	out := h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-10:300", "2026-09:400")
	if !strings.Contains(out, "不重复计入实收") || !strings.Contains(out, "当前归属客户：c2") {
		t.Fatal(out)
	}
	c1 := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(c1, "实收：0 分") {
		t.Fatal("原收款相同重放不应恢复旧归属实收")
	}
	// 自动登记身份：remit-auto 重放也不恢复首次分配。
	h2 := newHarness(t)
	settleTwoMonths(h2, "c1", "100", "5", "3")
	settleTwoMonths(h2, "c2", "100", "8", "8")
	h2.mustRun("bill", "remit-auto", "c1", "a1", "700", "自动汇款")
	h2.mustRun("bill", "reassign", "a1", "ta", "c2", "登错", "2026-09:600", "2026-10:100")
	out = h2.mustRun("bill", "remit-auto", "c1", "a1", "700", "自动汇款")
	if !strings.Contains(out, "当前归属客户：c2") ||
		!strings.Contains(out, "最新分配（经更正，以最新为准）：2026-09:600,2026-10:100") {
		t.Fatal(out)
	}
}

func TestReassignThenCorrectRefundUnpay(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错", "2026-09:600", "2026-10:100")

	// bill correct 只调整当前客户 c2 的月份。
	out := h.mustRun("bill", "correct", "r1", "cc1", "同客户调月份", "2026-09:500", "2026-10:200")
	if !strings.Contains(out, "当前归属客户 c2") {
		t.Fatal(out)
	}
	c2 := h.mustRun("bill", "show", "c2", "2026-09")
	if !strings.Contains(c2, "实收：500 分") {
		t.Fatal(c2)
	}
	// 退款作用于当前归属 c2 的最新分配。
	h.mustRun("bill", "refund", "r1", "rf1", "退一部分", "2026-09:100")
	c2 = h.mustRun("bill", "show", "c2", "2026-09")
	if !strings.Contains(c2, "实收：400 分") {
		t.Fatal(c2)
	}
	// 首次退款后禁止新增两类更正及整笔撤销。
	h.runExpectErr("bill", "correct", "r1", "cc2", "再调", "2026-09:400", "2026-10:300")
	h.runExpectErr("bill", "reassign", "r1", "t9", "c1", "再转", "2026-09:400", "2026-10:300")
	h.runExpectErr("bill", "unpay", "r1", "退汇")
	// 已有的分配更正、归属更正相同重放仍成功，不改变状态。
	if out := h.mustRun("bill", "correct", "r1", "cc1", "同客户调月份", "2026-10:200", "2026-09:500"); !strings.Contains(out, "返回原更正记录") {
		t.Fatal(out)
	}
	if out := h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错", "2026-09:600", "2026-10:100"); !strings.Contains(out, "返回原更正与当前状态") {
		t.Fatal(out)
	}

	// 未退款场景：归属转移后的整笔撤销只取消当前归属 c2 的最新分配。
	h2 := newHarness(t)
	setupTwoCustomers(t, h2)
	h2.mustRun("bill", "remit", "c1", "p1", "700", "汇款", "2026-09:400", "2026-10:300")
	h2.mustRun("bill", "reassign", "p1", "t1", "c2", "登错", "2026-09:600", "2026-10:100")
	h2.mustRun("bill", "correct", "p1", "cc1", "调整", "2026-09:500", "2026-10:200")
	out = h2.mustRun("bill", "unpay", "p1", "退汇")
	if !strings.Contains(out, "当前归属客户 c2") ||
		!strings.Contains(out, "月份 2026-09：分配 500 分") ||
		!strings.Contains(out, "月份 2026-10：分配 200 分") {
		t.Fatal(out)
	}
	for _, c := range []string{"c1", "c2"} {
		for _, m := range []string{"2026-09", "2026-10"} {
			sh := h2.mustRun("bill", "show", c, m)
			if !strings.Contains(sh, "实收：0 分") {
				t.Fatalf("撤销后 %s %s 应无实收:\n%s", c, m, sh)
			}
		}
	}
}

func TestReassignTieredTarget(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "ct", "阶梯客户", "p1")
	h.mustRun("customer", "add", "cf", "固定客户", "100")
	f := h.writeFile("ut.csv", csvHeader+
		"ut1,ct,2026-09-15T10:00:00Z,50\n"+
		"uf1,cf,2026-09-15T10:00:00Z,5\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "ct", "2026-09") // 50×10=500
	h.mustRun("bill", "settle", "cf", "2026-09") // 500
	h.mustRun("bill", "pay", "cf", "2026-09", "r1", "500", "汇错")
	h.mustRun("bill", "reassign", "r1", "t1", "ct", "登错给阶梯客户", "2026-09:500")
	ct := h.mustRun("bill", "show", "ct", "2026-09")
	if !strings.Contains(ct, "实收：500 分") || !strings.Contains(ct, "未收余额：0 分") {
		t.Fatal(ct)
	}
	cf := h.mustRun("bill", "show", "cf", "2026-09")
	if !strings.Contains(cf, "实收：0 分") {
		t.Fatal(cf)
	}
}

func TestReassignPersistsAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错客户", "2026-09:600", "2026-10:100")

	// 重启后归属、历史与判重保持（每次 run 都重新从磁盘载入）。
	c2 := h.mustRun("bill", "show", "c2", "2026-09")
	if !strings.Contains(c2, "实收：600 分") || !strings.Contains(c2, "归属转入 t1") {
		t.Fatal(c2)
	}
	out := h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错客户", "2026-09:600", "2026-10:100")
	if !strings.Contains(out, "返回原更正与当前状态") {
		t.Fatal(out)
	}
}

func TestReassignCorruptDataRejected(t *testing.T) {
	base := func(t *testing.T) *harness {
		h := newHarness(t)
		setupTwoCustomers(t, h)
		h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
		h.mustRun("bill", "reassign", "r1", "t1", "c2", "登错客户", "2026-09:600", "2026-10:100")
		return h
	}
	mutate := func(t *testing.T, h *harness, fn func(map[string]any)) {
		t.Helper()
		data, err := os.ReadFile(h.statePath())
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		fn(doc)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.statePath(), out, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 目标客户缺失。
	h := base(t)
	mutate(t, h, func(doc map[string]any) {
		doc["ownership_transfers"].(map[string]any)["t1"].(map[string]any)["to_customer_id"] = "ghost"
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "数据文件已损坏") {
		t.Fatal(msg)
	}
	// 引用收款缺失。
	h = base(t)
	mutate(t, h, func(doc map[string]any) {
		doc["ownership_transfers"].(map[string]any)["t1"].(map[string]any)["payment_id"] = "ghost"
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "引用了不存在的收款") {
		t.Fatal(msg)
	}
	// 转入分配引用缺失账单。
	h = base(t)
	mutate(t, h, func(doc map[string]any) {
		tr := doc["ownership_transfers"].(map[string]any)["t1"].(map[string]any)
		tr["allocations"] = []any{map[string]any{"month": "2026-11", "amount_fen": float64(700)}}
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "引用了不存在的账单") {
		t.Fatal(msg)
	}
	// 非法分配：月份重复。
	h = base(t)
	mutate(t, h, func(doc map[string]any) {
		tr := doc["ownership_transfers"].(map[string]any)["t1"].(map[string]any)
		tr["allocations"] = []any{
			map[string]any{"month": "2026-09", "amount_fen": float64(600)},
			map[string]any{"month": "2026-09", "amount_fen": float64(100)},
		}
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "分配月份 2026-09 重复") {
		t.Fatal(msg)
	}
	// 序号冲突：与收款序号相同。
	h = base(t)
	mutate(t, h, func(doc map[string]any) {
		doc["ownership_transfers"].(map[string]any)["t1"].(map[string]any)["seq"] = float64(1)
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "数据文件已损坏") {
		t.Fatal(msg)
	}
	// 归属链损坏：from 客户与序号发生时实际归属不符。
	h = base(t)
	mutate(t, h, func(doc map[string]any) {
		doc["customers"].(map[string]any)["c3"] = map[string]any{"id": "c3", "name": "丙", "price_fen": float64(100)}
		doc["ownership_transfers"].(map[string]any)["t1"].(map[string]any)["from_customer_id"] = "c3"
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "归属链损坏") {
		t.Fatal(msg)
	}
	// 跨类型复用更正标识。
	h = base(t)
	mutate(t, h, func(doc map[string]any) {
		doc["corrections"].(map[string]any)["t1"] = map[string]any{
			"id": "t1", "payment_id": "r1", "reason": "x",
			"allocations": []any{map[string]any{"month": "2026-09", "amount_fen": float64(700)}},
			"seq":         float64(3), "created_at": "2026-10-01T00:00:00Z",
		}
		doc["next_seq"] = float64(3)
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "不得跨类型复用") {
		t.Fatal(msg)
	}
	// 逐步余额越界：转入叠加其他收款后超过应付（最终余额非法，载入拒绝）。
	h = base(t)
	mutate(t, h, func(doc map[string]any) {
		doc["payments"].(map[string]any)["p2"] = map[string]any{
			"id": "p2", "customer_id": "c2", "total_fen": float64(300), "note": "另一笔",
			"allocations": []any{map[string]any{"month": "2026-09", "amount_fen": float64(300)}},
			"seq":         float64(3), "created_at": "2026-10-01T00:00:00Z",
		}
		doc["next_seq"] = float64(3)
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "实收 900 分超过应付 800 分") {
		t.Fatal(msg)
	}
	// 损坏文件不被改写。
	h = base(t)
	mutate(t, h, func(doc map[string]any) {
		doc["ownership_transfers"].(map[string]any)["t1"].(map[string]any)["to_customer_id"] = "ghost"
	})
	broken, err := os.ReadFile(h.statePath()) // 损坏后的完整文件内容
	if err != nil {
		t.Fatal(err)
	}
	h.runExpectErr("bill", "show", "c2", "2026-09")
	got, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(broken) {
		t.Fatal("载入失败后数据文件被改写")
	}
}

func TestReassignLegacyArchiveWithoutTransfers(t *testing.T) {
	h := newHarness(t)
	setupTwoCustomers(t, h)
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	// 删除归属更正字段的旧存档视为无归属更正，收款按首次归属正常读取。
	mutateState(t, h, func(doc map[string]any) { delete(doc, "ownership_transfers") })
	c1 := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(c1, "实收：400 分") {
		t.Fatal(c1)
	}
	c2 := h.mustRun("bill", "show", "c2", "2026-09")
	if !strings.Contains(c2, "实收：0 分") {
		t.Fatal(c2)
	}
}
