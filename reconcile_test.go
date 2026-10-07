package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// 跨账期对账报表（bill reconcile）的测试：每个用例使用独立临时数据目录。

// setupReconStore 构造一个跨三个月、含调整/汇款/更正/撤销及同名不同类型
// 记录的存档，返回 harness。操作序号依次为：
//
//	1 调整 adj-1（2026-01 应付 +100）
//	2 收款 pay-1（汇款 900：2026-01:500、2026-02:400）
//	3 更正 cor-1（pay-1 → 2026-01:300、2026-02:600）
//	4 调整 adj-2（2026-03 应付 -50）
//	5 撤销调整 adj-2
//	6 收款 adj-2（与调整 adj-2 同名，2026-03:100）
//	7 撤销收款 adj-2
func setupReconStore(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-01-15T10:00:00Z,10\n"+
		"u2,c1,2026-02-10T10:00:00Z,20\n"+
		"u3,c1,2026-03-05T10:00:00Z,5\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-01")
	h.mustRun("bill", "settle", "c1", "2026-02")
	h.mustRun("bill", "settle", "c1", "2026-03")
	h.mustRun("bill", "adjust", "c1", "2026-01", "adj-1", "100", "漏算")
	h.mustRun("bill", "remit", "c1", "pay-1", "900", "季度汇款", "2026-01:500", "2026-02:400")
	h.mustRun("bill", "correct", "pay-1", "cor-1", "入账月份错误", "2026-01:300", "2026-02:600")
	h.mustRun("bill", "adjust", "c1", "2026-03", "adj-2", "-50", "补偿")
	h.mustRun("bill", "revoke", "adj-2", "撤销补偿")
	h.mustRun("bill", "pay", "c1", "2026-03", "adj-2", "100", "同名收款")
	h.mustRun("bill", "unpay", "adj-2", "撤销同名收款")
	return h
}

func TestReconcileHappyPath(t *testing.T) {
	h := setupReconStore(t)

	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-03")
	for _, want := range []string{
		"客户：c1（甲方）",
		"账期范围：2026-01 至 2026-03（UTC 自然月，包含首尾）",
		"操作序号区间：起点 0、终点 7（省略，按 0 至存档最新序号）",
		"存档全局序号上限：7",
		"范围内账单：3 张",
		"月份 2026-01：账单 " + stableBillID("c1", "2026-01") + "，原总金额 1000 分（10.00 元）",
		"月份 2026-02：账单 " + stableBillID("c1", "2026-02") + "，原总金额 2000 分（20.00 元）",
		"月份 2026-03：账单 " + stableBillID("c1", "2026-03") + "，原总金额 500 分（5.00 元）",
		"原总金额合计：3500 分（35.00 元）",
		"起点合计：应付 3500 分（35.00 元），实收 0 分（0.00 元），未收余额 3500 分（35.00 元）",
		"终点合计：应付 3600 分（36.00 元），实收 900 分（9.00 元），未收余额 2700 分（27.00 元）",
		"序号 1 调整 adj-1：原因：漏算；范围内变化：2026-01 应付 +100 分",
		"序号 2 收款 pay-1：备注：季度汇款；汇款总额 900 分（9.00 元）；范围内变化：2026-01 实收 +500 分（分配 500 分），2026-02 实收 +400 分（分配 400 分）",
		"序号 3 更正 cor-1（关联收款 pay-1）：原因：入账月份错误；范围内变化：2026-01 实收 -200 分（分配 500 分 → 300 分），2026-02 实收 +200 分（分配 400 分 → 600 分）",
		"序号 4 调整 adj-2：原因：补偿；范围内变化：2026-03 应付 -50 分",
		"序号 5 撤销调整 adj-2（关联序号 4 的调整 adj-2）：原因：撤销补偿；范围内变化：2026-03 应付 +50 分",
		"序号 6 收款 adj-2：备注：同名收款；汇款总额 100 分（1.00 元）；范围内变化：2026-03 实收 +100 分（分配 100 分）",
		"序号 7 撤销收款 adj-2（关联序号 6 的收款 adj-2）：原因：撤销同名收款",
		"2026-03 实收 -100 分（取消分配 100 分）",
		"事后汇总：应付 3600 分，实收 900 分，未收余额 2700 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q：\n%s", want, out)
		}
	}
	// 同名不同类型记录各自只出现一次且按类型区分。
	if strings.Count(out, "调整 adj-2：") != 1 || strings.Count(out, "收款 adj-2：") != 1 {
		t.Fatalf("同名调整/收款未按类型区分或重复展示：\n%s", out)
	}

	// 逐月终点余额与 bill ledger（截止最新）及 bill show 一致。
	for month, bal := range map[string]string{
		"2026-01": "应付 1100 分（11.00 元），实收 300 分（3.00 元），未收余额 800 分（8.00 元）",
		"2026-02": "应付 2000 分（20.00 元），实收 600 分（6.00 元），未收余额 1400 分（14.00 元）",
		"2026-03": "应付 500 分（5.00 元），实收 0 分（0.00 元），未收余额 500 分（5.00 元）",
	} {
		ledger := h.mustRun("bill", "ledger", "c1", month)
		if !strings.Contains(ledger, "截止时余额："+bal) {
			t.Fatalf("%s ledger 余额异常：\n%s", month, ledger)
		}
		if !strings.Contains(out, "终点："+bal) {
			t.Fatalf("报表 %s 终点余额与 ledger 不一致：\n%s", month, out)
		}
		show := h.mustRun("bill", "show", "c1", month)
		for _, field := range strings.Split(bal, "，") {
			kv := strings.SplitN(field, " ", 2)
			if !strings.Contains(show, kv[0]+"："+kv[1]) {
				t.Fatalf("%s bill show 缺少 %q：\n%s", month, field, show)
			}
		}
	}

	// 同一存档重复查询结果稳定。
	if again := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-03"); again != out {
		t.Fatalf("重复查询结果不稳定：\n%s\n---\n%s", out, again)
	}
}

