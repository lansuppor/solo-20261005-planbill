package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// --- 收款客户归属更正（bill reassign）测试 ---

// setupReassignBase 登记两个固定单价客户并结算 2026-09（各 500 分）与
// 2026-10（c1 300 分）；c1 再登记一笔跨月汇款 r1（总额 800：
// 2026-09:500、2026-10:300）。c2 只有 2026-09 账单，2026-10 未结算。
func setupReassignBase(h *harness) {
	h.mustRun("customer", "add", "c1", "甲方", "100")
	h.mustRun("customer", "add", "c2", "乙方", "100")
	f := h.writeFile("ur.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,5\n"+
		"u2,c1,2026-10-15T10:00:00Z,3\n"+
		"u3,c2,2026-09-15T10:00:00Z,5\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.mustRun("bill", "settle", "c2", "2026-09")
	h.mustRun("bill", "remit", "c1", "r1", "800", "九月十月汇款", "2026-09:500", "2026-10:300")
}

func TestReassignHappyPathAndBalances(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h)

	// 把 r1 整笔从 c1 转到 c2 的 2026-09（应付 500），总额 800 全部计入
	// c2 九月（先补收 300 以容纳）。
	h.mustRun("bill", "adjust", "c2", "2026-09", "adj2", "300", "补收")
	out := h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错客户", "2026-09:800")
	for _, want := range []string{
		"已登记收款客户归属更正", "更正标识：rc1", "客户 c1 → 客户 c2",
		"更正前分配（客户 c1）：2026-09:500,2026-10:300",
		"更正后分配（客户 c2）：2026-09:800",
		"[转出] 客户 c1 月份 2026-09：撤去分配 500 分",
		"[转出] 客户 c1 月份 2026-10：撤去分配 300 分",
		"[转入] 客户 c2 月份 2026-09：计入分配 800 分",
		"当前归属客户 c2",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("归属更正输出缺少 %q:\n%s", want, out)
		}
	}

	// 转出侧两月实收归零、应付不变；转入侧实收 800、应付 800。
	for _, m := range []string{"2026-09", "2026-10"} {
		show := h.mustRun("bill", "show", "c1", m)
		if !strings.Contains(show, "实收：0 分") {
			t.Fatalf("c1 %s 应实收 0:\n%s", m, show)
		}
	}
	show := h.mustRun("bill", "show", "c2", "2026-09")
	for _, want := range []string{"当前应付：800 分", "实收：800 分", "未收余额：0 分"} {
		if !strings.Contains(show, want) {
			t.Fatalf("c2 九月缺少 %q:\n%s", want, show)
		}
	}
	// 原登记身份永久保留：转入侧历史说明款项来自原登记客户 c1。
	if !strings.Contains(show, "自客户 c1 整笔转入") {
		t.Fatalf("转入侧历史应说明原登记客户:\n%s", show)
	}
}

