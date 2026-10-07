package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// --- 部分退款登记测试 ---

// setupRefundBase 登记固定单价客户并结算 2026-09（500 分）与 2026-10（300 分）
// 两个账期，再登记一笔跨月汇款 pay1（总额 800 分：2026-09:500、2026-10:300）。
func setupRefundBase(h *harness) {
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,5\n"+
		"u2,c1,2026-10-15T10:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.mustRun("bill", "remit", "c1", "pay1", "800", "九月十月汇款", "2026-09:500", "2026-10:300")
}

func TestRefundHappyPathAndBalance(t *testing.T) {
	h := newHarness(t)
	setupRefundBase(h)

	out := h.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:200", "2026-10:100")
	for _, want := range []string{
		"退款标识：rf1", "关联收款：pay1", "原因：多收退回",
		"月份 2026-09：本次退款 200 分", "剩余可退 300 分",
		"月份 2026-10：本次退款 100 分", "剩余可退 200 分",
		"不可撤销",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("退款成功输出缺少 %q:\n%s", want, out)
		}
	}

	// 退款只减少对应月实收、增加未收，不改变应付。
	out = h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"当前应付：500 分", "实收：300 分", "未收余额：200 分",
		"退款 rf1：原因：多收退回（关联收款 pay1，本账单退款 200 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("bill show(2026-09) 缺少 %q:\n%s", want, out)
		}
	}
	out = h.mustRun("bill", "show", "c1", "2026-10")
	for _, want := range []string{"当前应付：300 分", "实收：200 分", "未收余额：100 分", "退款 rf1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("bill show(2026-10) 缺少 %q:\n%s", want, out)
		}
	}
	// 原收款总额、备注、原始分配永久保留。
	out = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "收款 pay1：总额 800 分") {
		t.Fatalf("原收款总额未保留:\n%s", out)
	}
}

func TestRefundIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	setupRefundBase(h)
	h.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:200")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 清单顺序无关的相同重放：返回原记录且不写盘。
	out := h.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:200")
	if !strings.Contains(out, "已存在且内容相同，返回原记录") {
		t.Fatalf("相同重放未幂等返回:\n%s", out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("相同重放改写了存档")
	}

	// 退尽后相同重放仍成功。
	h.mustRun("bill", "refund", "pay1", "rf2", "继续退回", "2026-09:300")
	out = h.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:200")
	if !strings.Contains(out, "返回原记录") {
		t.Fatalf("退尽后相同重放应成功:\n%s", out)
	}

	// 内容不同（金额、原因、目标收款任一不同）拒绝。
	if msg := h.runExpectErr("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:201"); !strings.Contains(msg, "内容不同") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "refund", "pay1", "rf1", "其他原因", "2026-09:200"); !strings.Contains(msg, "内容不同") {
		t.Fatal(msg)
	}
	h.mustRun("customer", "add", "c2", "乙方", "100")
	f2 := h.writeFile("u2.csv", csvHeader+"v1,c2,2026-09-15T10:00:00Z,5\n")
	h.mustRun("usage", "import", f2)
	h.mustRun("bill", "settle", "c2", "2026-09")
	h.mustRun("bill", "pay", "c2", "2026-09", "pay2", "500", "乙方九月")
	if msg := h.runExpectErr("bill", "refund", "pay2", "rf1", "多收退回", "2026-09:200"); !strings.Contains(msg, "内容不同") {
		t.Fatal(msg)
	}
	// 退款标识与收款、调整、更正标识独立，可同名。
	h.mustRun("bill", "refund", "pay2", "pay1", "与收款同名的退款标识", "2026-09:100")
}

func TestRefundValidation(t *testing.T) {
	h := newHarness(t)
	setupRefundBase(h)

	// 引用缺失与空参数。
	if msg := h.runExpectErr("bill", "refund", "ghost", "rf1", "原因", "2026-09:100"); !strings.Contains(msg, "不存在") {
		t.Fatal(msg)
	}
	h.runExpectErr("bill", "refund", "pay1", "", "原因", "2026-09:100")    // 空退款标识
	h.runExpectErr("bill", "refund", "pay1", "rf1", "  ", "2026-09:100") // 空原因
	h.runExpectErr("bill", "refund", "pay1", "rf1", "原因")                // 空清单
	h.runExpectErr("bill", "refund", "pay1", "rf1", "原因", "2026-9:100")
	h.runExpectErr("bill", "refund", "pay1", "rf1", "原因", "2026-09:0")
	h.runExpectErr("bill", "refund", "pay1", "rf1", "原因", "2026-09:-5")
	h.runExpectErr("bill", "refund", "pay1", "rf1", "原因", "2026-09:100", "2026-09:50") // 月份重复

	// 仅允许退最新分配涉及的月份。
	if msg := h.runExpectErr("bill", "refund", "pay1", "rf1", "原因", "2026-11:100"); !strings.Contains(msg, "最新分配不涉及月份 2026-11") {
		t.Fatal(msg)
	}
	// 超退：超过该月分配减累计退款，整笔拒绝且任一项不生效。
	if msg := h.runExpectErr("bill", "refund", "pay1", "rf1", "原因", "2026-09:501"); !strings.Contains(msg, "超过剩余可退额") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "refund", "pay1", "rf1", "原因", "2026-09:100", "2026-10:301"); !strings.Contains(msg, "整笔拒绝") {
		t.Fatal(msg)
	}
	// 失败不占退款标识与序号：同一标识可原样重试成功。
	h.mustRun("bill", "refund", "pay1", "rf1", "原因", "2026-09:100")

	// 已撤销收款不得退款（rf1 已退 100 分，2026-09 未收余额恢复为 100 分）。
	h.mustRun("bill", "pay", "c1", "2026-09", "pay3", "100", "九月补款")
	h.mustRun("bill", "unpay", "pay3", "误登记")
	if msg := h.runExpectErr("bill", "refund", "pay3", "rf9", "原因", "2026-09:50"); !strings.Contains(msg, "已撤销") {
		t.Fatal(msg)
	}
}

