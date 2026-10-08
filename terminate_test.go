package main

import (
	"os"
	"strings"
	"testing"
)

// --- 按月订阅终止测试 ---

func TestTerminateRegisterHappyPathAndQuery(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")

	out := h.mustRun("customer", "terminate", "c1", "2027-03", "停止合作")
	for _, want := range []string{
		"已登记按月订阅终止", "客户：c1", "终止月：2027-03", "原因：停止合作",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("登记终止输出缺少 %q:\n%s", want, out)
		}
	}

	// 查询：展示保存的终止月与原因。
	out = h.mustRun("customer", "termination", "c1")
	for _, want := range []string{"客户：c1（客户一）", "已登记", "终止月：2027-03", "原因：停止合作"} {
		if !strings.Contains(out, want) {
			t.Fatalf("终止查询输出缺少 %q:\n%s", want, out)
		}
	}

	// 指定月份：终止月（含）起限制生效，之前月份未生效。
	out = h.mustRun("customer", "termination", "c1", "2027-02")
	if !strings.Contains(out, "终止限制未生效") {
		t.Fatalf("终止前月份应说明限制未生效:\n%s", out)
	}
	for _, m := range []string{"2027-03", "2027-04", "2028-01"} {
		out = h.mustRun("customer", "termination", "c1", m)
		if !strings.Contains(out, "终止限制生效") {
			t.Fatalf("月份 %s 应说明终止限制生效:\n%s", m, out)
		}
	}
}

func TestTerminateQueryNotRegistered(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "add", "c9", "固定客户", "10")

	// 未登记时明确说明。
	out := h.mustRun("customer", "termination", "c1")
	if !strings.Contains(out, "未登记") {
		t.Fatalf("未登记终止应明确说明:\n%s", out)
	}
	out = h.mustRun("customer", "termination", "c1", "2027-01")
	if !strings.Contains(out, "未登记") || !strings.Contains(out, "终止限制未生效") {
		t.Fatalf("未登记时指定月份应说明限制未生效:\n%s", out)
	}

	// 固定单价客户说明不适用终止。
	out = h.mustRun("customer", "termination", "c9")
	if !strings.Contains(out, "不适用按月订阅终止") {
		t.Fatalf("固定单价客户应说明不适用终止:\n%s", out)
	}
	out = h.mustRun("customer", "termination", "c9", "2027-01")
	if !strings.Contains(out, "不适用终止") {
		t.Fatalf("固定单价客户指定月份应说明不适用:\n%s", out)
	}

	// 客户不存在与非法月份失败，且查询只读不落盘。
	h.runExpectErr("customer", "termination", "ghost")
	h.runExpectErr("customer", "termination", "c1", "2027-13")
	if _, err := os.Stat(h.statePath()); err != nil {
		t.Fatalf("只读查询不应创建数据文件: %v", err)
	}
}

func TestTerminateValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "add", "c9", "固定客户", "10")

	h.runExpectErr("customer", "terminate", "c1", "2027-13", "原因")    // 终止月非法
	h.runExpectErr("customer", "terminate", "c1", "2027-03", "")      // 空原因
	h.runExpectErr("customer", "terminate", "c1", "2027-03", "   ")   // 纯空白原因
	h.runExpectErr("customer", "terminate", "ghost", "2027-03", "原因") // 客户不存在
	h.runExpectErr("customer", "terminate", "c9", "2027-03", "原因")    // 固定单价客户不适用

	// 全部失败不占用终止登记，可正常登记。
	h.mustRun("customer", "terminate", "c1", "2027-03", "停止合作")
}

func TestTerminateConflictsWithSealedAndUsage(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.writeFile("u.csv", csvHeader+
		"u-001,c1,2026-09-15T10:00:00Z,3\n"+
		"u-002,c1,2026-10-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", h.dir+"/u.csv")
	h.mustRun("bill", "settle", "c1", "2026-09")

	// 终止月须晚于全部已封账月份：冲突指出月份。
	err := h.runExpectErr("customer", "terminate", "c1", "2026-09", "停止合作")
	if !strings.Contains(err, "2026-09") || !strings.Contains(err, "已封账") {
		t.Fatalf("封账冲突应指出月份:\n%s", err)
	}
	// 终止月及之后不得有有效用量：冲突指出用量标识。
	err = h.runExpectErr("customer", "terminate", "c1", "2026-10", "停止合作")
	if !strings.Contains(err, "u-002") {
		t.Fatalf("用量冲突应指出用量标识:\n%s", err)
	}

	// 已撤回记录不阻塞：撤回 u-002 后可登记。
	h.mustRun("usage", "withdraw", "u-002", "误导入")
	h.mustRun("customer", "terminate", "c1", "2026-10", "停止合作")
}