func TestReassignValidation(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h)

	h.runExpectErr("bill", "reassign", "", "rc1", "c2", "原因", "2026-09:500")
	h.runExpectErr("bill", "reassign", "r1", "", "c2", "原因", "2026-09:500")
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "  ", "2026-09:500")
	h.runExpectErr("bill", "reassign", "ghost", "rc1", "c2", "原因", "2026-09:500") // 收款不存在
	h.runExpectErr("bill", "reassign", "r1", "rc1", "ghost", "原因", "2026-09:500") // 目标不存在
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c1", "自我归属", "2026-09:500")  // 目标==当前归属
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "原因")                   // 空分配
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "原因", "2026-9:500")
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "原因", "2026-09:0")
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "原因", "2026-09:-5")
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "原因", "2026-09:500", "2026-09:300") // 月份重复
	// 目标客户无该月账单（c2 未结算 2026-10）。
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "原因", "2026-10:800")
	// 合计不等于原总额。
	h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "原因", "2026-09:799")
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "原因", "2026-09:801")
	// 合计溢出。
	h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "原因",
		"2026-09:9223372036854775806", "2026-10:2")
	// 转入超额（c2 九月应付 800，分配 800 合法；先构造超额：恢复不补收的 c2
	// 用另一目标）。这里用 c1 已收满的月份无法自我归属，改为 c2 不补收的新库。
	h2 := newHarness(t)
	h2.mustRun("customer", "add", "x1", "甲", "100")
	h2.mustRun("customer", "add", "x2", "乙", "100")
	fx := h2.writeFile("ux.csv", csvHeader+
		"x1u,x1,2026-09-15T10:00:00Z,5\nx2u,x2,2026-09-15T10:00:00Z,3\n")
	h2.mustRun("usage", "import", fx)
	h2.mustRun("bill", "settle", "x1", "2026-09")
	h2.mustRun("bill", "settle", "x2", "2026-09")
	h2.mustRun("bill", "remit", "x1", "pr", "500", "备注", "2026-09:500")
	// x2 九月应付仅 300，转入 500 超额：整笔拒绝。
	if msg := h2.runExpectErr("bill", "reassign", "pr", "rc", "x2", "原因", "2026-09:500"); !strings.Contains(msg, "超过当前应付") {
		t.Fatal(msg)
	}
	// 全部状态不变，标识未占用：x1 仍实收 500，x2 仍实收 0。
	if s := h2.mustRun("bill", "show", "x1", "2026-09"); !strings.Contains(s, "实收：500 分") {
		t.Fatal(s)
	}
	if s := h2.mustRun("bill", "show", "x2", "2026-09"); !strings.Contains(s, "实收：0 分") {
		t.Fatal(s)
	}
	// 标识可原样用于合法更正（先给 x2 补收到 500）。
	h2.mustRun("bill", "adjust", "x2", "2026-09", "ax", "200", "补收")
	h2.mustRun("bill", "reassign", "pr", "rc", "x2", "原因", "2026-09:500")
}

func TestReassignIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h)
	h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
	h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错客户", "2026-09:800")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 清单顺序无关的相同重放：返回原更正与当前状态，不写盘、不增序号。
	out := h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错客户", "2026-09:800")
	if !strings.Contains(out, "已存在且内容相同，返回原更正与当前状态") {
		t.Fatalf("相同重放未幂等返回:\n%s", out)
	}
	after, _ := os.ReadFile(h.statePath())
	if string(after) != string(before) {
		t.Fatal("相同重放改写了存档")
	}
	// 内容不同（收款、目标客户、原因、月份金额任一）拒绝。
	if msg := h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "记错客户", "2026-09:700", "2026-10:100"); !strings.Contains(msg, "内容不同") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "reassign", "r1", "rc1", "c2", "别的原因", "2026-09:800"); !strings.Contains(msg, "内容不同") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "reassign", "r1", "rc1", "c1", "记错客户", "2026-09:500", "2026-10:300"); !strings.Contains(msg, "内容不同") {
		t.Fatal(msg)
	}
}

func TestReassignCrossTypeIDNotReusable(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h)

	// 先占分配更正标识，再用于归属更正：拒绝。
	h.mustRun("bill", "correct", "r1", "same", "改月", "2026-09:500", "2026-10:300")
	if msg := h.runExpectErr("bill", "reassign", "r1", "same", "c2", "原因", "2026-09:800"); !strings.Contains(msg, "已由分配更正") {
		t.Fatal(msg)
	}
	// 反向：归属更正标识不能用于分配更正。
	h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
	h.mustRun("bill", "reassign", "r1", "rid", "c2", "记错", "2026-09:800")
	if msg := h.runExpectErr("bill", "correct", "r1", "rid", "改月", "2026-09:800"); !strings.Contains(msg, "已由归属更正占用") {
		t.Fatal(msg)
	}
	// 更正标识与收款、调整标识相互独立、可同名。
	h.mustRun("bill", "reassign", "r1", "r1", "c1", "与收款同名", "2026-09:500", "2026-10:300")
}

