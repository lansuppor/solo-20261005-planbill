package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// --- 未封账用量撤回与按标识查询测试 ---

func TestWithdrawHappyPathAndQuery(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "固定客户", "10")
	h.mustRun("plan", "add", "p1", "阶梯", "-:5")
	h.mustRun("customer", "add-plan", "c2", "阶梯客户", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-10T08:00:00+08:00,3\n"+ // UTC 2026-09-10 00:00
		"u2,c2,2026-09-05T00:00:00Z,7\n")
	h.mustRun("usage", "import", f)

	// 固定单价客户的用量可撤回。
	out := h.mustRun("usage", "withdraw", "u1", "误导入，重复上报")
	for _, want := range []string{"已撤回用量", "u1", "c1", "2026-09"} {
		if !strings.Contains(out, want) {
			t.Fatalf("撤回输出缺少 %q:\n%s", want, out)
		}
	}
	// 阶梯客户的用量同样可撤回。
	h.mustRun("usage", "withdraw", "u2", "测试数据")

	// 只读查询展示原始内容、UTC 月份、撤回状态与原因。
	out = h.mustRun("usage", "show", "u1")
	for _, want := range []string{
		"用量标识：u1", "客户：c1", "时间：2026-09-10T08:00:00+08:00", "UTC 月份：2026-09",
		"数量：3", "已撤回", "误导入，重复上报",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("查询输出缺少 %q:\n%s", want, out)
		}
	}
	// 跨进程（重新载入）后撤回状态保持。
	out = h.mustRun("usage", "show", "u2")
	if !strings.Contains(out, "已撤回") || !strings.Contains(out, "测试数据") {
		t.Fatalf("跨进程撤回状态丢失:\n%s", out)
	}
}

func TestWithdrawValidationAndUsageErrors(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "客户", "10")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)

	if msg := h.runExpectErr("usage", "withdraw", "ghost", "原因"); !strings.Contains(msg, "不存在") {
		t.Fatal(msg)
	}
	h.runExpectErr("usage", "withdraw", "u1", "  ") // 空原因
	h.runExpectErr("usage", "withdraw", "  ", "原因")
	if msg := h.runExpectErr("usage", "show", "ghost"); !strings.Contains(msg, "不存在") {
		t.Fatal(msg)
	}
	// 参数数量错误是用法错误（退出码 2）。
	for _, args := range [][]string{
		{"usage", "withdraw", "u1"},
		{"usage", "withdraw", "u1", "原因", "多余"},
		{"usage", "show"},
		{"usage", "show", "u1", "多余"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}
	// 失败不留下撤回标记：记录仍有效。
	out := h.mustRun("usage", "show", "u1")
	if strings.Contains(out, "已撤回") {
		t.Fatalf("失败的撤回留下了标记:\n%s", out)
	}
}

func TestWithdrawIdempotentReplayAndReasonConflict(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "客户", "10")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 相同原因重复撤回：幂等成功，不写盘。
	out := h.mustRun("usage", "withdraw", "u1", "误导入")
	if !strings.Contains(out, "已撤回") || !strings.Contains(out, "不写盘") {
		t.Fatalf("幂等撤回输出异常:\n%s", out)
	}
	after, _ := os.ReadFile(h.statePath())
	if string(before) != string(after) {
		t.Fatal("幂等撤回应不写盘，但 state.json 发生变化")
	}
	// 换原因拒绝，状态不变。
	if msg := h.runExpectErr("usage", "withdraw", "u1", "另一个原因"); !strings.Contains(msg, "已撤回") {
		t.Fatal(msg)
	}
	after, _ = os.ReadFile(h.statePath())
	if string(before) != string(after) {
		t.Fatal("拒绝的撤回改写了存档")
	}
}

