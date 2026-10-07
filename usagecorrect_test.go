package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// --- 未封账用量原子更正测试 ---

func TestUsageCorrectHappyPathFixed(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	h.mustRun("customer", "add", "c2", "乙方", "200")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)

	out := h.mustRun("usage", "correct", "u1", "u1-fix", "c2", "2026-09-16T10:00:00Z", "5", "客户与数量登记错误")
	for _, want := range []string{
		"已更正用量", "更正原因：客户与数量登记错误",
		"原记录", "用量标识：u1", "客户：c1（甲方）", "UTC 月份：2026-09", "数量：2",
		"当前状态：已撤回（撤回原因：客户与数量登记错误",
		"替代记录", "用量标识：u1-fix", "客户：c2（乙方）", "时间：2026-09-16T10:00:00Z",
		"UTC 月份：2026-09", "数量：5", "当前状态：有效",
		"u1（c1，UTC 月份 2026-09）→ u1-fix（c2，UTC 月份 2026-09）",
		"更正不可撤销",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("更正输出缺少 %q:\n%s", want, out)
		}
	}

	// usage show 展示直接前身/后继及原因。
	out = h.mustRun("usage", "show", "u1")
	for _, want := range []string{"当前状态：已撤回", "直接后继：u1-fix", "更正原因：客户与数量登记错误", "直接前身：无"} {
		if !strings.Contains(out, want) {
			t.Fatalf("u1 show 缺少 %q:\n%s", want, out)
		}
	}
	out = h.mustRun("usage", "show", "u1-fix")
	for _, want := range []string{"当前状态：有效", "直接前身：u1", "直接后继：无"} {
		if !strings.Contains(out, want) {
			t.Fatalf("u1-fix show 缺少 %q:\n%s", want, out)
		}
	}
	// 无关记录明确说明无关联。
	g := h.writeFile("g.csv", csvHeader+"g1,c1,2026-09-17T10:00:00Z,1\n")
	h.mustRun("usage", "import", g)
	out = h.mustRun("usage", "show", "g1")
	for _, want := range []string{"直接前身：无", "直接后继：无"} {
		if !strings.Contains(out, want) {
			t.Fatalf("g1 show 缺少 %q:\n%s", want, out)
		}
	}

	// 原内容不可改写：u1 仍是旧客户、旧时间、旧数量；u1-fix 是独立新记录。
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"id": "u1"`, `"customer_id": "c1"`, `"time": "2026-09-15T10:00:00Z"`, `"quantity": 2`,
		`"id": "u1-fix"`, `"customer_id": "c2"`, `"quantity": 5`,
		`"usage_corrections"`, `"replacement_id": "u1-fix"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("存档缺少 %q", want)
		}
	}
}

func TestUsageCorrectValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c2", "乙方", "p1")
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-10-15T10:00:00Z,2\n"+
		"same,c1,2026-10-16T10:00:00Z,7\n")
	h.mustRun("usage", "import", f)
	// 9 月记录用于封账月份检查。
	s := h.writeFile("s.csv", csvHeader+"s1,c1,2026-09-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", s)
	h.mustRun("bill", "settle", "c1", "2026-09")
	// 阶梯客户的暂停月份。
	h.mustRun("customer", "suspend", "c2", "2026-11", "2026-12", "暂停营业")

	cases := [][]string{
		{"usage", "correct", "", "u9", "c1", "2026-10-16T10:00:00Z", "1", "原因"},                     // 空原标识
		{"usage", "correct", "u1", "", "c1", "2026-10-16T10:00:00Z", "1", "原因"},                     // 空新标识
		{"usage", "correct", "u1", "u9", "", "2026-10-16T10:00:00Z", "1", "原因"},                     // 空新客户
		{"usage", "correct", "u1", "u9", "c1", "2026-10-16T10:00:00Z", "1", "  "},                   // 空原因
		{"usage", "correct", "u1", "u9", "c1", "not-a-time", "1", "原因"},                             // 坏时间
		{"usage", "correct", "u1", "u9", "c1", "2026-10-16T10:00:00Z", "0", "原因"},                   // 数量 0
		{"usage", "correct", "u1", "u9", "c1", "2026-10-16T10:00:00Z", "-3", "原因"},                  // 负数量
		{"usage", "correct", "u1", "u9", "c1", "2026-10-16T10:00:00Z", "x", "原因"},                   // 非整数
		{"usage", "correct", "ghost", "u9", "c1", "2026-10-16T10:00:00Z", "1", "原因"},                // 原记录不存在
		{"usage", "correct", "u1", "u1", "c1", "2026-10-16T10:00:00Z", "1", "原因"},                   // 新标识与原标识相同
		{"usage", "correct", "u1", "same", "c1", "2026-10-16T10:00:00Z", "7", "原因"},                 // 新标识已使用（且同内容）
		{"usage", "correct", "u1", "u9", "ghost", "2026-10-16T10:00:00Z", "1", "原因"},                // 新客户不存在
		{"usage", "correct", "u1", "u9", "c1", "2026-09-20T10:00:00Z", "1", "原因"},                   // 新月已封账
		{"usage", "correct", "u1", "u9", "c2", "2026-11-20T10:00:00Z", "1", "原因"},                   // 新月暂停
		{"usage", "correct", "u1", "u9", "c1", "2026-10-16T10:00:00Z", "9223372036854775807", "原因"}, // 固定单价溢出
		{"usage", "correct", "s1", "u9", "c1", "2026-10-16T10:00:00Z", "1", "原因"},                   // 原月已封账
	}
	for _, args := range cases {
		before, _ := os.ReadFile(h.statePath())
		msg := h.runExpectErr(args...)
		after, _ := os.ReadFile(h.statePath())
		if string(after) != string(before) {
			t.Fatalf("失败更正改写了存档: args=%v", args)
		}
		if msg == "" {
			t.Fatalf("args=%v 应失败", args)
		}
	}

	// 阶梯单条计价溢出：方案 -:2，数量 MaxInt64。
	h.mustRun("plan", "add", "p2", "高价", "-:2")
	h.mustRun("customer", "add-plan", "c3", "丙方", "p2")
	msg := h.runExpectErr("usage", "correct", "u1", "u9", "c3", "2026-10-16T10:00:00Z", "9223372036854775807", "原因")
	if !strings.Contains(msg, "溢出") {
		t.Fatal(msg)
	}

	// 已撤回的原记录不能更正。
	h.mustRun("usage", "withdraw", "u1", "误导入")
	if msg := h.runExpectErr("usage", "correct", "u1", "u9", "c1", "2026-10-16T10:00:00Z", "1", "再改"); !strings.Contains(msg, "已撤回") {
		t.Fatal(msg)
	}

	// 参数数量错误是用法错误（退出码 2）。
	for _, args := range [][]string{
		{"usage", "correct"},
		{"usage", "correct", "u1"},
		{"usage", "correct", "u1", "u9", "c1", "2026-10-16T10:00:00Z", "1"}, // 缺原因
		{"usage", "correct", "u1", "u9", "c1", "2026-10-16T10:00:00Z", "1", "原因", "多余"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}

	// 帮助包含更正入口与简短说明。
	if out, _ := h.run("--help"); !strings.Contains(out, "usage correct") {
		t.Fatal("帮助缺少 usage correct")
	}

	// 全部失败后未占用新标识、未留下撤回标记：可成功更正。
	h2 := newHarness(t)
	h2.mustRun("customer", "add", "c1", "甲方", "100")
	f2 := h2.writeFile("u.csv", csvHeader+"u1,c1,2026-10-15T10:00:00Z,2\n")
	h2.mustRun("usage", "import", f2)
	h2.runExpectErr("usage", "correct", "u1", "u9", "c1", "2026-09-01T00:00:00Z", "9223372036854775807", "溢出会失败")
	// u9 未被占用，原样以合法参数重试成功。
	h2.mustRun("usage", "correct", "u1", "u9", "c1", "2026-10-16T10:00:00Z", "3", "更正")
}

func TestUsageCorrectIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c2", "乙方", "p1")
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-10-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "correct", "u1", "u2", "c2", "2026-10-16T10:00:00Z", "5", "记错")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 完全相同重放：返回原更正及两条记录当前状态，不写盘。
	out := h.mustRun("usage", "correct", "u1", "u2", "c2", "2026-10-16T10:00:00Z", "5", "记错")
	if !strings.Contains(out, "内容相同，返回原更正") {
		t.Fatalf("相同重放未幂等返回:\n%s", out)
	}
	after, _ := os.ReadFile(h.statePath())
	if string(after) != string(before) {
		t.Fatal("相同重放改写了存档")
	}
	// 时间字符串不同但解析后同一瞬间：仍视为相同重放。
	out = h.mustRun("usage", "correct", "u1", "u2", "c2", "2026-10-16T18:00:00+08:00", "5", "记错")
	if !strings.Contains(out, "返回原更正") {
		t.Fatalf("同一瞬间的不同时区写法应幂等:\n%s", out)
	}
	after2, _ := os.ReadFile(h.statePath())
	if string(after2) != string(before) {
		t.Fatal("同一瞬间重放改写了存档")
	}
	// 任一项不同拒绝。
	for _, args := range [][]string{
		{"usage", "correct", "u1", "uX", "c2", "2026-10-16T10:00:00Z", "5", "记错"},   // 新标识不同
		{"usage", "correct", "u1", "u2", "c1", "2026-10-16T10:00:00Z", "5", "记错"},   // 客户不同
		{"usage", "correct", "u1", "u2", "c2", "2026-10-16T11:00:00Z", "5", "记错"},   // 时间点不同
		{"usage", "correct", "u1", "u2", "c2", "2026-10-16T10:00:00Z", "6", "记错"},   // 数量不同
		{"usage", "correct", "u1", "u2", "c2", "2026-10-16T10:00:00Z", "5", "别的原因"}, // 原因不同
	} {
		if msg := h.runExpectErr(args...); !strings.Contains(msg, "只能更正一次") {
			t.Fatalf("args=%v 应拒绝: %s", args, msg)
		}
	}

	// 后来封账不能阻止相同重放。
	h.mustRun("bill", "settle", "c2", "2026-10") // 替代记录入账并封账
	out = h.mustRun("usage", "correct", "u1", "u2", "c2", "2026-10-16T10:00:00Z", "5", "记错")
	if !strings.Contains(out, "返回原更正") {
		t.Fatalf("封账后重放应成功:\n%s", out)
	}
	// 重放不恢复任何记录。
	if shown := h.mustRun("usage", "show", "u1"); !strings.Contains(shown, "已撤回") {
		t.Fatal(shown)
	}
}

func TestUsageCorrectReplayAfterReplacementWithdrawn(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-10-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "correct", "u1", "u2", "c1", "2026-10-16T10:00:00Z", "5", "记错")
	// 替代记录在未封账月按原规则撤回；随后相同重放仍成功并返回其已撤回状态。
	h.mustRun("usage", "withdraw", "u2", "替代记录也要撤回")
	out := h.mustRun("usage", "correct", "u1", "u2", "c1", "2026-10-16T10:00:00Z", "5", "记错")
	if !strings.Contains(out, "返回原更正") || !strings.Contains(out, "当前状态：已撤回（撤回原因：替代记录也要撤回") {
		t.Fatalf("替代记录撤回后重放应返回其当前已撤回状态:\n%s", out)
	}
	if shown := h.mustRun("usage", "show", "u1"); !strings.Contains(shown, "已撤回") {
		t.Fatalf("重放不得恢复原记录:\n%s", shown)
	}
	// 两条记录都已撤回，该月无有效用量，拒绝结算且不封账。
	h.runExpectErr("bill", "settle", "c1", "2026-10")
}