func TestReassignChain(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h)
	h.mustRun("customer", "add", "c3", "丙方", "100")
	f := h.writeFile("u3.csv", csvHeader+"u9,c3,2026-09-15T10:00:00Z,8\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c3", "2026-09") // 应付 800

	// c1 -> c2（c2 九月补收到 800）-> c3（应付 800），每次只占一个序号。
	h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
	h.mustRun("bill", "reassign", "r1", "rc1", "c2", "错1", "2026-09:800")
	h.mustRun("bill", "reassign", "r1", "rc2", "c3", "错2", "2026-09:800")

	// 最终：c1、c2 实收 0，c3 实收 800。
	if s := h.mustRun("bill", "show", "c1", "2026-09"); !strings.Contains(s, "实收：0 分") {
		t.Fatal(s)
	}
	if s := h.mustRun("bill", "show", "c2", "2026-09"); !strings.Contains(s, "实收：0 分") {
		t.Fatal(s)
	}
	s := h.mustRun("bill", "show", "c3", "2026-09")
	if !strings.Contains(s, "实收：800 分") || !strings.Contains(s, "未收余额：0 分") {
		t.Fatal(s)
	}
	// 两侧历史都保留两次归属更正线索。
	if !strings.Contains(s, "自客户 c2 整笔转入") {
		t.Fatal(s)
	}
	c2show := h.mustRun("bill", "show", "c2", "2026-09")
	if !strings.Contains(c2show, "自客户 c1 整笔转入") || !strings.Contains(c2show, "整笔转出至客户 c3") {
		t.Fatal(c2show)
	}

	// 只能再转回异于当前归属（c3）的客户；转回 c1 用 c1 的两月账单。
	h.mustRun("bill", "reassign", "r1", "rc3", "c1", "错3", "2026-09:500", "2026-10:300")
	if s := h.mustRun("bill", "show", "c1", "2026-10"); !strings.Contains(s, "实收：300 分") {
		t.Fatal(s)
	}
	if s := h.mustRun("bill", "show", "c3", "2026-09"); !strings.Contains(s, "实收：0 分") {
		t.Fatal(s)
	}
}

func TestReassignThenCorrectOnlyCurrentOwner(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h)
	h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
	h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错", "2026-09:800")

	// 归属已在 c2：bill correct 只能用 c2 的月份，落到 c1 月份被拒绝。
	if msg := h.runExpectErr("bill", "correct", "r1", "cc1", "改月", "2026-09:500", "2026-10:300"); !strings.Contains(msg, "当前归属客户") {
		t.Fatal(msg)
	}
	// 用 c2 月份做同客户分配更正是合法的（总额仍 800，c2 九月应付 800）。
	h.mustRun("bill", "correct", "r1", "cc1", "改月", "2026-09:800")
}

func TestReassignRefundFreezesAndReplay(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h)
	h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
	h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错", "2026-09:800")
	h.mustRun("bill", "refund", "r1", "rf1", "多收退回", "2026-09:200")

	// 首次退款后：两类更正与整笔撤销都禁止。
	if msg := h.runExpectErr("bill", "correct", "r1", "cc", "x", "2026-09:600"); !strings.Contains(msg, "已固定") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "reassign", "r1", "rc", "c1", "x", "2026-09:500", "2026-10:300"); !strings.Contains(msg, "已固定") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "unpay", "r1", "撤销"); !strings.Contains(msg, "已固定") {
		t.Fatal(msg)
	}
	// 已有归属更正、退款的相同重放仍成功，不恢复旧归属或款项。
	if out := h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错", "2026-09:800"); !strings.Contains(out, "返回原更正与当前状态") {
		t.Fatal(out)
	}
	if out := h.mustRun("bill", "refund", "r1", "rf1", "多收退回", "2026-09:200"); !strings.Contains(out, "返回原记录") {
		t.Fatal(out)
	}
	// 原收款入口仍按首次登记身份判重。
	if out := h.mustRun("bill", "remit", "c1", "r1", "800", "九月十月汇款", "2026-09:500", "2026-10:300"); !strings.Contains(out, "返回已保存记录") {
		t.Fatal(out)
	}
	// 当前余额：c2 实收 600；c1 仍为 0，不因重放恢复。
	if s := h.mustRun("bill", "show", "c2", "2026-09"); !strings.Contains(s, "实收：600 分") {
		t.Fatal(s)
	}
	if s := h.mustRun("bill", "show", "c1", "2026-09"); !strings.Contains(s, "实收：0 分") {
		t.Fatal(s)
	}
}

