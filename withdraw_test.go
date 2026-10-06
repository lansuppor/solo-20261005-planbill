package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// --- 未封账用量撤回测试 ---

func TestWithdrawHappyPathAndShow(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,2\n"+
		"u2,c1,2026-09-16T10:00:00Z,3\n")
	h.mustRun("usage", "import", f)

	out := h.mustRun("usage", "withdraw", "u1", "误导入记录")
	for _, want := range []string{
		"已撤回用量", "用量标识：u1", "客户：c1（甲方）", "时间：2026-09-15T10:00:00Z",
		"UTC 月份：2026-09", "数量：2", "当前状态：已撤回", "撤回原因：误导入记录",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("撤回输出缺少 %q:\n%s", want, out)
		}
	}

	// 只读查询展示原始内容、UTC 月份、撤回状态与原因。
	out = h.mustRun("usage", "show", "u1")
	for _, want := range []string{"用量标识：u1", "数量：2", "UTC 月份：2026-09", "已撤回", "误导入记录"} {
		if !strings.Contains(out, want) {
			t.Fatalf("查询输出缺少 %q:\n%s", want, out)
		}
	}
	// 撤回只作用于该条：u2 仍有效。
	out = h.mustRun("usage", "show", "u2")
	if !strings.Contains(out, "当前状态：有效") || strings.Contains(out, "已撤回") {
		t.Fatalf("u2 不应受 u1 撤回影响:\n%s", out)
	}
}

func TestWithdrawValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)

	h.runExpectErr("usage", "withdraw", "u1", "  ")    // 空原因
	h.runExpectErr("usage", "withdraw", "", "原因")      // 空标识
	h.runExpectErr("usage", "withdraw", "ghost", "原因") // 目标不存在
	h.runExpectErr("usage", "show", "ghost")           // 查询不存在的用量
	h.runExpectErr("usage", "show", "")

	// 参数数量错误是用法错误（退出码 2）。
	for _, args := range [][]string{
		{"usage"},
		{"usage", "withdraw", "u1"},
		{"usage", "withdraw", "u1", "原因", "多余"},
		{"usage", "show"},
		{"usage", "show", "u1", "多余"},
		{"usage", "bogus"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}

	// 全部失败后未留下撤回标记，可原样撤回成功。
	h.mustRun("usage", "withdraw", "u1", "纠正误导入")
}

func TestWithdrawIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,2\n"+
		"u2,c1,2026-09-16T10:00:00Z,3\n"+
		"u3,c1,2026-10-15T10:00:00Z,4\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 相同原因重复撤回返回原记录且不写盘。
	out := h.mustRun("usage", "withdraw", "u1", "误导入")
	if !strings.Contains(out, "已撤回且撤回原因相同，返回原记录（不写盘）") {
		t.Fatalf("重复撤回未幂等返回:\n%s", out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("相同原因重复撤回改写了存档")
	}
	// 换原因拒绝，撤回不可恢复。
	if msg := h.runExpectErr("usage", "withdraw", "u1", "其他原因"); !strings.Contains(msg, "已撤回") {
		t.Fatal(msg)
	}

	// 封账该月后（u2 入账），相同原因重放仍成功。
	h.mustRun("bill", "settle", "c1", "2026-09")
	out = h.mustRun("usage", "withdraw", "u1", "误导入")
	if !strings.Contains(out, "返回原记录") {
		t.Fatalf("封账后相同原因重放应成功:\n%s", out)
	}

	// 暂停该月后，相同原因重放仍成功。
	h.mustRun("usage", "withdraw", "u3", "误导入")
	h.mustRun("customer", "suspend", "c1", "2026-10", "2026-11", "暂停营业")
	out = h.mustRun("usage", "withdraw", "u3", "误导入")
	if !strings.Contains(out, "返回原记录") {
		t.Fatalf("暂停后相同原因重放应成功:\n%s", out)
	}
}

func TestWithdrawSealedMonthRejected(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "2") // u-c1 已入 2026-09 账单并封账

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	msg := h.runExpectErr("usage", "withdraw", "u-c1", "想纠正")
	if !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("封账拒绝撤回应不改变任何状态")
	}
	// 账单与账后余额不变，记录仍有效。
	if out := h.mustRun("bill", "show", "c1", "2026-09"); !strings.Contains(out, "总金额：200 分") {
		t.Fatal(out)
	}
	if out := h.mustRun("usage", "show", "u-c1"); !strings.Contains(out, "当前状态：有效") {
		t.Fatal(out)
	}
}

