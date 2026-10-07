package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// --- 未封账用量原子更正测试 ---

func TestCorrectHappyPathFixedPrice(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,2\n"+
		"u2,c1,2026-09-16T10:00:00Z,3\n")
	h.mustRun("usage", "import", f)

	out := h.mustRun("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "客户时间数量录错")
	for _, want := range []string{
		"已原子更正用量", "更正原因：客户时间数量录错",
		"原记录（已按更正原因撤回", "用量标识：u1", "数量：2",
		"替代记录", "用量标识：u1x", "时间：2026-09-15T11:00:00Z", "UTC 月份：2026-09", "数量：4",
		"当前状态：有效", "直接前身：u1", "直接后继：u1x",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("更正输出缺少 %q:\n%s", want, out)
		}
	}

	// usage show 两端都展示关联与原因。
	old := h.mustRun("usage", "show", "u1")
	for _, want := range []string{"已撤回（用量更正撤回", "直接后继：u1x", "客户时间数量录错", "直接前身：无"} {
		if !strings.Contains(old, want) {
			t.Fatalf("原记录展示缺少 %q:\n%s", want, old)
		}
	}
	nu := h.mustRun("usage", "show", "u1x")
	for _, want := range []string{"当前状态：有效", "直接前身：u1（经用量更正被本记录替代，更正原因：客户时间数量录错）", "直接后继：无"} {
		if !strings.Contains(nu, want) {
			t.Fatalf("替代记录展示缺少 %q:\n%s", want, nu)
		}
	}

	// 结算只计有效记录：u2(3) + u1x(4) = 7，金额 700；u1 不进账单。
	shown := h.mustRun("bill", "settle", "c1", "2026-09")
	for _, want := range []string{"总数量：7", "总金额：700 分", "用量标识=u1x", "用量标识=u2"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("结算应按有效记录重新累计，缺少 %q:\n%s", want, shown)
		}
	}
	if strings.Contains(shown, "用量标识=u1 ") && !strings.Contains(shown, "u1x") {
		t.Fatalf("原记录不得进入账单:\n%s", shown)
	}
}

func TestCorrectCrossCustomerAndMonth(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	h.mustRun("customer", "add", "c2", "乙方", "200")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)

	// 更正到另一客户、另一 UTC 月份。
	out := h.mustRun("usage", "correct", "u1", "u1x", "c2", "2026-10-01T00:00:00Z", "5", "客户录错")
	if !strings.Contains(out, "替代记录") || !strings.Contains(out, "UTC 月份：2026-10") {
		t.Fatal(out)
	}
	// 原客户 9 月已无有效用量：拒绝结算且不封账。
	h.runExpectErr("bill", "settle", "c1", "2026-09")
	// 新客户 10 月按替代记录结算：5 × 200 = 1000。
	oct := h.mustRun("bill", "settle", "c2", "2026-10")
	if !strings.Contains(oct, "总金额：1000 分") || !strings.Contains(oct, "用量标识=u1x") {
		t.Fatal(oct)
	}
	// 已有账单不变（这里 c1 9 月根本无账单）；原内容仍可只读查询。
	if shown := h.mustRun("usage", "show", "u1"); !strings.Contains(shown, "客户：c1") {
		t.Fatal(shown)
	}
}

func TestCorrectTieredRepricing(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "std", "标准阶梯", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c1", "阶梯客户", "std")
	f := h.writeFile("u.csv", csvHeader+
		"t1,c1,2026-09-15T10:00:00Z,60\n"+
		"t2,c1,2026-09-16T10:00:00Z,60\n")
	h.mustRun("usage", "import", f)

	// 把 t1 数量从 60 改成 20：剩余有效记录按时间点排序为 t1x(20, 09-15)、
	// t2(60, 09-16)，从零重新累计共 80，全部落入第一档（80×10=800 分）。
	h.mustRun("usage", "correct", "t1", "t1x", "c1", "2026-09-15T10:00:00Z", "20", "数量录错")
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	for _, want := range []string{
		"总数量：80", "用量费：800 分",
		"用量标识=t1x", "用量标识=t2",
		"第 1 档：数量 80，单价 10 分，金额 800 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("阶梯应按有效记录重新累计，缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "用量标识=t1\n") || strings.Contains(out, "用量标识=t1 ") {
		t.Fatalf("原记录 t1 不得进入账单:\n%s", out)
	}
}

func TestCorrectTieredSingleLineOverflowRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "std", "标准阶梯", "100:10", "-:9223372036854775807")
	h.mustRun("customer", "add-plan", "c1", "阶梯客户", "std")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", f)

	// 替代数量在无上限档单价下溢出：拒绝更正，业务状态不变。
	msg := h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "2026-09-15T10:00:00Z", "9223372036854775807", "数量录错")
	if !strings.Contains(msg, "溢出") || !strings.Contains(msg, "拒绝更正") {
		t.Fatal(msg)
	}
	// 失败不占新标识、不留撤回标记或关联。
	h.runExpectErr("usage", "show", "u1x")
	if shown := h.mustRun("usage", "show", "u1"); !strings.Contains(shown, "当前状态：有效") {
		t.Fatal(shown)
	}
	// 可原样重试为合法数量。
	h.mustRun("usage", "correct", "u1", "u1x", "c1", "2026-09-15T10:00:00Z", "2", "数量录错")

	// 固定单价溢出同理。
	h.mustRun("customer", "add", "rich", "巨款客户", "9223372036854775800")
	g := h.writeFile("g.csv", csvHeader+"g1,rich,2026-09-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", g)
	msg = h.runExpectErr("usage", "correct", "g1", "g1x", "rich", "2026-09-15T10:00:00Z", "3", "数量录错")
	if !strings.Contains(msg, "溢出") {
		t.Fatal(msg)
	}
}

func TestUsageCorrectValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)

	// 参数格式问题。
	h.runExpectErr("usage", "correct", "", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "原因")
	h.runExpectErr("usage", "correct", "u1", "", "c1", "2026-09-15T11:00:00Z", "4", "原因")
	h.runExpectErr("usage", "correct", "u1", "u1x", "", "2026-09-15T11:00:00Z", "4", "原因")
	h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "  ")
	h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "not-a-time", "4", "原因")
	h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "0", "原因")
	h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "-2", "原因")
	h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "x", "原因")
	// 参数数量错误是用法错误（退出码 2）。
	for _, args := range [][]string{
		{"usage", "correct"},
		{"usage", "correct", "u1"},
		{"usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4"},
		{"usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "原因", "多余"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}

	// 原记录不存在。
	h.runExpectErr("usage", "correct", "ghost", "gx", "c1", "2026-09-15T11:00:00Z", "4", "原因")
	// 新客户不存在。
	h.runExpectErr("usage", "correct", "u1", "u1x", "ghost", "2026-09-15T11:00:00Z", "4", "原因")
	// 新旧标识相同。
	h.runExpectErr("usage", "correct", "u1", "u1", "c1", "2026-09-15T11:00:00Z", "4", "原因")

	// 新标识已使用：即使内容完全相同也不能充当替代记录。
	h.runExpectErr("usage", "correct", "u1", "u1", "c1", "2026-09-15T10:00:00Z", "2", "原因")
	// 先导入 u2，再尝试用 u2 作为新标识（哪怕替代内容与 u2 相同）：拒绝。
	f2 := h.writeFile("u2.csv", csvHeader+"u2,c1,2026-09-20T10:00:00Z,1\n")
	h.mustRun("usage", "import", f2)
	h.runExpectErr("usage", "correct", "u1", "u2", "c1", "2026-09-20T10:00:00Z", "1", "原因")

	// 全部失败后 u1 仍有效、u1x 未占用：首次合法更正照常成功。
	out := h.mustRun("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "首次更正")
	if !strings.Contains(out, "已原子更正用量") {
		t.Fatal(out)
	}
}

func TestCorrectSealedAndSuspendedRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "阶梯客户", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,2\n"+
		"u3,c1,2026-10-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")
	// 登记 2026-11 暂停区间：起月晚于已封账的 2026-09，区间内无有效用量。
	h.mustRun("customer", "suspend", "c1", "2026-11", "2026-12", "装修暂停")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 原月已封账：拒绝更正，账单与余额不变。
	msg := h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "原因")
	if !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
	// 目标月已封账：拒绝。
	msg = h.runExpectErr("usage", "correct", "u3", "u3x", "c1", "2026-09-20T10:00:00Z", "4", "原因")
	if !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
	// 目标月暂停：拒绝。
	msg = h.runExpectErr("usage", "correct", "u3", "u3x", "c1", "2026-11-15T10:00:00Z", "4", "原因")
	if !strings.Contains(msg, "暂停") {
		t.Fatal(msg)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("封账/暂停拒绝更正不应改变存档")
	}
	// 原记录仍有效，新标识未占用。
	if shown := h.mustRun("usage", "show", "u3"); !strings.Contains(shown, "当前状态：有效") {
		t.Fatal(shown)
	}
	h.runExpectErr("usage", "show", "u3x")
}