func TestReconcileSeqRange(t *testing.T) {
	h := setupReconStore(t)

	// 指定区间 (2, 3]：起点余额含收款，终点余额含更正。
	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-03", "2", "3")
	for _, want := range []string{
		"操作序号区间：起点 2、终点 3（指定）",
		"起点合计：应付 3600 分（36.00 元），实收 900 分（9.00 元），未收余额 2700 分（27.00 元）",
		"终点合计：应付 3600 分（36.00 元），实收 900 分（9.00 元），未收余额 2700 分（27.00 元）",
		"序号 3 更正 cor-1（关联收款 pay-1）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "序号 2 收款") || strings.Contains(out, "序号 4 调整") {
		t.Fatalf("区间外操作混入报表：\n%s", out)
	}
	// 起点逐月余额与相同截止的 bill ledger 一致。
	ledger := h.mustRun("bill", "ledger", "c1", "2026-01", "2")
	if !strings.Contains(ledger, "截止时余额：应付 1100 分（11.00 元），实收 500 分（5.00 元），未收余额 600 分（6.00 元）") {
		t.Fatalf("ledger 截止 2 余额异常：\n%s", ledger)
	}
	if !strings.Contains(out, "起点：应付 1100 分（11.00 元），实收 500 分（5.00 元），未收余额 600 分（6.00 元）") {
		t.Fatalf("报表起点余额与 ledger 截止 2 不一致：\n%s", out)
	}

	// 起点等于终点：变动区间为空，两端余额相同，无区间内操作。
	out = h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-03", "3", "3")
	if !strings.Contains(out, "区间内账后操作：无（序号 3 之后、3 以内无涉及范围内账单的操作）") {
		t.Fatalf("空区间应无操作：\n%s", out)
	}
	if !strings.Contains(out, "起点合计：应付 3600 分") || !strings.Contains(out, "终点合计：应付 3600 分") {
		t.Fatalf("空区间两端汇总应相同：\n%s", out)
	}

	// 空档合法：区间 (4,5] 只含序号 5（撤销调整），起点序号的操作不进入区间。
	out = h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-03", "4", "5")
	if !strings.Contains(out, "序号 5 撤销调整 adj-2") {
		t.Fatalf("区间 (4,5] 应包含序号 5：\n%s", out)
	}
	if strings.Contains(out, "序号 4 调整") {
		t.Fatalf("序号 4 不应出现在 (4,5] 区间：\n%s", out)
	}
}

func TestReconcileEmptyReport(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")

	// 客户存在但范围内无账单：成功输出空报表及零汇总。
	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-12")
	for _, want := range []string{
		"范围内账单：0 张",
		"逐月余额：无（该客户在账期范围内当前没有已存在账单）",
		"原总金额合计：0 分（0.00 元）",
		"起点合计：应付 0 分（0.00 元），实收 0 分（0.00 元），未收余额 0 分（0.00 元）",
		"终点合计：应付 0 分（0.00 元），实收 0 分（0.00 元），未收余额 0 分（0.00 元）",
		"区间内账后操作：无",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("空报表缺少 %q：\n%s", want, out)
		}
	}

	// 有账单但在范围外：同样空报表；范围外账单不被选中。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-06-15T10:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-06")
	out = h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-03")
	if !strings.Contains(out, "范围内账单：0 张") {
		t.Fatalf("范围外账单不应被选中：\n%s", out)
	}
	// 单月范围（起月等于止月）合法。
	out = h.mustRun("bill", "reconcile", "c1", "2026-06", "2026-06")
	if !strings.Contains(out, "范围内账单：1 张") || !strings.Contains(out, "原总金额合计：300 分") {
		t.Fatalf("单月范围报表异常：\n%s", out)
	}
}