func TestRefundFreezeAndReplay(t *testing.T) {
	h := newHarness(t)
	setupRefundBase(h)

	// 首次退款前可以更正；更正后的最新分配成为退款基准。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj0", "100", "补收")
	h.mustRun("bill", "correct", "pay1", "corr1", "修正入账月份", "2026-09:600", "2026-10:200")
	h.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:100")

	// 首次退款后：拒绝新增分配更正及整笔撤销。
	if msg := h.runExpectErr("bill", "correct", "pay1", "corr2", "再次更正", "2026-09:500", "2026-10:300"); !strings.Contains(msg, "已固定") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "unpay", "pay1", "整笔退回"); !strings.Contains(msg, "已固定") {
		t.Fatal(msg)
	}
	// 已有收款、更正的相同重放仍成功，不恢复已退金额或旧分配。
	if out := h.mustRun("bill", "remit", "c1", "pay1", "800", "九月十月汇款", "2026-09:500", "2026-10:300"); !strings.Contains(out, "返回已保存记录") {
		t.Fatalf("收款相同重放应成功:\n%s", out)
	}
	if out := h.mustRun("bill", "correct", "pay1", "corr1", "修正入账月份", "2026-10:200", "2026-09:600"); !strings.Contains(out, "返回原更正记录") {
		t.Fatalf("更正相同重放应成功:\n%s", out)
	}
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "实收：500 分") { // 最新分配 600 减退款 100
		t.Fatalf("重放后实收异常:\n%s", out)
	}

	// 无退款收款仍按原规则处理：可更正、可整笔撤销。
	h.mustRun("bill", "pay", "c1", "2026-10", "pay4", "100", "十月补款")
	h.mustRun("bill", "correct", "pay4", "corr9", "修正", "2026-09:100")
	h.mustRun("bill", "unpay", "pay4", "误登记")
}

func TestRefundLedgerAndReconcile(t *testing.T) {
	h := newHarness(t)
	setupRefundBase(h)
	h.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:200", "2026-10:100")

	// 流水：收款序号 1、退款序号 2；截止 1 时退款不影响历史余额。
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "序号 2 退款 rf1：实收 -200 分") || !strings.Contains(out, "截止时余额：应付 500 分（5.00 元），实收 300 分") {
		t.Fatalf("ledger 缺少退款事件或余额异常:\n%s", out)
	}
	out = h.mustRun("bill", "ledger", "c1", "2026-09", "1")
	if strings.Contains(out, "退款") || !strings.Contains(out, "截止时余额：应付 500 分（5.00 元），实收 500 分") {
		t.Fatalf("截止后的退款不应影响历史余额:\n%s", out)
	}

	// 对账：跨月退款作为一次操作，逐月变化合并在同一条目。
	out = h.mustRun("bill", "reconcile", "c1", "2026-09", "2026-10")
	if !strings.Contains(out, "序号 2 退款 rf1（关联收款 pay1）") ||
		!strings.Contains(out, "2026-09 实收 -200 分（退款 200 分），2026-10 实收 -100 分（退款 100 分）") {
		t.Fatalf("reconcile 未把跨月退款作为一次操作展示:\n%s", out)
	}
	if !strings.Contains(out, "终点合计：应付 800 分") || !strings.Contains(out, "实收 500 分") {
		t.Fatalf("reconcile 汇总异常:\n%s", out)
	}
	// 终点余额与 bill show 一致。
	if out2 := h.mustRun("bill", "show", "c1", "2026-10"); !strings.Contains(out2, "实收：200 分") {
		t.Fatalf("bill show 与 reconcile 不一致:\n%s", out2)
	}
	// 查询只读：存档不变。
	before, _ := os.ReadFile(h.statePath())
	h.mustRun("bill", "reconcile", "c1", "2026-09", "2026-10", "0", "2")
	h.mustRun("bill", "ledger", "c1", "2026-09", "0")
	after, _ := os.ReadFile(h.statePath())
	if string(before) != string(after) {
		t.Fatal("只读查询改写了存档")
	}
}