func TestWithdrawExcludedFromSettlement(t *testing.T) {
	h := newHarness(t)
	// 固定单价客户：撤回一条后结算只计剩余记录。
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,2\n"+
		"u2,c1,2026-09-16T10:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "总数量：3") || !strings.Contains(out, "总金额：300 分") ||
		!strings.Contains(out, "用量标识=u2") || strings.Contains(out, "用量标识=u1") {
		t.Fatalf("固定单价结算应只计未撤回用量:\n%s", out)
	}

	// 阶梯客户：剩余记录按原时间点及标识顺序从零累计重新分档。
	h.mustRun("plan", "add", "std", "标准阶梯", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c2", "阶梯客户", "std")
	g := h.writeFile("t.csv", csvHeader+
		"t1,c2,2026-09-15T10:00:00Z,60\n"+
		"t2,c2,2026-09-16T10:00:00Z,60\n")
	h.mustRun("usage", "import", g)
	h.mustRun("usage", "withdraw", "t1", "误导入")
	out = h.mustRun("bill", "settle", "c2", "2026-09")
	// t2 单独从零累计：60 全部落入第一档（60×10=600），第二档为 0。
	for _, want := range []string{
		"总数量：60", "用量费：600 分", "用量标识=t2",
		"第 1 档：数量 60，单价 10 分，金额 600 分", "第 2 档：数量 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("阶梯结算应重新分档，缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "用量标识=t1") {
		t.Fatalf("已撤回记录不得进入账单:\n%s", out)
	}

	// 批量结算同样只计未撤回用量。
	h.mustRun("customer", "add", "c3", "丙方", "100")
	b := h.writeFile("b.csv", csvHeader+
		"b1,c3,2026-09-15T10:00:00Z,2\n"+
		"b2,c3,2026-09-16T10:00:00Z,5\n")
	h.mustRun("usage", "import", b)
	h.mustRun("usage", "withdraw", "b1", "误导入")
	out = h.mustRun("bill", "settle-batch", "c3", "2026-09")
	if !strings.Contains(out, "原总金额 500 分") {
		t.Fatalf("批量结算应只计未撤回用量:\n%s", out)
	}
	if shown := h.mustRun("bill", "show", "c3", "2026-09"); !strings.Contains(shown, "总数量：5") {
		t.Fatal(shown)
	}
}

func TestWithdrawLastRecordNoUsageRules(t *testing.T) {
	h := newHarness(t)
	// 零月费阶梯方案：撤回最后一条后按无用量处理，拒绝结算且不封账。
	h.mustRun("plan", "add", "p0", "零月费阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p0")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")
	if msg := h.runExpectErr("bill", "settle", "c1", "2026-09"); !strings.Contains(msg, "没有用量") {
		t.Fatal(msg)
	}
	// 未封账：该月仍可导入全新用量。
	more := h.writeFile("more.csv", csvHeader+"u2,c1,2026-09-20T10:00:00Z,1\n")
	h.mustRun("usage", "import", more)

	// 正月费方案的非暂停月：撤回最后一条后仍可出仅月费账单。
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c2", "客户二", "sub")
	g := h.writeFile("s.csv", csvHeader+"s1,c2,2026-09-15T10:00:00Z,5\n")
	h.mustRun("usage", "import", g)
	h.mustRun("usage", "withdraw", "s1", "误导入")
	out := h.mustRun("bill", "settle", "c2", "2026-09")
	for _, want := range []string{"总数量：0", "月费：1000 分", "原总金额：1000 分", "仅收取月费"} {
		if !strings.Contains(out, want) {
			t.Fatalf("仅月费账单缺少 %q:\n%s", want, out)
		}
	}

	// 固定单价客户：撤回唯一用量后拒绝结算且不封账。
	h.mustRun("customer", "add", "c3", "客户三", "100")
	x := h.writeFile("x.csv", csvHeader+"x1,c3,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", x)
	h.mustRun("usage", "withdraw", "x1", "误导入")
	h.runExpectErr("bill", "settle", "c3", "2026-09")
}

func TestWithdrawImportReplay(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,2\n"+
		"u2,c1,2026-09-16T10:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")

	// 已撤回记录的相同重放计入重复跳过，不能恢复。
	out := h.mustRun("usage", "import", f)
	if !strings.Contains(out, "新增 0 条，重复跳过 2 条") {
		t.Fatal(out)
	}
	if shown := h.mustRun("usage", "show", "u1"); !strings.Contains(shown, "已撤回") {
		t.Fatalf("重放不得恢复已撤回记录:\n%s", shown)
	}

	// 封账后相同重放仍计入重复跳过。
	h.mustRun("bill", "settle", "c1", "2026-09")
	out = h.mustRun("usage", "import", f)
	if !strings.Contains(out, "重复跳过 2 条") {
		t.Fatal(out)
	}
	if shown := h.mustRun("bill", "show", "c1", "2026-09"); !strings.Contains(shown, "总数量：3") {
		t.Fatalf("重放不得改变已封账账单:\n%s", shown)
	}

	// 标识不可复用：相同标识内容不同使整批失败，合法记录也不留下。
	bad := h.writeFile("bad.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,99\n"+
		"u9,c1,2026-10-15T10:00:00Z,1\n")
	msg := h.runExpectErr("usage", "import", bad)
	if !strings.Contains(msg, "内容不同") || !strings.Contains(msg, "整批未生效") {
		t.Fatal(msg)
	}
	h.runExpectErr("usage", "show", "u9") // 整批失败，u9 未导入
}