func TestReconcileParamValidation(t *testing.T) {
	h := setupReconStore(t)

	// 月份非法、起月晚于止月。
	h.runExpectErr("bill", "reconcile", "c1", "2026-13", "2026-12")
	h.runExpectErr("bill", "reconcile", "c1", "2026-1", "2026-12")
	h.runExpectErr("bill", "reconcile", "c1", "abc", "2026-12")
	h.runExpectErr("bill", "reconcile", "c1", "2026-01", "2026-13")
	h.runExpectErr("bill", "reconcile", "c1", "2026-03", "2026-01")
	// 客户不存在。
	if msg := h.runExpectErr("bill", "reconcile", "nobody", "2026-01", "2026-03"); !strings.Contains(msg, "不存在") {
		t.Fatalf("客户缺失应说明原因：%s", msg)
	}
	// 序号非法：负数、非整数、起点晚于终点、超过存档上限。
	h.runExpectErr("bill", "reconcile", "c1", "2026-01", "2026-03", "-1", "3")
	h.runExpectErr("bill", "reconcile", "c1", "2026-01", "2026-03", "0", "x")
	h.runExpectErr("bill", "reconcile", "c1", "2026-01", "2026-03", "4", "3")
	if msg := h.runExpectErr("bill", "reconcile", "c1", "2026-01", "2026-03", "0", "8"); !strings.Contains(msg, "超过存档全局序号上限 7") {
		t.Fatalf("序号超上限应说明原因：%s", msg)
	}
	// 参数个数错误（含只给一个序号）为用法错误。
	for _, args := range [][]string{
		{"bill", "reconcile"},
		{"bill", "reconcile", "c1", "2026-01"},
		{"bill", "reconcile", "c1", "2026-01", "2026-03", "0"},
		{"bill", "reconcile", "c1", "2026-01", "2026-03", "0", "3", "extra"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}
}

func TestReconcileReadOnlyAndFailureKeepsState(t *testing.T) {
	h := setupReconStore(t)
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 成功与失败的查询都不改写存档、不占用序号。
	h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-03")
	h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-03", "0", "7")
	h.runExpectErr("bill", "reconcile", "c1", "2026-01", "2026-03", "0", "99")
	h.runExpectErr("bill", "reconcile", "nobody", "2026-01", "2026-03")
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("只读报表改写了存档")
	}
	// 后续账后操作序号连续（报表未占用序号）。
	out := h.mustRun("bill", "adjust", "c1", "2026-01", "adj-9", "10", "后续调整")
	_ = out
	ledger := h.mustRun("bill", "ledger", "c1", "2026-01")
	if !strings.Contains(ledger, "存档全局序号上限：8") || !strings.Contains(ledger, "序号 8 调整 adj-9") {
		t.Fatalf("报表占用了操作序号：\n%s", ledger)
	}
}

