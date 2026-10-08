package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 结算草案（bill draft / draft-show / draft-confirm）的端到端测试。
// 每次 run 都重新从磁盘载入，天然覆盖跨进程持久化。

func draftHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.mustRun("customer", "add", "acme", "固定客户", "100")
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "100:10", "500:8", "-:5")
	h.mustRun("customer", "add-plan", "beta", "阶梯客户", "sub")
	return h
}

func draftWriteUsage(t *testing.T, h *harness, name, csvBody string) {
	t.Helper()
	p := h.writeFile(name, csvHeader+csvBody)
	h.mustRun("usage", "import", p)
}

func TestDraftCreateFixedSnapshot(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")
	out := h.mustRun("bill", "draft", "dr-1", "acme", "2026-09")
	for _, want := range []string{
		"已创建结算草案", "草案标识：dr-1", "固定单价", "单价：100 分",
		"总数量：3", "总金额：300 分", "u-1", "待确认", "关联账单：无（待确认）",
		"不生成账单、不封账",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("草案输出缺少 %q:\n%s", want, out)
		}
	}
	// 创建草案不封账、不生成账单：bill show 仍无账单，新用量仍可进入该月。
	if msg := h.runExpectErr("bill", "show", "acme", "2026-09"); !strings.Contains(msg, "尚无账单") {
		t.Fatalf("草案创建后不应有账单: %s", msg)
	}
	draftWriteUsage(t, h, "u2.csv", "u-2,acme,2026-09-16T10:00:00Z,2\n")
}

func TestDraftCreateTieredSnapshot(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv",
		"u-2,beta,2026-09-10T10:00:00Z,150\nu-3,beta,2026-09-20T10:00:00Z,400\n")
	out := h.mustRun("bill", "draft", "dr-2", "beta", "2026-09")
	for _, want := range []string{
		"阶梯计费", "方案：sub（订阅阶梯）", "月费：1000 分", "用量费：4450 分",
		"原总金额：5450 分", "总数量：550",
		"第 1 档：数量 100，单价 10 分，金额 1000 分",
		"第 2 档：数量 400，单价 8 分，金额 3200 分",
		"第 3 档：数量 50，单价 5 分，金额 250 分",
		"分段 1：第 1 档 数量=100 单价=10 分 小计=1000 分",
		"分段 2：第 2 档 数量=50 单价=8 分 小计=400 分",
		"分段 1：第 2 档 数量=350 单价=8 分 小计=2800 分",
		"分段 2：第 3 档 数量=50 单价=5 分 小计=250 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("阶梯草案输出缺少 %q:\n%s", want, out)
		}
	}
}

func TestDraftFeeOnlySnapshot(t *testing.T) {
	h := draftHarness(t)
	out := h.mustRun("bill", "draft", "dr-fee", "beta", "2026-09")
	for _, want := range []string{"总数量：0", "月费：1000 分", "用量费：0 分", "原总金额：1000 分",
		"计费输入：无", "明细：无（本账期无用量，仅收取月费）"} {
		if !strings.Contains(out, want) {
			t.Fatalf("仅月费草案输出缺少 %q:\n%s", want, out)
		}
	}
	// 确认后是仅月费账单。
	out = h.mustRun("bill", "draft-confirm", "dr-fee")
	if !strings.Contains(out, "正式出账并封账") || !strings.Contains(out, "原总金额：1000 分") {
		t.Fatalf("仅月费确认异常:\n%s", out)
	}
}

