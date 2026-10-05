package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 每个用例使用独立临时数据目录，每次调用 run 都重新从磁盘载入，
// 天然模拟“跨进程”持久化。

type harness struct {
	t   *testing.T
	dir string
	buf bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir()}
	prev := stdout
	stdout = &h.buf
	t.Cleanup(func() { stdout = prev })
	return h
}

func (h *harness) run(args ...string) (string, error) {
	h.buf.Reset()
	full := append([]string{"--data-dir", h.dir}, args...)
	err := run(full)
	return h.buf.String(), err
}

func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	out, err := h.run(args...)
	if err != nil {
		h.t.Fatalf("run %v 意外失败: %v", args, err)
	}
	return out
}

func (h *harness) runExpectErr(args ...string) string {
	h.t.Helper()
	out, err := h.run(args...)
	if err == nil {
		h.t.Fatalf("run %v 应失败却成功，输出:\n%s", args, out)
	}
	return err.Error()
}

func (h *harness) writeFile(name, content string) string {
	h.t.Helper()
	p := filepath.Join(h.dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) statePath() string { return filepath.Join(h.dir, "state.json") }

const csvHeader = "usage_id,customer_id,time,quantity\n"

func TestHelpAndNoArgs(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{nil, {"--help"}, {"-h"}} {
		out, err := h.run(args...)
		if err != nil {
			t.Fatalf("args=%v 出错: %v", args, err)
		}
		if !strings.Contains(out, "planbill") || !strings.Contains(out, "customer add") {
			t.Fatalf("args=%v 帮助内容异常:\n%s", args, out)
		}
	}
}

func TestInvalidCommandsExitNonzeroWithUsageError(t *testing.T) {
	h := newHarness(t)
	cases := [][]string{
		{"bogus"},
		{"customer"},
		{"customer", "delete", "x"},
		{"usage"},
		{"usage", "export"},
		{"bill"},
		{"bill", "frobnicate", "c", "2026-09"},
		{"customer", "add", "only-two"},
		{"--help", "customer"},
	}
	for _, args := range cases {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}
}

func TestCustomerRegistrationValidation(t *testing.T) {
	h := newHarness(t)

	out := h.mustRun("customer", "add", "c1", "甲方", "150")
	if !strings.Contains(out, "已登记客户") {
		t.Fatal(out)
	}

	// 重复标识拒绝。
	h.runExpectErr("customer", "add", "c1", "另一个名称", "200")
	// 空标识 / 纯空白标识、空名称拒绝。
	h.runExpectErr("customer", "add", "", "名称", "100")
	h.runExpectErr("customer", "add", "   ", "名称", "100")
	h.runExpectErr("customer", "add", "c2", "", "100")
	h.runExpectErr("customer", "add", "c2", "   ", "100")
	// 单价必须是非负整数分。
	h.runExpectErr("customer", "add", "c2", "名称", "-1")
	h.runExpectErr("customer", "add", "c2", "名称", "1.5")
	h.runExpectErr("customer", "add", "c2", "名称", "abc")
	h.runExpectErr("customer", "add", "c2", "名称", "99999999999999999999999")

	// 零单价合法；且重复登记被拒后库状态不变（c2 仍可首次创建）。
	h.mustRun("customer", "add", "c2", "乙方", "0")
	out = h.mustRun("customer", "add", "c3", "丙", "10")
	_ = out
}

func TestImportHappyPathAndDuplicateReplay(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")

	f := h.writeFile("u1.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,3\n"+
		"u2,c1,2026-09-16T10:00:00+08:00,2\n")

	out := h.mustRun("usage", "import", f)
	if !strings.Contains(out, "新增 2 条") || !strings.Contains(out, "重复跳过 0 条") {
		t.Fatal(out)
	}

	// 完全重放：两条都按重复跳过，不新增。
	out = h.mustRun("usage", "import", f)
	if !strings.Contains(out, "新增 0 条") || !strings.Contains(out, "重复跳过 2 条") {
		t.Fatal(out)
	}

	// 同一时刻的不同时区写法（Z 与 +00:00）视为相同内容。
	f2 := h.writeFile("u2.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00+00:00,3\n")
	out = h.mustRun("usage", "import", f2)
	if !strings.Contains(out, "重复跳过 1 条") {
		t.Fatal(out)
	}
}

func TestImportConflictingIDRejectsWholeBatch(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")

	base := h.writeFile("base.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,3\n")
	h.mustRun("usage", "import", base)

	// 与库中冲突 + 另有一条合法新记录：整批失败，合法记录不得留下。
	bad := h.writeFile("bad.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,9\n"+
		"u9,c1,2026-09-20T10:00:00Z,1\n")
	errMsg := h.runExpectErr("usage", "import", bad)
	if !strings.Contains(errMsg, "u1") || !strings.Contains(errMsg, "整批未生效") {
		t.Fatal(errMsg)
	}

	// 整批失败后，u9 未入账：9 月仍只有 u1 一笔（数量 3），
	// 用结算结果验证业务状态未变。
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "总数量：3") {
		t.Fatalf("失败的导入污染了数据:\n%s", out)
	}
}