func TestTerminateReplayAndImmutability(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.writeFile("u.csv", csvHeader+"u-001,c1,2026-09-15T10:00:00Z,3\n")
	h.mustRun("usage", "import", h.dir+"/u.csv")
	h.mustRun("customer", "terminate", "c1", "2026-11", "停止合作")

	// 同月同原因重放返回原记录、不写盘。
	before, _ := os.ReadFile(h.statePath())
	out := h.mustRun("customer", "terminate", "c1", "2026-11", "停止合作")
	if !strings.Contains(out, "已存在且内容相同") || !strings.Contains(out, "不写盘") {
		t.Fatalf("相同重放应返回原记录且不写盘:\n%s", out)
	}
	after, _ := os.ReadFile(h.statePath())
	if string(before) != string(after) {
		t.Fatal("相同重放不应改写存档")
	}

	// 不同月份或原因拒绝，每客户只能登记一次。
	h.runExpectErr("customer", "terminate", "c1", "2026-12", "停止合作")
	h.runExpectErr("customer", "terminate", "c1", "2026-11", "其他原因")

	// 后来补结算较早月份、发生账后操作后重放仍成功。
	h.mustRun("bill", "settle", "c1", "2026-09")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "50", "补收")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "100", "转账")
	out = h.mustRun("customer", "terminate", "c1", "2026-11", "停止合作")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatalf("补结算与账后操作后相同重放应成功:\n%s", out)
	}
}

func TestTerminateBlocksUsageImportSettleAndCorrect(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "add", "c9", "固定客户", "10")
	h.writeFile("u.csv", csvHeader+
		"u-001,c1,2026-09-15T10:00:00Z,3\n"+
		"u-002,c9,2026-09-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", h.dir+"/u.csv")
	h.mustRun("customer", "terminate", "c1", "2026-11", "停止合作")

	// 导入混入终止月及之后的新用量：整批拒绝并指出行号，合法项也不留下。
	h.writeFile("u2.csv", csvHeader+
		"u-101,c1,2026-11-15T10:00:00Z,3\n"+
		"u-102,c9,2026-10-15T10:00:00Z,1\n")
	err := h.runExpectErr("usage", "import", h.dir+"/u2.csv")
	if !strings.Contains(err, "第 2 行") || !strings.Contains(err, "u-101") {
		t.Fatalf("终止月新用量应整批拒绝并指出行号:\n%s", err)
	}
	out := h.mustRun("usage", "show", "u-001")
	_ = out
	h.runExpectErr("usage", "show", "u-102") // 合法项未留下

	// 已有用量的相同导入继续成功（重复跳过），不恢复记录。
	out = h.mustRun("usage", "import", h.dir+"/u.csv")
	if !strings.Contains(out, "重复跳过 2 条") {
		t.Fatalf("已有用量相同导入应判重跳过:\n%s", out)
	}

	// 终止月（含）起单笔结算拒绝，不封账、不生成零金额账单（方案月费 > 0 也一样）。
	h.runExpectErr("bill", "settle", "c1", "2026-11")
	h.runExpectErr("bill", "settle", "c1", "2027-01")

	// 批量清单包含终止月及之后月份时整批拒绝，不新增账单或封账。
	err = h.runExpectErr("bill", "settle-batch", "c1", "2026-09", "c1", "2026-12")
	if !strings.Contains(err, "整批未生效") || !strings.Contains(err, "2026-12") {
		t.Fatalf("批量结算含终止月应整批拒绝:\n%s", err)
	}
	h.runExpectErr("bill", "show", "c1", "2026-09") // 整批未生效，未封账

	// 终止前月份仍按原规则补结算。
	h.mustRun("bill", "settle", "c1", "2026-09")

	// 更正替代记录的目标客户与目标 UTC 月份受终止限制：更正到终止月及之后拒绝。
	h.writeFile("u3.csv", csvHeader+"u-201,c9,2026-10-15T10:00:00Z,3\n")
	h.mustRun("usage", "import", h.dir+"/u3.csv")
	err = h.runExpectErr("usage", "correct", "u-201", "u-201-fix", "c1", "2026-11-15T10:00:00Z", "3", "改到终止月")
	if !strings.Contains(err, "终止") {
		t.Fatalf("更正到终止月应拒绝:\n%s", err)
	}
	// 更正被拒绝时原记录、替代标识及关联均不变。
	out = h.mustRun("usage", "show", "u-201")
	if !strings.Contains(out, "当前状态：有效") {
		t.Fatalf("更正被拒绝后原记录应保持有效:\n%s", out)
	}
	h.runExpectErr("usage", "show", "u-201-fix")

	// 终止前未封账用量可按原规则更正到其他客户（c1 的 2026-09 已封账，用 c9 的记录更正到 c9 自身其他月份验证正常路径）。
	h.mustRun("usage", "correct", "u-201", "u-201-fix", "c9", "2026-12-15T10:00:00Z", "3", "正常更正")
	// 已有更正的相同重放继续成功，不恢复记录。
	out = h.mustRun("usage", "correct", "u-201", "u-201-fix", "c9", "2026-12-15T10:00:00Z", "3", "正常更正")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatalf("已有更正的相同重放应成功:\n%s", out)
	}
}

