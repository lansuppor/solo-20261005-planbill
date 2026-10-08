package main

import (
	"os"
	"strings"
	"testing"
)

// 方案变更撤销（plan revoke）测试。每个用例使用独立临时数据目录，每次调用
// run 都重新从磁盘载入，模拟跨进程持久化与重启幂等。

// setupRevokeChain 登记三个方案与一个绑定 p1 的阶梯客户。
func setupRevokeChain(t *testing.T, h *harness) {
	t.Helper()
	h.mustRun("plan", "add", "p1", "方案一", "100:10", "-:5")
	h.mustRun("plan", "add", "p2", "方案二", "50:20", "-:8")
	h.mustRun("plan", "add", "p3", "方案三", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
}

func TestPlanRevokeHappyPathRestoresInitial(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)

	out := h.mustRun("plan", "revoke", "c1", "2026-11", "价格安排误登记")
	for _, want := range []string{
		"已撤销方案变更",
		"目标方案：p2（方案二）",
		"原变更原因：续期新价",
		"撤销原因：价格安排误登记",
		"受影响区间：2026-11（含）起及之后所有月份",
		"区间内有效方案：p1（方案一）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("撤销输出缺少 %q:\n%s", want, out)
		}
	}

	// 撤销后该月有效方案回到初始方案；schedule 展示撤销状态与原因。
	sched := h.mustRun("plan", "schedule", "c1", "2026-11")
	if !strings.Contains(sched, "月份 2026-11 的有效方案：p1（方案一）") {
		t.Fatalf("撤销后有效方案未回到初始方案:\n%s", sched)
	}
	if !strings.Contains(sched, "［已撤销（撤销原因：价格安排误登记，不参与有效方案）］") {
		t.Fatalf("安排查询缺少撤销状态:\n%s", sched)
	}
	// 结算按撤销后方案 p1 计价：60×10=600（撤销前按 p2 应为 60×20=1200）。
	bill := h.mustRun("bill", "settle", "c1", "2026-11")
	if !strings.Contains(bill, "方案：p1（方案一）") || !strings.Contains(bill, "总金额：600 分") {
		t.Fatalf("撤销后结算未按初始方案计价:\n%s", bill)
	}
}

func TestPlanRevokeRestoresPreviousChangeAndBoundedByNext(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	// init p1 -> p2@2026-11 -> p3@2026-12；撤销中间项后区间 [11,12) 回落到
	// 此前最后一项未撤销变更，即初始 p1；2026-12 起仍是 p3。
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	h.mustRun("plan", "change", "c1", "2026-12", "p3", "改三")
	out := h.mustRun("plan", "revoke", "c1", "2026-11", "误")
	if !strings.Contains(out, "受影响区间：2026-11（含）至 2026-12（不含）") {
		t.Fatal(out)
	}
	// 前置不是初始方案的情形：p1@init, p3@2026-10, p2@2026-11(目标), p3@2026-12。
	h2 := newHarness(t)
	setupRevokeChain(t, h2)
	h2.mustRun("plan", "change", "c1", "2026-10", "p3", "改三")
	h2.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	h2.mustRun("plan", "change", "c1", "2026-12", "p3", "又改三")
	out = h2.mustRun("plan", "revoke", "c1", "2026-11", "误")
	if !strings.Contains(out, "受影响区间：2026-11（含）至 2026-12（不含）") ||
		!strings.Contains(out, "区间内有效方案：p3（方案三）") {
		t.Fatalf("撤销后未沿用上一项未撤销变更:\n%s", out)
	}
	for _, m := range []string{"2026-10", "2026-11", "2026-12"} {
		got := h2.mustRun("plan", "schedule", "c1", m)
		var want string
		switch m {
		case "2026-10":
			want = "月份 2026-10 的有效方案：p3（方案三）"
		case "2026-11":
			want = "月份 2026-11 的有效方案：p3（方案三）"
		case "2026-12":
			want = "月份 2026-12 的有效方案：p3（方案三）"
		}
		if !strings.Contains(got, want) {
			t.Fatalf("月份 %s 有效方案异常:\n%s", m, got)
		}
	}
}