func TestImportValidationErrors(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")

	cases := map[string]string{
		"客户不存在":  "u1,ghost,2026-09-15T10:00:00Z,3\n",
		"时间格式错误": "u1,c1,not-a-time,3\n",
		"数量为零":   "u1,c1,2026-09-15T10:00:00Z,0\n",
		"数量为负":   "u1,c1,2026-09-15T10:00:00Z,-2\n",
		"数量非整数":  "u1,c1,2026-09-15T10:00:00Z,x\n",
		"标识为空":   ",c1,2026-09-15T10:00:00Z,3\n",
		"字段缺失":   "u1,c1,2026-09-15T10:00:00Z\n",
	}
	for name, body := range cases {
		f := h.writeFile("x.csv", csvHeader+body)
		msg := h.runExpectErr("usage", "import", f)
		if !strings.Contains(msg, "整批未生效") {
			t.Fatalf("用例 %s 错误信息未说明整批失败: %s", name, msg)
		}
	}

	// 表头错误。
	badHeader := h.writeFile("h.csv", "id,cust,time,qty\nu1,c1,2026-09-15T10:00:00Z,3\n")
	h.runExpectErr("usage", "import", badHeader)

	// 空文件。
	empty := h.writeFile("empty.csv", "")
	h.runExpectErr("usage", "import", empty)

	// 所有导入均失败后，9 月无用量可结。
	h.runExpectErr("bill", "settle", "c1", "2026-09")
}

func TestImportDuplicatesWithinFile(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")

	// 文件内相同记录出现两次：1 新增 1 重复。
	same := h.writeFile("same.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,3\n"+
		"u1,c1,2026-09-15T10:00:00Z,3\n")
	out := h.mustRun("usage", "import", same)
	if !strings.Contains(out, "新增 1 条") || !strings.Contains(out, "重复跳过 1 条") {
		t.Fatal(out)
	}

	// 文件内同标识不同内容：整批拒绝。
	diff := h.writeFile("diff.csv", csvHeader+
		"u2,c1,2026-09-15T10:00:00Z,3\n"+
		"u2,c1,2026-09-16T10:00:00Z,3\n")
	msg := h.runExpectErr("usage", "import", diff)
	if !strings.Contains(msg, "u2") {
		t.Fatal(msg)
	}
}

func TestImportAmountOverflowRejectsBatch(t *testing.T) {
	h := newHarness(t)
	// 单价接近 MaxInt64。
	h.mustRun("customer", "add", "rich", "巨款客户", "9223372036854775800")
	f := h.writeFile("big.csv", csvHeader+
		"u1,rich,2026-09-15T10:00:00Z,3\n") // 数量 3 即溢出
	msg := h.runExpectErr("usage", "import", f)
	if !strings.Contains(msg, "溢出") {
		t.Fatal(msg)
	}
}

