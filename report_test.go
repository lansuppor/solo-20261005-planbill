package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// 跨账期对账报表（bill report）测试：只读命令，核对多个账期在一段账后
// 操作中的余额变化；两端逐月余额与 bill ledger 一致，终点为最新时与
// bill show 一致；成功或失败均不改写存档、不占用序号。

// settleReportFixture 构造报表测试场景：c1 有 2026-09（1000 分）与
// 2026-10（500 分）两张账单，c2 有 2026-09（100 分）一张；随后发生：
//
//	序号 1：调整 adj-1（c1 2026-09，+200）
//	序号 2：汇款 r1（c1，总额 700：2026-09:400，2026-10:300）
//	序号 3：调整 adj-c2（c2 2026-09，+50）——其他客户，制造序号空档
//	序号 4：更正 cor-1（r1 → 2026-09:500，2026-10:200）
//	序号 5：撤销调整 adj-1
//	序号 6：撤销收款 r1
func settleReportFixture(h *harness) {
	h.t.Helper()
	settleTwoMonths(h, "c1", "10", "100", "50")                                         // c1 2026-09: 1000，2026-10: 500
	settleBill(h, "c2", "5", "20")                                                      // c2 2026-09: 100
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "200", "漏算用量")                // 序号 1
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300") // 序号 2
	h.mustRun("bill", "adjust", "c2", "2026-09", "adj-c2", "50", "其他客户调整")              // 序号 3
	h.mustRun("bill", "correct", "r1", "cor-1", "修正入账月份", "2026-09:500", "2026-10:200") // 序号 4
	h.mustRun("bill", "revoke", "adj-1", "录入错误")                                        // 序号 5
	h.mustRun("bill", "unpay", "r1", "账号登记错误")                                          // 序号 6
}