func TestCorrectRejectedWithWithdrawnOld(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")

	// 已被普通撤回的记录不能更正。
	msg := h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "原因")
	if !strings.Contains(msg, "已撤回") {
		t.Fatal(msg)
	}
}

func TestUsageCorrectIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "旧方案", "-:1")
	h.mustRun("plan", "add", "p2", "新方案", "-:2")
	h.mustRun("customer", "add-plan", "c1", "阶梯客户", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,2\n"+
		"u2,c1,2026-09-16T10:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "录错了")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 相同五项重放：返回原更正且不写盘（Z 与 +00:00 视为同一瞬间）。
	out := h.mustRun("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00+00:00", "4", "录错了")
	if !strings.Contains(out, "内容相同，返回原更正及两条记录当前状态（不写盘）") {
		t.Fatalf("相同重放应幂等返回:\n%s", out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("相同重放改写了存档")
	}

	// 任一项不同拒绝：新标识、客户、时间点、数量、原因。
	h.mustRun("customer", "add-plan", "c2", "客户二", "p1")
	h.runExpectErr("usage", "correct", "u1", "other", "c1", "2026-09-15T11:00:00Z", "4", "录错了")
	h.runExpectErr("usage", "correct", "u1", "u1x", "c2", "2026-09-15T11:00:00Z", "4", "录错了")
	h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "2026-09-15T12:00:00Z", "4", "录错了")
	h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "5", "录错了")
	h.runExpectErr("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "别的原因")

	// 后来封账、方案变化、替代记录被撤回，都不阻止相同重放。先在封账前撤回
	// 替代记录（撤回仍受未封账限制），由 u2 支撑该月封账。
	h.mustRun("usage", "withdraw", "u1x", "替代记录也录错")
	h.mustRun("bill", "settle", "c1", "2026-09")
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	out = h.mustRun("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "录错了")
	if !strings.Contains(out, "不写盘") {
		t.Fatalf("封账/方案变化/替代撤回后相同重放仍应成功:\n%s", out)
	}
	// 重放展示替代记录的当前状态（已撤回）。
	if !strings.Contains(out, "替代记录") || !strings.Contains(out, "当前状态：已撤回（撤回原因：替代记录也录错") {
		t.Fatalf("重放应展示替代记录当前状态:\n%s", out)
	}
	// 任何记录都没有被恢复。
	if shown := h.mustRun("usage", "show", "u1"); !strings.Contains(shown, "已撤回") {
		t.Fatal(shown)
	}
	if shown := h.mustRun("usage", "show", "u1x"); !strings.Contains(shown, "已撤回") {
		t.Fatal(shown)
	}
}

func TestCorrectChainAndReplacementWithdraw(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)

	// 更正链 u1 -> u2 -> u3：替代记录可继续更正。
	h.mustRun("usage", "correct", "u1", "u2", "c1", "2026-09-15T11:00:00Z", "4", "第一次更正")
	h.mustRun("usage", "correct", "u2", "u3", "c1", "2026-09-15T12:00:00Z", "6", "第二次更正")

	mid := h.mustRun("usage", "show", "u2")
	for _, want := range []string{
		"已撤回（用量更正撤回，更正原因：第二次更正",
		"直接前身：u1（经用量更正被本记录替代，更正原因：第一次更正）",
		"直接后继：u3（经用量更正替代本记录，更正原因：第二次更正）",
	} {
		if !strings.Contains(mid, want) {
			t.Fatalf("链条中间记录展示缺少 %q:\n%s", want, mid)
		}
	}
	last := h.mustRun("usage", "show", "u3")
	if !strings.Contains(last, "直接前身：u2") || !strings.Contains(last, "直接后继：无") || !strings.Contains(last, "当前状态：有效") {
		t.Fatal(last)
	}

	// 每条原用量只能更正一次：u2 的不同重放被拒绝。
	h.runExpectErr("usage", "correct", "u2", "u9", "c1", "2026-09-15T12:00:00Z", "6", "第二次更正")
	// 已作为前身/替代的标识都不可复用。
	h.runExpectErr("usage", "correct", "u3", "u1", "c1", "2026-09-15T12:00:00Z", "6", "第三次更正")
	h.runExpectErr("usage", "correct", "u3", "u2", "c1", "2026-09-15T12:00:00Z", "6", "第三次更正")

	// 结算只计当前有效记录 u3。
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "总数量：6") || !strings.Contains(out, "用量标识=u3") ||
		strings.Contains(out, "用量标识=u2 ") {
		t.Fatalf("链式更正后只应计最新有效记录:\n%s", out)
	}

	// 最新替代记录也可按原规则撤回（此时是该月唯一有效记录）。
	h2 := newHarness(t)
	h2.mustRun("customer", "add", "a", "甲方", "100")
	g := h2.writeFile("g.csv", csvHeader+"g1,a,2026-10-15T10:00:00Z,2\n")
	h2.mustRun("usage", "import", g)
	h2.mustRun("usage", "correct", "g1", "g2", "a", "2026-10-15T11:00:00Z", "4", "更正")
	h2.mustRun("usage", "withdraw", "g2", "替代记录误导入")
	shown := h2.mustRun("usage", "show", "g2")
	if !strings.Contains(shown, "已撤回（撤回原因：替代记录误导入") || !strings.Contains(shown, "直接前身：g1") {
		t.Fatal(shown)
	}
	// 两条记录均撤回，该月无有效用量：拒绝结算且不封账。
	h2.runExpectErr("bill", "settle", "a", "2026-10")
}