func TestWithdrawReplayAfterSealOrSuspend(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户", "sub")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-05T00:00:00Z,3\n"+
		"u2,c1,2026-09-06T00:00:00Z,4\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")

	// 该月随后封账：相同原因重放仍成功（不写盘）。
	h.mustRun("bill", "settle", "c1", "2026-09")
	out := h.mustRun("usage", "withdraw", "u1", "误导入")
	if !strings.Contains(out, "不写盘") {
		t.Fatalf("封账后幂等撤回失败:\n%s", out)
	}
	// 换原因仍拒绝。
	h.runExpectErr("usage", "withdraw", "u1", "别的原因")

	// 撤回记录所在月随后进入暂停区间：重放仍成功。
	h2 := newHarness(t)
	h2.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h2.mustRun("customer", "add-plan", "c1", "客户", "p1")
	f2 := h2.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,3\n")
	h2.mustRun("usage", "import", f2)
	h2.mustRun("usage", "withdraw", "u1", "误导入")
	h2.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")
	out = h2.mustRun("usage", "withdraw", "u1", "误导入")
	if !strings.Contains(out, "不写盘") {
		t.Fatalf("暂停后幂等撤回失败:\n%s", out)
	}
}

func TestWithdrawSealedMonthRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "客户", "10")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 已封账用量拒绝撤回，账单与账后余额不变。
	msg := h.runExpectErr("usage", "withdraw", "u1", "想撤回")
	if !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
	after, _ := os.ReadFile(h.statePath())
	if string(before) != string(after) {
		t.Fatal("拒绝的撤回改写了存档")
	}
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "总金额：30 分") {
		t.Fatalf("账单被改变:\n%s", out)
	}
}

func TestWithdrawExcludedFromSettlement(t *testing.T) {
	// 固定单价客户：撤回一条后结算只计剩余记录。
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "客户", "10")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-05T00:00:00Z,3\n"+
		"u2,c1,2026-09-06T00:00:00Z,4\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "总数量：4") || !strings.Contains(out, "总金额：40 分") {
		t.Fatalf("撤回记录仍参与结算:\n%s", out)
	}
	if strings.Contains(out, "u1") {
		t.Fatalf("账单明细包含已撤回记录:\n%s", out)
	}

	// 阶梯客户：撤回后剩余记录按原时间点及标识顺序从零累计重新分档。
	h2 := newHarness(t)
	h2.mustRun("plan", "add", "p1", "阶梯", "10:10", "-:1")
	h2.mustRun("customer", "add-plan", "c1", "客户", "p1")
	f2 := h2.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-05T00:00:00Z,8\n"+ // 撤回
		"u2,c1,2026-09-06T00:00:00Z,5\n") // 剩余 5 全部落入第一档：5×10=50
	h2.mustRun("usage", "import", f2)
	h2.mustRun("usage", "withdraw", "u1", "误导入")
	out = h2.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "总数量：5") || !strings.Contains(out, "小计=50 分") {
		t.Fatalf("阶梯结算未从零重新累计:\n%s", out)
	}
}

func TestWithdrawLastRecordNoUsageRules(t *testing.T) {
	// 零月费阶梯客户：撤回最后一条后拒绝结算且不封账。
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户", "p1")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")
	if msg := h.runExpectErr("bill", "settle", "c1", "2026-09"); !strings.Contains(msg, "没有用量") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "尚无账单") {
		t.Fatalf("零月费撤回后仍封账:\n%s", msg)
	}

	// 固定单价客户：同样拒绝且不封账。
	h2 := newHarness(t)
	h2.mustRun("customer", "add", "c1", "客户", "10")
	f2 := h2.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h2.mustRun("usage", "import", f2)
	h2.mustRun("usage", "withdraw", "u1", "误导入")
	h2.runExpectErr("bill", "settle", "c1", "2026-09")

	// 正月费方案的非暂停月：撤回最后一条后仍出仅月费账单。
	h3 := newHarness(t)
	h3.mustRun("plan", "add-fee", "sub", "订阅", "1000", "-:10")
	h3.mustRun("customer", "add-plan", "c1", "客户", "sub")
	f3 := h3.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h3.mustRun("usage", "import", f3)
	h3.mustRun("usage", "withdraw", "u1", "误导入")
	out := h3.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "总数量：0") || !strings.Contains(out, "原总金额：1000 分") {
		t.Fatalf("仅月费账单未生成:\n%s", out)
	}
}

func TestWithdrawImportReplayRules(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "客户", "10")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")

	// 相同内容重放（时间写法不同但瞬间相同）：计入重复跳过，不能恢复。
	f2 := h.writeFile("u2.csv", csvHeader+"u1,c1,2026-09-05T08:00:00+08:00,3\n")
	out := h.mustRun("usage", "import", f2)
	if !strings.Contains(out, "新增 0 条，重复跳过 1 条") {
		t.Fatalf("已撤回记录的重放未按重复跳过:\n%s", out)
	}
	if out := h.mustRun("usage", "show", "u1"); !strings.Contains(out, "已撤回") {
		t.Fatalf("重放恢复了撤回记录:\n%s", out)
	}
	// 内容不同（数量变了）：整批失败。
	f3 := h.writeFile("u3.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,4\n")
	if msg := h.runExpectErr("usage", "import", f3); !strings.Contains(msg, "内容不同") {
		t.Fatal(msg)
	}
}