func TestSettleHappyPathAndTotals(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "150")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-01T00:00:00Z,2\n"+ // 300
		"u2,c1,2026-09-30T23:59:59Z,4\n", // 600
	)
	h.mustRun("usage", "import", f)

	out := h.mustRun("bill", "settle", "c1", "2026-09")
	for _, want := range []string{
		"BILL-", "已封账", "总数量：6", "总金额：900 分",
		"用量标识=u1", "用量标识=u2", "单价：150 分", "9.00 元",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("结算输出缺少 %q:\n%s", want, out)
		}
	}
}

func TestSettleValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "150")

	h.runExpectErr("bill", "settle", "ghost", "2026-09")
	h.runExpectErr("bill", "settle", "c1", "2026-9")
	h.runExpectErr("bill", "settle", "c1", "2026-13")
	h.runExpectErr("bill", "settle", "c1", "2026-09-01")
	// 该月无用量：拒绝且不封账（之后补录再结算应成功）。
	h.runExpectErr("bill", "settle", "c1", "2026-09")

	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")
}

func TestSettleIdempotentAndStableBillID(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)

	first := h.mustRun("bill", "settle", "c1", "2026-09")
	second := h.mustRun("bill", "settle", "c1", "2026-09")

	id1 := extractBillID(first)
	id2 := extractBillID(second)
	if id1 == "" || id1 != id2 {
		t.Fatalf("账单标识不稳定: %q vs %q", id1, id2)
	}
	if !strings.Contains(second, "幂等") {
		t.Fatalf("重复结算应提示返回原账单:\n%s", second)
	}
	// 跨“进程”查询得到同一张账单。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if extractBillID(shown) != id1 {
		t.Fatalf("查询账单标识不一致")
	}
	h.runExpectErr("bill", "show", "c1", "2026-10")
}

func TestSealedMonthBlocksNewUsageButAllowsReplay(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")

	// 封账后，新用量进入 9 月被拒（整批失败）。
	intruder := h.writeFile("new.csv", csvHeader+"u2,c1,2026-09-30T10:00:00Z,1\n")
	msg := h.runExpectErr("usage", "import", intruder)
	if !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}

	// 已入账记录的相同重放仍成功跳过。
	out := h.mustRun("usage", "import", f)
	if !strings.Contains(out, "重复跳过 1 条") {
		t.Fatal(out)
	}

	// 其他月份不受影响。
	other := h.writeFile("oct.csv", csvHeader+"u3,c1,2026-10-01T00:00:00Z,1\n")
	h.mustRun("usage", "import", other)
	h.mustRun("bill", "settle", "c1", "2026-10")

	// 原账单内容不被改变（数量仍为 2）。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "总数量：2") {
		t.Fatal(shown)
	}
}

func TestOtherCustomersAndMonthsUnaffected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "a", "客户A", "100")
	h.mustRun("customer", "add", "b", "客户B", "200")
	fa := h.writeFile("a.csv", csvHeader+
		"a1,a,2026-09-10T00:00:00Z,1\n"+
		"a2,a,2026-10-10T00:00:00Z,1\n")
	fb := h.writeFile("b.csv", csvHeader+"b1,b,2026-09-10T00:00:00Z,1\n")
	h.mustRun("usage", "import", fa)
	h.mustRun("usage", "import", fb)

	outA := h.mustRun("bill", "settle", "a", "2026-09")
	outB := h.mustRun("bill", "settle", "b", "2026-09")
	if extractBillID(outA) == extractBillID(outB) {
		t.Fatal("不同客户账单标识不应相同")
	}
	if !strings.Contains(outB, "总金额：200 分") {
		t.Fatal(outB)
	}
	// 客户 A 的 10 月仍可结算。
	h.mustRun("bill", "settle", "a", "2026-10")
	// 客户 B 的 10 月无用量。
	h.runExpectErr("bill", "settle", "b", "2026-10")
}