func TestCorrectImportReplayBothEnds(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	old := h.writeFile("old.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", old)
	h.mustRun("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "录错")

	// 原内容重放：按已撤回的原记录判重跳过，不恢复。
	out := h.mustRun("usage", "import", old)
	if !strings.Contains(out, "新增 0 条，重复跳过 1 条") {
		t.Fatal(out)
	}
	// 替代记录内容重放：同样按已有记录判重跳过。
	repl := h.writeFile("repl.csv", csvHeader+"u1x,c1,2026-09-15T11:00:00Z,4\n")
	out = h.mustRun("usage", "import", repl)
	if !strings.Contains(out, "新增 0 条，重复跳过 1 条") {
		t.Fatal(out)
	}
	// 状态不变。
	if shown := h.mustRun("usage", "show", "u1"); !strings.Contains(shown, "已撤回") {
		t.Fatal(shown)
	}
	if shown := h.mustRun("usage", "show", "u1x"); !strings.Contains(shown, "当前状态：有效") {
		t.Fatal(shown)
	}
}

func TestCorrectDoesNotConsumeSeq(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "2")                                   // u-c1 在 2026-09 已封账
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "50", "补收") // 序号 1
	f := h.writeFile("u.csv", csvHeader+"ux,c1,2026-10-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "correct", "ux", "uxx", "c1", "2026-10-15T11:00:00Z", "3", "录错") // 不占序号
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "30", "补收")                   // 序号 2

	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\"next_seq\": 2") {
		t.Fatalf("更正不应占用账后操作序号:\n%s", data)
	}
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if strings.Contains(out, "更正") || !strings.Contains(out, "序号 2 调整 adj-2") {
		t.Fatalf("账后流水不应出现用量更正:\n%s", out)
	}
}

func TestCorrectFailureLeavesNoMarksAndRetriable(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 新客户不存在：失败不留任何痕迹。
	h.runExpectErr("usage", "correct", "u1", "u1x", "ghost", "2026-09-15T11:00:00Z", "4", "原因")
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("失败的更正不应产生任何写盘变化之外的内存痕迹（存档应一致）")
	}
	// 新标识未占用：马上可以成功使用。
	h.mustRun("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "原因")
}

func TestUsageCorrectPersistsAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "录错")

	// 全新 harness 指向同一目录：关联、状态与幂等保持。
	h2 := &harness{t: t, dir: h.dir}
	prev := stdout
	stdout = &h2.buf
	defer func() { stdout = prev }()

	out, err := h2.run("usage", "show", "u1")
	if err != nil || !strings.Contains(out, "已撤回") || !strings.Contains(out, "直接后继：u1x") {
		t.Fatalf("跨进程原记录关联丢失: %v\n%s", err, out)
	}
	out, err = h2.run("usage", "show", "u1x")
	if err != nil || !strings.Contains(out, "直接前身：u1") {
		t.Fatalf("跨进程替代记录关联丢失: %v\n%s", err, out)
	}
	// 相同重放仍幂等不写盘。
	out, err = h2.run("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "录错")
	if err != nil || !strings.Contains(out, "不写盘") {
		t.Fatalf("跨进程相同重放应幂等: %v\n%s", err, out)
	}
	// 不同内容仍拒绝。
	if _, err = h2.run("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "5", "录错"); err == nil {
		t.Fatal("跨进程后内容不同的重放应拒绝")
	}
}