func TestReassignUnpayActsOnCurrentOwner(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h)
	h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
	h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错", "2026-09:800")
	h.mustRun("bill", "unpay", "r1", "错汇退回")

	// 撤销作用于当前归属 c2 的最新分配：c2 实收归零，c1 保持 0。
	if s := h.mustRun("bill", "show", "c2", "2026-09"); !strings.Contains(s, "实收：0 分") {
		t.Fatal(s)
	}
	for _, m := range []string{"2026-09", "2026-10"} {
		if s := h.mustRun("bill", "show", "c1", m); !strings.Contains(s, "实收：0 分") {
			t.Fatal(s)
		}
	}
	// 撤销后不能再新增归属更正。
	if msg := h.runExpectErr("bill", "reassign", "r1", "rc2", "c1", "x", "2026-09:500", "2026-10:300"); !strings.Contains(msg, "已撤销") {
		t.Fatal(msg)
	}
}

func TestReassignLedgerAndReconcileTwoSides(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h) // r1 序号 1；reassign 将占序号 2
	h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
	h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错", "2026-09:800")

	// 转出侧 c1：序号 1 收款，序号 2 补收（c2），序号 3 转出，截止 1 时迁移不提前影响。
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "序号 3 归属更正 rc1：实收 -500 分") || !strings.Contains(out, "整笔转出至客户 c2") {
		t.Fatalf("c1 流水缺少转出事件:\n%s", out)
	}
	if !strings.Contains(out, "截止时余额：应付 500 分（5.00 元），实收 0 分") {
		t.Fatalf("c1 截止余额异常:\n%s", out)
	}
	cut := h.mustRun("bill", "ledger", "c1", "2026-09", "1")
	if !strings.Contains(cut, "截止时余额：应付 500 分（5.00 元），实收 500 分") || strings.Contains(cut, "归属更正") {
		t.Fatalf("截止 1 不应提前计入转出:\n%s", cut)
	}
	// 转入侧 c2：同一序号 2 列转入。
	in := h.mustRun("bill", "ledger", "c2", "2026-09")
	if !strings.Contains(in, "序号 3 归属更正 rc1：实收 +800 分") || !strings.Contains(in, "自客户 c1 整笔转入") {
		t.Fatalf("c2 流水缺少转入事件:\n%s", in)
	}
	if !strings.Contains(in, "截止时余额：应付 800 分（8.00 元），实收 800 分") {
		t.Fatalf("c2 截止余额异常:\n%s", in)
	}
	cutIn := h.mustRun("bill", "ledger", "c2", "2026-09", "1")
	if !strings.Contains(cutIn, "流水：无") {
		t.Fatalf("截止 1 时 c2 不应有转入:\n%s", cutIn)
	}

	// reconcile 两侧各自只计自己范围内金额，共用同一序号。
	rc1 := h.mustRun("bill", "reconcile", "c1", "2026-09", "2026-10")
	if !strings.Contains(rc1, "序号 3 归属更正 rc1（关联收款 r1，整笔转出至客户 c2）") ||
		!strings.Contains(rc1, "2026-09 实收 -500 分（整笔转出 500 分至客户 c2）") ||
		!strings.Contains(rc1, "2026-10 实收 -300 分（整笔转出 300 分至客户 c2）") {
		t.Fatalf("c1 报表异常:\n%s", rc1)
	}
	if !strings.Contains(rc1, "终点合计：应付 800 分（8.00 元），实收 0 分（0.00 元），未收余额 800 分") {
		t.Fatalf("c1 报表汇总异常:\n%s", rc1)
	}
	rc2 := h.mustRun("bill", "reconcile", "c2", "2026-09", "2026-10")
	if !strings.Contains(rc2, "序号 3 归属更正 rc1（关联收款 r1，自客户 c1 整笔转入）") ||
		!strings.Contains(rc2, "2026-09 实收 +800 分（自客户 c1 整笔转入 800 分）") {
		t.Fatalf("c2 报表异常:\n%s", rc2)
	}
	// 查询只读。
	before, _ := os.ReadFile(h.statePath())
	h.mustRun("bill", "ledger", "c1", "2026-09", "0")
	h.mustRun("bill", "reconcile", "c2", "2026-09", "2026-09")
	after, _ := os.ReadFile(h.statePath())
	if string(before) != string(after) {
		t.Fatal("只读查询改写了存档")
	}
}