func TestPlanRevokeConsecutive(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	h.mustRun("plan", "change", "c1", "2026-12", "p3", "改三")
	h.mustRun("plan", "revoke", "c1", "2026-11", "撤11")
	// 连续撤销第二项：此前最后一项未撤销变更已无（11 已撤销），回落初始 p1。
	out := h.mustRun("plan", "revoke", "c1", "2026-12", "撤12")
	if !strings.Contains(out, "受影响区间：2026-12（含）起及之后所有月份") ||
		!strings.Contains(out, "区间内有效方案：p1（方案一）") {
		t.Fatalf("连续撤销区间异常:\n%s", out)
	}
	for _, m := range []string{"2026-11", "2026-12", "2027-06"} {
		got := h.mustRun("plan", "schedule", "c1", m)
		if !strings.Contains(got, "月份 "+m+" 的有效方案：p1（方案一）") && m != "2027-06" {
			t.Fatalf("连续撤销后 %s 未回到初始方案:\n%s", m, got)
		}
		if m == "2027-06" && !strings.Contains(got, "月份 2027-06 的有效方案：p1（方案一）") {
			t.Fatalf("连续撤销后远期月份异常:\n%s", got)
		}
	}
	// schedule 仍按生效月列出两项原变更及各自撤销原因。
	sched := h.mustRun("plan", "schedule", "c1")
	if !strings.Contains(sched, "自 2026-11 起改用 p2") || !strings.Contains(sched, "撤销原因：撤11") ||
		!strings.Contains(sched, "自 2026-12 起改用 p3") || !strings.Contains(sched, "撤销原因：撤12") {
		t.Fatalf("连续撤销后追溯展示异常:\n%s", sched)
	}
}

func TestPlanRevokeSealedInIntervalRejects(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-10-05T00:00:00Z,60\n"+
		"u2,c1,2026-11-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-10") // 区间之前封账
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	h.mustRun("bill", "settle", "c1", "2026-11") // 区间内封账
	msg := h.runExpectErr("plan", "revoke", "c1", "2026-11", "误")
	if !strings.Contains(msg, "2026-11 已封账") || !strings.Contains(msg, "受影响区间") {
		t.Fatal(msg)
	}
	// 拒绝后撤销未登记，安排仍为 p2。
	sched := h.mustRun("plan", "schedule", "c1", "2026-11")
	if !strings.Contains(sched, "月份 2026-11 的有效方案：p2（方案二）") ||
		strings.Contains(sched, "已撤销") {
		t.Fatalf("封账拒绝后状态被改动:\n%s", sched)
	}

	// 区间外（区间结束月及其后）已封账不妨碍：p3@2026-12 把区间收窄到
	// [11,12)；2026-12 封账在区间外，但 2026-11 仍封账故仍拒绝——另构造
	// 一个区间完全避开封账月的用例。
	h2 := newHarness(t)
	setupRevokeChain(t, h2)
	f2 := h2.writeFile("u2.csv", csvHeader+
		"v0,c1,2026-09-05T00:00:00Z,60\n"+
		"v2,c1,2026-12-05T00:00:00Z,60\n")
	h2.mustRun("usage", "import", f2)
	h2.mustRun("bill", "settle", "c1", "2026-09") // 早于区间
	h2.mustRun("plan", "change", "c1", "2026-10", "p2", "改二")
	h2.mustRun("plan", "change", "c1", "2026-12", "p1", "回一")
	h2.mustRun("bill", "settle", "c1", "2026-12") // 区间结束月（不含），在区间外
	out := h2.mustRun("plan", "revoke", "c1", "2026-10", "误登记")
	if !strings.Contains(out, "受影响区间：2026-10（含）至 2026-12（不含）") {
		t.Fatal(out)
	}
}

func TestPlanRevokeOverflowPrecheck(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "spike", "天价", "-:9223372036854775807")
	h.mustRun("plan", "add", "cheap", "低价", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "spike")
	// 在 cheap 安排下导入 2026-11 用量（对 spike 会溢出），再撤销使其回落
	// spike：预检逐条从零计价溢出，整项拒绝并指出记录。
	h.mustRun("plan", "change", "c1", "2026-10", "cheap", "降价")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "change", "c1", "2026-12", "cheap", "续") // 区间收窄到 [10,12)
	msg := h.runExpectErr("plan", "revoke", "c1", "2026-10", "误")
	if !strings.Contains(msg, "u1") || !strings.Contains(msg, "溢出") ||
		!strings.Contains(msg, "整项撤销拒绝") {
		t.Fatal(msg)
	}
	// 失败不留撤销登记，2026-11 仍按 cheap。
	sched := h.mustRun("plan", "schedule", "c1", "2026-11")
	if !strings.Contains(sched, "月份 2026-11 的有效方案：cheap（低价）") ||
		strings.Contains(sched, "已撤销") {
		t.Fatalf("预检失败后状态被改动:\n%s", sched)
	}
	// 区间之外的用量不参与预检：2026-12 的记录按区间结束月排除。
	f2 := h.writeFile("u2.csv", csvHeader+"u2,c1,2026-12-05T00:00:00Z,2\n")
	h.mustRun("usage", "import", f2)
	// 先解除 2026-12 的用量影响：区间 [10,12) 本就不含 12，撤销仍因 u1 拒绝，
	// 这里仅确认报错仍只指向区间内的 u1 而非 u2。
	msg = h.runExpectErr("plan", "revoke", "c1", "2026-10", "误")
	if strings.Contains(msg, "u2") {
		t.Fatalf("区间外用量 u2 不应参与预检: %s", msg)
	}
}