func TestWithdrawImportReplayIgnoresSuspendSealAndOverflow(t *testing.T) {
	// 撤回后该月封账：相同重放仍跳过。
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "客户", "10")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-05T00:00:00Z,3\n"+
		"u2,c1,2026-09-06T00:00:00Z,4\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")
	h.mustRun("bill", "settle", "c1", "2026-09")
	out := h.mustRun("usage", "import", f)
	if !strings.Contains(out, "新增 0 条，重复跳过 2 条") {
		t.Fatalf("封账后重放未全部跳过:\n%s", out)
	}

	// 撤回记录所在月随后暂停：相同重放不受暂停限制。
	h2 := newHarness(t)
	h2.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h2.mustRun("customer", "add-plan", "c1", "客户", "p1")
	f2 := h2.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,3\n")
	h2.mustRun("usage", "import", f2)
	h2.mustRun("usage", "withdraw", "u1", "误导入")
	h2.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")
	out = h2.mustRun("usage", "import", f2)
	if !strings.Contains(out, "重复跳过 1 条") {
		t.Fatalf("暂停月重放被阻止:\n%s", out)
	}

	// 相同重放不受后续方案计价溢出影响：撤回后方案变更到大单价，
	// 新方案下单条计价溢出，但重放仍按重复跳过。
	h3 := newHarness(t)
	h3.mustRun("plan", "add", "cheap", "便宜", "-:1")
	h3.mustRun("plan", "add", "exp", "昂贵", "-:2")
	h3.mustRun("customer", "add-plan", "c1", "客户", "cheap")
	f3 := h3.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,9223372036854775807\n")
	h3.mustRun("usage", "import", f3)
	h3.mustRun("usage", "withdraw", "u1", "误导入")
	h3.mustRun("plan", "change", "c1", "2026-09", "exp", "涨价") // 预检跳过已撤回用量
	out = h3.mustRun("usage", "import", f3)
	if !strings.Contains(out, "重复跳过 1 条") {
		t.Fatalf("方案变更后重放被金额预检阻止:\n%s", out)
	}
}

func TestWithdrawIgnoredBySuspendAndPlanChangeChecks(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:1")
	h.mustRun("plan", "add", "p2", "贵阶梯", "-:2")
	h.mustRun("customer", "add-plan", "c1", "客户", "p1")
	// 数量接近上限：p2 下单条计价溢出，未撤回时方案变更预检会拒绝。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,9223372036854775807\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")

	// 暂停区间冲突检查忽略已撤回记录。
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")
	// 方案变更预检忽略已撤回记录（生效月晚于所有已封账月份即可）。
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "涨价")
}

func TestWithdrawDoesNotConsumeSeq(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "客户", "10")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-05T00:00:00Z,3\n"+
		"u2,c1,2026-09-06T00:00:00Z,4\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")
	h.mustRun("bill", "settle", "c1", "2026-09")
	// 撤回不占序号：随后的第一笔调整序号为 1。
	h.mustRun("bill", "adjust", "c1", "2026-09", "a1", "5", "补收")
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "存档全局序号上限：1") || !strings.Contains(out, "序号 1 调整 a1") {
		t.Fatalf("撤回占用了操作序号:\n%s", out)
	}
}