func TestReportHappyPathAndConsistency(t *testing.T) {
	h := newHarness(t)
	settleReportFixture(h)

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	out := h.mustRun("bill", "report", "c1", "2026-09", "2026-10")
	for _, want := range []string{
		"客户：c1", "账期范围：2026-09 至 2026-10（含首尾，UTC 自然月）",
		"存档全局序号上限：6",
		"操作序号起点：0（省略，取 0 至存档最新）", "操作序号终点：6（省略，取 0 至存档最新）",
		"选中账单：2 张",
		// 逐月两端余额：起点为序号 0 之后（原总金额、零实收）。
		"原总金额 1000 分（10.00 元）",
		"起点：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）",
		"终点：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）",
		"原总金额 500 分（5.00 元）",
		"起点：应付 500 分（5.00 元），实收 0 分（0.00 元），未收余额 500 分（5.00 元）",
		"终点：应付 500 分（5.00 元），实收 0 分（0.00 元），未收余额 500 分（5.00 元）",
		// 各列汇总。
		"原总金额合计：1500 分（15.00 元）",
		"起点合计：应付 1500 分（15.00 元），实收 0 分（0.00 元），未收余额 1500 分（15.00 元）",
		"终点合计：应付 1500 分（15.00 元），实收 0 分（0.00 元），未收余额 1500 分（15.00 元）",
		// 区间内操作合并展示：多月汇款只展示一次，按范围内分配计入实收。
		"序号 1 调整 adj-1：原因：漏算用量",
		"2026-09 应付 +200 分（2.00 元，补收）",
		"序号 2 收款 r1：备注：季度汇款（汇款总额 700 分（7.00 元））",
		"2026-09 实收 +400 分（4.00 元，本账单分配 400 分）",
		"2026-10 实收 +300 分（3.00 元，本账单分配 300 分）",
		"事后汇总：应付 1700 分（17.00 元），实收 700 分（7.00 元），未收余额 1000 分（10.00 元）",
		"序号 4 更正 cor-1：原因：修正入账月份（关联收款 r1）",
		"2026-09 实收 +100 分（1.00 元，本账单分配 400 分 → 500 分）",
		"2026-10 实收 -100 分（-1.00 元，本账单分配 300 分 → 200 分）",
		"序号 5 撤销调整 adj-1：原因：录入错误（关联序号 1 的调整 adj-1）",
		"2026-09 应付 -200 分（-2.00 元，减免）",
		"序号 6 撤销收款 r1：原因：账号登记错误（关联序号 2 的收款 r1，汇款总额 700 分（7.00 元））",
		"2026-09 实收 -500 分（-5.00 元，取消本账单分配 500 分）",
		"2026-10 实收 -200 分（-2.00 元，取消本账单分配 200 分）",
		"事后汇总：应付 1500 分（15.00 元），实收 0 分（0.00 元），未收余额 1500 分（15.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q:\n%s", want, out)
		}
	}
	// 每次操作只展示一次；其他客户的操作（序号 3）不混入。
	if strings.Count(out, "序号 2 收款 r1") != 1 || strings.Count(out, "序号 4 更正 cor-1") != 1 {
		t.Fatalf("同一操作被重复展示:\n%s", out)
	}
	if strings.Contains(out, "adj-c2") {
		t.Fatalf("其他客户的操作混入报表:\n%s", out)
	}

	// 两端逐月余额与相同截止的 bill ledger 一致。
	for _, m := range []struct{ month, end string }{
		{"2026-09", "截止时余额：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）"},
		{"2026-10", "截止时余额：应付 500 分（5.00 元），实收 0 分（0.00 元），未收余额 500 分（5.00 元）"},
	} {
		ledger := h.mustRun("bill", "ledger", "c1", m.month)
		if !strings.Contains(ledger, m.end) {
			t.Fatalf("bill ledger %s 缺少 %q:\n%s", m.month, m.end, ledger)
		}
		if !strings.Contains(out, "终点："+strings.TrimPrefix(m.end, "截止时余额：")) {
			t.Fatalf("报表 %s 终点余额与 bill ledger 不一致:\n%s", m.month, out)
		}
	}
	// 终点为最新时与 bill show 一致。
	show := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{"当前应付：1000 分", "实收：0 分", "未收余额：1000 分"} {
		if !strings.Contains(show, want) {
			t.Fatalf("bill show 缺少 %q:\n%s", want, show)
		}
	}

	// 同一存档重复查询结果稳定。
	if again := h.mustRun("bill", "report", "c1", "2026-09", "2026-10"); again != out {
		t.Fatalf("两次报表输出不一致:\n%s\n---\n%s", out, again)
	}
	// 只读：不改写存档、不占用序号。
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("只读报表改写了数据文件")
	}
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "10", "后续调整") // 应占序号 7
	out = h.mustRun("bill", "report", "c1", "2026-09", "2026-10")
	if !strings.Contains(out, "存档全局序号上限：7") || !strings.Contains(out, "序号 7 调整 adj-2") {
		t.Fatal(out)
	}
}

