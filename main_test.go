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

// --- 费用调整与撤销 ---

// settleBill 是测试辅助：登记客户、导入一条用量并结算，返回账单总金额（分）。
func settleBill(h *harness, customerID, price, qty string) {
	h.t.Helper()
	h.mustRun("customer", "add", customerID, "客户"+customerID, price)
	f := h.writeFile("u-"+customerID+".csv", csvHeader+
		"u-"+customerID+","+customerID+",2026-09-15T10:00:00Z,"+qty+"\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", customerID, "2026-09")
}

func TestAdjustHappyPathAndShow(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "150", "6") // 总金额 900

	out := h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "漏算用量")
	if !strings.Contains(out, "补收") || !strings.Contains(out, "当前应付 1400 分") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "-200", "客户补偿")
	if !strings.Contains(out, "减免") || !strings.Contains(out, "当前应付 1200 分") {
		t.Fatal(out)
	}

	shown := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"总金额：900 分", "调整净额：+300 分", "当前应付：1200 分",
		"调整 adj-1：补收 +500 分", "漏算用量", "生效中",
		"调整 adj-2：减免 -200 分", "客户补偿",
	} {
		if !strings.Contains(shown, want) {
			t.Fatalf("账单展示缺少 %q:\n%s", want, shown)
		}
	}
	// 原账单字段不变。
	if !strings.Contains(shown, "总数量：6") || !strings.Contains(shown, "单价：150 分") {
		t.Fatal(shown)
	}

	// 重复结算不创建账单或调整，返回含调整历史的原账单。
	again := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(again, "幂等") || !strings.Contains(again, "当前应付：1200 分") ||
		!strings.Contains(again, "调整 adj-1") {
		t.Fatal(again)
	}
}

func TestAdjustValidation(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1")

	// 客户不存在 / 账单不存在（未结算月份）。
	h.runExpectErr("bill", "adjust", "ghost", "2026-09", "a1", "100", "原因")
	h.runExpectErr("bill", "adjust", "c1", "2026-10", "a1", "100", "原因")
	// 月份格式非法。
	h.runExpectErr("bill", "adjust", "c1", "2026-9", "a1", "100", "原因")
	// 调整标识为空、原因为空。
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "", "100", "原因")
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "100", "")
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "100", "   ")
	// 金额：零、非整数、超范围。
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "0", "原因")
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "1.5", "原因")
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "abc", "原因")
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "99999999999999999999999", "原因")

	// 以上全部失败，标识 a1 不应被占用：首次成功新增照常进行。
	out := h.mustRun("bill", "adjust", "c1", "2026-09", "a1", "100", "首次生效")
	if !strings.Contains(out, "当前应付 200 分") {
		t.Fatal(out)
	}
}

func TestAdjustIdempotentAndConflict(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1") // 总金额 100
	settleBill(h, "c2", "100", "1")

	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补收一")

	// 内容完全相同的重放：返回已保存记录，不再次增减应付。
	out := h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补收一")
	if !strings.Contains(out, "不重复增减应付") || !strings.Contains(out, "当前应付：600 分") {
		t.Fatal(out)
	}

	// 任一字段不同均拒绝：金额、原因、跨客户、跨月份。
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "adj-1", "501", "补收一")
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "adj-1", "500", "另一个原因")
	h.runExpectErr("bill", "adjust", "c2", "2026-09", "adj-1", "500", "补收一")
	h.mustRun("usage", "import", h.writeFile("oct.csv", csvHeader+"u-oct,c1,2026-10-01T00:00:00Z,1\n"))
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.runExpectErr("bill", "adjust", "c1", "2026-10", "adj-1", "500", "补收一")

	// 冲突拒绝后应付不变。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：600 分") {
		t.Fatal(shown)
	}
}