func TestReassignNewPaymentAndAdjustConstrained(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h)
	h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
	h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错", "2026-09:800")

	// 转入后 c2 九月已无未收余额，新收款超额拒绝。
	if msg := h.runExpectErr("bill", "pay", "c2", "2026-09", "p2", "1", "多余"); !strings.Contains(msg, "超过未收余额") {
		t.Fatal(msg)
	}
	// 转出释放了 c1 九月欠款 500，可重新登记收款。
	h.mustRun("bill", "pay", "c1", "2026-09", "p3", "500", "重新收到")
	// 调整不得把转入侧应付降到实收以下。
	if msg := h.runExpectErr("bill", "adjust", "c2", "2026-09", "a3", "-1", "超额减免"); !strings.Contains(msg, "低于实收") {
		t.Fatal(msg)
	}
}

func TestReassignPersistsAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	setupReassignBase(h)
	h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
	h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错", "2026-09:800")

	// 重启后归属、余额、判重、序号全部保持。
	if s := h.mustRun("bill", "show", "c2", "2026-09"); !strings.Contains(s, "实收：800 分") {
		t.Fatal(s)
	}
	if s := h.mustRun("bill", "show", "c1", "2026-09"); !strings.Contains(s, "实收：0 分") {
		t.Fatal(s)
	}
	if out := h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错", "2026-09:800"); !strings.Contains(out, "返回原更正与当前状态") {
		t.Fatal(out)
	}
	// 账后序号：r1=1、补收 a2=2、reassign=3（customer/usage/settle 不占序号）。
	out := h.mustRun("bill", "ledger", "c2", "2026-09")
	if !strings.Contains(out, "存档全局序号上限：3") {
		t.Fatalf("归属更正只应占一个序号:\n%s", out)
	}
}

func TestReassignAutoPaymentIdentityKept(t *testing.T) {
	h := newHarness(t)
	// c1 两月欠款共 800，自动分配后整笔转给 c2。
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("customer", "add", "c2", "乙方", "100")
	f := h.writeFile("ua.csv", csvHeader+"ua1,c2,2026-09-15T10:00:00Z,8\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c2", "2026-09") // 应付 800
	h.mustRun("bill", "remit-auto", "c1", "auto1", "800", "自动汇款")
	h.mustRun("bill", "reassign", "auto1", "rc1", "c2", "记错", "2026-09:800")

	// 自动登记身份不改写：remit-auto 相同重放仍按首次（客户、总额、备注）判重。
	if out := h.mustRun("bill", "remit-auto", "c1", "auto1", "800", "自动汇款"); !strings.Contains(out, "返回原收款及当前状态") {
		t.Fatal(out)
	}
	// 当前归属 c2、实收 800；c1 两月实收 0。
	if s := h.mustRun("bill", "show", "c2", "2026-09"); !strings.Contains(s, "实收：800 分") {
		t.Fatal(s)
	}
	for _, m := range []string{"2026-09", "2026-10"} {
		if s := h.mustRun("bill", "show", "c1", m); !strings.Contains(s, "实收：0 分") {
			t.Fatal(s)
		}
	}
}