func TestUsageCorrectChain(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-10-15T10:00:00Z,2\n"+
		"x1,c1,2026-10-17T10:00:00Z,9\n")
	h.mustRun("usage", "import", f)
	// u1 -> u2 -> u3 连续更正。
	h.mustRun("usage", "correct", "u1", "u2", "c1", "2026-10-16T10:00:00Z", "3", "第一次更正")
	h.mustRun("usage", "correct", "u2", "u3", "c1", "2026-10-16T11:00:00Z", "4", "第二次更正")

	out := h.mustRun("usage", "show", "u2")
	for _, want := range []string{
		"当前状态：已撤回（撤回原因：第二次更正", "直接前身：u1", "直接后继：u3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("u2 链上状态异常，缺少 %q:\n%s", want, out)
		}
	}
	out = h.mustRun("usage", "show", "u1")
	if !strings.Contains(out, "直接后继：u2") || strings.Contains(out, "直接前身：u2") {
		t.Fatalf("u1 只有后继 u2:\n%s", out)
	}
	out = h.mustRun("usage", "show", "u3")
	if !strings.Contains(out, "直接前身：u2") || !strings.Contains(out, "直接后继：无") || !strings.Contains(out, "当前状态：有效") {
		t.Fatalf("u3 是链尾且有效:\n%s", out)
	}
	// 每条原用量只能更正一次：u1 的不同更正仍被拒绝。
	h.runExpectErr("usage", "correct", "u1", "u9", "c1", "2026-10-16T10:00:00Z", "3", "第一次更正")
	// 第一次更正的相同重放仍成功（不受链式变化影响）。
	out = h.mustRun("usage", "correct", "u1", "u2", "c1", "2026-10-16T10:00:00Z", "3", "第一次更正")
	if !strings.Contains(out, "返回原更正") {
		t.Fatal(out)
	}
	// 链尾 u3 可按原规则撤回；结算只计未撤回且未被替代的 x1（u1/u2/u3 均已撤回）。
	h.mustRun("usage", "withdraw", "u3", "链尾撤回")
	out = h.mustRun("bill", "settle", "c1", "2026-10")
	if !strings.Contains(out, "总数量：9") || strings.Contains(out, "用量标识=u1") ||
		strings.Contains(out, "用量标识=u2") || strings.Contains(out, "用量标识=u3") {
		t.Fatalf("结算应只计有效记录 x1:\n%s", out)
	}
}

func TestUsageCorrectSettlementAndImportReplay(t *testing.T) {
	h := newHarness(t)
	// 阶梯客户：更正后按有效记录从零重新累计分档。
	h.mustRun("plan", "add", "std", "标准阶梯", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c2", "阶梯客户", "std")
	f := h.writeFile("t.csv", csvHeader+
		"t1,c2,2026-09-15T10:00:00Z,60\n"+
		"t2,c2,2026-09-16T10:00:00Z,60\n")
	h.mustRun("usage", "import", f)
	// 把 t1 更正为数量 20 的替代记录；剩余有效记录为 20+60=80，全部第一档。
	h.mustRun("usage", "correct", "t1", "t1-fix", "c2", "2026-09-15T10:00:00Z", "20", "数量多录")
	out := h.mustRun("bill", "settle", "c2", "2026-09")
	for _, want := range []string{
		"总数量：80", "用量费：800 分", "用量标识=t1-fix", "用量标识=t2",
		"第 1 档：数量 80", "第 2 档：数量 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("阶梯结算应按有效记录重新累计，缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "用量标识=t1 ") {
		t.Fatalf("原记录不得进入账单:\n%s", out)
	}

	// 两端原内容重放均按原规则跳过，不恢复任何已撤回记录。
	out = h.mustRun("usage", "import", f)
	if !strings.Contains(out, "新增 0 条，重复跳过 2 条") {
		t.Fatal(out)
	}
	fix := h.writeFile("fix.csv", csvHeader+"t1-fix,c2,2026-09-15T10:00:00Z,20\n")
	out = h.mustRun("usage", "import", fix)
	if !strings.Contains(out, "重复跳过 1 条") {
		t.Fatal(out)
	}
	if shown := h.mustRun("usage", "show", "t1"); !strings.Contains(shown, "已撤回") {
		t.Fatal(shown)
	}
	// 账单不变。
	if shown := h.mustRun("bill", "show", "c2", "2026-09"); !strings.Contains(shown, "总数量：80") {
		t.Fatal(shown)
	}
}

func TestUsageCorrectDoesNotConsumeSeq(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	sf := h.writeFile("s.csv", csvHeader+"s1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", sf)
	h.mustRun("bill", "settle", "c1", "2026-09")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "50", "补收") // 序号 1
	uf := h.writeFile("u.csv", csvHeader+"u1,c1,2026-10-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", uf)
	h.mustRun("usage", "correct", "u1", "u2", "c1", "2026-10-16T10:00:00Z", "4", "更正") // 不占序号
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "30", "补收")                  // 序号 2

	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\"next_seq\": 2") {
		t.Fatalf("用量更正不应占用操作序号:\n%s", data)
	}
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "序号 1 调整 adj-1") || !strings.Contains(out, "序号 2 调整 adj-2") ||
		strings.Contains(out, "更正") {
		t.Fatalf("流水不应出现用量更正事件且序号连续:\n%s", out)
	}
}

func TestUsageCorrectPersistsAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-10-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("usage", "correct", "u1", "u2", "c1", "2026-10-16T10:00:00Z", "5", "记错")

	// 全新 harness 指向同一目录，验证跨进程保持关联、状态与幂等。
	h2 := &harness{t: t, dir: h.dir}
	prev := stdout
	stdout = &h2.buf
	defer func() { stdout = prev }()
	out, err := h2.run("usage", "show", "u2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "直接前身：u1") || !strings.Contains(out, "记错") {
		t.Fatal(out)
	}
	out, err = h2.run("usage", "correct", "u1", "u2", "c1", "2026-10-16T10:00:00Z", "5", "记错")
	if err != nil || !strings.Contains(out, "返回原更正") {
		t.Fatalf("跨进程相同重放应幂等: %v\n%s", err, out)
	}
}

// --- 载入校验：更正关联损坏 ---

const validCorrectionState = `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {
    "u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2},
    "u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-16T10:00:00Z", "quantity": 5}
  },
  "bills": {},
  "withdrawals": {"u1": {"usage_id": "u1", "reason": "记错了", "created_at": "2026-09-20T00:00:00Z"}},
  "usage_corrections": {"u1": {"usage_id": "u1", "replacement_id": "u2", "new_customer_id": "c1", "new_time": "2026-09-16T10:00:00Z", "new_quantity": 5, "reason": "记错了", "created_at": "2026-09-20T00:00:00Z"}}
}`