func TestReportSeqRange(t *testing.T) {
	h := newHarness(t)
	settleReportFixture(h)

	// 终点 2：终点之后（含更正与撤销）的操作不提前影响结果。
	out := h.mustRun("bill", "report", "c1", "2026-09", "2026-10", "0", "2")
	for _, want := range []string{
		"操作序号起点：0（指定）", "操作序号终点：2（指定）",
		"终点：应付 1200 分（12.00 元），实收 400 分（4.00 元），未收余额 800 分（8.00 元）",
		"终点：应付 500 分（5.00 元），实收 300 分（3.00 元），未收余额 200 分（2.00 元）",
		"终点合计：应付 1700 分（17.00 元），实收 700 分（7.00 元），未收余额 1000 分（10.00 元）",
		"序号 1 调整 adj-1", "序号 2 收款 r1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "更正") || strings.Contains(out, "撤销") {
		t.Fatalf("终点之后的操作混入报表:\n%s", out)
	}

	// 起点 2、终点 4：起点余额取序号 2 之后的状态；区间内只含序号 4
	// （序号 3 是其他客户的操作，空档合法）；起点处的操作不重复计入。
	out = h.mustRun("bill", "report", "c1", "2026-09", "2026-10", "2", "4")
	for _, want := range []string{
		"起点：应付 1200 分（12.00 元），实收 400 分（4.00 元），未收余额 800 分（8.00 元）",
		"起点：应付 500 分（5.00 元），实收 300 分（3.00 元），未收余额 200 分（2.00 元）",
		"终点：应付 1200 分（12.00 元），实收 500 分（5.00 元），未收余额 700 分（7.00 元）",
		"终点：应付 500 分（5.00 元），实收 200 分（2.00 元），未收余额 300 分（3.00 元）",
		"序号 4 更正 cor-1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "序号 1 调整") || strings.Contains(out, "序号 2 收款") ||
		strings.Contains(out, "序号 3 ") || strings.Contains(out, "序号 5 撤销") {
		t.Fatalf("区间外的操作混入报表:\n%s", out)
	}

	// 起点等于终点：变动区间为空，两端余额相同，无区间内操作。
	out = h.mustRun("bill", "report", "c1", "2026-09", "2026-10", "3", "3")
	if !strings.Contains(out, "区间内账后操作（序号大于 3 且不超过 3）：无") ||
		!strings.Contains(out, "起点合计：应付 1700 分（17.00 元），实收 700 分（7.00 元），未收余额 1000 分（10.00 元）") ||
		!strings.Contains(out, "终点合计：应付 1700 分（17.00 元），实收 700 分（7.00 元），未收余额 1000 分（10.00 元）") {
		t.Fatal(out)
	}
}

func TestReportEmptyAndValidation(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "10") // c1 仅 2026-09 有账单

	// 范围内无账单：成功显示空报表及零汇总。
	out := h.mustRun("bill", "report", "c1", "2027-01", "2027-03")
	for _, want := range []string{
		"选中账单：0 张", "按月余额：无",
		"原总金额合计：0 分（0.00 元）",
		"起点合计：应付 0 分（0.00 元），实收 0 分（0.00 元），未收余额 0 分（0.00 元）",
		"终点合计：应付 0 分（0.00 元），实收 0 分（0.00 元），未收余额 0 分（0.00 元）",
		"区间内账后操作（序号大于 0 且不超过 0）：无",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("空报表缺少 %q:\n%s", want, out)
		}
	}

	// 月份非法、起月晚于末月、序号非法、客户缺失均非零退出并说明原因。
	for _, args := range [][]string{
		{"bill", "report", "c1", "2026-9", "2026-10"},
		{"bill", "report", "c1", "2026-09", "2026-13"},
		{"bill", "report", "c1", "2026-10", "2026-09"},
		{"bill", "report", "c1", "2026-09", "2026-10", "-1", "0"},
		{"bill", "report", "c1", "2026-09", "2026-10", "abc", "0"},
		{"bill", "report", "c1", "2026-09", "2026-10", "1", "0"},
		{"bill", "report", "c1", "2026-09", "2026-10", "0", "1"}, // 上限为 0
		{"bill", "report", "ghost", "2026-09", "2026-10"},
	} {
		h.runExpectErr(args...)
	}
	// 参数数量不对（含只给一个序号）：用法错误。
	for _, args := range [][]string{
		{"bill", "report", "c1", "2026-09"},
		{"bill", "report", "c1", "2026-09", "2026-10", "0"},
		{"bill", "report", "c1", "2026-09", "2026-10", "0", "0", "extra"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}
	// 失败后合法查询照常，存档不变。
	out = h.mustRun("bill", "report", "c1", "2026-09", "2026-09")
	if !strings.Contains(out, "选中账单：1 张") {
		t.Fatal(out)
	}
}