func TestReassignTieredCustomer(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯P", "10:5", "-:2")
	h.mustRun("customer", "add-plan", "t1", "阶梯客", "p1")
	h.mustRun("customer", "add", "f1", "固定客", "100")
	f := h.writeFile("ut.csv", csvHeader+
		"vt,t1,2026-09-15T10:00:00Z,20\nvf,f1,2026-09-15T10:00:00Z,7\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "t1", "2026-09") // 10*5+10*2=70
	h.mustRun("bill", "settle", "f1", "2026-09") // 700
	h.mustRun("bill", "remit", "t1", "rr", "70", "汇", "2026-09:70")
	h.mustRun("bill", "reassign", "rr", "rc1", "f1", "记错", "2026-09:70")
	if s := h.mustRun("bill", "show", "f1", "2026-09"); !strings.Contains(s, "实收：70 分") {
		t.Fatal(s)
	}
	if s := h.mustRun("bill", "show", "t1", "2026-09"); !strings.Contains(s, "实收：0 分") {
		t.Fatal(s)
	}
}

func TestReassignLoadValidation(t *testing.T) {
	// 构造一份含合法归属更正的存档，再逐一破坏，载入必须拒绝并保留原文件。
	build := func(t *testing.T) *harness {
		h := newHarness(t)
		setupReassignBase(h)
		h.mustRun("bill", "adjust", "c2", "2026-09", "a2", "300", "补收")
		h.mustRun("bill", "reassign", "r1", "rc1", "c2", "记错", "2026-09:800")
		return h
	}

	// 目标客户缺失。
	h := build(t)
	mutateState(t, h, func(doc map[string]any) {
		doc["corrections"].(map[string]any)["rc1"].(map[string]any)["target_customer_id"] = "ghost"
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "数据文件已损坏") {
		t.Fatal(msg)
	}

	// 分配引用目标客户不存在的账单。
	h = build(t)
	mutateState(t, h, func(doc map[string]any) {
		rc := doc["corrections"].(map[string]any)["rc1"].(map[string]any)
		rc["allocations"].([]any)[0].(map[string]any)["month"] = "2026-12"
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "数据文件已损坏") {
		t.Fatal(msg)
	}

	// 目标客户与当前归属相同（非法自我归属更正）。
	h = build(t)
	mutateState(t, h, func(doc map[string]any) {
		doc["corrections"].(map[string]any)["rc1"].(map[string]any)["target_customer_id"] = "c1"
	})
	if msg := h.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "数据文件已损坏") {
		t.Fatal(msg)
	}

	// 分配合计不等于原总额。
	h = build(t)
	mutateState(t, h, func(doc map[string]any) {
		doc["corrections"].(map[string]any)["rc1"].(map[string]any)["allocations"].([]any)[0].(map[string]any)["amount_fen"] = 799
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "数据文件已损坏") {
		t.Fatal(msg)
	}

	// 序号冲突：把归属更正序号改成与收款相同。
	h = build(t)
	mutateState(t, h, func(doc map[string]any) {
		doc["corrections"].(map[string]any)["rc1"].(map[string]any)["seq"] = 1
	})
	if msg := h.runExpectErr("bill", "show", "c2", "2026-09"); !strings.Contains(msg, "数据文件已损坏") {
		t.Fatal(msg)
	}

	// 非法操作先后：首次退款后又出现归属更正。
	h = build(t)
	h.mustRun("bill", "refund", "r1", "rf1", "退", "2026-09:100")
	corrupt := h.statePath()
	raw, _ := os.ReadFile(corrupt)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	// 追加一条序号晚于首次退款的归属更正（转回 c1）。
	doc["corrections"].(map[string]any)["rc2"] = map[string]any{
		"id": "rc2", "payment_id": "r1", "reason": "退款后更正",
		"allocations":        []any{map[string]any{"month": "2026-09", "amount_fen": float64(500)}, map[string]any{"month": "2026-10", "amount_fen": float64(300)}},
		"reassign":           true,
		"target_customer_id": "c1",
		"seq":                float64(doc["next_seq"].(float64) + 1),
		"created_at":         "2026-11-01T00:00:00Z",
	}
	doc["next_seq"] = float64(doc["next_seq"].(float64) + 1)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "操作先后非法") {
		t.Fatal(msg)
	}

	// 逐步余额越界：构造转入实收超过目标账单应付的归属更正。
	h2 := newHarness(t)
	h2.mustRun("customer", "add", "x1", "甲", "100")
	h2.mustRun("customer", "add", "x2", "乙", "100")
	fx := h2.writeFile("ux2.csv", csvHeader+
		"x1u,x1,2026-09-15T10:00:00Z,5\nx2u,x2,2026-09-15T10:00:00Z,3\n")
	h2.mustRun("usage", "import", fx)
	h2.mustRun("bill", "settle", "x1", "2026-09")
	h2.mustRun("bill", "settle", "x2", "2026-09") // 应付仅 300
	h2.mustRun("bill", "remit", "x1", "pr", "500", "备注", "2026-09:500")
	mutateState(t, h2, func(doc map[string]any) {
		doc["corrections"] = map[string]any{"rc": map[string]any{
			"id": "rc", "payment_id": "pr", "reason": "越界",
			"allocations":        []any{map[string]any{"month": "2026-09", "amount_fen": float64(500)}},
			"reassign":           true,
			"target_customer_id": "x2",
			"seq":                float64(2),
			"created_at":         "2026-10-01T00:00:00Z",
		}}
		doc["next_seq"] = float64(2)
	})
	if msg := h2.runExpectErr("bill", "show", "x2", "2026-09"); !strings.Contains(msg, "逐步余额越界") {
		t.Fatal(msg)
	}
}