func TestDraftCreateValidation(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")

	// 用法错误（参数数量不对）以退出码 2 报告。
	for _, args := range [][]string{
		{"bill", "draft", "dr"},
		{"bill", "draft", "dr", "acme"},
		{"bill", "draft", "dr", "acme", "2026-09", "extra"},
		{"bill", "draft-show"},
		{"bill", "draft-confirm", "dr", "extra"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !asUsageError(err, &ue) {
			t.Fatalf("args=%v 应为用法错误，实际: %v", args, err)
		}
	}

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"bill", "draft", " ", "acme", "2026-09"}, "草案标识不能为空"},
		{[]string{"bill", "draft", "dr", "acme", "2026-9"}, "月份"},
		{[]string{"bill", "draft", "dr", "ghost", "2026-09"}, "不存在"},
		{[]string{"bill", "draft", "dr", "acme", "2026-11"}, "没有用量"},
		{[]string{"bill", "draft-show", "ghost"}, "不存在"},
		{[]string{"bill", "draft-confirm", "ghost"}, "不存在"},
	}
	for _, c := range cases {
		if msg := h.runExpectErr(c.args...); !strings.Contains(msg, c.want) {
			t.Fatalf("args=%v 应含 %q，实际: %s", c.args, c.want, msg)
		}
	}

	// 暂停月与终止月不可创建草案。
	h.mustRun("customer", "suspend", "beta", "2026-11", "2026-12", "装修")
	if msg := h.runExpectErr("bill", "draft", "dr-sus", "beta", "2026-11"); !strings.Contains(msg, "暂停") {
		t.Fatalf("暂停月草案应拒绝: %s", msg)
	}
	h.mustRun("customer", "terminate", "beta", "2027-01", "停止合作")
	if msg := h.runExpectErr("bill", "draft", "dr-term", "beta", "2027-01"); !strings.Contains(msg, "终止") {
		t.Fatalf("终止月草案应拒绝: %s", msg)
	}

	// 已封账月份不能创建草案。
	h.mustRun("bill", "settle", "acme", "2026-09")
	if msg := h.runExpectErr("bill", "draft", "dr-sealed", "acme", "2026-09"); !strings.Contains(msg, "已封账") {
		t.Fatalf("已封账月草案应拒绝: %s", msg)
	}
}

func asUsageError(err error, ue *usageErrorf) bool {
	if err == nil {
		return false
	}
	u, ok := err.(usageErrorf)
	if !ok {
		return false
	}
	*ue = u
	return true
}

func TestDraftReplayByIdentity(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")
	h.mustRun("bill", "draft", "dr-1", "acme", "2026-09")
	before := h.readState(t)

	// 同客户同月份重放：返回原快照，不写盘、不重新计算。
	out := h.mustRun("bill", "draft", "dr-1", "acme", "2026-09")
	if !strings.Contains(out, "已存在，返回原计费快照") {
		t.Fatalf("重放应返回原快照:\n%s", out)
	}
	if after := h.readState(t); after != before {
		t.Fatalf("同内容重放不应写盘")
	}

	// 重放期间费用变化不阻止：补入用量使当前费用不同，重放仍返回原快照。
	draftWriteUsage(t, h, "u2.csv", "u-2,acme,2026-09-16T10:00:00Z,2\n")
	out = h.mustRun("bill", "draft", "dr-1", "acme", "2026-09")
	if !strings.Contains(out, "总金额：300 分") || strings.Contains(out, "总金额：500 分") {
		t.Fatalf("重放必须返回原快照 300 分:\n%s", out)
	}

	// 原结算命令出账后，同内容重放仍成功且不写盘。
	h.mustRun("bill", "settle", "acme", "2026-09")
	before = h.readState(t)
	out = h.mustRun("bill", "draft", "dr-1", "acme", "2026-09")
	if !strings.Contains(out, "已存在") || !strings.Contains(out, "总金额：300 分") {
		t.Fatalf("封账后重放应返回原快照:\n%s", out)
	}
	if after := h.readState(t); after != before {
		t.Fatalf("封账后重放不应写盘")
	}

	// 客户或月份不同：拒绝。
	if msg := h.runExpectErr("bill", "draft", "dr-1", "acme", "2026-10"); !strings.Contains(msg, "内容不同") {
		t.Fatalf("同标识不同月份应拒绝: %s", msg)
	}
	if msg := h.runExpectErr("bill", "draft", "dr-1", "beta", "2026-09"); !strings.Contains(msg, "内容不同") {
		t.Fatalf("同标识不同客户应拒绝: %s", msg)
	}
}

