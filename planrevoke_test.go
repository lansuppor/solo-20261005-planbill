package main

import (
	"os"
	"strings"
	"testing"
)

// 方案变更撤销（plan revoke）测试：区间回退、封账拒绝、逐条用量预检、
// 幂等重放、连续撤销、导入/更正/结算计价、序号、存档损坏与旧存档兼容。

func setupRevokePlans(t *testing.T, h *harness) {
	t.Helper()
	// p1 旧阶梯 100:10/-:5；p2 续期 100:1/-:1；p0 初始高价（预检用）。
	h.mustRun("plan", "add", "p1", "旧阶梯", "100:10", "-:5")
	h.mustRun("plan", "add", "p2", "续期阶梯", "100:1", "-:1")
	h.mustRun("plan", "add", "p3", "第三阶梯", "-:2")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
}

func TestPlanRevokeHappyPathFallbackToInitial(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-11-05T00:00:00Z,60\n"+
		"u2,c1,2026-12-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")

	// 撤销 2026-11 变更：其后无未撤销变更，受影响区间覆盖之后所有月份，
	// 安排回退到初始方案 p1。
	out := h.mustRun("plan", "revoke", "c1", "2026-11", "价格安排登记错误")
	for _, want := range []string{
		"已撤销方案变更",
		"目标变更生效月：2026-11",
		"原变更内容（永久保留）：改用方案 p2（续期阶梯）",
		"原原因：续期新价",
		"当前状态：已撤销（撤销原因：价格安排登记错误",
		"受影响区间：2026-11（含）起覆盖之后所有月份",
		"撤销后有效方案：p1（旧阶梯）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("撤销输出缺少 %q:\n%s", want, out)
		}
	}

	// schedule 保留全部变更、原原因及撤销状态；指定月份展示实际有效方案。
	out = h.mustRun("plan", "schedule", "c1")
	if !strings.Contains(out, "自 2026-11 起改用 p2（续期阶梯）") ||
		!strings.Contains(out, "原因：续期新价") ||
		!strings.Contains(out, "当前状态：已撤销（撤销原因：价格安排登记错误") {
		t.Fatalf("schedule 未保留已撤销变更追溯:\n%s", out)
	}
	if out = h.mustRun("plan", "schedule", "c1", "2026-10"); !strings.Contains(out, "月份 2026-10 的有效方案：p1") {
		t.Fatalf("生效月前安排异常:\n%s", out)
	}
	if out = h.mustRun("plan", "schedule", "c1", "2026-11"); !strings.Contains(out, "月份 2026-11 的有效方案：p1") {
		t.Fatalf("撤销后 2026-11 未回退初始方案:\n%s", out)
	}
	if out = h.mustRun("plan", "schedule", "c1", "2027-06"); !strings.Contains(out, "月份 2027-06 的有效方案：p1") {
		t.Fatalf("撤销后远期月份未回退初始方案:\n%s", out)
	}

	// 结算按撤销后方案 p1：2026-11 的 60 单位为 600 分（第 1 档 10 分）。
	out = h.mustRun("bill", "settle", "c1", "2026-11")
	if !strings.Contains(out, "方案：p1（旧阶梯）") || !strings.Contains(out, "总金额：600 分") {
		t.Fatalf("撤销后结算未按旧方案:\n%s", out)
	}
}

func TestPlanRevokeFallsBackToPreviousActiveChange(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "第一次变更")
	h.mustRun("plan", "change", "c1", "2027-01", "p3", "第二次变更")

	// 撤销中间的 2027-01 变更：受影响区间 [2027-01, 无)，回退到上一项
	// 未撤销变更（2026-11 的 p2）。
	out := h.mustRun("plan", "revoke", "c1", "2027-01", "登记错误")
	if !strings.Contains(out, "撤销后有效方案：p2（续期阶梯）") {
		t.Fatalf("撤销后未回退到上一项未撤销变更:\n%s", out)
	}
	for _, m := range []string{"2026-11", "2026-12", "2027-02"} {
		want := "p2"
		if out = h.mustRun("plan", "schedule", "c1", m); !strings.Contains(out, "月份 "+m+" 的有效方案："+want) {
			t.Fatalf("%s 有效方案异常:\n%s", m, out)
		}
	}
	if out = h.mustRun("plan", "schedule", "c1", "2026-10"); !strings.Contains(out, "月份 2026-10 的有效方案：p1") {
		t.Fatalf("撤销后 2026-10 应仍为初始方案:\n%s", out)
	}

	// 再撤销 2026-11：当时受影响区间 [2026-11, 无)（2027-01 已撤销不算
	// 后一项），此后全部回退初始方案 p1。
	out = h.mustRun("plan", "revoke", "c1", "2026-11", "一并取消")
	if !strings.Contains(out, "覆盖之后所有月份") || !strings.Contains(out, "撤销后有效方案：p1（旧阶梯）") {
		t.Fatalf("连续第二次撤销区间异常:\n%s", out)
	}
	if out = h.mustRun("plan", "schedule", "c1", "2027-06"); !strings.Contains(out, "有效方案：p1") {
		t.Fatalf("连续撤销后远期月份未回退初始:\n%s", out)
	}
}