func TestUTCMonthBoundaries(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("edge.csv", csvHeader+
		// UTC 10/1 00:00（西八区 9/30 16:00）-> 归 10 月
		"e1,c1,2026-09-30T16:00:00-08:00,1\n"+
		// UTC 9/30 16:30（东八区 10/1 00:30）-> 归 9 月
		"e2,c1,2026-10-01T00:30:00+08:00,1\n"+
		// UTC 10/1 00:00:00 本身 -> 归 10 月（左闭右开）
		"e3,c1,2026-10-01T00:00:00Z,1\n",
	)
	h.mustRun("usage", "import", f)

	sep := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(sep, "总数量：1") || !strings.Contains(sep, "用量标识=e2") {
		t.Fatalf("9 月归集错误:\n%s", sep)
	}
	oct := h.mustRun("bill", "settle", "c1", "2026-10")
	if !strings.Contains(oct, "总数量：2") ||
		!strings.Contains(oct, "e1") || !strings.Contains(oct, "e3") {
		t.Fatalf("10 月归集错误:\n%s", oct)
	}
}

func TestSettleAggregateOverflowRejectsWithoutSealing(t *testing.T) {
	h := newHarness(t)
	// 单价 1，用多条用量让总数量本身溢出 int64。
	h.mustRun("customer", "add", "c1", "甲方", "1")
	var body strings.Builder
	body.WriteString(csvHeader)
	body.WriteString("u1,c1,2026-09-01T00:00:00Z,9223372036854775807\n")
	body.WriteString("u2,c1,2026-09-02T00:00:00Z,1\n")
	f := h.writeFile("big.csv", body.String())
	h.mustRun("usage", "import", f)

	msg := h.runExpectErr("bill", "settle", "c1", "2026-09")
	if !strings.Contains(msg, "溢出") {
		t.Fatal(msg)
	}

	// 未封账：导入 9 月新用量应能成功（若已封账会被拒）。
	more := h.writeFile("more.csv", csvHeader+"u3,c1,2026-09-03T00:00:00Z,1\n")
	h.mustRun("usage", "import", more)
}

func TestCorruptDataIsRejectedNotOverwritten(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")

	garbage := []byte("{ this is not json")
	if err := os.WriteFile(h.statePath(), garbage, 0o644); err != nil {
		t.Fatal(err)
	}

	// 任何命令都应报损坏错误，而不是当成空库。
	for _, args := range [][]string{
		{"customer", "add", "c2", "乙方", "1"},
		{"bill", "show", "c1", "2026-09"},
		{"bill", "settle", "c1", "2026-09"},
	} {
		msg := h.runExpectErr(args...)
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("args=%v 未报损坏: %s", args, msg)
		}
	}

	// 失败后损坏文件原封不动，没有被空库覆盖。
	got, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(garbage) {
		t.Fatalf("损坏数据被改写:\n%s", got)
	}

	// 语义不一致的数据同样按损坏处理。
	semantic := `{"version":1,"customers":{},"usage":{"u1":{"id":"u1","customer_id":"ghost","time":"2026-09-15T10:00:00Z","quantity":1}},"bills":{}}`
	if err := os.WriteFile(h.statePath(), []byte(semantic), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.runExpectErr("customer", "add", "c2", "乙方", "1")
	if !strings.Contains(msg, "损坏") {
		t.Fatal(msg)
	}
}

func TestPersistenceAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "250")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,4\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")

	// 全新的 harness 指向同一目录，验证磁盘持久化。
	h2 := &harness{t: t, dir: h.dir}
	prev := stdout
	stdout = &h2.buf
	defer func() { stdout = prev }()
	out, err := h2.run("bill", "show", "c1", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "总金额：1000 分") || !strings.Contains(out, "甲方") {
		t.Fatal(out)
	}
}

func TestMoneyFenFormatting(t *testing.T) {
	cases := map[int64]string{
		0:    "0.00 元",
		5:    "0.05 元",
		99:   "0.99 元",
		100:  "1.00 元",
		1234: "12.34 元",
		-105: "-1.05 元",
	}
	for fen, want := range cases {
		if got := moneyFen(fen); got != want {
			t.Errorf("moneyFen(%d) = %q, want %q", fen, got, want)
		}
	}
}

func extractBillID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "账单标识：") {
			return strings.TrimPrefix(line, "账单标识：")
		}
	}
	return ""
}