func TestPlanRevokeWithdrawnUsageExcluded(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "spike", "天价", "-:9223372036854775807")
	h.mustRun("plan", "add", "cheap", "低价", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "spike")
	h.mustRun("plan", "change", "c1", "2026-11", "cheap", "降价")
	f := h.writeFile("u.csv", csvHeader+"uw,c1,2026-11-05T00:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "uw", "误导撤回")
	// 唯一记录已撤回：不参与逐条预检（否则对 spike 溢出），也无封账，撤销成功。
	out := h.mustRun("plan", "revoke", "c1", "2026-11", "撤销误登记")
	if !strings.Contains(out, "区间内有效方案：spike（天价）") {
		t.Fatal(out)
	}
	// 撤回最后一条后无有效用量、spike 月费为 0：结算仍拒绝且不封账。
	msg := h.runExpectErr("bill", "settle", "c1", "2026-11")
	if !strings.Contains(msg, "没有用量") {
		t.Fatal(msg)
	}
}

func TestPlanRevokeReplaySameReasonNoWrite(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	h.mustRun("plan", "revoke", "c1", "2026-11", "同一原因")
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("plan", "revoke", "c1", "2026-11", "同一原因")
	if !strings.Contains(out, "已撤销且撤销原因相同，返回原撤销和当前安排（不写盘）") {
		t.Fatal(out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("相同原因重放不应写盘")
	}
	// 换原因拒绝。
	if msg := h.runExpectErr("plan", "revoke", "c1", "2026-11", "另一原因"); !strings.Contains(msg, "改用其他原因") {
		t.Fatal(msg)
	}
}

func TestPlanRevokeReplayReflectsLaterChangesAndSeals(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	h.mustRun("plan", "revoke", "c1", "2026-11", "同一原因")
	// 追加变更后重放：原撤销返回，当前安排反映新区间（受 2027-01 收窄）。
	h.mustRun("plan", "change", "c1", "2027-01", "p3", "改三")
	out := h.mustRun("plan", "revoke", "c1", "2026-11", "同一原因")
	if !strings.Contains(out, "受影响区间：2026-11（含）至 2027-01（不含）") {
		t.Fatalf("重放未反映当前安排:\n%s", out)
	}
	// 撤销另一项后重放仍成功：2027-01 也被撤销后再无后续未撤销变更，区间变为
	// 开放，覆盖之后所有月份，回落初始方案 p1。
	h.mustRun("plan", "revoke", "c1", "2027-01", "撤三")
	out = h.mustRun("plan", "revoke", "c1", "2026-11", "同一原因")
	if !strings.Contains(out, "受影响区间：2026-11（含）起及之后所有月份") ||
		!strings.Contains(out, "区间内有效方案：p1（方案一）") {
		t.Fatalf("撤销其他项后重放异常:\n%s", out)
	}
	// 后来区间内封账也不阻止同原因重放（不写盘）。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-12-05T00:00:00Z,10\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-12")
	out = h.mustRun("plan", "revoke", "c1", "2026-11", "同一原因")
	if !strings.Contains(out, "返回原撤销和当前安排") {
		t.Fatalf("封账后同原因重放应成功:\n%s", out)
	}
}

func TestPlanChangeReplayAfterRevoke(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	h.mustRun("plan", "revoke", "c1", "2026-11", "撤销原因")
	// 相同 plan change 重放返回已撤销状态，不能恢复安排。
	out := h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	if !strings.Contains(out, "该变更已撤销") || !strings.Contains(out, "不恢复安排") {
		t.Fatal(out)
	}
	sched := h.mustRun("plan", "schedule", "c1", "2026-11")
	if !strings.Contains(sched, "月份 2026-11 的有效方案：p1（方案一）") {
		t.Fatalf("重放不应恢复已撤销安排:\n%s", sched)
	}
	// 不同内容仍拒绝。
	if msg := h.runExpectErr("plan", "change", "c1", "2026-11", "p3", "改二"); !strings.Contains(msg, "不可改写") {
		t.Fatal(msg)
	}
}

func TestPlanChangeMonthReuseAfterRevoke(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误")
	// 已撤销月份不能复用；新生效月仍须晚于所有已登记变更（含已撤销项）。
	if msg := h.runExpectErr("plan", "change", "c1", "2026-11", "p3", "复用"); !strings.Contains(msg, "不可改写") {
		t.Fatal(msg)
	}
	// 晚于已登记最大月份（2026-11）的新变更可以追加。
	h.mustRun("plan", "change", "c1", "2026-12", "p3", "新追加")
	// 已封账月份限制保持。
	f := h.writeFile("u.csv", csvHeader+"u9,c1,2027-02-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2027-02")
	if msg := h.runExpectErr("plan", "change", "c1", "2027-02", "p1", "太晚"); !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
}

func TestPlanRevokeImportCorrectAndBatchUseNewArrangement(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	// 撤销后新导入按撤销后方案预检：回落 spike 场景在另案验证溢出；此处验证
	// 正常回落 p1 后批量结算按 p1 计价。
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "revoke", "c1", "2026-11", "误")
	out := h.mustRun("bill", "settle-batch", "c1", "2026-11")
	if !strings.Contains(out, "新增账单 1 张") {
		t.Fatal(out)
	}
	bill := h.mustRun("bill", "show", "c1", "2026-11")
	if !strings.Contains(bill, "方案：p1（方案一）") || !strings.Contains(bill, "原总金额：600 分") {
		t.Fatalf("批量结算未按撤销后方案:\n%s", bill)
	}

	// 撤销后导入落在回落方案（天价）的月份：单条预检溢出，整批拒绝。
	h2 := newHarness(t)
	h2.mustRun("plan", "add", "spike", "天价", "-:9223372036854775807")
	h2.mustRun("plan", "add", "cheap", "低价", "-:1")
	h2.mustRun("customer", "add-plan", "c1", "客户一", "spike")
	h2.mustRun("plan", "change", "c1", "2026-11", "cheap", "降价")
	h2.mustRun("plan", "revoke", "c1", "2026-11", "误")
	f2 := h2.writeFile("u2.csv", csvHeader+"u2,c1,2026-11-05T00:00:00Z,2\n")
	if msg := h2.runExpectErr("usage", "import", f2); !strings.Contains(msg, "溢出") {
		t.Fatal(msg)
	}
}