func TestReportRemitPartialRange(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "10", "100", "50") // 2026-09: 1000，2026-10: 500
	h.mustRun("customer", "add", "c1b", "客户c1b", "1")
	// 第三个月账单（范围外）：2026-11 总额 300。
	f := h.writeFile("u-nov.csv", csvHeader+"u-nov,c1,2026-11-15T10:00:00Z,30\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-11")
	// 汇款横跨范围内（09、10）与范围外（11）月份。
	h.mustRun("bill", "remit", "c1", "r1", "1000", "三月汇款", "2026-09:400", "2026-10:300", "2026-11:300")

	out := h.mustRun("bill", "report", "c1", "2026-09", "2026-10")
	// 多月汇款按范围内分配计入实收，不重复累加汇款总额。
	for _, want := range []string{
		"选中账单：2 张",
		"序号 1 收款 r1：备注：三月汇款（汇款总额 1000 分（10.00 元））",
		"2026-09 实收 +400 分（4.00 元，本账单分配 400 分）",
		"2026-10 实收 +300 分（3.00 元，本账单分配 300 分）",
		"事后汇总：应付 1500 分（15.00 元），实收 700 分（7.00 元），未收余额 800 分（8.00 元）",
		"终点合计：应付 1500 分（15.00 元），实收 700 分（7.00 元），未收余额 800 分（8.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "2026-11") || strings.Contains(out, "实收 1000 分") {
		t.Fatalf("范围外月份或汇款总额混入实收:\n%s", out)
	}
}

func TestReportCorrectionAndRevokeScope(t *testing.T) {
	h := newHarness(t)
	// c1：2026-09、2026-10（范围内）与 2026-11（范围外）各 200 分账单。
	h.mustRun("customer", "add", "c1", "客户c1", "10")
	f := h.writeFile("u3.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,20\n"+
		"u2,c1,2026-10-15T10:00:00Z,20\n"+
		"u3,c1,2026-11-15T10:00:00Z,20\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.mustRun("bill", "settle", "c1", "2026-11")

	h.mustRun("bill", "pay", "c1", "2026-09", "p-in", "100", "范围内收款")       // 序号 1
	h.mustRun("bill", "correct", "p-in", "cor-move", "月内转移", "2026-10:100") // 序号 2：09 → 10，范围内转移
	h.mustRun("bill", "pay", "c1", "2026-11", "p-out", "50", "范围外收款")       // 序号 3：登记时分配在范围外
	h.mustRun("bill", "correct", "p-out", "cor-in", "转入范围", "2026-09:50")   // 序号 4：跨范围转入
	h.mustRun("bill", "pay", "c1", "2026-10", "p-gone", "50", "将转出的收款")     // 序号 5
	h.mustRun("bill", "correct", "p-gone", "cor-out", "转出范围", "2026-11:50") // 序号 6：跨范围转出
	h.mustRun("bill", "unpay", "p-gone", "撤销转出收款")                          // 序号 7：撤销时最新分配在范围外

	out := h.mustRun("bill", "report", "c1", "2026-09", "2026-10")
	for _, want := range []string{
		// 范围内转移即使汇总变化为 0 也保留。
		"序号 2 更正 cor-move：原因：月内转移（关联收款 p-in）",
		"2026-09 实收 -100 分（-1.00 元，本账单分配 100 分 → 0 分）",
		"2026-10 实收 +100 分（1.00 元，本账单分配 0 分 → 100 分）",
		// 跨范围转入按实际差额计入；收款 p-out 登记时分配在范围外，不展示。
		"序号 4 更正 cor-in：原因：转入范围（关联收款 p-out）",
		"2026-09 实收 +50 分（0.50 元，本账单分配 0 分 → 50 分）",
		// 跨范围转出按实际差额计入。
		"序号 6 更正 cor-out：原因：转出范围（关联收款 p-gone）",
		"2026-10 实收 -50 分（-0.50 元，本账单分配 50 分 → 0 分）",
		// 终点：09 实收 50（p-out 转入），10 实收 100（p-in 转入；p-gone 已撤销）。
		"终点：应付 200 分（2.00 元），实收 50 分（0.50 元），未收余额 150 分（1.50 元）",
		"终点：应付 200 分（2.00 元），实收 100 分（1.00 元），未收余额 100 分（1.00 元）",
		"终点合计：应付 400 分（4.00 元），实收 150 分（1.50 元），未收余额 250 分（2.50 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q:\n%s", want, out)
		}
	}
	// 收款 p-out 登记时分配在范围外：其收款事件不展示（更正中的“关联收款
	// p-out”是合法的关联说明）；撤销 p-gone 时最新分配在范围外：撤销不展示；
	// 范围外月份不出现。
	if strings.Contains(out, "序号 3 收款 p-out") || strings.Contains(out, "撤销收款 p-gone") ||
		strings.Contains(out, "2026-11") {
		t.Fatalf("范围外操作混入报表:\n%s", out)
	}
	// 收款 p-gone 登记时分配在范围内：展示。
	if !strings.Contains(out, "序号 5 收款 p-gone") {
		t.Fatalf("范围内收款未展示:\n%s", out)
	}
}