func TestDraftDoesNotRestrictLaterOperations(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,beta,2026-09-10T10:00:00Z,50\n")
	h.mustRun("bill", "draft", "dr", "beta", "2026-09")
	// 后续用量（同一账期）、方案变更都不被草案限制。
	draftWriteUsage(t, h, "u2.csv", "u-2,beta,2026-09-11T10:00:00Z,50\n")
	h.mustRun("plan", "add", "pro", "续期阶梯", "100:20", "-:9")
	h.mustRun("plan", "change", "beta", "2026-10", "pro", "续期换价")
	// 其他月份可直接结算并账后操作。
	draftWriteUsage(t, h, "u3.csv", "u-3,beta,2026-10-10T10:00:00Z,10\n")
	h.mustRun("bill", "settle", "beta", "2026-10")
	h.mustRun("bill", "adjust", "beta", "2026-10", "adj1", "100", "补收")
}

func TestDraftConfirmHappyPath(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv",
		"u-2,beta,2026-09-10T10:00:00Z,150\nu-3,beta,2026-09-20T10:00:00Z,400\n")
	h.mustRun("bill", "draft", "dr-2", "beta", "2026-09")

	out := h.mustRun("bill", "draft-confirm", "dr-2")
	for _, want := range []string{"已确认", "正式出账并封账", "BILL-", "原总金额：5450 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("确认输出缺少 %q:\n%s", want, out)
		}
	}
	// bill show 可读，重复结算返回原账单。
	show := h.mustRun("bill", "show", "beta", "2026-09")
	if !strings.Contains(show, "原总金额：5450 分") {
		t.Fatalf("bill show 异常:\n%s", show)
	}
	rep := h.mustRun("bill", "settle", "beta", "2026-09")
	if !strings.Contains(rep, "已结算，返回原账单") {
		t.Fatalf("重复结算应返回原账单:\n%s", rep)
	}
	// 账后操作照常，且草案确认不占序号：首笔收款序号为 1。
	pay := h.mustRun("bill", "pay", "beta", "2026-09", "pay-1", "5000", "定金")
	if !strings.Contains(pay, "序号 1") && !strings.Contains(pay, "5000 分") {
		t.Fatalf("账后收款异常:\n%s", pay)
	}
	ledger := h.mustRun("bill", "ledger", "beta", "2026-09")
	if !strings.Contains(ledger, "序号 1 收款 pay-1") {
		t.Fatalf("收款应占序号 1（草案确认不占序号）:\n%s", ledger)
	}

	// 已确认草案重放：返回关联账单，不写盘、不重新收费、不占序号。
	before := h.readState(t)
	out = h.mustRun("bill", "draft-confirm", "dr-2")
	if !strings.Contains(out, "已确认，返回关联账单") || !strings.Contains(out, "5000") {
		t.Fatalf("确认重放应返回关联账单及当前账后状态:\n%s", out)
	}
	if after := h.readState(t); after != before {
		t.Fatalf("确认重放不应写盘")
	}
	// draft-show 展示已确认状态、关联账单与当前账后状态。
	sh := h.mustRun("bill", "draft-show", "dr-2")
	for _, want := range []string{"状态：已确认（关联账单 BILL-", "当前应付", "实收 5000 分", "未收余额 450 分"} {
		if !strings.Contains(sh, want) {
			t.Fatalf("已确认查询缺少 %q:\n%s", want, sh)
		}
	}
}

func TestDraftConfirmMismatchRejects(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")
	h.mustRun("bill", "draft", "dr", "acme", "2026-09")

	// 新增用量后确认：逐项不一致（不只总额），拒绝且不封账。
	draftWriteUsage(t, h, "u2.csv", "u-2,acme,2026-09-16T10:00:00Z,2\n")
	msg := h.runExpectErr("bill", "draft-confirm", "dr")
	for _, want := range []string{"已过时", "有效用量条数变化", "总数量变化", "原总金额变化", "逐条明细条数变化"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("过时确认应指出 %q:\n%s", want, msg)
		}
	}
	if msg := h.runExpectErr("bill", "show", "acme", "2026-09"); !strings.Contains(msg, "尚无账单") {
		t.Fatalf("确认失败不应封账: %s", msg)
	}
	// 待确认草案保留且可查询。
	if sh := h.mustRun("bill", "draft-show", "dr"); !strings.Contains(sh, "待确认") {
		t.Fatalf("失败确认应保留待确认草案:\n%s", sh)
	}

	// 原结算命令先出账后确认：拒绝且不认领已有账单。
	h.mustRun("bill", "settle", "acme", "2026-09")
	msg = h.runExpectErr("bill", "draft-confirm", "dr")
	if !strings.Contains(msg, "已由其他结算出账封账") || !strings.Contains(msg, "不认领已有账单") {
		t.Fatalf("已出账月确认应拒绝不认领: %s", msg)
	}
}