func TestReconcileCrossRangeCorrection(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-01-15T10:00:00Z,10\n"+
		"u2,c1,2026-02-10T10:00:00Z,20\n"+
		"u3,c1,2026-03-05T10:00:00Z,10\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-01")
	h.mustRun("bill", "settle", "c1", "2026-02")
	h.mustRun("bill", "settle", "c1", "2026-03")
	// 1 收款 pay-1：汇款 600，2026-01:300（范围内）、2026-03:300（范围外）。
	h.mustRun("bill", "remit", "c1", "pay-1", "600", "跨期汇款", "2026-01:300", "2026-03:300")
	// 2 更正 cor-1：范围内转移 2026-01 → 2026-02，汇总变化为 0 也保留。
	h.mustRun("bill", "correct", "pay-1", "cor-1", "月份记错", "2026-02:300", "2026-03:300")
	// 3 更正 cor-2：跨范围转出（2026-02 → 2026-03），按实际差额计入。
	h.mustRun("bill", "correct", "pay-1", "cor-2", "再次记错", "2026-03:600")
	// 4 收款 pay-2：2026-01:100（范围内）。
	h.mustRun("bill", "pay", "c1", "2026-01", "pay-2", "100", "单笔收款")
	// 5 更正 cor-3：pay-2 全部转出范围（2026-01 → 2026-03）。
	h.mustRun("bill", "correct", "pay-2", "cor-3", "转出范围", "2026-03:100")
	// 6 撤销 pay-2：撤销时最新分配全在范围外，不纳入报表。
	h.mustRun("bill", "unpay", "pay-2", "撤销单笔")

	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	for _, want := range []string{
		// 多月汇款只按范围内分配计入实收，不重复累加汇款总额。
		"序号 1 收款 pay-1：备注：跨期汇款；汇款总额 600 分（6.00 元）；范围内变化：2026-01 实收 +300 分（分配 300 分）",
		"事后汇总：应付 3000 分，实收 300 分，未收余额 2700 分",
		// 范围内转移：汇总变化为 0 仍保留，逐月差额正确。
		"序号 2 更正 cor-1（关联收款 pay-1）：原因：月份记错；范围内变化：2026-01 实收 -300 分（分配 300 分 → 0 分），2026-02 实收 +300 分（分配 0 分 → 300 分）",
		// 跨范围转出：只计入范围内的实际差额。
		"序号 3 更正 cor-2（关联收款 pay-1）：原因：再次记错；范围内变化：2026-02 实收 -300 分（分配 300 分 → 0 分）",
		"序号 4 收款 pay-2：备注：单笔收款；汇款总额 100 分（1.00 元）；范围内变化：2026-01 实收 +100 分（分配 100 分）",
		"序号 5 更正 cor-3（关联收款 pay-2）：原因：转出范围；范围内变化：2026-01 实收 -100 分（分配 100 分 → 0 分）",
		// 终点汇总：范围内实收已全部转出。
		"终点合计：应付 3000 分（30.00 元），实收 0 分（0.00 元），未收余额 3000 分（30.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q：\n%s", want, out)
		}
	}
	// 撤销时最新分配全在范围外：该撤销不纳入报表。
	if strings.Contains(out, "撤销收款 pay-2") {
		t.Fatalf("撤销时最新分配在范围外，不应纳入报表：\n%s", out)
	}
	// 范围外月份不出现在逐月余额中。
	if strings.Contains(out, "月份 2026-03") {
		t.Fatalf("范围外账期不应出现在报表中：\n%s", out)
	}
}

func TestReconcileRevokeUsesLatestAllocation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-01-15T10:00:00Z,10\n"+
		"u2,c1,2026-02-10T10:00:00Z,20\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-01")
	h.mustRun("bill", "settle", "c1", "2026-02")
	// 1 收款 2026-01:500；2 更正为 2026-02:500；3 撤销（取消最新分配 2026-02:500）。
	h.mustRun("bill", "pay", "c1", "2026-01", "pay-1", "500", "收款")
	h.mustRun("bill", "correct", "pay-1", "cor-1", "更正月份", "2026-02:500")
	h.mustRun("bill", "unpay", "pay-1", "撤销")

	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	if !strings.Contains(out, "序号 3 撤销收款 pay-1（关联序号 1 的收款 pay-1）：原因：撤销；汇款总额 500 分（5.00 元）；范围内变化：2026-02 实收 -500 分（取消分配 500 分）") {
		t.Fatalf("撤销应取消撤销时的最新分配：\n%s", out)
	}
	if !strings.Contains(out, "终点合计：应付 3000 分（30.00 元），实收 0 分（0.00 元），未收余额 3000 分（30.00 元）") {
		t.Fatalf("撤销后终点汇总异常：\n%s", out)
	}

	// 终点早于撤销：终点之后的撤销不提前影响结果。
	out = h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02", "0", "2")
	if strings.Contains(out, "撤销收款") {
		t.Fatalf("终点之后的撤销不应出现：\n%s", out)
	}
	if !strings.Contains(out, "终点合计：应付 3000 分（30.00 元），实收 500 分（5.00 元），未收余额 2500 分（25.00 元）") {
		t.Fatalf("终点之后的撤销提前影响了结果：\n%s", out)
	}
}

func TestReconcileSummaryOverflowExact(t *testing.T) {
	h := newHarness(t)
	// 单账单余额仍在有符号 64 位内，两月合计超出上限，汇总须精确输出。
	h.mustRun("customer", "add", "c1", "甲方", "9223372036854775807")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-01-15T10:00:00Z,1\n"+
		"u2,c1,2026-02-10T10:00:00Z,1\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-01")
	h.mustRun("bill", "settle", "c1", "2026-02")

	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	// 2 × (2^63-1) = 18446744073709551614 分 = 184467440737095516.14 元。
	for _, want := range []string{
		"原总金额合计：18446744073709551614 分（184467440737095516.14 元）",
		"起点合计：应付 18446744073709551614 分（184467440737095516.14 元），实收 0 分（0.00 元），未收余额 18446744073709551614 分（184467440737095516.14 元）",
		"终点合计：应付 18446744073709551614 分（184467440737095516.14 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("超上限汇总缺少 %q：\n%s", want, out)
		}
	}
}