func TestReportUnpayCancelsLatestAllocation(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "10", "100", "100")                                        // 2026-09: 1000，2026-10: 1000
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300") // 序号 1
	h.mustRun("bill", "correct", "r1", "cor-1", "修正", "2026-09:100", "2026-10:600")     // 序号 2
	h.mustRun("bill", "unpay", "r1", "登记错误")                                            // 序号 3

	// 撤销取消发生时的最新分配（09:100、10:600），而非首次登记分配。
	out := h.mustRun("bill", "report", "c1", "2026-09", "2026-10")
	for _, want := range []string{
		"序号 3 撤销收款 r1：原因：登记错误（关联序号 1 的收款 r1，汇款总额 700 分（7.00 元））",
		"2026-09 实收 -100 分（-1.00 元，取消本账单分配 100 分）",
		"2026-10 实收 -600 分（-6.00 元，取消本账单分配 600 分）",
		"终点合计：应付 2000 分（20.00 元），实收 0 分（0.00 元），未收余额 2000 分（20.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q:\n%s", want, out)
		}
	}
}

func TestReportSummaryExceedsInt64(t *testing.T) {
	h := newHarness(t)
	// 两张账单各为最大 int64 分：单账单合法，多账单汇总超出 64 位上限。
	h.mustRun("customer", "add", "big", "大户", "9223372036854775807")
	f := h.writeFile("u-big.csv", csvHeader+
		"b1,big,2026-09-15T10:00:00Z,1\n"+
		"b2,big,2026-10-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "big", "2026-09")
	h.mustRun("bill", "settle", "big", "2026-10")

	out := h.mustRun("bill", "report", "big", "2026-09", "2026-10")
	// 汇总 = 2 × 9223372036854775807 = 18446744073709551614 分，精确输出。
	for _, want := range []string{
		"原总金额合计：18446744073709551614 分（184467440737095516.14 元）",
		"起点合计：应付 18446744073709551614 分（184467440737095516.14 元），实收 0 分（0.00 元），未收余额 18446744073709551614 分（184467440737095516.14 元）",
		"终点合计：应付 18446744073709551614 分（184467440737095516.14 元），实收 0 分（0.00 元），未收余额 18446744073709551614 分（184467440737095516.14 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("大数汇总缺少 %q:\n%s", want, out)
		}
	}
	// 单张账单仍按 64 位精确展示。
	if !strings.Contains(out, "原总金额 9223372036854775807 分（92233720368547758.07 元）") {
		t.Fatal(out)
	}
}