func TestDraftConfirmWithdrawAndCorrect(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")
	h.mustRun("bill", "draft", "dr", "acme", "2026-09")
	// 撤回后草案过时：无用量不可结算，拒绝但草案保留。
	h.mustRun("usage", "withdraw", "u-1", "误导入")
	if msg := h.runExpectErr("bill", "draft-confirm", "dr"); !strings.Contains(msg, "过时") {
		t.Fatalf("撤回后应过时: %s", msg)
	}
	// 再导入一条同时间同数量但新标识的记录：标识不同，逐项比较仍拒绝。
	draftWriteUsage(t, h, "ufix.csv", "u-1-fix,acme,2026-09-15T10:00:00Z,3\n")
	msg := h.runExpectErr("bill", "draft-confirm", "dr")
	if !strings.Contains(msg, "用量标识变化") || strings.Contains(msg, "数量变化") {
		t.Fatalf("仅标识变化时应只报标识差异:\n%s", msg)
	}
}

func TestDraftConfirmPlanChangeStaleness(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,beta,2026-09-01T00:00:00Z,10\n")
	h.mustRun("bill", "draft", "dr", "beta", "2026-09")
	h.mustRun("plan", "add", "pro", "续期阶梯", "100:20", "-:9")
	h.mustRun("plan", "change", "beta", "2026-09", "pro", "续期换价")
	msg := h.runExpectErr("bill", "draft-confirm", "dr")
	for _, want := range []string{"当月有效方案变化", "阶梯规则变化", "小计变化", "月费变化"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("方案变更过时应指出 %q:\n%s", want, msg)
		}
	}
	// 撤销变更后快照重新一致，确认成功。
	h.mustRun("plan", "revoke", "beta", "2026-09", "录错")
	out := h.mustRun("bill", "draft-confirm", "dr")
	if !strings.Contains(out, "方案：sub") || !strings.Contains(out, "原总金额：1100 分") {
		t.Fatalf("撤销后应按原方案确认:\n%s", out)
	}
}

func TestTwoDraftsSameMonthFirstWins(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")
	h.mustRun("bill", "draft", "dr-a", "acme", "2026-09")
	h.mustRun("bill", "draft", "dr-b", "acme", "2026-09")
	out := h.mustRun("bill", "draft-confirm", "dr-a")
	if !strings.Contains(out, "正式出账并封账") {
		t.Fatalf("dr-a 应确认成功:\n%s", out)
	}
	// 第二个草案不能认领 dr-a 的账单。
	msg := h.runExpectErr("bill", "draft-confirm", "dr-b")
	if !strings.Contains(msg, "已由其他结算出账封账") {
		t.Fatalf("dr-b 应被拒绝: %s", msg)
	}
	if sh := h.mustRun("bill", "draft-show", "dr-b"); !strings.Contains(sh, "待确认") {
		t.Fatalf("dr-b 应保持待确认:\n%s", sh)
	}
	// settle-batch 同样封账、阻止待确认草案。
	h = draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")
	h.mustRun("bill", "draft", "dr", "acme", "2026-09")
	h.mustRun("bill", "settle-batch", "acme", "2026-09")
	if msg := h.runExpectErr("bill", "draft-confirm", "dr"); !strings.Contains(msg, "不认领已有账单") {
		t.Fatalf("批量封账后确认应拒绝: %s", msg)
	}
}