func TestPlanRevokeMonthlyFeeRestored(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "fee", "有月费", "1000", "-:10")
	h.mustRun("plan", "add-fee", "nofee", "无月费", "0", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "fee")
	// 变更到零月费方案后，2026-11 无用量不可出账；撤销后回落有月费方案，
	// 无用量也须出仅月费账单。
	h.mustRun("plan", "change", "c1", "2026-11", "nofee", "改零费")
	h.runExpectErr("bill", "settle", "c1", "2026-11")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误")
	out := h.mustRun("bill", "settle", "c1", "2026-11")
	if !strings.Contains(out, "方案：fee（有月费）") || !strings.Contains(out, "月费：1000 分") ||
		!strings.Contains(out, "原总金额：1000 分") || !strings.Contains(out, "明细：无") {
		t.Fatalf("撤销后月费恢复出账异常:\n%s", out)
	}
}

func TestPlanRevokeDoesNotConsumeSeqOrTouchSuspension(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	// 登记暂停与撤销，二者都不占账后序号。
	h.mustRun("customer", "suspend", "c1", "2028-01", "2028-03", "装修")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "5", "补收")
	ledger := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(ledger, "存档全局序号上限：1") || !strings.Contains(ledger, "序号 1 调整 adj-1") {
		t.Fatalf("撤销占用了操作序号:\n%s", ledger)
	}
	// 暂停记录不受撤销影响。
	sus := h.mustRun("customer", "suspensions", "c1")
	if !strings.Contains(sus, "2028-01（含）至 2028-03（不含）") {
		t.Fatalf("暂停记录被撤销改动:\n%s", sus)
	}
}

func TestPlanRevokeValidation(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	h.mustRun("customer", "add", "c9", "固定客户", "10")
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")

	h.runExpectErr("plan", "revoke", "c1", "2026-13", "原因")    // 月份非法
	h.runExpectErr("plan", "revoke", "c1", "2026-11", "   ")   // 空原因
	h.runExpectErr("plan", "revoke", "ghost", "2026-11", "原因") // 客户不存在
	h.runExpectErr("plan", "revoke", "c1", "2026-09", "原因")    // 目标不存在
	if msg := h.runExpectErr("plan", "revoke", "c9", "2026-11", "原因"); !strings.Contains(msg, "固定单价") {
		t.Fatal(msg)
	}
	// 全部失败后撤销仍可登记（失败不占用）。
	h.mustRun("plan", "revoke", "c1", "2026-11", "正式撤销")
}