func TestPlanRevokeAffectedRangeStopsAtNextActiveChange(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-11-05T00:00:00Z,10\n"+
		"u2,c1,2027-01-05T00:00:00Z,10\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "第一次")
	h.mustRun("plan", "change", "c1", "2027-01", "p3", "第二次")

	// 撤销 2026-11：受影响区间为 [2026-11, 2027-01)，2027-01 起仍为 p3。
	out := h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	if !strings.Contains(out, "受影响区间：2026-11（含）至 2027-01（不含") {
		t.Fatalf("受影响区间未在下一项未撤销变更前截止:\n%s", out)
	}
	if out = h.mustRun("plan", "schedule", "c1", "2026-12"); !strings.Contains(out, "有效方案：p1") {
		t.Fatalf("区间内未回退:\n%s", out)
	}
	if out = h.mustRun("plan", "schedule", "c1", "2027-01"); !strings.Contains(out, "有效方案：p3") {
		t.Fatalf("后一项未撤销变更失效:\n%s", out)
	}
}

func TestPlanRevokeRejectsSealedBillInRange(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-11-05T00:00:00Z,10\n"+
		"u2,c1,2026-12-05T00:00:00Z,10\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	h.mustRun("bill", "settle", "c1", "2026-11")

	// 受影响区间 [2026-11, 无) 内 2026-11 已封账：拒绝撤销。
	msg := h.runExpectErr("plan", "revoke", "c1", "2026-11", "误登记")
	if !strings.Contains(msg, "封账账单") || !strings.Contains(msg, "2026-11") {
		t.Fatal(msg)
	}
	// 失败不占撤销登记：仍可看到变更有效，且可用原原因重试场景（此处安排未变）。
	if out := h.mustRun("plan", "schedule", "c1", "2026-11"); !strings.Contains(out, "有效方案：p2") {
		t.Fatalf("拒绝撤销后安排被改变:\n%s", out)
	}
}

func TestPlanRevokeSealedBillOutsideRangeAllowed(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	// 变更链：2026-11 -> p2，2027-01 -> p3。先结算 2027-01（区间外）。
	f := h.writeFile("u.csv", csvHeader+"u2,c1,2027-01-05T00:00:00Z,10\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "第一次")
	h.mustRun("plan", "change", "c1", "2027-01", "p3", "第二次")
	h.mustRun("bill", "settle", "c1", "2027-01")

	// 撤销 2026-11 的受影响区间为 [2026-11, 2027-01)；区间外的 2027-01
	// 已封账不妨碍登记。
	out := h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	if !strings.Contains(out, "受影响区间：2026-11（含）至 2027-01（不含") {
		t.Fatal(out)
	}
	// 已封账账单快照保持为 p3。
	if show := h.mustRun("bill", "show", "c1", "2027-01"); !strings.Contains(show, "方案：p3（第三阶梯）") {
		t.Fatalf("区间外已封账账单快照被改变:\n%s", show)
	}
}

func TestPlanRevokeUsageOverflowPrecheck(t *testing.T) {
	// 初始为天价 spike、变更为低价 cheap：先登记 cheap 变更使用量可按低价
	// 导入，再撤销 cheap 变更——撤销后区间回到 spike，区间内 2 单位用量
	// 单条计价溢出，整项拒绝并指出用量。
	h := newHarness(t)
	h.mustRun("plan", "add", "spike", "天价", "-:9223372036854775807")
	h.mustRun("plan", "add", "cheap", "低价", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "spike")
	h.mustRun("plan", "change", "c1", "2026-11", "cheap", "降价")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	msg := h.runExpectErr("plan", "revoke", "c1", "2026-11", "误登记")
	if !strings.Contains(msg, "u1") || !strings.Contains(msg, "溢出") || !strings.Contains(msg, "整项撤销拒绝") {
		t.Fatal(msg)
	}
	// 预检失败：变更仍有效，用量与安排不变，可原样重试。
	if out := h.mustRun("plan", "schedule", "c1", "2026-11"); !strings.Contains(out, "有效方案：cheap") {
		t.Fatalf("预检失败后安排改变:\n%s", out)
	}
	if out := h.mustRun("usage", "import", f); !strings.Contains(out, "重复跳过 1 条") {
		t.Fatalf("预检失败后用量状态改变:\n%s", out)
	}
}

func TestPlanRevokeWithdrawnUsageNotInPrecheck(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "spike", "天价", "-:9223372036854775807")
	h.mustRun("plan", "add", "cheap", "低价", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "spike")
	h.mustRun("plan", "change", "c1", "2026-11", "cheap", "降价")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	// 撤回唯一用量后再撤销：已撤回用量不参与预检，也不因天价方案溢出。
	h.mustRun("usage", "withdraw", "u1", "误导入")
	out := h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	if !strings.Contains(out, "撤销后有效方案：spike") {
		t.Fatal(out)
	}
}

func TestPlanRevokeIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")

	// 同原因重放：返回原撤销与当前安排、不写盘。
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	if !strings.Contains(out, "撤销已存在且原因相同") {
		t.Fatalf("同原因重放未幂等返回:\n%s", out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 后来封账、追加变更都不影响同原因重放；先验证“不写盘”。
	if string(before) != string(after) {
		t.Fatal("同原因重放改写了存档")
	}

	// 追加新变更后重放仍成功，返回当前安排（仍为回退后的 p1）。
	h.mustRun("plan", "change", "c1", "2027-01", "p3", "后续变更")
	out = h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	if !strings.Contains(out, "撤销已存在且原因相同") || !strings.Contains(out, "撤销后有效方案：p1") {
		t.Fatalf("追加变更后重放异常:\n%s", out)
	}
	// 撤销其他项不影响本项重放。
	h.mustRun("plan", "revoke", "c1", "2027-01", "另一项误登记")
	out = h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	if !strings.Contains(out, "撤销已存在且原因相同") {
		t.Fatalf("撤销其他项后本项重放异常:\n%s", out)
	}
	// 换原因拒绝。
	if msg := h.runExpectErr("plan", "revoke", "c1", "2026-11", "另一个原因"); !strings.Contains(msg, "改用其他原因") {
		t.Fatal(msg)
	}
}

func TestPlanRevokeOriginalChangeReplayReturnsRevoked(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")

	// 原 plan change 的相同重放须返回已撤销状态，不能恢复安排。
	out := h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	if !strings.Contains(out, "已撤销") || !strings.Contains(out, "撤销原因：误登记") {
		t.Fatalf("原变更相同重放未返回已撤销状态:\n%s", out)
	}
	if out = h.mustRun("plan", "schedule", "c1", "2026-11"); !strings.Contains(out, "有效方案：p1") {
		t.Fatalf("相同重放恢复了安排:\n%s", out)
	}
	// 不同内容仍拒绝（不可改写）。
	if msg := h.runExpectErr("plan", "change", "c1", "2026-11", "p3", "续期新价"); !strings.Contains(msg, "不可改写") {
		t.Fatal(msg)
	}
}

func TestPlanRevokeValidation(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	h.mustRun("customer", "add", "c9", "固定客户", "10")
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")

	h.runExpectErr("plan", "revoke", "c1", "2026-13", "原因") // 月份非法
	h.runExpectErr("plan", "revoke", "c1", "2026-11", "  ") // 空原因
	h.runExpectErr("plan", "revoke", "ghost", "2026-11", "原因")
	if msg := h.runExpectErr("plan", "revoke", "c9", "2026-11", "原因"); !strings.Contains(msg, "固定单价") {
		t.Fatal(msg)
	}
	// 目标必须存在：该客户该月没有变更。
	if msg := h.runExpectErr("plan", "revoke", "c1", "2026-12", "原因"); !strings.Contains(msg, "方案变更不存在") {
		t.Fatal(msg)
	}
	// 各类失败后不占撤销登记：合法撤销仍可首次成功。
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	// 已撤销后再撤销只能同原因幂等；不同原因拒绝（上面已覆盖），这里确认
	// 撤销不恢复，月份槽位仍占用。
	if msg := h.runExpectErr("plan", "change", "c1", "2026-11", "p3", "复用月份"); !strings.Contains(msg, "不可改写") {
		t.Fatal(msg)
	}
}

func TestPlanRevokedMonthSlotNotReusable(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")

	// 新变更生效月仍须晚于所有已登记变更月份（含已撤销项）：2026-11 槽位
	// 永久占用，不能复用；必须用更晚月份。
	if msg := h.runExpectErr("plan", "change", "c1", "2026-11", "p3", "重新登记"); !strings.Contains(msg, "不可改写") {
		t.Fatal(msg)
	}
	h.mustRun("plan", "change", "c1", "2026-12", "p3", "更晚的新变更")
	if out := h.mustRun("plan", "schedule", "c1", "2026-12"); !strings.Contains(out, "有效方案：p3") {
		t.Fatal(out)
	}
}

func TestPlanRevokeImportSettleUseRevokedSchedule(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "spike", "天价", "-:9223372036854775807")
	h.mustRun("plan", "add", "cheap", "低价", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "spike")
	h.mustRun("plan", "change", "c1", "2026-11", "cheap", "降价")
	// 变更有效期内按低价导入一条用量（2026-12，1 单位）。
	ok := h.writeFile("ok.csv", csvHeader+"u2,c1,2026-12-05T00:00:00Z,1\n")
	h.mustRun("usage", "import", ok)
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")

	// 撤销后导入 2026-11 用量：按撤销后方案 spike 单条计价，2 单位溢出，整批拒绝。
	bad := h.writeFile("bad.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,2\n")
	if msg := h.runExpectErr("usage", "import", bad); !strings.Contains(msg, "溢出") || !strings.Contains(msg, "整批未生效") {
		t.Fatal(msg)
	}

	// 用量更正同样按撤销后当月有效方案预检：把未封账的 u2 更正到天价月
	// 2026-11、数量 2，单条溢出拒绝（原记录 2026-12 尚未封账）。
	if msg := h.runExpectErr("usage", "correct", "u2", "u3", "c1", "2026-11-05T00:00:00Z", "2", "更正到天价月"); !strings.Contains(msg, "溢出") {
		t.Fatal(msg)
	}

	// 结算按撤销后方案 spike 计价：u2（1 单位）× 最高单价 = MaxInt64 分。
	out := h.mustRun("bill", "settle", "c1", "2026-12")
	if !strings.Contains(out, "方案：spike（天价）") || !strings.Contains(out, "9223372036854775807 分") {
		t.Fatalf("撤销后结算未按撤销后方案:\n%s", out)
	}
}