func TestReconcileOldFormatPayment(t *testing.T) {
	h := newHarness(t)
	// 旧版单账单收款格式（month + amount_fen，无 allocations）按一项分配读取。
	h.writeFile("state.json", `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-01-15T10:00:00Z", "quantity": 10}},
  "bills": {"c1|2026-01": {
    "id": "BILL-old", "customer_id": "c1", "month": "2026-01",
    "total_quantity": 10, "unit_price_fen": 100, "total_fee_fen": 1000,
    "lines": [{"usage_id": "u1", "time": "2026-01-15T10:00:00Z", "quantity": 10, "line_fee_fen": 1000}],
    "created_at": "2026-02-01T00:00:00Z"
  }},
  "payments": {"p1": {"id": "p1", "customer_id": "c1", "month": "2026-01", "amount_fen": 500,
    "note": "旧收款", "seq": 1, "created_at": "2026-02-02T00:00:00Z"}},
  "next_seq": 1
}`)
	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-01")
	for _, want := range []string{
		"范围内账单：1 张",
		"序号 1 收款 p1：备注：旧收款；汇款总额 500 分（5.00 元）；范围内变化：2026-01 实收 +500 分（分配 500 分）",
		"终点合计：应付 1000 分（10.00 元），实收 500 分（5.00 元），未收余额 500 分（5.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("旧格式收款报表缺少 %q：\n%s", want, out)
		}
	}
}

func TestReconcileCorruptLedgerRejected(t *testing.T) {
	h := newHarness(t)
	// 最终余额合法但中间步骤越界（序号 2 之后实收 500 > 应付 0）：
	// 即使查询终点在异常之前也拒绝，不输出部分报表。
	h.writeFile("state.json", `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-01-15T10:00:00Z", "quantity": 10}},
  "bills": {"c1|2026-01": {
    "id": "BILL-x", "customer_id": "c1", "month": "2026-01",
    "total_quantity": 10, "unit_price_fen": 100, "total_fee_fen": 1000,
    "lines": [{"usage_id": "u1", "time": "2026-01-15T10:00:00Z", "quantity": 10, "line_fee_fen": 1000}],
    "created_at": "2026-02-01T00:00:00Z"
  }},
  "adjustments": {
    "a1": {"id": "a1", "customer_id": "c1", "month": "2026-01", "amount_fen": -1000,
      "reason": "减免", "seq": 2, "created_at": "2026-02-02T00:00:00Z"},
    "a2": {"id": "a2", "customer_id": "c1", "month": "2026-01", "amount_fen": 1000,
      "reason": "补回", "seq": 3, "created_at": "2026-02-03T00:00:00Z"}
  },
  "payments": {"p1": {"id": "p1", "customer_id": "c1", "total_fen": 500, "note": "收款",
    "allocations": [{"month": "2026-01", "amount_fen": 500}], "seq": 1, "created_at": "2026-02-01T12:00:00Z"}},
  "next_seq": 3
}`)
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 终点在异常序号之前同样拒绝。
	if msg := h.runExpectErr("bill", "reconcile", "c1", "2026-01", "2026-01", "0", "1"); !strings.Contains(msg, "数据异常") {
		t.Fatalf("中间越界应按数据异常拒绝：%s", msg)
	}
	h.runExpectErr("bill", "reconcile", "c1", "2026-01", "2026-01")
	// 失败不改写存档。
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("失败的报表改写了存档")
	}
}

func TestReconcileCorruptOrUnreadableState(t *testing.T) {
	h := newHarness(t)
	// 损坏的 JSON。
	h.writeFile("state.json", `{"version": 1, "customers": `)
	if msg := h.runExpectErr("bill", "reconcile", "c1", "2026-01", "2026-02"); !strings.Contains(msg, "损坏") {
		t.Fatalf("损坏文件应说明原因：%s", msg)
	}
	// JSON 之后存在多余内容。
	h.writeFile("state.json", `{"version":1,"customers":{},"usage":{},"bills":{}} ]`)
	h.runExpectErr("bill", "reconcile", "c1", "2026-01", "2026-02")
}