func TestDraftOtherMonthsAndPostbillDoNotBlock(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv",
		"u-1,acme,2026-09-01T00:00:00Z,5\nu-2,acme,2026-08-01T00:00:00Z,7\n")
	h.mustRun("bill", "draft", "dr", "acme", "2026-09")
	// 其他月份结算、调整、收款都不阻塞草案确认。
	h.mustRun("bill", "settle", "acme", "2026-08")
	h.mustRun("bill", "adjust", "acme", "2026-08", "adj1", "100", "补收")
	h.mustRun("bill", "pay", "acme", "2026-08", "pay1", "800", "备注")
	out := h.mustRun("bill", "draft-confirm", "dr")
	if !strings.Contains(out, "总金额：500 分") {
		t.Fatalf("其他月份账后操作不应阻塞确认:\n%s", out)
	}
}

func TestDraftFailureLeavesStateUnchanged(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")
	before := h.readState(t)
	for _, args := range [][]string{
		{"bill", "draft", "dr", "ghost", "2026-09"},
		{"bill", "draft", "dr", "acme", "2026-11"},
	} {
		if err := h.runExpectErr(args...); err == "" {
			t.Fatalf("args=%v 应失败", args)
		}
		if after := h.readState(t); after != before {
			t.Fatalf("args=%v 失败后状态被修改", args)
		}
	}
	// 失败创建不占标识：同一标识随后可成功创建。
	h.mustRun("bill", "draft", "dr", "acme", "2026-09")
}

// ---- 载入校验：损坏草案拒绝并保留原文件 ----

func (h *harness) readState(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (h *harness) mutateState(t *testing.T, mutate func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.statePath(), out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func draftMap(m map[string]any, id string) map[string]any {
	return m["settlement_drafts"].(map[string]any)[id].(map[string]any)
}

func TestDraftCorruptReferenceMissing(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")
	h.mustRun("bill", "draft", "dr", "acme", "2026-09")
	// 删除草案引用的用量：草案引用缺失，按损坏拒绝。
	h.mutateState(t, func(m map[string]any) {
		delete(m["usage"].(map[string]any), "u-1")
	})
	msg := h.runExpectErr("bill", "draft-show", "dr")
	if !strings.Contains(msg, "损坏") || !strings.Contains(msg, "草案") {
		t.Fatalf("引用缺失应报损坏: %s", msg)
	}
}

func TestDraftCorruptSnapshotInconsistent(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")
	h.mustRun("bill", "draft", "dr", "acme", "2026-09")
	// 篡改快照逐条小计：快照计价不自洽。
	h.mutateState(t, func(m map[string]any) {
		d := draftMap(m, "dr")
		lines := d["snapshot"].(map[string]any)["lines"].([]any)
		lines[0].(map[string]any)["line_fee_fen"] = float64(999)
	})
	msg := h.runExpectErr("bill", "draft-show", "dr")
	if !strings.Contains(msg, "损坏") || !strings.Contains(msg, "快照计价不自洽") {
		t.Fatalf("快照不自洽应报损坏: %s", msg)
	}
}

func TestDraftCorruptConfirmedBillMismatch(t *testing.T) {
	h := draftHarness(t)
	draftWriteUsage(t, h, "u.csv", "u-1,acme,2026-09-15T10:00:00Z,3\n")
	h.mustRun("bill", "draft", "dr", "acme", "2026-09")
	h.mustRun("bill", "draft-confirm", "dr")
	// 篡改已确认草案的关联账单标识：载入时必须拒绝。
	h.mutateState(t, func(m map[string]any) {
		draftMap(m, "dr")["bill_id"] = "BILL-deadbeefdeadbeef"
	})
	msg := h.runExpectErr("bill", "draft-show", "dr")
	if !strings.Contains(msg, "损坏") {
		t.Fatalf("已确认草案关联账单不符应报损坏: %s", msg)
	}
}

func TestOldStateWithoutDraftsLoads(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	// 旧风格存档（无 settlement_drafts 节）正常读取，草案查询按不存在处理。
	msg := h.runExpectErr("bill", "draft-show", "dr")
	if !strings.Contains(msg, "不存在") {
		t.Fatalf("旧存档无草案应视为空: %s", msg)
	}
}