func TestPlanRevokeCorruptStateRejected(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误")
	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 失效撤销引用：删掉目标变更后撤销记录悬空。
	broken := strings.Replace(string(good),
		`"c1|2026-11": {
      "customer_id": "c1",
      "month": "2026-11",
      "plan_id": "p2"`,
		`"c9|2026-11": {
      "customer_id": "c1",
      "month": "2026-11",
      "plan_id": "p2"`, 1)
	if broken == string(good) {
		t.Fatal("替换未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("plan", "schedule", "c1"); !strings.Contains(msg, "损坏") {
		t.Fatalf("悬空撤销引用未报损坏: %s", msg)
	}

	// 空撤销原因。
	broken = strings.Replace(string(good), `"reason": "误"`, `"reason": "  "`, 1)
	if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("plan", "schedule", "c1"); !strings.Contains(msg, "损坏") {
		t.Fatalf("空撤销原因未报损坏: %s", msg)
	}

	// 账单方案与撤销后有效安排不符：先在 p2 下封账 2026-12，再手工补一条
	// 撤销让该月安排回到 p1。
	if err := os.WriteFile(h.statePath(), good, 0o644); err != nil {
		t.Fatal(err)
	}
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-12-05T00:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "change", "c1", "2026-12", "p2", "改二")
	h.mustRun("bill", "settle", "c1", "2026-12")
	sealed, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	inject := strings.Replace(string(sealed),
		`"plan_change_revokes": {
    "c1|2026-11": {`,
		`"plan_change_revokes": {
    "c1|2026-12": {
      "customer_id": "c1",
      "month": "2026-12",
      "reason": "手工注入",
      "created_at": "2026-01-01T00:00:00Z"
    },
    "c1|2026-11": {`, 1)
	if inject == string(sealed) {
		t.Fatal("注入撤销记录失败")
	}
	if err := os.WriteFile(h.statePath(), []byte(inject), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-12"); !strings.Contains(msg, "损坏") ||
		!strings.Contains(msg, "安排的方案") {
		t.Fatalf("账单方案与撤销后安排不符未报损坏: %s", msg)
	}
}

func TestOldStateFileWithoutPlanChangeRevokes(t *testing.T) {
	h := newHarness(t)
	old := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "老客户", "price_fen": 0, "plan_id": "p1"}},
  "plans": {"p1": {"id": "p1", "name": "阶梯一", "tiers": [{"limit": 0, "price_fen": 10}]},
           "p2": {"id": "p2", "name": "阶梯二", "tiers": [{"limit": 0, "price_fen": 1}]}},
  "usage": {},
  "bills": {},
  "plan_changes": {"c1|2026-11": {"customer_id":"c1","month":"2026-11","plan_id":"p2","reason":"r","created_at":"2026-01-01T00:00:00Z"}}
}
`
	if err := os.WriteFile(h.statePath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	// 旧存档无撤销信息：全部变更视为有效。
	out := h.mustRun("plan", "schedule", "c1", "2026-11")
	if !strings.Contains(out, "月份 2026-11 的有效方案：p2（阶梯二）") {
		t.Fatalf("旧存档应视全部变更有效:\n%s", out)
	}
	// 在旧档上登记撤销并重启保持。
	h.mustRun("plan", "revoke", "c1", "2026-11", "旧档撤销")
	out = h.mustRun("plan", "schedule", "c1", "2026-11")
	if !strings.Contains(out, "月份 2026-11 的有效方案：p1（阶梯一）") ||
		!strings.Contains(out, "撤销原因：旧档撤销") {
		t.Fatalf("旧档撤销后安排异常:\n%s", out)
	}
}

func TestPlanRevokePersistsAcrossRestartAndIdempotent(t *testing.T) {
	h := newHarness(t)
	setupRevokeChain(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "改二")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误")
	// 重新载入（新进程）后安排、撤销状态与幂等全部保持。
	out := h.mustRun("plan", "revoke", "c1", "2026-11", "误")
	if !strings.Contains(out, "不写盘") {
		t.Fatal(out)
	}
	sched := h.mustRun("plan", "schedule", "c1")
	if strings.Count(sched, "已撤销") != 1 {
		t.Fatalf("重启后撤销状态丢失或重复:\n%s", sched)
	}
}