func TestAdjustPayableBounds(t *testing.T) {
	h := newHarness(t)
	// 总金额接近 MaxInt64。
	settleBill(h, "rich", "9223372036854775800", "1")

	// 上溢：拒绝。
	msg := h.runExpectErr("bill", "adjust", "rich", "2026-09", "up", "100", "越界补收")
	if !strings.Contains(msg, "拒绝调整") {
		t.Fatal(msg)
	}
	// 减免到 0 合法。
	h.mustRun("bill", "adjust", "rich", "2026-09", "down", "-9223372036854775800", "全额减免")
	// 再减 1 分即小于 0：拒绝。
	msg = h.runExpectErr("bill", "adjust", "rich", "2026-09", "down2", "-1", "过度减免")
	if !strings.Contains(msg, "拒绝调整") {
		t.Fatal(msg)
	}
	shown := h.mustRun("bill", "show", "rich", "2026-09")
	if !strings.Contains(shown, "当前应付：0 分") {
		t.Fatal(shown)
	}
}

func TestRevokeFlow(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1") // 总金额 100
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补收")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "-100", "减免")

	out := h.mustRun("bill", "revoke", "adj-1", "录入错误")
	if !strings.Contains(out, "已撤销调整") || !strings.Contains(out, "当前应付 0 分") {
		t.Fatal(out)
	}

	// 相同标识与相同原因重复撤销：幂等成功，不新增记录。
	out = h.mustRun("bill", "revoke", "adj-1", "录入错误")
	if !strings.Contains(out, "幂等") {
		t.Fatal(out)
	}
	// 改用其他原因：拒绝。
	h.runExpectErr("bill", "revoke", "adj-1", "另一个原因")
	// 撤销目标不存在、原因为空。
	h.runExpectErr("bill", "revoke", "ghost", "原因")
	h.runExpectErr("bill", "revoke", "adj-2", "")

	// 撤销后重放原调整：返回已撤销状态，不能重新生效。
	out = h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补收")
	if !strings.Contains(out, "已撤销") || !strings.Contains(out, "当前应付：0 分") {
		t.Fatal(out)
	}

	// 历史按成功操作顺序：调整 adj-1、调整 adj-2、撤销 adj-1。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	i1 := strings.Index(shown, "调整 adj-1")
	i2 := strings.Index(shown, "调整 adj-2")
	i3 := strings.Index(shown, "撤销 adj-1")
	if i1 < 0 || i2 < 0 || i3 < 0 || !(i1 < i2 && i2 < i3) {
		t.Fatalf("历史顺序错误:\n%s", shown)
	}
	if !strings.Contains(shown, "当前状态：已撤销") || !strings.Contains(shown, "录入错误") ||
		!strings.Contains(shown, "当前应付：0 分") {
		t.Fatal(shown)
	}
}

func TestRevokeOutOfBoundsKeepsEffective(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1")                                  // 总金额 100
	h.mustRun("bill", "adjust", "c1", "2026-09", "a1", "100", "补收")  // 应付 200
	h.mustRun("bill", "adjust", "c1", "2026-09", "a2", "-200", "减免") // 应付 0

	// 撤销 a1 后应付将为 -100：拒绝，a1 仍有效。
	msg := h.runExpectErr("bill", "revoke", "a1", "想撤销")
	if !strings.Contains(msg, "仍有效") {
		t.Fatal(msg)
	}
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：0 分") || strings.Contains(shown, "已撤销") {
		t.Fatal(shown)
	}

	// 其他合法调整改变余额后可重试并成功。
	h.mustRun("bill", "adjust", "c1", "2026-09", "a3", "200", "再补收") // 应付 200
	out := h.mustRun("bill", "revoke", "a1", "想撤销")
	if !strings.Contains(out, "当前应付 100 分") {
		t.Fatal(out)
	}
}

func TestAdjustPersistsAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "2") // 总金额 200
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "50", "补收")
	h.mustRun("bill", "revoke", "adj-1", "撤销补收")

	// 全新 harness 指向同一目录：记录、顺序、重复判断与撤销约束保持。
	h2 := &harness{t: t, dir: h.dir}
	prev := stdout
	stdout = &h2.buf
	defer func() { stdout = prev }()

	out, err := h2.run("bill", "show", "c1", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "当前应付：200 分") || !strings.Contains(out, "已撤销") {
		t.Fatal(out)
	}
	// 重放已撤销调整仍返回已撤销状态。
	out, err = h2.run("bill", "adjust", "c1", "2026-09", "adj-1", "50", "补收")
	if err != nil || !strings.Contains(out, "已撤销") {
		t.Fatalf("out=%s err=%v", out, err)
	}
	// 换内容复用标识仍拒绝。
	if _, err = h2.run("bill", "adjust", "c1", "2026-09", "adj-1", "51", "补收"); err == nil {
		t.Fatal("应拒绝标识冲突")
	}
	// 相同原因重复撤销幂等。
	if _, err = h2.run("bill", "revoke", "adj-1", "撤销补收"); err != nil {
		t.Fatal(err)
	}
}

func TestOldStateFileWithoutAdjustments(t *testing.T) {
	h := newHarness(t)
	// 手工构造无 adjustments/next_seq 字段的旧版数据文件。
	legacy := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2}},
  "bills": {"c1|2026-09": {
    "id": "BILL-legacy", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 2, "unit_price_fen": 100, "total_fee_fen": 200,
    "lines": [{"usage_id": "u1", "time": "2026-09-15T10:00:00Z", "quantity": 2, "line_fee_fen": 200}],
    "created_at": "2026-10-01T00:00:00Z"
  }}
}`
	if err := os.WriteFile(h.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	// 旧账单视为零调整。
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "调整净额：+0 分") || !strings.Contains(out, "当前应付：200 分") ||
		!strings.Contains(out, "调整与撤销历史：无") {
		t.Fatal(out)
	}
	// 旧数据上可直接新增调整。
	out = h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "30", "补收")
	if !strings.Contains(out, "当前应付 230 分") {
		t.Fatal(out)
	}
}

func TestCorruptAdjustmentDataRejected(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "50", "补收")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 调整引用了不存在的账单：按损坏处理，原文件不被改写。
	broken := strings.Replace(string(good), `"month": "2026-09"`, `"month": "2026-11"`, 1)
	if broken == string(good) {
		t.Fatal("替换未生效")
	}
	// 只改调整记录里的月份（第一处出现在账单里，需改调整那处）。
	idx := strings.LastIndex(string(good), `"month": "2026-09"`)
	broken = string(good)[:idx] + `"month": "2026-11"` + string(good)[idx+len(`"month": "2026-09"`):]
	if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.runExpectErr("bill", "show", "c1", "2026-09")
	if !strings.Contains(msg, "损坏") {
		t.Fatal(msg)
	}
	got, _ := os.ReadFile(h.statePath())
	if string(got) != broken {
		t.Fatal("损坏文件被改写")
	}
}

// --- 收款登记与撤销 ---

func TestPayHappyPathAndShow(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "150", "6") // 总金额 900

	out := h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "500", "首期回款")
	if !strings.Contains(out, "已登记收款") || !strings.Contains(out, "实收 500 分") ||
		!strings.Contains(out, "未收余额 400 分") {
		t.Fatal(out)
	}
	// 允许分笔收款。
	out = h.mustRun("bill", "pay", "c1", "2026-09", "pay-2", "400", "尾款")
	if !strings.Contains(out, "实收 900 分") || !strings.Contains(out, "未收余额 0 分") {
		t.Fatal(out)
	}

	shown := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"总金额：900 分", "当前应付：900 分", "实收：900 分", "未收余额：0 分",
		"收款 pay-1：500 分", "首期回款", "收款 pay-2：400 分", "尾款", "已收",
	} {
		if !strings.Contains(shown, want) {
			t.Fatalf("账单展示缺少 %q:\n%s", want, shown)
		}
	}
	// 历史按成功操作顺序：pay-1 在 pay-2 前。
	i1 := strings.Index(shown, "收款 pay-1")
	i2 := strings.Index(shown, "收款 pay-2")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("收款历史顺序错误:\n%s", shown)
	}

	// 重复结算返回原账单与收款历史，不产生新收款。
	again := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(again, "幂等") || !strings.Contains(again, "实收：900 分") ||
		!strings.Contains(again, "收款 pay-1") {
		t.Fatal(again)
	}
}

func TestPayValidation(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1") // 总金额 100

	// 客户不存在 / 账单不存在（未结算月份）。
	h.runExpectErr("bill", "pay", "ghost", "2026-09", "p1", "50", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-10", "p1", "50", "备注")
	// 月份格式非法。
	h.runExpectErr("bill", "pay", "c1", "2026-9", "p1", "50", "备注")
	// 收款标识为空、备注为空。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "", "50", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "50", "")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "50", "   ")
	// 金额：零、负数、非整数、超范围。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "0", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "-50", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "1.5", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "abc", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "99999999999999999999999", "备注")

	// 以上全部失败，标识 p1 不应被占用：首次成功登记照常进行。
	out := h.mustRun("bill", "pay", "c1", "2026-09", "p1", "50", "首次生效")
	if !strings.Contains(out, "实收 50 分") {
		t.Fatal(out)
	}
}

func TestPayIdempotentAndConflict(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1") // 总金额 100
	settleBill(h, "c2", "100", "1")

	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "60", "首期")

	// 内容完全相同的重放：返回已保存记录，不重复计入实收。
	out := h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "60", "首期")
	if !strings.Contains(out, "不重复计入实收") || !strings.Contains(out, "实收：60 分") {
		t.Fatal(out)
	}

	// 任一字段不同均拒绝：金额、备注、跨客户、跨月份。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "pay-1", "61", "首期")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "pay-1", "60", "另一个备注")
	h.runExpectErr("bill", "pay", "c2", "2026-09", "pay-1", "60", "首期")
	h.mustRun("usage", "import", h.writeFile("oct.csv", csvHeader+"u-oct,c1,2026-10-01T00:00:00Z,1\n"))
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.runExpectErr("bill", "pay", "c1", "2026-10", "pay-1", "60", "首期")

	// 收款与调整标识互不占用（两个方向）。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "10", "补收")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "adj-1", "10", "备注")
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "pay-1", "10", "原因")

	// 冲突拒绝后实收不变。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "实收：60 分") || !strings.Contains(shown, "当前应付：110 分") {
		t.Fatal(shown)
	}
}

func TestPayExceedsOutstandingRejected(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1") // 总金额 100

	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "60", "首期")
	// 超过未收余额 40：拒绝。
	msg := h.runExpectErr("bill", "pay", "c1", "2026-09", "p2", "50", "超额")
	if !strings.Contains(msg, "超过未收余额") {
		t.Fatal(msg)
	}
	// 恰好收足未收余额：成功。
	h.mustRun("bill", "pay", "c1", "2026-09", "p2", "40", "尾款")
	// 已全额收款后任何正额收款都被拒。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p3", "1", "多余")

	// 零应付账单不能登记正额收款。
	settleBill(h, "c2", "100", "1") // 总金额 100
	h.mustRun("bill", "adjust", "c2", "2026-09", "zero", "-100", "全额减免")
	shown := h.mustRun("bill", "show", "c2", "2026-09")
	if !strings.Contains(shown, "当前应付：0 分") {
		t.Fatal(shown)
	}
	h.runExpectErr("bill", "pay", "c2", "2026-09", "pz", "1", "零应付")
}

func TestUnpayFlow(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1") // 总金额 100
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "60", "首期")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-2", "40", "尾款")

	out := h.mustRun("bill", "unpay", "pay-1", "重复录入")
	if !strings.Contains(out, "已撤销收款") || !strings.Contains(out, "实收：40 分") {
		t.Fatal(out)
	}

	// 相同标识与相同原因重复撤销：幂等成功，不新增历史。
	out = h.mustRun("bill", "unpay", "pay-1", "重复录入")
	if !strings.Contains(out, "幂等") {
		t.Fatal(out)
	}
	// 改用其他原因：拒绝；目标不存在、原因为空：拒绝。
	h.runExpectErr("bill", "unpay", "pay-1", "另一个原因")
	h.runExpectErr("bill", "unpay", "ghost", "原因")
	h.runExpectErr("bill", "unpay", "pay-2", "")

	// 撤销后重放原收款：返回已撤销状态，不恢复实收。
	out = h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "60", "首期")
	if !strings.Contains(out, "已撤销") || !strings.Contains(out, "实收：40 分") {
		t.Fatal(out)
	}

	// 撤销只取消该笔实收：应付、其他收款不变；历史含撤销原因与关联。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"当前应付：100 分", "实收：40 分", "未收余额：60 分",
		"收款 pay-1：60 分", "当前状态：已撤销", "撤销收款 pay-1：原因：重复录入",
		"收款 pay-2：40 分", "已收",
	} {
		if !strings.Contains(shown, want) {
			t.Fatalf("账单展示缺少 %q:\n%s", want, shown)
		}
	}
	// 历史顺序：收款 pay-1、收款 pay-2、撤销收款 pay-1。
	i1 := strings.Index(shown, "收款 pay-1：")
	i2 := strings.Index(shown, "收款 pay-2：")
	i3 := strings.Index(shown, "撤销收款 pay-1")
	if i1 < 0 || i2 < 0 || i3 < 0 || !(i1 < i2 && i2 < i3) {
		t.Fatalf("收款历史顺序错误:\n%s", shown)
	}
}

func TestAdjustBlockedWhenPayableBelowReceived(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1") // 总金额 100
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "100", "全额回款")

	// 新增调整使应付低于实收：拒绝并保持原状态。
	msg := h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "-1", "减免")
	if !strings.Contains(msg, "低于实收") {
		t.Fatal(msg)
	}
	// 撤销调整使应付低于实收同样被拒：先补收再全额收款，然后撤销补收。
	h.mustRun("bill", "adjust", "c1", "2026-09", "a2", "50", "补收") // 应付 150
	h.mustRun("bill", "pay", "c1", "2026-09", "p2", "50", "补收回款")
	msg = h.runExpectErr("bill", "revoke", "a2", "想撤销")
	if !strings.Contains(msg, "低于实收") || !strings.Contains(msg, "仍有效") {
		t.Fatal(msg)
	}
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：150 分") || !strings.Contains(shown, "实收：150 分") {
		t.Fatal(shown)
	}

	// 先撤销误登记的收款，再重试符合约束的调整即可成功。
	h.mustRun("bill", "unpay", "p2", "误登记")
	out := h.mustRun("bill", "revoke", "a2", "想撤销")
	if !strings.Contains(out, "当前应付 100 分") {
		t.Fatal(out)
	}
	// 实收仍为 100（p1），减免 1 分使应付 99 低于实收：仍拒绝。
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "-1", "减免")
	// 撤销误登记的全额回款后即可调整。
	h.mustRun("bill", "unpay", "p1", "误登记")
	out = h.mustRun("bill", "adjust", "c1", "2026-09", "a1", "-1", "减免")
	if !strings.Contains(out, "当前应付 99 分") {
		t.Fatal(out)
	}
}

func TestPayPersistsAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "2") // 总金额 200
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "120", "首期")
	h.mustRun("bill", "unpay", "pay-1", "误登记")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-2", "50", "重收")

	// 全新 harness 指向同一目录：余额、顺序、幂等与撤销约束保持。
	h2 := &harness{t: t, dir: h.dir}
	prev := stdout
	stdout = &h2.buf
	defer func() { stdout = prev }()

	out, err := h2.run("bill", "show", "c1", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "实收：50 分") || !strings.Contains(out, "未收余额：150 分") ||
		!strings.Contains(out, "已撤销") {
		t.Fatal(out)
	}
	i1 := strings.Index(out, "收款 pay-1：")
	i2 := strings.Index(out, "撤销收款 pay-1")
	i3 := strings.Index(out, "收款 pay-2：")
	if i1 < 0 || i2 < 0 || i3 < 0 || !(i1 < i2 && i2 < i3) {
		t.Fatalf("重启后收款历史顺序错误:\n%s", out)
	}
	// 重放已撤销收款仍返回已撤销状态，不恢复实收。
	out, err = h2.run("bill", "pay", "c1", "2026-09", "pay-1", "120", "首期")
	if err != nil || !strings.Contains(out, "已撤销") || !strings.Contains(out, "实收：50 分") {
		t.Fatalf("out=%s err=%v", out, err)
	}
	// 换内容复用标识仍拒绝。
	if _, err = h2.run("bill", "pay", "c1", "2026-09", "pay-1", "121", "首期"); err == nil {
		t.Fatal("应拒绝标识冲突")
	}
	// 相同原因重复撤销幂等。
	if _, err = h2.run("bill", "unpay", "pay-1", "误登记"); err != nil {
		t.Fatal(err)
	}
	// 实收约束重启后仍生效：未收余额 150，收 151 被拒。
	if _, err = h2.run("bill", "pay", "c1", "2026-09", "pay-3", "151", "超额"); err == nil {
		t.Fatal("应拒绝超过未收余额的收款")
	}
	if _, err = h2.run("bill", "pay", "c1", "2026-09", "pay-3", "150", "结清"); err != nil {
		t.Fatal(err)
	}
}

func TestOldStateFileWithoutPayments(t *testing.T) {
	h := newHarness(t)
	// 手工构造无 payments/adjustments/next_seq 字段的旧版数据文件。
	legacy := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2}},
  "bills": {"c1|2026-09": {
    "id": "BILL-legacy", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 2, "unit_price_fen": 100, "total_fee_fen": 200,
    "lines": [{"usage_id": "u1", "time": "2026-09-15T10:00:00Z", "quantity": 2, "line_fee_fen": 200}],
    "created_at": "2026-10-01T00:00:00Z"
  }}
}`
	if err := os.WriteFile(h.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	// 旧账单视为零实收。
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "实收：0 分") || !strings.Contains(out, "未收余额：200 分") ||
		!strings.Contains(out, "收款与撤销历史：无") {
		t.Fatal(out)
	}
	// 旧数据上可直接登记收款。
	out = h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "200", "一次性结清")
	if !strings.Contains(out, "实收 200 分") || !strings.Contains(out, "未收余额 0 分") {
		t.Fatal(out)
	}
}