func TestCorrectLoadValidation(t *testing.T) {
	// 合法基线：原记录 u1 因更正撤回，替代记录 u2 有效且已入封账账单。
	valid := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {
    "u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2},
    "u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-15T11:00:00Z", "quantity": 4}
  },
  "bills": {"c1|2026-09": {
    "id": "BILL-x", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 4, "unit_price_fen": 100, "total_fee_fen": 400,
    "lines": [{"usage_id": "u2", "time": "2026-09-15T11:00:00Z", "quantity": 4, "line_fee_fen": 400}],
    "created_at": "2026-10-01T00:00:00Z"
  }},
  "withdrawals": {"u1": {"usage_id": "u1", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}},
  "usage_corrections": {"u1": {"old_usage_id": "u1", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}
}`
	h := newHarness(t)
	if err := os.WriteFile(h.statePath(), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := h.mustRun("usage", "show", "u2"); !strings.Contains(out, "直接前身：u1") {
		t.Fatal(out)
	}

	cases := map[string]string{
		"关联引用缺失-原记录":  `"usage_corrections": {"u9": {"old_usage_id": "u9", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`,
		"关联引用缺失-替代记录": `"usage_corrections": {"u1": {"old_usage_id": "u1", "new_usage_id": "u9", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`,
		"撤回原因不符":      `"usage_corrections": {"u1": {"old_usage_id": "u1", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "别的原因", "created_at": "2026-09-20T00:00:00Z"}}`,
		"更正键不一致":      `"usage_corrections": {"u1": {"old_usage_id": "u2", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`,
		"更正原因为空":      `"usage_corrections": {"u1": {"old_usage_id": "u1", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": " ", "created_at": "2026-09-20T00:00:00Z"}}`,
		"登记时间与替代记录不符": `"usage_corrections": {"u1": {"old_usage_id": "u1", "new_usage_id": "u2", "new_time": "2026-09-15T12:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`,
	}
	for name, replacement := range cases {
		hc := newHarness(t)
		broken := strings.Replace(valid,
			`"usage_corrections": {"u1": {"old_usage_id": "u1", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`,
			replacement, 1)
		if broken == valid {
			t.Fatalf("%s：替换未生效", name)
		}
		if err := os.WriteFile(hc.statePath(), []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := hc.runExpectErr("usage", "show", "u1")
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("%s：应按损坏拒绝: %s", name, msg)
		}
		got, _ := os.ReadFile(hc.statePath())
		if string(got) != broken {
			t.Fatalf("%s：损坏文件被改写", name)
		}
	}

	// 原记录未撤回：移除 u1 的撤回标记（同时账单仍引用 u2，互不影响）。
	notWithdrawn := strings.Replace(valid,
		`  "withdrawals": {"u1": {"usage_id": "u1", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}},
`, "", 1)
	if notWithdrawn == valid {
		t.Fatal("原记录未撤回：替换未生效")
	}
	hn := newHarness(t)
	if err := os.WriteFile(hn.statePath(), []byte(notWithdrawn), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := hn.runExpectErr("usage", "show", "u1")
	if !strings.Contains(msg, "损坏") || !strings.Contains(msg, "未撤回") {
		t.Fatalf("原记录未撤回应按损坏拒绝: %s", msg)
	}

	// 同一替代记录有多个来源：u1 与 u3 都指向 u2（u3 也需已撤回且有记录）。
	multi := strings.Replace(valid,
		`"u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-15T11:00:00Z", "quantity": 4}`,
		`"u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-15T11:00:00Z", "quantity": 4},
    "u3": {"id": "u3", "customer_id": "c1", "time": "2026-09-10T10:00:00Z", "quantity": 1}`, 1)
	multi = strings.Replace(multi,
		`"withdrawals": {"u1": {"usage_id": "u1", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`,
		`"withdrawals": {"u1": {"usage_id": "u1", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}, "u3": {"usage_id": "u3", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`, 1)
	multi = strings.Replace(multi,
		`"usage_corrections": {"u1": {"old_usage_id": "u1", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`,
		`"usage_corrections": {
      "u1": {"old_usage_id": "u1", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"},
      "u3": {"old_usage_id": "u3", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}
    }`, 1)
	hm := newHarness(t)
	if err := os.WriteFile(hm.statePath(), []byte(multi), 0o644); err != nil {
		t.Fatal(err)
	}
	msgM := hm.runExpectErr("usage", "show", "u1")
	if !strings.Contains(msgM, "损坏") || !strings.Contains(msgM, "多个来源") {
		t.Fatalf("同一替代记录多个来源应按损坏拒绝: %s", msgM)
	}

	// 关联成环：u1 -> u2 且 u2 -> u1（u2 也需撤回）。
	cyclic := strings.Replace(valid,
		`"withdrawals": {"u1": {"usage_id": "u1", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`,
		`"withdrawals": {"u1": {"usage_id": "u1", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}, "u2": {"usage_id": "u2", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`, 1)
	// 成环时两条记录都不在账单中，删除封账账单以免触发其他损坏判定。
	cyclic = strings.Replace(cyclic,
		`"bills": {"c1|2026-09": {
    "id": "BILL-x", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 4, "unit_price_fen": 100, "total_fee_fen": 400,
    "lines": [{"usage_id": "u2", "time": "2026-09-15T11:00:00Z", "quantity": 4, "line_fee_fen": 400}],
    "created_at": "2026-10-01T00:00:00Z"
  }},`, `"bills": {},`, 1)
	cyclic = strings.Replace(cyclic,
		`"usage_corrections": {"u1": {"old_usage_id": "u1", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}`,
		`"usage_corrections": {
      "u1": {"old_usage_id": "u1", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"},
      "u2": {"old_usage_id": "u2", "new_usage_id": "u1", "new_time": "2026-09-15T10:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}
    }`, 1)
	hc2 := newHarness(t)
	if err := os.WriteFile(hc2.statePath(), []byte(cyclic), 0o644); err != nil {
		t.Fatal(err)
	}
	msg = hc2.runExpectErr("usage", "show", "u1")
	if !strings.Contains(msg, "损坏") || !strings.Contains(msg, "成环") {
		t.Fatalf("关联成环应按损坏拒绝: %s", msg)
	}

	// 替代记录后来被合法撤回：不是损坏（在合法基线上追加 u2 的撤回标记；
	// u2 已在账单中，账单引用已撤回记录会损坏，故改用无账单的合法存档）。
	withdrawn := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {
    "u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2},
    "u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-15T11:00:00Z", "quantity": 4}
  },
  "bills": {},
  "withdrawals": {
    "u1": {"usage_id": "u1", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"},
    "u2": {"usage_id": "u2", "reason": "替代也录错", "created_at": "2026-09-21T00:00:00Z"}
  },
  "usage_corrections": {"u1": {"old_usage_id": "u1", "new_usage_id": "u2", "new_time": "2026-09-15T11:00:00Z", "reason": "录错", "created_at": "2026-09-20T00:00:00Z"}}
}`
	hw := newHarness(t)
	if err := os.WriteFile(hw.statePath(), []byte(withdrawn), 0o644); err != nil {
		t.Fatal(err)
	}
	out := hw.mustRun("usage", "show", "u2")
	for _, want := range []string{"已撤回（撤回原因：替代也录错", "直接前身：u1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("替代记录合法撤回不应被判损坏，缺少 %q:\n%s", want, out)
		}
	}
}

func TestCorrectOldStateWithoutCorrections(t *testing.T) {
	// 旧存档缺少 usage_corrections：视为无更正，无关联时明确说明。
	h := newHarness(t)
	legacy := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2}},
  "bills": {}
}`
	if err := os.WriteFile(h.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("usage", "show", "u1")
	if !strings.Contains(out, "直接前身：无") || !strings.Contains(out, "直接后继：无") {
		t.Fatalf("旧存档缺少更正信息应视为无更正:\n%s", out)
	}
	// 旧存档上可直接更正。
	h.mustRun("usage", "correct", "u1", "u1x", "c1", "2026-09-15T11:00:00Z", "4", "录错")
}