func TestWithdrawLoadValidation(t *testing.T) {
	newBase := func(t *testing.T) *harness {
		h := newHarness(t)
		h.mustRun("customer", "add", "c1", "客户", "10")
		f := h.writeFile("u.csv", csvHeader+
			"u1,c1,2026-09-05T00:00:00Z,3\n"+
			"u2,c1,2026-09-06T00:00:00Z,4\n")
		h.mustRun("usage", "import", f)
		return h
	}

	// 撤回目标失效：撤回信息引用了不存在的用量。
	h := newBase(t)
	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(good), `"usage": {`, `"usage_withdrawals": {"ghost": {"usage_id": "ghost", "reason": "x", "created_at": "2026-09-07T00:00:00Z"}}, "usage": {`, 1)
	if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("usage", "show", "u1"); !strings.Contains(msg, "撤回目标失效") {
		t.Fatal(msg)
	}
	got, _ := os.ReadFile(h.statePath())
	if got := string(got); got != broken {
		t.Fatal("损坏存档被改写")
	}

	// 撤回原因为空。
	h = newBase(t)
	good, _ = os.ReadFile(h.statePath())
	broken = strings.Replace(string(good), `"usage": {`, `"usage_withdrawals": {"u1": {"usage_id": "u1", "reason": " ", "created_at": "2026-09-07T00:00:00Z"}}, "usage": {`, 1)
	if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("usage", "show", "u1"); !strings.Contains(msg, "撤回原因为空") {
		t.Fatal(msg)
	}

	// 账单引用已撤回用量：先结算再手工补撤回信息。
	h = newBase(t)
	h.mustRun("bill", "settle", "c1", "2026-09")
	good, _ = os.ReadFile(h.statePath())
	broken = strings.Replace(string(good), `"usage": {`, `"usage_withdrawals": {"u1": {"usage_id": "u1", "reason": "事后撤回", "created_at": "2026-09-07T00:00:00Z"}}, "usage": {`, 1)
	if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "已撤回用量") {
		t.Fatal(msg)
	}

	// 封账月未完整包含当月有效用量：撤回 u2 后结算（账单只含 u1），
	// 再手工移除撤回信息使 u2 重新成为有效用量。
	h = newBase(t)
	h.mustRun("usage", "withdraw", "u2", "误导入")
	h.mustRun("bill", "settle", "c1", "2026-09")
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "usage_withdrawals")
	brokenBytes, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.statePath(), brokenBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "未计入账单") {
		t.Fatal(msg)
	}
}

func TestWithdrawLegacyArchiveAndSuspendedMonthBill(t *testing.T) {
	// 旧存档缺少撤回信息：全部用量视为有效，正常结算。
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "客户", "10")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "usage_withdrawals") {
		t.Fatal("无撤回时不应写出撤回信息节")
	}
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "总金额：30 分") {
		t.Fatalf("旧存档结算异常:\n%s", out)
	}

	// 撤回记录可存在于随后暂停的月份：不参与费用或冲突判断，载入正常。
	h2 := newHarness(t)
	h2.mustRun("plan", "add-fee", "sub", "订阅", "1000", "-:10")
	h2.mustRun("customer", "add-plan", "c1", "客户", "sub")
	f2 := h2.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,3\n")
	h2.mustRun("usage", "import", f2)
	h2.mustRun("usage", "withdraw", "u1", "误导入")
	h2.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")
	// 暂停月不结算；恢复月无用量但月费为正，出仅月费账单。
	h2.runExpectErr("bill", "settle", "c1", "2026-11")
	out = h2.mustRun("bill", "settle", "c1", "2027-01")
	if !strings.Contains(out, "原总金额：1000 分") {
		t.Fatalf("恢复月仅月费账单异常:\n%s", out)
	}
	// 批量结算同样只计未撤回用量。
	h3 := newHarness(t)
	h3.mustRun("customer", "add", "c1", "客户", "10")
	f3 := h3.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-05T00:00:00Z,3\n"+
		"u2,c1,2026-09-06T00:00:00Z,4\n")
	h3.mustRun("usage", "import", f3)
	h3.mustRun("usage", "withdraw", "u1", "误导入")
	out = h3.mustRun("bill", "settle-batch", "c1", "2026-09")
	if !strings.Contains(out, "原总金额 40 分") {
		t.Fatalf("批量结算计入已撤回用量:\n%s", out)
	}
}

func TestUsageShowReadOnly(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "客户", "10")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 未撤回记录的查询展示有效状态；成功或失败均不改写存档。
	out := h.mustRun("usage", "show", "u1")
	for _, want := range []string{"用量标识：u1", "UTC 月份：2026-09", "数量：3", "有效"} {
		if !strings.Contains(out, want) {
			t.Fatalf("查询输出缺少 %q:\n%s", want, out)
		}
	}
	h.runExpectErr("usage", "show", "ghost")
	after, _ := os.ReadFile(h.statePath())
	if string(before) != string(after) {
		t.Fatal("只读查询改写了存档")
	}
}