func TestRefundFreesBalance(t *testing.T) {
	h := newHarness(t)
	setupRefundBase(h)
	h.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:200", "2026-10:100")

	// 新收款按退款后余额约束：2026-09 未收余额恢复为 200 分，可再登记。
	h.mustRun("bill", "pay", "c1", "2026-09", "pay5", "200", "重新收到")
	if msg := h.runExpectErr("bill", "pay", "c1", "2026-09", "pay6", "1", "超额"); !strings.Contains(msg, "超过未收余额") {
		t.Fatal(msg)
	}
	// 费用调整及其撤销按退款后余额约束：应付可降至退款后实收水平（200 分）。
	h.mustRun("bill", "adjust", "c1", "2026-10", "adj1", "-100", "减免")
	if msg := h.runExpectErr("bill", "adjust", "c1", "2026-10", "adj2", "-101", "超额减免"); !strings.Contains(msg, "低于实收") {
		t.Fatal(msg)
	}
}

func TestRefundSeqAndAtomicity(t *testing.T) {
	h := newHarness(t)
	setupRefundBase(h)
	h.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:100") // 序号 2
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj1", "50", "补收")  // 序号 3
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "序号 2 退款 rf1") || !strings.Contains(out, "序号 3 调整 adj1") {
		t.Fatalf("退款未占用递增的全局序号:\n%s", out)
	}
	// 重启（重新载入）后退款、幂等与限制保持。
	if out := h.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:100"); !strings.Contains(out, "返回原记录") {
		t.Fatalf("重启后相同重放应成功:\n%s", out)
	}
	if msg := h.runExpectErr("bill", "unpay", "pay1", "整笔退回"); !strings.Contains(msg, "已固定") {
		t.Fatal(msg)
	}
}

// mutateState 读取存档、按 fn 修改 JSON 后写回，用于构造损坏/兼容场景。
func mutateState(t *testing.T, h *harness, fn func(map[string]any)) {
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

func TestRefundLoadValidation(t *testing.T) {
	// 旧文件缺少退款字段视为无退款，旧单月收款仍可用。
	h := newHarness(t)
	setupRefundBase(h)
	mutateState(t, h, func(doc map[string]any) { delete(doc, "refunds") })
	if out := h.mustRun("bill", "show", "c1", "2026-09"); !strings.Contains(out, "实收：500 分") {
		t.Fatalf("旧文件应按无退款载入:\n%s", out)
	}
	h.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:100")

	// 失效引用：退款目标不存在。
	h2 := newHarness(t)
	setupRefundBase(h2)
	h2.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:100")
	mutateState(t, h2, func(doc map[string]any) {
		doc["refunds"].(map[string]any)["rf1"].(map[string]any)["payment_id"] = "ghost"
	})
	if msg := h2.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "退款目标失效") {
		t.Fatal(msg)
	}

	// 累计超退。
	h3 := newHarness(t)
	setupRefundBase(h3)
	h3.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:100")
	mutateState(t, h3, func(doc map[string]any) {
		rf := doc["refunds"].(map[string]any)["rf1"].(map[string]any)
		rf["allocations"].([]any)[0].(map[string]any)["amount_fen"] = 600
	})
	if msg := h3.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "累计超退") {
		t.Fatal(msg)
	}

	// 非法操作先后：首次退款后又出现更正。
	h4 := newHarness(t)
	setupRefundBase(h4)
	h4.mustRun("bill", "adjust", "c1", "2026-09", "adj0", "100", "补收")
	h4.mustRun("bill", "correct", "pay1", "corr1", "修正", "2026-09:600", "2026-10:200")
	h4.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:100")
	mutateState(t, h4, func(doc map[string]any) {
		doc["corrections"].(map[string]any)["corr1"].(map[string]any)["seq"] = 9
		doc["next_seq"] = 9
	})
	if msg := h4.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "操作先后非法") {
		t.Fatal(msg)
	}

	// 非法操作先后：退款后整笔撤销。
	h5 := newHarness(t)
	setupRefundBase(h5)
	h5.mustRun("bill", "refund", "pay1", "rf1", "多收退回", "2026-09:100")
	mutateState(t, h5, func(doc map[string]any) {
		p := doc["payments"].(map[string]any)["pay1"].(map[string]any)
		p["revoked"] = true
		p["revoke_reason"] = "伪造撤销"
		p["revoke_seq"] = 9
		doc["next_seq"] = 9
	})
	if msg := h5.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "操作先后非法") {
		t.Fatal(msg)
	}
}

func TestRefundUsageErrors(t *testing.T) {
	h := newHarness(t)
	// 参数数量错误是用法错误（退出码 2）；空清单属于业务错误（退出码 1）。
	for _, args := range [][]string{
		{"bill", "refund"},
		{"bill", "refund", "pay1", "rf1"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}
	if _, err := h.run("bill", "refund", "pay1", "rf1", "原因"); err == nil {
		t.Fatal("空清单应失败")
	} else {
		var ue usageErrorf
		if errors.As(err, &ue) {
			t.Fatalf("空清单应为业务错误(1)，得到用法错误: %v", err)
		}
	}
}