func TestWithdrawReplayUnblockedBySuspendAndPlanOverflow(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "旧方案", "-:1")
	h.mustRun("plan", "add", "p2", "新方案", "-:2")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,2\n"+
		"big1,c1,2026-11-15T10:00:00Z,9223372036854775807\n"+
		"big2,c1,2026-11-16T10:00:00Z,9223372036854775807\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")
	h.mustRun("usage", "withdraw", "big1", "误导入")

	// 方案变更预检只约束未撤回用量：big2 仍有效，按新方案单条计价溢出，拒绝。
	if msg := h.runExpectErr("plan", "change", "c1", "2026-11", "p2", "续期"); !strings.Contains(msg, "big2") {
		t.Fatal(msg)
	}
	// 撤回 big2 后预检不再受阻，变更成功。
	h.mustRun("usage", "withdraw", "big2", "误导入")
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期")
	// 已撤回记录的相同重放不受后续方案计价溢出影响，计入重复跳过。
	out := h.mustRun("usage", "import", f)
	if !strings.Contains(out, "新增 0 条，重复跳过 3 条") {
		t.Fatal(out)
	}

	// 暂停区间冲突检查只约束未撤回用量：u1 已撤回，9 月可登记暂停。
	h.mustRun("customer", "suspend", "c1", "2026-09", "2026-10", "暂停营业")
	// 暂停月内已撤回记录的相同重放仍计入重复跳过，不被暂停拒绝。
	out = h.mustRun("usage", "import", f)
	if !strings.Contains(out, "重复跳过 3 条") {
		t.Fatal(out)
	}
	// 但暂停月不接收全新用量。
	intruder := h.writeFile("new.csv", csvHeader+"u9,c1,2026-09-20T10:00:00Z,1\n")
	if msg := h.runExpectErr("usage", "import", intruder); !strings.Contains(msg, "暂停") {
		t.Fatal(msg)
	}
	// 未撤回用量仍阻止登记暂停区间。
	h.mustRun("customer", "add-plan", "c2", "客户二", "p1")
	g := h.writeFile("g.csv", csvHeader+"g1,c2,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", g)
	if msg := h.runExpectErr("customer", "suspend", "c2", "2026-09", "2026-10", "暂停"); !strings.Contains(msg, "g1") {
		t.Fatal(msg)
	}
}

func TestWithdrawDoesNotConsumeSeq(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "2")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "50", "补收") // 序号 1
	f := h.writeFile("u.csv", csvHeader+"ux,c1,2026-10-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "ux", "误导入")                       // 不占用账后操作序号
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "30", "补收") // 序号 2

	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\"next_seq\": 2") {
		t.Fatalf("撤回不应占用操作序号:\n%s", data)
	}
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "序号 1 调整 adj-1") || !strings.Contains(out, "序号 2 调整 adj-2") ||
		strings.Contains(out, "撤回") {
		t.Fatalf("流水不应出现撤回事件且序号连续:\n%s", out)
	}
}

func TestWithdrawPersistsAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "withdraw", "u1", "误导入")

	// 全新的 harness 指向同一目录，验证跨进程保持撤回状态与重放规则。
	h2 := &harness{t: t, dir: h.dir}
	prev := stdout
	stdout = &h2.buf
	defer func() { stdout = prev }()
	out, err := h2.run("usage", "show", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "已撤回") || !strings.Contains(out, "误导入") {
		t.Fatal(out)
	}
	out, err = h2.run("usage", "import", f)
	if err != nil || !strings.Contains(out, "重复跳过 1 条") {
		t.Fatalf("跨进程重放应计入重复跳过: %v\n%s", err, out)
	}
}