func TestPlanRevokeBatchSettleUsesRevokedSchedule(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")

	// 批量结算按撤销后方案 p1 计价：60×10=600。
	out := h.mustRun("bill", "settle-batch", "c1", "2026-11")
	if !strings.Contains(out, "新增账单 1 张") || !strings.Contains(out, "600 分") {
		t.Fatalf("批量结算未按撤销后方案:\n%s", out)
	}
	show := h.mustRun("bill", "show", "c1", "2026-11")
	if !strings.Contains(show, "方案：p1（旧阶梯）") {
		t.Fatalf("批量账单快照方案错误:\n%s", show)
	}
}

func TestPlanRevokeDoesNotConsumeSeq(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "5", "补收")
	ledger := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(ledger, "存档全局序号上限：1") || !strings.Contains(ledger, "序号 1 调整 adj-1") {
		t.Fatalf("方案变更撤销占用了操作序号:\n%s", ledger)
	}
}

func TestPlanRevokeKeepsBillsAndBalancesUnchanged(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	// 在受影响区间之外（2026-10，变更生效月之前）结算并发生账后操作，
	// 撤销 2026-11 不得改动这些账单快照与余额。
	f := h.writeFile("u.csv", csvHeader+"u0,c1,2026-10-05T00:00:00Z,10\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.mustRun("bill", "adjust", "c1", "2026-10", "adj-1", "5", "补收")
	h.mustRun("bill", "pay", "c1", "2026-10", "pay-1", "105", "转账")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	show := h.mustRun("bill", "show", "c1", "2026-10")
	for _, want := range []string{"方案：p1（旧阶梯）", "当前应付：105 分", "实收：105 分", "未收余额：0 分", "调整 adj-1", "收款 pay-1"} {
		if !strings.Contains(show, want) {
			t.Fatalf("撤销改动了区间外账单/余额（缺 %q）:\n%s", want, show)
		}
	}
}

func TestPlanRevokePersistsAcrossRestart(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	// 跨进程重新载入：撤销与幂等保持。
	out := h.mustRun("plan", "schedule", "c1", "2026-11")
	if !strings.Contains(out, "有效方案：p1") || !strings.Contains(out, "已撤销（撤销原因：误登记") {
		t.Fatalf("重启后撤销状态丢失:\n%s", out)
	}
	out = h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	if !strings.Contains(out, "撤销已存在且原因相同") {
		t.Fatalf("重启后幂等丢失:\n%s", out)
	}
}

func TestPlanRevokeCorruptStateRejected(t *testing.T) {
	h := newHarness(t)
	setupRevokePlans(t, h)
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, old, new string }{
		{
			"撤销引用目标缺失",
			`"c1|2026-11": {
      "customer_id": "c1",
      "month": "2026-11",
      "reason": "误登记"`,
			`"c1|2026-12": {
      "customer_id": "c1",
      "month": "2026-12",
      "reason": "误登记"`,
		},
		{
			"撤销原因为空",
			`"month": "2026-11",
      "reason": "误登记"`,
			`"month": "2026-11",
      "reason": "  "`,
		},
	}
	for _, tc := range cases {
		broken := strings.Replace(string(good), tc.old, tc.new, 1)
		if broken == string(good) {
			t.Fatalf("用例 %s 替换未生效", tc.name)
		}
		if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := h.runExpectErr("plan", "schedule", "c1")
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("用例 %s 未报损坏: %s", tc.name, msg)
		}
		got, _ := os.ReadFile(h.statePath())
		if string(got) != broken {
			t.Fatalf("用例 %s 报错后改写了文件", tc.name)
		}
	}

	// 账单方案与撤销后有效安排不符即损坏：先按 p2 封账 2026-11，再手工注入
	// 撤销记录使该月安排回到 p1，已封账账单快照仍为 p2，载入即损坏。
	h2 := newHarness(t)
	h2.mustRun("plan", "add", "p1", "旧阶梯", "-:10")
	h2.mustRun("plan", "add", "p2", "续期", "-:1")
	h2.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f2 := h2.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,10\n")
	h2.mustRun("usage", "import", f2)
	h2.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	h2.mustRun("bill", "settle", "c1", "2026-11")
	g2, err := os.ReadFile(h2.statePath())
	if err != nil {
		t.Fatal(err)
	}
	revocation := `  "plan_change_revocations": {"c1|2026-11": {"customer_id": "c1", "month": "2026-11", "reason": "误登记", "created_at": "2026-10-08T00:00:00Z"}},
  "next_seq"`
	inject := strings.Replace(string(g2), `"next_seq"`, revocation, 1)
	if inject == string(g2) {
		t.Fatal("注入撤销记录的锚点未命中")
	}
	if err := os.WriteFile(h2.statePath(), []byte(inject), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h2.runExpectErr("bill", "show", "c1", "2026-11")
	if !strings.Contains(msg, "损坏") {
		t.Fatalf("账单方案与撤销后安排不符未报损坏: %s", msg)
	}
}