func TestTerminatePostbillOpsUnaffected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.writeFile("u.csv", csvHeader+"u-001,c1,2026-09-15T10:00:00Z,3\n")
	h.mustRun("usage", "import", h.dir+"/u.csv")
	h.mustRun("bill", "settle", "c1", "2026-09")
	h.mustRun("customer", "terminate", "c1", "2026-11", "停止合作")

	// 历史账单的调整、收款、更正、退款、撤销及对账继续可用。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "50", "补收")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "200", "转账")
	h.mustRun("bill", "correct", "pay-1", "corr-1", "入账月份不变", "2026-09:200")
	h.mustRun("bill", "refund", "pay-1", "rf-1", "多收退回", "2026-09:50")
	h.mustRun("bill", "revoke", "adj-1", "录入错误")
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "截止时余额") {
		t.Fatalf("终止后账后流水应可用:\n%s", out)
	}
	out = h.mustRun("bill", "reconcile", "c1", "2026-09", "2026-09")
	if !strings.Contains(out, "汇总") {
		t.Fatalf("终止后跨账期对账应可用:\n%s", out)
	}
	out = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "实收：150 分") {
		t.Fatalf("终止不改变历史账单余额（200 收款 - 50 退款）:\n%s", out)
	}
}

func TestTerminateDuringSuspensionAndSchedulesKept(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯一", "-:10")
	h.mustRun("plan", "add", "p2", "阶梯二", "-:8")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("plan", "change", "c1", "2026-12", "p2", "续期新价")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-02", "店面装修")

	// 可在暂停期间终止。
	h.mustRun("customer", "terminate", "c1", "2027-01", "提前终止")

	// 方案变更、暂停及提前恢复记录保留，相关查询照常。
	out := h.mustRun("plan", "schedule", "c1")
	if !strings.Contains(out, "2026-12") || !strings.Contains(out, "p2") {
		t.Fatalf("终止后方案安排应保留:\n%s", out)
	}
	out = h.mustRun("customer", "suspensions", "c1")
	if !strings.Contains(out, "2026-11") || !strings.Contains(out, "2027-02") {
		t.Fatalf("终止后暂停记录应保留:\n%s", out)
	}

	// 提前恢复仍按原规则登记，但不能越过终止边界恢复服务。
	h.mustRun("customer", "resume", "c1", "2026-11", "2026-12", "提前复工")
	h.writeFile("u.csv", csvHeader+"u-001,c1,2027-01-15T10:00:00Z,3\n")
	err := h.runExpectErr("usage", "import", h.dir+"/u.csv")
	if !strings.Contains(err, "终止") {
		t.Fatalf("恢复不能越过终止边界：终止月起仍不接收用量:\n%s", err)
	}
	h.runExpectErr("bill", "settle", "c1", "2027-01")
}

func TestTerminatePersistenceAndLoadValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "terminate", "c1", "2027-03", "停止合作")

	// 重启（重新载入）保持状态和判重。
	out := h.mustRun("customer", "terminate", "c1", "2027-03", "停止合作")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatalf("重启后相同重放应返回原记录:\n%s", out)
	}
	out = h.mustRun("customer", "termination", "c1", "2027-03")
	if !strings.Contains(out, "终止限制生效") {
		t.Fatalf("重启后查询应保持:\n%s", out)
	}

	// 损坏存档：终止月及之后存在有效用量 → 载入拒绝并保留原文件。
	h2 := newHarness(t)
	h2.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h2.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h2.writeFile("u.csv", csvHeader+"u-1,c1,2027-04-15T10:00:00Z,3\n")
	h2.mustRun("usage", "import", h2.dir+"/u.csv")
	injectTermination(t, h2.statePath(), `"terminations":{"c1":{"customer_id":"c1","month":"2027-03","reason":"x","created_at":"2026-10-01T00:00:00Z"}}`)
	before, _ := os.ReadFile(h2.statePath())
	err := h2.runExpectErr("customer", "termination", "c1")
	if !strings.Contains(err, "已损坏") || !strings.Contains(err, "u-1") {
		t.Fatalf("终止月后存在有效用量的存档应按损坏拒绝:\n%s", err)
	}
	after, _ := os.ReadFile(h2.statePath())
	if string(before) != string(after) {
		t.Fatal("损坏存档应保留原文件")
	}

	// 损坏存档：终止月及之后存在账单 → 载入拒绝。
	h3 := newHarness(t)
	h3.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:5")
	h3.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h3.mustRun("bill", "settle", "c1", "2027-04") // 仅月费账单
	injectTermination(t, h3.statePath(), `"terminations":{"c1":{"customer_id":"c1","month":"2027-03","reason":"x","created_at":"2026-10-01T00:00:00Z"}}`)
	err = h3.runExpectErr("customer", "termination", "c1")
	if !strings.Contains(err, "已损坏") || !strings.Contains(err, "账单") {
		t.Fatalf("终止月后存在账单的存档应按损坏拒绝:\n%s", err)
	}

	// 损坏存档：非阶梯客户、非法终止月、空原因、失效客户引用 → 载入拒绝。
	cases := []string{
		`"terminations":{"ghost":{"customer_id":"ghost","month":"2027-03","reason":"x","created_at":"2026-10-01T00:00:00Z"}}`,
		`"terminations":{"c1":{"customer_id":"c1","month":"2027-13","reason":"x","created_at":"2026-10-01T00:00:00Z"}}`,
		`"terminations":{"c1":{"customer_id":"c1","month":"2027-03","reason":" ","created_at":"2026-10-01T00:00:00Z"}}`,
	}
	for _, frag := range cases {
		hc := newHarness(t)
		hc.mustRun("plan", "add", "p1", "阶梯", "-:10")
		hc.mustRun("customer", "add-plan", "c1", "客户一", "p1")
		injectTermination(t, hc.statePath(), frag)
		if err := hc.runExpectErr("customer", "termination", "c1"); !strings.Contains(err, "已损坏") {
			t.Fatalf("非法终止存档 %s 应按损坏拒绝:\n%s", frag, err)
		}
	}
	// 固定单价客户的终止记录 → 载入拒绝。
	hf := newHarness(t)
	hf.mustRun("customer", "add", "c9", "固定客户", "10")
	injectTermination(t, hf.statePath(), `"terminations":{"c9":{"customer_id":"c9","month":"2027-03","reason":"x","created_at":"2026-10-01T00:00:00Z"}}`)
	if err := hf.runExpectErr("customer", "termination", "c9"); !strings.Contains(err, "已损坏") {
		t.Fatalf("固定单价客户的终止存档应按损坏拒绝:\n%s", err)
	}

	// 旧存档缺少终止信息视为未终止：直接读取无 terminations 字段的存档。
	h4 := newHarness(t)
	h4.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h4.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	out = h4.mustRun("customer", "termination", "c1")
	if !strings.Contains(out, "未登记") {
		t.Fatalf("旧存档缺少终止信息应视为未终止:\n%s", out)
	}
}

// injectTermination 把终止登记片段注入存档 JSON（替换掉末尾的 "}"），
// 用于构造绕过正常登记的损坏存档。
func injectTermination(t *testing.T, path, fragment string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(data))
	if !strings.HasSuffix(text, "}") {
		t.Fatalf("存档格式异常: %s", path)
	}
	text = text[:len(text)-1] + "," + fragment + "}\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