func TestWithdrawLoadValidation(t *testing.T) {
	// 合法存档：已封账月份中已撤回用量不在账单内（有效用量完整入账）。
	valid := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {
    "u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2},
    "u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-16T10:00:00Z", "quantity": 3}
  },
  "bills": {"c1|2026-09": {
    "id": "BILL-x", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 3, "unit_price_fen": 100, "total_fee_fen": 300,
    "lines": [{"usage_id": "u2", "time": "2026-09-16T10:00:00Z", "quantity": 3, "line_fee_fen": 300}],
    "created_at": "2026-10-01T00:00:00Z"
  }},
  "withdrawals": {"u1": {"usage_id": "u1", "reason": "误导入", "created_at": "2026-09-20T00:00:00Z"}}
}`
	h := newHarness(t)
	if err := os.WriteFile(h.statePath(), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := h.mustRun("usage", "show", "u1"); !strings.Contains(out, "已撤回") {
		t.Fatal(out)
	}
	if out := h.mustRun("usage", "show", "u2"); !strings.Contains(out, "当前状态：有效") {
		t.Fatal(out)
	}

	cases := map[string]string{
		"撤回目标失效":  `"withdrawals": {"u9": {"usage_id": "u9", "reason": "误导入", "created_at": "2026-09-20T00:00:00Z"}}`,
		"撤回键不一致":  `"withdrawals": {"u1": {"usage_id": "u2", "reason": "误导入", "created_at": "2026-09-20T00:00:00Z"}}`,
		"撤回原因为空":  `"withdrawals": {"u1": {"usage_id": "u1", "reason": " ", "created_at": "2026-09-20T00:00:00Z"}}`,
		"撤回记录为空":  `"withdrawals": {"u1": null}`,
		"账单引用已撤回": `"withdrawals": {"u2": {"usage_id": "u2", "reason": "误导入", "created_at": "2026-09-20T00:00:00Z"}}`,
	}
	for name, w := range cases {
		h := newHarness(t)
		broken := strings.Replace(valid, `"withdrawals": {"u1": {"usage_id": "u1", "reason": "误导入", "created_at": "2026-09-20T00:00:00Z"}}`, w, 1)
		if broken == valid {
			t.Fatalf("%s：替换未生效", name)
		}
		if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := h.runExpectErr("usage", "show", "u1")
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("%s：应按损坏拒绝: %s", name, msg)
		}
		got, _ := os.ReadFile(h.statePath())
		if string(got) != broken {
			t.Fatalf("%s：损坏文件被改写", name)
		}
	}

	// 封账月存在未入账的有效用量：仍按损坏拒绝（未完整包含当月有效用量）。
	h2 := newHarness(t)
	orphan := strings.Replace(valid,
		`"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2}`,
		`"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2},
    "u3": {"id": "u3", "customer_id": "c1", "time": "2026-09-17T10:00:00Z", "quantity": 1}`, 1)
	if err := os.WriteFile(h2.statePath(), []byte(orphan), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h2.runExpectErr("usage", "show", "u1"); !strings.Contains(msg, "损坏") {
		t.Fatalf("封账月游离有效用量应按损坏拒绝: %s", msg)
	}
}

func TestWithdrawOldStateAndSuspendedMonth(t *testing.T) {
	// 旧存档缺少撤回信息：视为全部用量有效。
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
	if out := h.mustRun("usage", "show", "u1"); !strings.Contains(out, "当前状态：有效") {
		t.Fatalf("旧存档缺少撤回信息应视为全部有效:\n%s", out)
	}
	// 旧存档上可直接撤回。
	h.mustRun("usage", "withdraw", "u1", "误导入")

	// 已撤回记录可存在于随后暂停的月份：载入不视为用量冲突。
	h2 := newHarness(t)
	suspended := `{
  "version": 1,
  "plans": {"p1": {"id": "p1", "name": "阶梯", "tiers": [{"limit": 0, "price_fen": 10}]}},
  "customers": {"c1": {"id": "c1", "name": "客户一", "price_fen": 0, "plan_id": "p1"}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-11-15T10:00:00Z", "quantity": 2}},
  "bills": {},
  "suspensions": {"c1|2026-11": {"customer_id": "c1", "start_month": "2026-11", "end_month": "2026-12", "reason": "装修", "created_at": "2026-10-01T00:00:00Z"}},
  "withdrawals": {"u1": {"usage_id": "u1", "reason": "误导入", "created_at": "2026-09-20T00:00:00Z"}}
}`
	if err := os.WriteFile(h2.statePath(), []byte(suspended), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h2.mustRun("customer", "suspensions", "c1", "2026-11")
	if !strings.Contains(out, "已暂停") {
		t.Fatalf("已撤回记录存在于暂停月份不应视为冲突:\n%s", out)
	}
	if out := h2.mustRun("usage", "show", "u1"); !strings.Contains(out, "已撤回") {
		t.Fatal(out)
	}
}