func TestUsageCorrectLoadValidation(t *testing.T) {
	// 合法存档可读，show 展示关联。
	h := newHarness(t)
	if err := os.WriteFile(h.statePath(), []byte(validCorrectionState), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("usage", "show", "u2")
	if !strings.Contains(out, "直接前身：u1") || !strings.Contains(out, "记错了") {
		t.Fatal(out)
	}

	// 每个损坏用例都是完整存档：共同的头（version/customers）加各自的
	// usage 节，再接 bills/withdrawals/usage_corrections 节。
	head := func(usageJSON string) string {
		return `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": ` + usageJSON + `,
`
	}
	both := head(`{
    "u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2},
    "u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-16T10:00:00Z", "quantity": 5}
  }`)
	onlyU1 := head(`{
    "u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2}
  }`)
	onlyU2 := head(`{
    "u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-16T10:00:00Z", "quantity": 5}
  }`)
	corr := func(replacementID, newCustomer, newTime string, newQty int64, reason string) string {
		return fmt.Sprintf(`  "usage_corrections": {"u1": {"usage_id": "u1", "replacement_id": %q, "new_customer_id": %q, "new_time": %q, "new_quantity": %d, "reason": %q, "created_at": "2026-09-20T00:00:00Z"}}
}`, replacementID, newCustomer, newTime, newQty, reason)
	}
	withdrawn := func(reason string) string {
		return fmt.Sprintf(`  "bills": {},
  "withdrawals": {"u1": {"usage_id": "u1", "reason": %q, "created_at": "2026-09-20T00:00:00Z"}},
`, reason)
	}
	cases := map[string]string{
		"关联引用替代缺失": onlyU1 + withdrawn("记错了") + corr("u2", "c1", "2026-09-16T10:00:00Z", 5, "记错了"),
		"关联引用原记录缺失": onlyU2 +
			`  "bills": {},
` + corr("u2", "c1", "2026-09-16T10:00:00Z", 5, "记错了"),
		"原记录未撤回": both + `  "bills": {},
  "withdrawals": {},
` + corr("u2", "c1", "2026-09-16T10:00:00Z", 5, "记错了"),
		"撤回原因不符": both + withdrawn("别的原因") + corr("u2", "c1", "2026-09-16T10:00:00Z", 5, "记错了"),
		"替代内容不符": both + withdrawn("记错了") + corr("u2", "c1", "2026-09-16T10:00:00Z", 9, "记错了"),
		"新客户不存在": both + withdrawn("记错了") + corr("u2", "c9", "2026-09-16T10:00:00Z", 5, "记错了"),
		"更正原因为空": both + withdrawn("记错了") + corr("u2", "c1", "2026-09-16T10:00:00Z", 5, " "),
		"更正记录为空": both + `  "bills": {},
  "usage_corrections": {"u1": null}
}`,
		"更正键不一致": both + withdrawn("记错了") + strings.Replace(
			corr("u2", "c1", "2026-09-16T10:00:00Z", 5, "记错了"),
			`"usage_corrections": {"u1"`, `"usage_corrections": {"ux"`, 1),
	}
	for name, broken := range cases {
		h := newHarness(t)
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
}

func TestUsageCorrectLoadMultiSourceAndCycle(t *testing.T) {
	// 同一替代记录有两个来源：损坏。
	multi := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {
    "u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2},
    "u3": {"id": "u3", "customer_id": "c1", "time": "2026-09-17T10:00:00Z", "quantity": 3},
    "u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-16T10:00:00Z", "quantity": 5}
  },
  "bills": {},
  "withdrawals": {
    "u1": {"usage_id": "u1", "reason": "r1", "created_at": "2026-09-20T00:00:00Z"},
    "u3": {"usage_id": "u3", "reason": "r3", "created_at": "2026-09-20T00:00:00Z"}
  },
  "usage_corrections": {
    "u1": {"usage_id": "u1", "replacement_id": "u2", "new_customer_id": "c1", "new_time": "2026-09-16T10:00:00Z", "new_quantity": 5, "reason": "r1", "created_at": "2026-09-20T00:00:00Z"},
    "u3": {"usage_id": "u3", "replacement_id": "u2", "new_customer_id": "c1", "new_time": "2026-09-16T10:00:00Z", "new_quantity": 5, "reason": "r3", "created_at": "2026-09-20T00:00:00Z"}
  }
}`
	h := newHarness(t)
	if err := os.WriteFile(h.statePath(), []byte(multi), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("usage", "show", "u1"); !strings.Contains(msg, "多个来源") {
		t.Fatalf("同一替代记录多来源应按损坏拒绝: %s", msg)
	}

	// 关联成环：损坏。
	cycle := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {
    "u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2},
    "u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-16T10:00:00Z", "quantity": 5}
  },
  "bills": {},
  "withdrawals": {
    "u1": {"usage_id": "u1", "reason": "r1", "created_at": "2026-09-20T00:00:00Z"},
    "u2": {"usage_id": "u2", "reason": "r2", "created_at": "2026-09-20T00:00:00Z"}
  },
  "usage_corrections": {
    "u1": {"usage_id": "u1", "replacement_id": "u2", "new_customer_id": "c1", "new_time": "2026-09-16T10:00:00Z", "new_quantity": 5, "reason": "r1", "created_at": "2026-09-20T00:00:00Z"},
    "u2": {"usage_id": "u2", "replacement_id": "u1", "new_customer_id": "c1", "new_time": "2026-09-15T10:00:00Z", "new_quantity": 2, "reason": "r2", "created_at": "2026-09-20T00:00:00Z"}
  }
}`
	h2 := newHarness(t)
	if err := os.WriteFile(h2.statePath(), []byte(cycle), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h2.runExpectErr("usage", "show", "u1"); !strings.Contains(msg, "成环") {
		t.Fatalf("关联成环应按损坏拒绝: %s", msg)
	}
}

func TestUsageCorrectLoadLegitWithdrawAndChain(t *testing.T) {
	// 替代记录后来合法撤回：载入不判损坏，撤回原因不必等于更正原因。
	replWithdrawn := strings.Replace(validCorrectionState,
		`"withdrawals": {"u1": {"usage_id": "u1", "reason": "记错了", "created_at": "2026-09-20T00:00:00Z"}}`,
		`"withdrawals": {
    "u1": {"usage_id": "u1", "reason": "记错了", "created_at": "2026-09-20T00:00:00Z"},
    "u2": {"usage_id": "u2", "reason": "替代记录另因撤回", "created_at": "2026-09-25T00:00:00Z"}
  }`, 1)
	h := newHarness(t)
	if err := os.WriteFile(h.statePath(), []byte(replWithdrawn), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("usage", "show", "u2")
	if !strings.Contains(out, "当前状态：已撤回（撤回原因：替代记录另因撤回") || !strings.Contains(out, "直接前身：u1") {
		t.Fatalf("替代记录合法撤回不应判损坏:\n%s", out)
	}

	// 继续更正形成 u1 -> u2 -> u3：合法。
	chain := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {
    "u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2},
    "u2": {"id": "u2", "customer_id": "c1", "time": "2026-09-16T10:00:00Z", "quantity": 5},
    "u3": {"id": "u3", "customer_id": "c1", "time": "2026-09-16T11:00:00Z", "quantity": 6}
  },
  "bills": {},
  "withdrawals": {
    "u1": {"usage_id": "u1", "reason": "r1", "created_at": "2026-09-20T00:00:00Z"},
    "u2": {"usage_id": "u2", "reason": "r2", "created_at": "2026-09-21T00:00:00Z"}
  },
  "usage_corrections": {
    "u1": {"usage_id": "u1", "replacement_id": "u2", "new_customer_id": "c1", "new_time": "2026-09-16T10:00:00Z", "new_quantity": 5, "reason": "r1", "created_at": "2026-09-20T00:00:00Z"},
    "u2": {"usage_id": "u2", "replacement_id": "u3", "new_customer_id": "c1", "new_time": "2026-09-16T11:00:00Z", "new_quantity": 6, "reason": "r2", "created_at": "2026-09-21T00:00:00Z"}
  }
}`
	h2 := newHarness(t)
	if err := os.WriteFile(h2.statePath(), []byte(chain), 0o644); err != nil {
		t.Fatal(err)
	}
	out = h2.mustRun("usage", "show", "u2")
	if !strings.Contains(out, "直接前身：u1") || !strings.Contains(out, "直接后继：u3") {
		t.Fatalf("链式更正应正常载入:\n%s", out)
	}

	// 旧存档缺少更正信息：视为无更正，且 show 明确无关联。
	h3 := newHarness(t)
	legacy := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2}},
  "bills": {}
}`
	if err := os.WriteFile(h3.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	out = h3.mustRun("usage", "show", "u1")
	if !strings.Contains(out, "直接前身：无") || !strings.Contains(out, "直接后继：无") || !strings.Contains(out, "当前状态：有效") {
		t.Fatalf("旧存档应视为无更正:\n%s", out)
	}
	// 在旧存档上可以正常发起更正并再次读回。
	h3.mustRun("usage", "correct", "u1", "u2", "c1", "2026-09-16T10:00:00Z", "3", "记错")
}