func TestOldStateFileWithoutPlanRevocations(t *testing.T) {
	h := newHarness(t)
	// 旧格式：有变更但无撤销字段，视为全部变更有效。
	old := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "老客户", "price_fen": 0, "plan_id": "p1"}},
  "plans": {
    "p1": {"id": "p1", "name": "旧阶梯", "tiers": [{"limit": 0, "price_fen": 10}]},
    "p2": {"id": "p2", "name": "新阶梯", "tiers": [{"limit": 0, "price_fen": 1}]}
  },
  "usage": {},
  "bills": {},
  "plan_changes": {"c1|2026-11": {"customer_id": "c1", "month": "2026-11", "plan_id": "p2", "reason": "续期", "created_at": "2026-10-01T00:00:00Z"}}
}
`
	if err := os.WriteFile(h.statePath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("plan", "schedule", "c1", "2026-11")
	if !strings.Contains(out, "月份 2026-11 的有效方案：p2") {
		t.Fatalf("旧存档应视全部变更有效:\n%s", out)
	}
	// 载入旧存档后可正常撤销并落盘新字段。
	h.mustRun("plan", "revoke", "c1", "2026-11", "补登记撤销")
	if out = h.mustRun("plan", "schedule", "c1", "2026-11"); !strings.Contains(out, "有效方案：p1") {
		t.Fatalf("旧存档撤销后安排异常:\n%s", out)
	}
}

func TestPlanRevokeWithMonthlyFee(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "fee1", "月费旧", "1000", "-:10")
	h.mustRun("plan", "add-fee", "fee2", "月费新", "2000", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "fee1")
	h.mustRun("plan", "change", "c1", "2026-11", "fee2", "续期")
	h.mustRun("plan", "revoke", "c1", "2026-11", "误登记")
	// 撤销后无用量也按月费旧方案出仅月费账单 1000 分。
	out := h.mustRun("bill", "settle", "c1", "2026-11")
	if !strings.Contains(out, "方案：fee1（月费旧）") || !strings.Contains(out, "月费：1000 分") || !strings.Contains(out, "原总金额：1000 分") {
		t.Fatalf("撤销后月费账单异常:\n%s", out)
	}
}