func TestReportLegacyAndOldArchive(t *testing.T) {
	h := newHarness(t)
	// 旧存档：无 corrections 等字段；旧版单账单收款（month + amount_fen）。
	legacy := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2}},
  "bills": {"c1|2026-09": {
    "id": "BILL-legacy", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 2, "unit_price_fen": 100, "total_fee_fen": 200,
    "lines": [{"usage_id": "u1", "time": "2026-09-15T10:00:00Z", "quantity": 2, "line_fee_fen": 200}],
    "created_at": "2026-10-01T00:00:00Z"
  }},
  "payments": {"pay-1": {
    "id": "pay-1", "customer_id": "c1", "month": "2026-09", "amount_fen": 150,
    "note": "旧格式收款", "seq": 1, "created_at": "2026-10-02T00:00:00Z"
  }},
  "next_seq": 1
}`
	if err := os.WriteFile(h.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	out := h.mustRun("bill", "report", "c1", "2026-09", "2026-12")
	for _, want := range []string{
		"选中账单：1 张",
		"终点：应付 200 分（2.00 元），实收 150 分（1.50 元），未收余额 50 分（0.50 元）",
		"序号 1 收款 pay-1：备注：旧格式收款（汇款总额 150 分（1.50 元））",
		"2026-09 实收 +150 分（1.50 元，本账单分配 150 分）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("旧存档报表缺少 %q:\n%s", want, out)
		}
	}
}

func TestReportSameNameDifferentTypes(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "10")                                // 2026-09 总额 1000
	h.mustRun("bill", "adjust", "c1", "2026-09", "x", "50", "同名调整") // 序号 1
	h.mustRun("bill", "pay", "c1", "2026-09", "x", "100", "同名收款")   // 序号 2
	h.mustRun("bill", "correct", "x", "x", "同名更正", "2026-09:100")   // 序号 3：更正标识亦可同名

	out := h.mustRun("bill", "report", "c1", "2026-09", "2026-09")
	// 同名不同类型记录按类型分别展示，互不混淆。
	for _, want := range []string{
		"序号 1 调整 x：原因：同名调整",
		"序号 2 收款 x：备注：同名收款（汇款总额 100 分（1.00 元））",
		"序号 3 更正 x：原因：同名更正（关联收款 x）",
		"终点：应付 1050 分（10.50 元），实收 100 分（1.00 元），未收余额 950 分（9.50 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q:\n%s", want, out)
		}
	}
}

func TestReportRejectsCorruptLedger(t *testing.T) {
	h := newHarness(t)
	// 手工构造中间状态越界但最终状态合法的存档：序号 2 之后实收 100 超过
	// 应付 50；序号 3 撤销收款后最终余额合法，载入校验通过，但流水回放
	// 必须拒绝——即使查询终点在异常之前（终点之后的异常同样拒绝）。
	corrupt := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 1}},
  "bills": {"c1|2026-09": {
    "id": "BILL-x", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 1, "unit_price_fen": 100, "total_fee_fen": 100,
    "lines": [{"usage_id": "u1", "time": "2026-09-15T10:00:00Z", "quantity": 1, "line_fee_fen": 100}],
    "created_at": "2026-10-01T00:00:00Z"
  }},
  "adjustments": {"adj-1": {
    "id": "adj-1", "customer_id": "c1", "month": "2026-09", "amount_fen": -50,
    "reason": "减免", "seq": 2, "created_at": "2026-10-02T00:00:00Z"
  }},
  "payments": {"pay-1": {
    "id": "pay-1", "customer_id": "c1", "total_fen": 100, "note": "转账",
    "allocations": [{"month": "2026-09", "amount_fen": 100}],
    "seq": 1, "created_at": "2026-10-02T00:00:00Z",
    "revoked": true, "revoke_reason": "撤销", "revoke_seq": 3, "revoked_at": "2026-10-03T00:00:00Z"
  }},
  "next_seq": 3
}`
	p := h.statePath()
	if err := os.WriteFile(p, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)

	// 终点在异常序号之前也拒绝：输出前核验完整流水，不输出部分报表。
	msg := h.runExpectErr("bill", "report", "c1", "2026-09", "2026-09", "0", "1")
	if !strings.Contains(msg, "数据异常") {
		t.Fatalf("错误信息异常: %s", msg)
	}
	msg = h.runExpectErr("bill", "report", "c1", "2026-09", "2026-09")
	if !strings.Contains(msg, "数据异常") {
		t.Fatalf("错误信息异常: %s", msg)
	}
	// 失败后保留原文件。
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("失败的报表改写了数据文件")
	}
}