func TestCorruptPaymentDataRejected(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "50", "首期")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 收款引用了不存在的账单：按损坏处理，原文件不被改写。
	idx := strings.LastIndex(string(good), `"month": "2026-09"`)
	broken := string(good)[:idx] + `"month": "2026-11"` + string(good)[idx+len(`"month": "2026-09"`):]
	if broken == string(good) {
		t.Fatal("替换未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.runExpectErr("bill", "show", "c1", "2026-09")
	if !strings.Contains(msg, "损坏") {
		t.Fatal(msg)
	}
	got, _ := os.ReadFile(h.statePath())
	if string(got) != broken {
		t.Fatal("损坏文件被改写")
	}

	// 实收超过当前应付的金额状态不一致：同样按损坏处理。
	var inconsistent string
	if idx := strings.LastIndex(string(good), `"amount_fen": 50`); idx >= 0 {
		inconsistent = string(good)[:idx] + `"amount_fen": 150` + string(good)[idx+len(`"amount_fen": 50`):]
	}
	if inconsistent == "" || inconsistent == string(good) {
		t.Fatal("替换未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(inconsistent), 0o644); err != nil {
		t.Fatal(err)
	}
	msg = h.runExpectErr("bill", "show", "c1", "2026-09")
	if !strings.Contains(msg, "损坏") {
		t.Fatal(msg)
	}
	got, _ = os.ReadFile(h.statePath())
	if string(got) != inconsistent {
		t.Fatal("损坏文件被改写")
	}
}
