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

	out := h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "300", "银行转账")
	if !strings.Contains(out, "已登记收款") ||
		!strings.Contains(out, "实收 300 分") || !strings.Contains(out, "未收余额 600 分") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "pay", "c1", "2026-09", "pay-2", "600", "现金尾款")
	if !strings.Contains(out, "实收 900 分") || !strings.Contains(out, "未收余额 0 分") {
		t.Fatal(out)
	}

	shown := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"总金额：900 分", "当前应付：900 分", "实收：900 分", "未收余额：0 分",
		"收款 pay-1：总额 300 分", "银行转账", "收款 pay-2：总额 600 分", "现金尾款",
		"本账单分配 300 分", "本账单分配 600 分", "实收中",
	} {
		if !strings.Contains(shown, want) {
			t.Fatalf("账单展示缺少 %q:\n%s", want, shown)
		}
	}
	// 原账单字段保留。
	if !strings.Contains(shown, "总数量：6") || !strings.Contains(shown, "单价：150 分") {
		t.Fatal(shown)
	}

	// 重复结算不产生收款，返回含收款历史的原账单。
	again := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(again, "幂等") || !strings.Contains(again, "实收：900 分") ||
		!strings.Contains(again, "收款 pay-1") {
		t.Fatal(again)
	}
}

func TestPayValidation(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1")

	// 客户不存在 / 账单不存在 / 月份非法。
	h.runExpectErr("bill", "pay", "ghost", "2026-09", "p1", "10", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-10", "p1", "10", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-9", "p1", "10", "备注")
	// 收款标识为空、备注为空。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "", "10", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "10", "")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "10", "   ")
	// 金额：零、负数、小数、非数字、超范围都必须拒绝。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "0", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "-1", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "1.5", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "abc", "备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "99999999999999999999999", "备注")

	// 全部失败后 p1 未被占用：首次成功登记照常进行。
	out := h.mustRun("bill", "pay", "c1", "2026-09", "p1", "100", "首次收款")
	if !strings.Contains(out, "实收 100 分") {
		t.Fatal(out)
	}
}

func TestPayIdempotentAndConflict(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "10") // 总金额 1000
	settleBill(h, "c2", "100", "10")

	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "500", "第一次备注")

	// 内容完全相同的重放：返回原记录，不再次计入实收。
	out := h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "500", "第一次备注")
	if !strings.Contains(out, "不重复计入实收") || !strings.Contains(out, "实收合计 500 分") {
		t.Fatal(out)
	}

	// 任一字段不同均拒绝：金额、备注、跨客户、跨月份。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "pay-1", "501", "第一次备注")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "pay-1", "500", "另一个备注")
	h.runExpectErr("bill", "pay", "c2", "2026-09", "pay-1", "500", "第一次备注")
	h.mustRun("usage", "import", h.writeFile("oct.csv", csvHeader+"u-oct,c1,2026-10-01T00:00:00Z,1\n"))
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.runExpectErr("bill", "pay", "c1", "2026-10", "pay-1", "500", "第一次备注")

	// 冲突拒绝后实收不变。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "实收：500 分") {
		t.Fatal(shown)
	}

	// 撤销后以相同内容重放：返回已撤销状态，不恢复实收。
	h.mustRun("bill", "unpay", "pay-1", "录错了")
	out = h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "500", "第一次备注")
	if !strings.Contains(out, "已撤销") || !strings.Contains(out, "实收合计 0 分") {
		t.Fatal(out)
	}
	// 撤销后换内容重放仍拒绝。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "pay-1", "501", "第一次备注")
}

func TestPaymentAndAdjustmentSameNameAllowed(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "10") // 总金额 1000

	// 收款与调整允许同名：各自判重与撤销，互不影响。
	h.mustRun("bill", "pay", "c1", "2026-09", "same-id", "100", "收款")
	out := h.mustRun("bill", "adjust", "c1", "2026-09", "same-id", "10", "补收")
	if !strings.Contains(out, "当前应付 1010 分") {
		t.Fatal(out)
	}
	// 各自按自身内容判重：相同重放幂等，内容不同拒绝。
	h.mustRun("bill", "pay", "c1", "2026-09", "same-id", "100", "收款")
	h.mustRun("bill", "adjust", "c1", "2026-09", "same-id", "10", "补收")
	h.runExpectErr("bill", "pay", "c1", "2026-09", "same-id", "101", "收款")
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "same-id", "11", "补收")

	// 各自撤销：撤销调整不影响收款，撤销收款不影响调整。
	h.mustRun("bill", "revoke", "same-id", "撤销调整")
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：1000 分") || !strings.Contains(shown, "实收：100 分") {
		t.Fatal(shown)
	}
	h.mustRun("bill", "unpay", "same-id", "撤销收款")
	shown = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "实收：0 分") || !strings.Contains(shown, "当前应付：1000 分") {
		t.Fatal(shown)
	}

	// 撤销不存在的目标仍拒绝。
	h.runExpectErr("bill", "unpay", "ghost", "原因")
	h.runExpectErr("bill", "revoke", "ghost", "原因")
}

func TestPayExceedsOutstandingRejected(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "10") // 总金额 1000

	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "600", "第一笔")

	// 超过未收余额 400：拒绝，且 p2 不被占用（改小后同标识可成功）。
	msg := h.runExpectErr("bill", "pay", "c1", "2026-09", "p2", "401", "超额")
	if !strings.Contains(msg, "超过未收余额") {
		t.Fatal(msg)
	}
	h.mustRun("bill", "pay", "c1", "2026-09", "p2", "400", "补齐")

	// 已收清后任何正额收款都拒绝。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p3", "1", "多付")

	// 撤销一笔后余额恢复，可用新标识补登。
	h.mustRun("bill", "unpay", "p1", "撤销第一笔")
	out := h.mustRun("bill", "pay", "c1", "2026-09", "p4", "600", "重新收款")
	if !strings.Contains(out, "实收 1000 分") || !strings.Contains(out, "未收余额 0 分") {
		t.Fatal(out)
	}
}

func TestZeroPayableBillRejectsPositivePayment(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1") // 总金额 100
	h.mustRun("bill", "adjust", "c1", "2026-09", "full", "-100", "全额减免")

	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：0 分") {
		t.Fatal(shown)
	}
	msg := h.runExpectErr("bill", "pay", "c1", "2026-09", "zp", "1", "想付款")
	if !strings.Contains(msg, "当前应付为 0 分") {
		t.Fatal(msg)
	}

	// 补收使应付恢复为正后可以登记。
	out := h.mustRun("bill", "adjust", "c1", "2026-09", "back", "100", "重新补收")
	if !strings.Contains(out, "当前应付 100 分") {
		t.Fatal(out)
	}
	h.mustRun("bill", "pay", "c1", "2026-09", "zp", "100", "现在能付了")
}

func TestUnpayFlowAndHistoryOrder(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "10")                                   // 总金额 1000
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "200", "补收") // 应付 1200
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "500", "首笔")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-2", "300", "次笔")

	out := h.mustRun("bill", "unpay", "pay-1", "账号录错")
	if !strings.Contains(out, "已撤销收款") ||
		!strings.Contains(out, "实收 300 分") || !strings.Contains(out, "未收余额 900 分") {
		t.Fatal(out)
	}

	// 撤销不改变应付与其他收款。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：1200 分") ||
		!strings.Contains(shown, "实收：300 分") || !strings.Contains(shown, "未收余额：900 分") {
		t.Fatal(shown)
	}

	// 相同原因重复撤销幂等成功，不新增历史；不同原因拒绝。
	out = h.mustRun("bill", "unpay", "pay-1", "账号录错")
	if !strings.Contains(out, "幂等") {
		t.Fatal(out)
	}
	h.runExpectErr("bill", "unpay", "pay-1", "另一个原因")
	// 目标不存在、原因为空。
	h.runExpectErr("bill", "unpay", "ghost", "原因")
	h.runExpectErr("bill", "unpay", "pay-2", "")

	// 收款历史按成功操作顺序：登记 pay-1、登记 pay-2、撤销 pay-1。
	i1 := strings.Index(shown, "收款 pay-1：")
	i2 := strings.Index(shown, "收款 pay-2：")
	i3 := strings.Index(shown, "撤销收款 pay-1：")
	if i1 < 0 || i2 < 0 || i3 < 0 || !(i1 < i2 && i2 < i3) {
		t.Fatalf("收款历史顺序错误:\n%s", shown)
	}
	if !strings.Contains(shown, "当前状态：已撤销") ||
		!strings.Contains(shown, "撤销收款 pay-1：原因：账号录错") ||
		!strings.Contains(shown, "关联收款 pay-1，总额 500 分，本账单分配 500 分") {
		t.Fatal(shown)
	}
}

func TestAdjustmentRejectedWhenPayableWouldBeBelowReceived(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "10")                                // 总金额 1000
	h.mustRun("bill", "adjust", "c1", "2026-09", "up", "200", "补收") // 应付 1200
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "1200", "收清")

	// 减免 10 会使应付 1190 < 实收 1200：拒绝且状态不变，up2 不被占用。
	msg := h.runExpectErr("bill", "adjust", "c1", "2026-09", "up2", "-10", "减免")
	if !strings.Contains(msg, "低于实收") {
		t.Fatal(msg)
	}
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：1200 分") || !strings.Contains(shown, "实收：1200 分") {
		t.Fatal(shown)
	}

	// 先撤销误登记的部分收款，再重试调整成功。
	h.mustRun("bill", "unpay", "p1", "录多了")
	out := h.mustRun("bill", "adjust", "c1", "2026-09", "up2", "-10", "减免")
	if !strings.Contains(out, "当前应付 1190 分") {
		t.Fatal(out)
	}

	// 撤销补收调整同样受实收约束：先恢复一笔收款使撤销不成立，再验证拒绝。
	h.mustRun("bill", "pay", "c1", "2026-09", "p2", "1190", "再次收清")
	msg = h.runExpectErr("bill", "revoke", "up", "想撤销补收")
	if !strings.Contains(msg, "低于实收") {
		t.Fatal(msg)
	}
	// 撤销后应付 990 < 实收 1190：先撤销收款再重试成功。
	h.mustRun("bill", "unpay", "p2", "先腾退")
	out = h.mustRun("bill", "revoke", "up", "想撤销补收")
	if !strings.Contains(out, "当前应付 990 分") {
		t.Fatal(out)
	}
}

func TestPaymentPersistsAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "10") // 总金额 1000
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "300", "首笔")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-2", "200", "次笔")
	h.mustRun("bill", "unpay", "pay-1", "录错")

	// 全新 harness 指向同一目录：余额、顺序、幂等与撤销约束保持。
	h2 := &harness{t: t, dir: h.dir}
	prev := stdout
	stdout = &h2.buf
	defer func() { stdout = prev }()

	out, err := h2.run("bill", "show", "c1", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "当前应付：1000 分") ||
		!strings.Contains(out, "实收：200 分") || !strings.Contains(out, "未收余额：800 分") {
		t.Fatal(out)
	}
	// 已撤销收款重放仍返回已撤销状态，不恢复实收。
	out, err = h2.run("bill", "pay", "c1", "2026-09", "pay-1", "300", "首笔")
	if err != nil || !strings.Contains(out, "已撤销") || !strings.Contains(out, "实收合计 200 分") {
		t.Fatalf("out=%s err=%v", out, err)
	}
	// 相同原因重复撤销幂等，不同原因拒绝。
	if _, err = h2.run("bill", "unpay", "pay-1", "录错"); err != nil {
		t.Fatal(err)
	}
	if _, err = h2.run("bill", "unpay", "pay-1", "别的原因"); err == nil {
		t.Fatal("应拒绝不同原因的重复撤销")
	}
	// 超额收款在重启后仍被拒绝。
	if _, err = h2.run("bill", "pay", "c1", "2026-09", "pay-3", "801", "超额"); err == nil {
		t.Fatal("应拒绝超过未收余额的收款")
	}
	// 历史顺序保持：pay-1、pay-2、撤销 pay-1。
	shown, err := h2.run("bill", "show", "c1", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	j1 := strings.Index(shown, "收款 pay-1：")
	j2 := strings.Index(shown, "收款 pay-2：")
	j3 := strings.Index(shown, "撤销收款 pay-1：")
	if j1 < 0 || j2 < 0 || j3 < 0 || !(j1 < j2 && j2 < j3) {
		t.Fatalf("重启后历史顺序错误:\n%s", shown)
	}
}

func TestOldStateFileWithoutPayments(t *testing.T) {
	h := newHarness(t)
	// 手工构造无 adjustments/payments/next_seq 字段的旧版数据文件。
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

	// 旧账单缺少收款记录：视为零实收。
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "实收：0 分") || !strings.Contains(out, "未收余额：200 分") ||
		!strings.Contains(out, "收款与撤销历史：无") {
		t.Fatal(out)
	}
	// 旧数据上可直接登记收款。
	out = h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "150", "补登")
	if !strings.Contains(out, "实收 150 分") || !strings.Contains(out, "未收余额 50 分") {
		t.Fatal(out)
	}
}

func TestCorruptPaymentDataRejected(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1") // 总金额 100
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "50", "备注")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 把分配金额改到与总额不一致（且超过当前应付）：按损坏处理，原文件不改写。
	broken := strings.Replace(string(good), `"amount_fen": 50`, `"amount_fen": 500`, 1)
	if broken == string(good) {
		t.Fatal("替换未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"bill", "show", "c1", "2026-09"},
		{"bill", "pay", "c1", "2026-09", "p2", "1", "备注"},
		{"bill", "unpay", "pay-1", "原因"},
	} {
		msg := h.runExpectErr(args...)
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("args=%v 未报损坏: %s", args, msg)
		}
	}
	got, _ := os.ReadFile(h.statePath())
	if string(got) != broken {
		t.Fatal("损坏文件被改写")
	}

	// 收款引用不存在的账单同样按损坏处理（收款月份在文件中最后出现）。
	idx := strings.LastIndex(string(good), `"month": "2026-09"`)
	dangling := string(good)[:idx] + `"month": "2026-11"` + string(good)[idx+len(`"month": "2026-09"`):]
	if err := os.WriteFile(h.statePath(), []byte(dangling), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.runExpectErr("bill", "show", "c1", "2026-09")
	if !strings.Contains(msg, "损坏") {
		t.Fatal(msg)
	}
	got, _ = os.ReadFile(h.statePath())
	if string(got) != dangling {
		t.Fatal("损坏文件被改写")
	}
}

// --- 汇款分配到同一客户多个已结算月份 ---

// settleTwoMonths 是测试辅助：登记客户并结算 2026-09 与 2026-10 两张账单。
func settleTwoMonths(h *harness, customerID, price, qtySep, qtyOct string) {
	h.t.Helper()
	h.mustRun("customer", "add", customerID, "客户"+customerID, price)
	f := h.writeFile("u2m-"+customerID+".csv", csvHeader+
		"u-"+customerID+"-09,"+customerID+",2026-09-15T10:00:00Z,"+qtySep+"\n"+
		"u-"+customerID+"-10,"+customerID+",2026-10-15T10:00:00Z,"+qtyOct+"\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", customerID, "2026-09")
	h.mustRun("bill", "settle", customerID, "2026-10")
}

func TestRemitHappyPathAcrossMonths(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3") // 9月 500，10月 300

	out := h.mustRun("bill", "remit", "c1", "remit-1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	for _, want := range []string{
		"已登记收款", "总额 700 分", "2026-09", "2026-10",
		"分配 400 分", "分配 300 分", "未收余额 100 分", "未收余额 0 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("登记输出缺少 %q:\n%s", want, out)
		}
	}

	// 各月账单分别计入各自分配，原账单与应付不变。
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"总金额：500 分", "当前应付：500 分", "实收：400 分", "未收余额：100 分",
		"收款 remit-1：总额 700 分", "本账单分配 400 分", "季度汇款", "实收中",
	} {
		if !strings.Contains(sep, want) {
			t.Fatalf("9 月账单缺少 %q:\n%s", want, sep)
		}
	}
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "实收：300 分") || !strings.Contains(oct, "未收余额：0 分") ||
		!strings.Contains(oct, "本账单分配 300 分") {
		t.Fatal(oct)
	}

	// 重复结算不产生收款，返回含收款历史的原账单。
	again := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(again, "幂等") || !strings.Contains(again, "收款 remit-1") ||
		!strings.Contains(again, "实收：400 分") {
		t.Fatal(again)
	}
}

func TestRemitValidation(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")

	// 总额：零、负、小数、非数字、超范围。
	h.runExpectErr("bill", "remit", "c1", "r1", "0", "备注", "2026-09:100")
	h.runExpectErr("bill", "remit", "c1", "r1", "-5", "备注", "2026-09:100")
	h.runExpectErr("bill", "remit", "c1", "r1", "1.5", "备注", "2026-09:100")
	h.runExpectErr("bill", "remit", "c1", "r1", "abc", "备注", "2026-09:100")
	h.runExpectErr("bill", "remit", "c1", "r1", "99999999999999999999999", "备注", "2026-09:100")
	// 收款标识为空、备注为空。
	h.runExpectErr("bill", "remit", "c1", "", "100", "备注", "2026-09:100")
	h.runExpectErr("bill", "remit", "c1", "r1", "100", "", "2026-09:100")
	h.runExpectErr("bill", "remit", "c1", "r1", "100", "   ", "2026-09:100")
	// 分配项：格式非法、月份非法、金额非正、月份重复。
	h.runExpectErr("bill", "remit", "c1", "r1", "100", "备注", "2026-09")
	h.runExpectErr("bill", "remit", "c1", "r1", "100", "备注", "2026-09:100:1")
	h.runExpectErr("bill", "remit", "c1", "r1", "100", "备注", "2026-9:100")
	h.runExpectErr("bill", "remit", "c1", "r1", "100", "备注", "2026-13:100")
	h.runExpectErr("bill", "remit", "c1", "r1", "100", "备注", "2026-09:0")
	h.runExpectErr("bill", "remit", "c1", "r1", "100", "备注", "2026-09:-1")
	h.runExpectErr("bill", "remit", "c1", "r1", "100", "备注", "2026-09:abc")
	h.runExpectErr("bill", "remit", "c1", "r1", "200", "备注", "2026-09:100", "2026-09:100")
	// 分配合计不等于总额。
	h.runExpectErr("bill", "remit", "c1", "r1", "150", "备注", "2026-09:100", "2026-10:100")
	h.runExpectErr("bill", "remit", "c1", "r1", "250", "备注", "2026-09:100", "2026-10:100")
	// 客户不存在、分配月份无账单。
	h.runExpectErr("bill", "remit", "ghost", "r1", "100", "备注", "2026-09:100")
	h.runExpectErr("bill", "remit", "c1", "r1", "100", "备注", "2026-11:100")

	// 全部失败后 r1 未被占用：首次合法登记照常成功。
	out := h.mustRun("bill", "remit", "c1", "r1", "100", "首次", "2026-09:100")
	if !strings.Contains(out, "已登记收款") {
		t.Fatal(out)
	}
}

func TestRemitAllocationSumOverflow(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	// 两项分配各自合法但合计溢出有符号 64 位整数：拒绝。
	h.runExpectErr("bill", "remit", "c1", "r1", "9223372036854775807", "备注",
		"2026-09:9223372036854775806", "2026-10:2")
}

func TestRemitExceedsOutstandingRejectsWhole(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3") // 9月 500，10月 300
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "400", "先收")

	// 9 月未收 100，分配 200 超额：整笔拒绝，10 月分配也不生效，标识不占用。
	msg := h.runExpectErr("bill", "remit", "c1", "r1", "300", "汇款", "2026-09:200", "2026-10:100")
	if !strings.Contains(msg, "超过未收余额") {
		t.Fatal(msg)
	}
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：400 分") {
		t.Fatal(sep)
	}
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "实收：0 分") || !strings.Contains(oct, "收款与撤销历史：无") {
		t.Fatal(oct)
	}
	// 缩小分配后同标识可成功（失败新增不占标识）。
	h.mustRun("bill", "remit", "c1", "r1", "200", "汇款", "2026-09:100", "2026-10:100")
}

func TestRemitIdempotentAcrossEntriesAndOrder(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")

	h.mustRun("bill", "remit", "c1", "r1", "700", "汇款", "2026-09:400", "2026-10:300")

	// 分配顺序不同的相同内容重放：返回原记录，不重复计入实收。
	out := h.mustRun("bill", "remit", "c1", "r1", "700", "汇款", "2026-10:300", "2026-09:400")
	if !strings.Contains(out, "不重复计入实收") {
		t.Fatal(out)
	}
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：400 分") {
		t.Fatal(sep)
	}

	// 内容不同均拒绝：总额、备注、分配金额、月份组合、跨客户。
	h.runExpectErr("bill", "remit", "c1", "r1", "701", "汇款", "2026-09:400", "2026-10:301")
	h.runExpectErr("bill", "remit", "c1", "r1", "700", "另一个备注", "2026-09:400", "2026-10:300")
	h.runExpectErr("bill", "remit", "c1", "r1", "700", "汇款", "2026-09:300", "2026-10:400")
	h.runExpectErr("bill", "remit", "c1", "r1", "700", "汇款", "2026-09:700")
	settleTwoMonths(h, "c2", "100", "5", "3")
	h.runExpectErr("bill", "remit", "c2", "r1", "700", "汇款", "2026-09:400", "2026-10:300")

	// 单账单收款即一项分配：bill pay 与 bill remit 的相同内容互通判重。
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "100", "尾款")
	out = h.mustRun("bill", "remit", "c1", "p1", "100", "尾款", "2026-09:100")
	if !strings.Contains(out, "不重复计入实收") {
		t.Fatal(out)
	}
	// 同标识同总额但分配月份不同：拒绝。
	h.runExpectErr("bill", "remit", "c1", "p1", "100", "尾款", "2026-10:100")
	// bill pay 重放 remit 登记的多月汇款：月份组合不同，拒绝。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "r1", "700", "汇款")
}

func TestRemitReplayUnaffectedByBalanceChange(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "200", "汇款", "2026-09:100", "2026-10:100")

	// 之后余额变化：两个月份都被后续收款收清。
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "400", "收尾")
	h.mustRun("bill", "pay", "c1", "2026-10", "p2", "200", "收尾")

	// 按当前余额该汇款已无法登记（未收余额为 0），但相同重放仍返回原记录。
	out := h.mustRun("bill", "remit", "c1", "r1", "200", "汇款", "2026-09:100", "2026-10:100")
	if !strings.Contains(out, "不重复计入实收") || !strings.Contains(out, "实收合计 500 分") {
		t.Fatal(out)
	}
}

func TestRemitUnpayRevokesAllAllocations(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "汇款", "2026-09:400", "2026-10:300")

	out := h.mustRun("bill", "unpay", "r1", "汇错账户")
	if !strings.Contains(out, "已撤销收款") || !strings.Contains(out, "总额 700 分") {
		t.Fatal(out)
	}
	// 整笔撤销：两个月份的实收都取消，应付不变，原记录与原因保留。
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：0 分") || !strings.Contains(sep, "当前应付：500 分") ||
		!strings.Contains(sep, "撤销收款 r1：原因：汇错账户") ||
		!strings.Contains(sep, "关联收款 r1，总额 700 分，本账单分配 400 分") {
		t.Fatal(sep)
	}
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "实收：0 分") || !strings.Contains(oct, "当前状态：已撤销") {
		t.Fatal(oct)
	}
	// 历史按成功操作顺序：登记 r1、撤销 r1。
	i1 := strings.Index(sep, "收款 r1：")
	i2 := strings.Index(sep, "撤销收款 r1：")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("历史顺序错误:\n%s", sep)
	}

	// 相同原因重复撤销幂等成功、不增历史；不同原因拒绝。
	out = h.mustRun("bill", "unpay", "r1", "汇错账户")
	if !strings.Contains(out, "幂等") {
		t.Fatal(out)
	}
	h.runExpectErr("bill", "unpay", "r1", "另一个原因")

	// 撤销后重放不恢复实收，仍返回已撤销状态。
	out = h.mustRun("bill", "remit", "c1", "r1", "700", "汇款", "2026-09:400", "2026-10:300")
	if !strings.Contains(out, "已撤销") {
		t.Fatal(out)
	}
	sep = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：0 分") {
		t.Fatal(sep)
	}
}

func TestRemitAdjustCountsAllocationsAsReceived(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3") // 9月 500，10月 300
	h.mustRun("bill", "remit", "c1", "r1", "700", "汇款", "2026-09:400", "2026-10:300")

	// 9 月减免使应付 350 < 实收 400：拒绝；10 月同样受各自分配约束。
	msg := h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "-150", "减免")
	if !strings.Contains(msg, "低于实收") {
		t.Fatal(msg)
	}
	msg = h.runExpectErr("bill", "adjust", "c1", "2026-10", "a2", "-1", "减免")
	if !strings.Contains(msg, "低于实收") {
		t.Fatal(msg)
	}
	// 9 月小幅减免（应付 450 ≥ 实收 400）合法。
	h.mustRun("bill", "adjust", "c1", "2026-09", "a3", "-50", "减免")
	// 撤销该补收类调整的逆操作同样受限：补收后撤销会低于实收时拒绝。
	h.mustRun("bill", "adjust", "c1", "2026-10", "a4", "50", "补收") // 10月应付 350
	h.mustRun("bill", "pay", "c1", "2026-10", "p1", "50", "补尾款")   // 10月实收 350
	msg = h.runExpectErr("bill", "revoke", "a4", "想撤销")
	if !strings.Contains(msg, "低于实收") {
		t.Fatal(msg)
	}
	// 撤销汇款后调整可行。
	h.mustRun("bill", "unpay", "r1", "重新汇款")
	out := h.mustRun("bill", "adjust", "c1", "2026-09", "a1", "-150", "减免")
	if !strings.Contains(out, "当前应付 300 分") {
		t.Fatal(out)
	}
}

func TestRemitPersistsAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "unpay", "r1", "撤销重汇")

	// 全新 harness 指向同一目录：余额、历史、顺序与幂等保持。
	h2 := &harness{t: t, dir: h.dir}
	prev := stdout
	stdout = &h2.buf
	defer func() { stdout = prev }()

	out, err := h2.run("bill", "show", "c1", "2026-09")
	if err != nil || !strings.Contains(out, "实收：0 分") ||
		!strings.Contains(out, "本账单分配 400 分") || !strings.Contains(out, "已撤销") {
		t.Fatalf("out=%s err=%v", out, err)
	}
	// 重启后重放（分配顺序不同）仍返回已撤销状态，不恢复实收。
	out, err = h2.run("bill", "remit", "c1", "r1", "700", "汇款", "2026-10:300", "2026-09:400")
	if err != nil || !strings.Contains(out, "已撤销") {
		t.Fatalf("out=%s err=%v", out, err)
	}
	// 相同原因重复撤销幂等，不同原因拒绝。
	if _, err = h2.run("bill", "unpay", "r1", "撤销重汇"); err != nil {
		t.Fatal(err)
	}
	if _, err = h2.run("bill", "unpay", "r1", "别的原因"); err == nil {
		t.Fatal("应拒绝不同原因的重复撤销")
	}
}

func TestLegacySingleBillPaymentCompatible(t *testing.T) {
	h := newHarness(t)
	// 旧版格式：收款只有 month + amount_fen，没有 total_fen/allocations。
	legacy := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2}},
  "bills": {"c1|2026-09": {
    "id": "BILL-legacy", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 2, "unit_price_fen": 100, "total_fee_fen": 200,
    "lines": [{"usage_id": "u1", "time": "2026-09-15T10:00:00Z", "quantity": 2, "line_fee_fen": 200}],
    "created_at": "2026-10-01T00:00:00Z"
  }},
  "adjustments": {},
  "payments": {"pay-1": {
    "id": "pay-1", "customer_id": "c1", "month": "2026-09", "amount_fen": 150,
    "note": "旧格式收款", "seq": 1, "created_at": "2026-10-02T00:00:00Z"
  }},
  "next_seq": 1
}`
	if err := os.WriteFile(h.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	// 旧格式收款视为一项分配：余额、历史正常展示。
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "实收：150 分") || !strings.Contains(out, "未收余额：50 分") ||
		!strings.Contains(out, "收款 pay-1：总额 150 分") || !strings.Contains(out, "本账单分配 150 分") {
		t.Fatal(out)
	}
	// bill pay 相同内容重放旧收款：单账单收款即一项分配，幂等返回。
	out = h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "150", "旧格式收款")
	if !strings.Contains(out, "不重复计入实收") {
		t.Fatal(out)
	}
	// bill remit 的单项分配与旧收款内容相同：同样幂等。
	out = h.mustRun("bill", "remit", "c1", "pay-1", "150", "旧格式收款", "2026-09:150")
	if !strings.Contains(out, "不重复计入实收") {
		t.Fatal(out)
	}
	// 撤销旧收款正常，撤销后重放仍返回已撤销状态。
	h.mustRun("bill", "unpay", "pay-1", "登记错误")
	out = h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "150", "旧格式收款")
	if !strings.Contains(out, "已撤销") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "实收：0 分") {
		t.Fatal(out)
	}
}

func TestCorruptRemitDataRejected(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "汇款", "2026-09:400", "2026-10:300")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 分配引用失效：把一处分配月份改到无账单的月份，按损坏处理，原文件保留。
	broken := strings.Replace(string(good), `"month": "2026-10"`, `"month": "2026-11"`, 1)
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

	// 金额不一致：分配合计与总额不符，按损坏处理。
	broken2 := strings.Replace(string(good), `"total_fen": 700`, `"total_fen": 701`, 1)
	if broken2 == string(good) {
		t.Fatal("替换未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(broken2), 0o644); err != nil {
		t.Fatal(err)
	}
	msg = h.runExpectErr("bill", "remit", "c1", "r2", "100", "备注", "2026-09:100")
	if !strings.Contains(msg, "损坏") {
		t.Fatal(msg)
	}
	got, _ = os.ReadFile(h.statePath())
	if string(got) != broken2 {
		t.Fatal("损坏文件被改写")
	}
}

// --- 账后对账流水 ---

func TestLedgerHappyPathAndCutoff(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "10") // c1 2026-09 总金额 1000
	settleBill(h, "c2", "100", "5")  // c2 2026-09 总金额 500
	// 其他客户的操作制造序号空档（1、4），对本账单流水合法。
	h.mustRun("bill", "adjust", "c2", "2026-09", "adj-c2", "50", "其他客户调整") // 序号 1
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "200", "漏算用量")   // 序号 2
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "300", "银行转账")      // 序号 3
	h.mustRun("bill", "pay", "c2", "2026-09", "pay-c2", "100", "其他客户收款")   // 序号 4
	h.mustRun("bill", "revoke", "adj-1", "录入错误")                           // 序号 5
	h.mustRun("bill", "unpay", "pay-1", "账号登记错误")                          // 序号 6

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 省略截止序号 = 最新：完整流水按序号升序，撤销在其发生序号抵消原操作。
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	for _, want := range []string{
		"客户：c1", "月份：2026-09", "原总金额：1000 分",
		"存档全局序号上限：6", "截止操作序号：6（省略，按最新）",
		"初始余额：应付 1000 分",
		"序号 2 调整 adj-1：应付 +200 分", "原因：漏算用量",
		"事后：应付 1200 分，实收 0 分，未收余额 1200 分",
		"序号 3 收款 pay-1：实收 +300 分", "汇款总额 300 分，本账单分配 300 分", "备注：银行转账",
		"事后：应付 1200 分，实收 300 分，未收余额 900 分",
		"序号 5 撤销调整 adj-1：应付 -200 分", "关联序号 2 的调整 adj-1", "原因：录入错误",
		"事后：应付 1000 分，实收 300 分，未收余额 700 分",
		"序号 6 撤销收款 pay-1：实收 -300 分", "关联序号 3 的收款 pay-1", "取消本账单分配 300 分",
		"截止时余额：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("流水缺少 %q:\n%s", want, out)
		}
	}
	// 其他客户的事件不混入本账单流水。
	if strings.Contains(out, "adj-c2") || strings.Contains(out, "pay-c2") {
		t.Fatal(out)
	}
	// 最新余额与 bill show 一致。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{"当前应付：1000 分", "实收：0 分", "未收余额：1000 分"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("bill show 缺少 %q:\n%s", want, shown)
		}
	}
	// 重启（重新载入）后顺序与历史余额一致。
	if again := h.mustRun("bill", "ledger", "c1", "2026-09"); again != out {
		t.Fatalf("两次流水输出不一致:\n%s\n---\n%s", out, again)
	}

	// 截止 3：只含序号 2、3；截止之后的撤销不影响历史余额也不混入流水。
	out = h.mustRun("bill", "ledger", "c1", "2026-09", "3")
	if !strings.Contains(out, "截止操作序号：3（指定）") ||
		!strings.Contains(out, "序号 3 收款 pay-1") ||
		!strings.Contains(out, "截止时余额：应付 1200 分（12.00 元），实收 300 分（3.00 元），未收余额 900 分（9.00 元）") {
		t.Fatal(out)
	}
	if strings.Contains(out, "撤销") {
		t.Fatalf("截止之后的撤销混入流水:\n%s", out)
	}
	// 截止 4：序号空档合法，流水与截止 3 相同。
	out = h.mustRun("bill", "ledger", "c1", "2026-09", "4")
	if !strings.Contains(out, "截止时余额：应付 1200 分（12.00 元），实收 300 分（3.00 元），未收余额 900 分（9.00 元）") {
		t.Fatal(out)
	}
	// 截止 0：只返回初始余额，明确空流水。
	out = h.mustRun("bill", "ledger", "c1", "2026-09", "0")
	if !strings.Contains(out, "流水：无") ||
		!strings.Contains(out, "截止时余额：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）") {
		t.Fatal(out)
	}

	// 查询不改写存档、不占用序号：文件逐字节一致，后续操作序号照常递增。
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("只读查询改写了数据文件")
	}
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "10", "后续调整") // 应占序号 7
	out = h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "存档全局序号上限：7") || !strings.Contains(out, "序号 7 调整 adj-2") {
		t.Fatal(out)
	}
}

func TestLedgerCutoffValidation(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "1")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "10", "补收") // 序号 1，上限为 1

	// 负数、非整数、超过存档全局序号上限均拒绝。
	for _, bad := range []string{"-1", "abc", "1.5", "", "  ", "2", "99999999999999999999999"} {
		msg := h.runExpectErr("bill", "ledger", "c1", "2026-09", bad)
		if !strings.Contains(msg, "截止操作序号") {
			t.Fatalf("cutoff=%q 错误信息异常: %s", bad, msg)
		}
	}
	// 参数数量不对：用法错误。
	h.runExpectErr("bill", "ledger", "c1")
	h.runExpectErr("bill", "ledger", "c1", "2026-09", "1", "extra")
	// 失败后状态不变，合法查询照常。
	out := h.mustRun("bill", "ledger", "c1", "2026-09", "1")
	if !strings.Contains(out, "序号 1 调整 adj-1") {
		t.Fatal(out)
	}
}

func TestLedgerEmptyAndErrors(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "150", "6") // 总金额 900，无任何账后操作

	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	for _, want := range []string{
		"存档全局序号上限：0", "截止操作序号：0",
		"初始余额：应付 900 分", "流水：无",
		"截止时余额：应付 900 分（9.00 元），实收 0 分（0.00 元），未收余额 900 分（9.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("空流水缺少 %q:\n%s", want, out)
		}
	}
	// 上限为 0 时截止 0 合法，截止 1 拒绝。
	h.mustRun("bill", "ledger", "c1", "2026-09", "0")
	h.runExpectErr("bill", "ledger", "c1", "2026-09", "1")

	// 客户不存在、账单不存在、月份非法均非零退出。
	h.runExpectErr("bill", "ledger", "ghost", "2026-09")
	h.runExpectErr("bill", "ledger", "c1", "2026-10")
	h.runExpectErr("bill", "ledger", "c1", "2026-9")
}

func TestLedgerRemitAcrossMonths(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")                                           // 2026-09: 500，2026-10: 300
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300") // 序号 1
	h.mustRun("bill", "adjust", "c1", "2026-10", "adj-o", "100", "补收")                  // 序号 2

	// 多月汇款只以本账单分配改变各月实收。
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "序号 1 收款 r1：实收 +400 分") ||
		!strings.Contains(out, "汇款总额 700 分，本账单分配 400 分") ||
		!strings.Contains(out, "截止时余额：应付 500 分（5.00 元），实收 400 分（4.00 元），未收余额 100 分（1.00 元）") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "ledger", "c1", "2026-10")
	if !strings.Contains(out, "序号 1 收款 r1：实收 +300 分") ||
		!strings.Contains(out, "本账单分配 300 分") ||
		!strings.Contains(out, "序号 2 调整 adj-o：应付 +100 分") ||
		!strings.Contains(out, "截止时余额：应付 400 分（4.00 元），实收 300 分（3.00 元），未收余额 100 分（1.00 元）") {
		t.Fatal(out)
	}

	// 整笔撤销在同一序号取消本账单分配；截止在撤销之前不受影响。
	h.mustRun("bill", "unpay", "r1", "汇错账户") // 序号 3
	out = h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "序号 3 撤销收款 r1：实收 -400 分") ||
		!strings.Contains(out, "取消本账单分配 400 分") ||
		!strings.Contains(out, "截止时余额：应付 500 分（5.00 元），实收 0 分（0.00 元），未收余额 500 分（5.00 元）") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "ledger", "c1", "2026-10", "2")
	if !strings.Contains(out, "截止时余额：应付 400 分（4.00 元），实收 300 分（3.00 元），未收余额 100 分（1.00 元）") {
		t.Fatal(out)
	}
	if strings.Contains(out, "撤销收款") {
		t.Fatalf("截止之后的撤销混入流水:\n%s", out)
	}
}

func TestLedgerSameNameAdjustAndPayment(t *testing.T) {
	h := newHarness(t)
	settleBill(h, "c1", "100", "10")                                      // 总金额 1000
	h.mustRun("bill", "pay", "c1", "2026-09", "same-id", "100", "同名收款")   // 序号 1
	h.mustRun("bill", "adjust", "c1", "2026-09", "same-id", "10", "同名调整") // 序号 2
	h.mustRun("bill", "revoke", "same-id", "撤销调整")                        // 序号 3：只撤销调整

	// 同名调整与收款按类型分别关联：撤销调整不影响同名收款。
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "序号 1 收款 same-id：实收 +100 分") ||
		!strings.Contains(out, "序号 2 调整 same-id：应付 +10 分") ||
		!strings.Contains(out, "序号 3 撤销调整 same-id：应付 -10 分") ||
		!strings.Contains(out, "关联序号 2 的调整 same-id") ||
		!strings.Contains(out, "截止时余额：应付 1000 分（10.00 元），实收 100 分（1.00 元），未收余额 900 分（9.00 元）") {
		t.Fatal(out)
	}
	if strings.Contains(out, "撤销收款") {
		t.Fatalf("同名收款被误关联撤销:\n%s", out)
	}
}

func TestLedgerRejectsCorruptHistory(t *testing.T) {
	h := newHarness(t)
	// 手工构造：中间步骤实收超过应付（序号 2 之后 1000 > 500），但最终余额
	// 合法（应付 1500 ≥ 实收 1000），载入校验可通过；流水查询必须逐步核验
	// 并拒绝，即使截止序号在异常之前。
	corrupt := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 10}},
  "bills": {"c1|2026-09": {
    "id": "BILL-x", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 10, "unit_price_fen": 100, "total_fee_fen": 1000,
    "lines": [{"usage_id": "u1", "time": "2026-09-15T10:00:00Z", "quantity": 10, "line_fee_fen": 1000}],
    "created_at": "2026-10-01T00:00:00Z"
  }},
  "adjustments": {
    "adj-1": {"id": "adj-1", "customer_id": "c1", "month": "2026-09", "amount_fen": -500,
      "reason": "减免", "seq": 1, "created_at": "2026-10-02T00:00:00Z"},
    "adj-2": {"id": "adj-2", "customer_id": "c1", "month": "2026-09", "amount_fen": 1000,
      "reason": "补收", "seq": 3, "created_at": "2026-10-02T00:00:00Z"}
  },
  "payments": {"pay-1": {
    "id": "pay-1", "customer_id": "c1", "total_fen": 1000, "note": "转账",
    "allocations": [{"month": "2026-09", "amount_fen": 1000}],
    "seq": 2, "created_at": "2026-10-02T00:00:00Z"
  }},
  "next_seq": 3
}`
	if err := os.WriteFile(h.statePath(), []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	// 最终余额合法，bill show 不受影响。
	h.mustRun("bill", "show", "c1", "2026-09")
	// 完整流水核验失败：无截止、截止在异常之前都拒绝，不输出部分正常报告。
	for _, args := range [][]string{
		{"bill", "ledger", "c1", "2026-09"},
		{"bill", "ledger", "c1", "2026-09", "1"},
		{"bill", "ledger", "c1", "2026-09", "0"},
	} {
		msg := h.runExpectErr(args...)
		if !strings.Contains(msg, "数据异常") {
			t.Fatalf("args=%v 错误信息异常: %s", args, msg)
		}
	}
	got, _ := os.ReadFile(h.statePath())
	if string(got) != corrupt {
		t.Fatal("查询失败后数据文件被改写")
	}

	// 中间步骤应付越出有符号 64 位最大值（最终净额为 0、余额合法）：同样拒绝。
	overflow := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 1}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 9223372036854775807}},
  "bills": {"c1|2026-09": {
    "id": "BILL-y", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 9223372036854775807, "unit_price_fen": 1, "total_fee_fen": 9223372036854775807,
    "lines": [{"usage_id": "u1", "time": "2026-09-15T10:00:00Z", "quantity": 9223372036854775807, "line_fee_fen": 9223372036854775807}],
    "created_at": "2026-10-01T00:00:00Z"
  }},
  "adjustments": {
    "adj-1": {"id": "adj-1", "customer_id": "c1", "month": "2026-09", "amount_fen": 1,
      "reason": "补收", "seq": 1, "created_at": "2026-10-02T00:00:00Z"},
    "adj-2": {"id": "adj-2", "customer_id": "c1", "month": "2026-09", "amount_fen": -1,
      "reason": "减免", "seq": 2, "created_at": "2026-10-02T00:00:00Z"}
  },
  "payments": {},
  "next_seq": 2
}`
	if err := os.WriteFile(h.statePath(), []byte(overflow), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.runExpectErr("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(msg, "数据异常") {
		t.Fatal(msg)
	}
	got, _ = os.ReadFile(h.statePath())
	if string(got) != overflow {
		t.Fatal("查询失败后数据文件被改写")
	}
}

func TestLedgerCumulativeOccurrenceOverflowStillSucceeds(t *testing.T) {
	h := newHarness(t)
	// 单价 0：原总金额为 0，允许极大调整与收款往返。累计补收/收款发生额
	// 远超有符号 64 位上限，但每步余额都合法，查询必须成功。
	h.mustRun("customer", "add", "c1", "零价客户", "0")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-15T10:00:00Z,5\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")

	const max = "9223372036854775807"
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", max, "巨额补收") // 序号 1
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", max, "巨额收款")    // 序号 2
	h.mustRun("bill", "unpay", "pay-1", "退回")                          // 序号 3
	h.mustRun("bill", "revoke", "adj-1", "撤回")                         // 序号 4
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", max, "再次补收") // 序号 5
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-2", max, "再次收款")    // 序号 6

	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "截止时余额：应付 9223372036854775807 分") ||
		!strings.Contains(out, "实收 9223372036854775807 分") ||
		!strings.Contains(out, "未收余额 0 分") {
		t.Fatal(out)
	}
	// 与 bill show 一致。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：9223372036854775807 分") ||
		!strings.Contains(shown, "实收：9223372036854775807 分") ||
		!strings.Contains(shown, "未收余额：0 分") {
		t.Fatal(shown)
	}
	// 截止 0：初始余额为原总金额 0。
	out = h.mustRun("bill", "ledger", "c1", "2026-09", "0")
	if !strings.Contains(out, "流水：无") ||
		!strings.Contains(out, "截止时余额：应付 0 分（0.00 元），实收 0 分（0.00 元），未收余额 0 分（0.00 元）") {
		t.Fatal(out)
	}
}

// --- 收款分配更正 ---

func TestCorrectHappyPathAndBalances(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3") // 9月 500，10月 300

	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")

	// 把 10 月的 100 分更正到 9 月：合计仍等于原总额，不重复收钱。
	out := h.mustRun("bill", "correct", "r1", "corr-1", "入账月份登记错误", "2026-09:500", "2026-10:200")
	for _, want := range []string{
		"已登记收款分配更正", "更正标识：corr-1", "关联收款：r1",
		"原因：入账月份登记错误",
		"更正前分配：2026-09:400,2026-10:300",
		"更正后分配：2026-09:500,2026-10:200",
		"月份 2026-09：本笔分配 400 分 → 500 分；当前应付 500 分",
		"月份 2026-10：本笔分配 300 分 → 200 分；当前应付 300 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("更正输出缺少 %q:\n%s", want, out)
		}
	}

	// 各月实收按最新分配计入，应付与原账单不变。
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"总金额：500 分", "当前应付：500 分", "实收：500 分", "未收余额：0 分",
		"收款 r1：总额 700 分", "本账单分配 400 分",
		"更正 corr-1：原因：入账月份登记错误（关联收款 r1，本账单分配 400 分 → 500 分）",
	} {
		if !strings.Contains(sep, want) {
			t.Fatalf("9 月账单缺少 %q:\n%s", want, sep)
		}
	}
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "实收：200 分") || !strings.Contains(oct, "未收余额：100 分") ||
		!strings.Contains(oct, "本账单分配 300 分 → 200 分") {
		t.Fatal(oct)
	}
}

func TestCorrectExceedsPayableRejected(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3") // 9月 500，10月 300
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")

	// 10 月应付只有 300，更正后实收 400 超额：整笔拒绝，状态不变，标识不占用。
	msg := h.runExpectErr("bill", "correct", "r1", "corr-1", "理由", "2026-09:300", "2026-10:400")
	if !strings.Contains(msg, "超过当前应付") {
		t.Fatal(msg)
	}
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：400 分") {
		t.Fatal(sep)
	}
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "实收：300 分") {
		t.Fatal(oct)
	}
	// 标识未占用：合法更正可同名重试成功。
	out := h.mustRun("bill", "correct", "r1", "corr-1", "理由", "2026-09:500", "2026-10:200")
	if !strings.Contains(out, "已登记收款分配更正") {
		t.Fatal(out)
	}
	sep = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：500 分") || !strings.Contains(sep, "未收余额：0 分") {
		t.Fatal(sep)
	}
	oct = h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "实收：200 分") || !strings.Contains(oct, "未收余额：100 分") {
		t.Fatal(oct)
	}
}

func TestCorrectValidation(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")

	// 空收款标识、空更正标识、空原因。
	h.runExpectErr("bill", "correct", "", "c1", "理由", "2026-09:700")
	h.runExpectErr("bill", "correct", "r1", "", "理由", "2026-09:700")
	h.runExpectErr("bill", "correct", "r1", "  ", "理由", "2026-09:700")
	h.runExpectErr("bill", "correct", "r1", "c1", "", "2026-09:700")
	h.runExpectErr("bill", "correct", "r1", "c1", "   ", "2026-09:700")
	// 收款不存在。
	h.runExpectErr("bill", "correct", "ghost", "c1", "理由", "2026-09:700")
	// 新列表为空、格式非法、月份非法、金额非正、月份重复。
	h.runExpectErr("bill", "correct", "r1", "c1", "理由")
	h.runExpectErr("bill", "correct", "r1", "c1", "理由", "2026-09")
	h.runExpectErr("bill", "correct", "r1", "c1", "理由", "2026-9:700")
	h.runExpectErr("bill", "correct", "r1", "c1", "理由", "2026-13:700")
	h.runExpectErr("bill", "correct", "r1", "c1", "理由", "2026-09:0")
	h.runExpectErr("bill", "correct", "r1", "c1", "理由", "2026-09:-1")
	h.runExpectErr("bill", "correct", "r1", "c1", "理由", "2026-09:abc")
	h.runExpectErr("bill", "correct", "r1", "c1", "理由", "2026-09:400", "2026-09:300")
	// 合计不等于原总额（700）。
	h.runExpectErr("bill", "correct", "r1", "c1", "理由", "2026-09:600")
	h.runExpectErr("bill", "correct", "r1", "c1", "理由", "2026-09:400", "2026-10:400")
	// 合计溢出。
	h.runExpectErr("bill", "correct", "r1", "c1", "理由",
		"2026-09:9223372036854775806", "2026-10:2")
	// 月份无账单（未结算）。
	h.runExpectErr("bill", "correct", "r1", "c1", "理由", "2026-11:700")

	// 全部失败后更正标识未被占用：首次合法更正照常成功。
	out := h.mustRun("bill", "correct", "r1", "c1", "理由", "2026-09:500", "2026-10:200")
	if !strings.Contains(out, "已登记收款分配更正") {
		t.Fatal(out)
	}
}

func TestCorrectRejectsRevokedPayment(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "unpay", "r1", "退汇")

	msg := h.runExpectErr("bill", "correct", "r1", "corr-1", "理由", "2026-09:700")
	if !strings.Contains(msg, "已撤销") {
		t.Fatal(msg)
	}
	// 状态不变：两月实收仍为 0。
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：0 分") {
		t.Fatal(sep)
	}
}

func TestCorrectIdempotentAndConflict(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "correct", "r1", "corr-1", "月份登错", "2026-09:500", "2026-10:200")

	// 列表顺序不同的相同内容重放：返回原更正记录，不改分配、不增历史。
	out := h.mustRun("bill", "correct", "r1", "corr-1", "月份登错", "2026-10:200", "2026-09:500")
	if !strings.Contains(out, "返回原更正记录") || !strings.Contains(out, "更正后分配：2026-09:500,2026-10:200") {
		t.Fatal(out)
	}
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：500 分") || strings.Count(sep, "更正 corr-1") != 1 {
		t.Fatal(sep)
	}

	// 内容不同（原因、分配、目标收款任一不同）拒绝。
	h.runExpectErr("bill", "correct", "r1", "corr-1", "另一个原因", "2026-09:500", "2026-10:200")
	h.runExpectErr("bill", "correct", "r1", "corr-1", "月份登错", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "remit", "c1", "r2", "100", "补充汇款", "2026-10:100")
	h.runExpectErr("bill", "correct", "r2", "corr-1", "月份登错", "2026-10:100")

	// 后续更正与收款撤销不影响重放规则：仍返回原更正记录。
	// （此时 9 月实收 500=应付、10 月实收 300=应付，唯一合法的更正是保持分配。）
	h.mustRun("bill", "correct", "r1", "corr-2", "再次修正", "2026-09:500", "2026-10:200")
	out = h.mustRun("bill", "correct", "r1", "corr-1", "月份登错", "2026-09:500", "2026-10:200")
	if !strings.Contains(out, "返回原更正记录") {
		t.Fatal(out)
	}
	h.mustRun("bill", "unpay", "r1", "退汇")
	out = h.mustRun("bill", "correct", "r1", "corr-1", "月份登错", "2026-09:500", "2026-10:200")
	if !strings.Contains(out, "返回原更正记录") {
		t.Fatal(out)
	}
	// 重放不改分配：9 月实收仍为 0（已撤销）。
	sep = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：0 分") {
		t.Fatal(sep)
	}
}

func TestCorrectChainStartsFromLatest(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")

	// 连续更正：第二次以第一次的结果为起点。
	h.mustRun("bill", "correct", "r1", "corr-1", "第一次", "2026-09:500", "2026-10:200")
	out := h.mustRun("bill", "correct", "r1", "corr-2", "第二次", "2026-09:450", "2026-10:250")
	if !strings.Contains(out, "更正前分配：2026-09:500,2026-10:200") ||
		!strings.Contains(out, "更正后分配：2026-09:450,2026-10:250") {
		t.Fatal(out)
	}
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：450 分") ||
		!strings.Contains(sep, "更正 corr-1：原因：第一次（关联收款 r1，本账单分配 400 分 → 500 分）") ||
		!strings.Contains(sep, "更正 corr-2：原因：第二次（关联收款 r1，本账单分配 500 分 → 450 分）") {
		t.Fatal(sep)
	}
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "实收：250 分") || !strings.Contains(oct, "未收余额：50 分") {
		t.Fatal(oct)
	}
}

func TestCorrectChainBalanceEnforced(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3") // 9月 500，10月 300
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")

	// 第一次更正把 10 月降到 200；第二次试图把 10 月提到 400（超过应付 300）拒绝。
	h.mustRun("bill", "correct", "r1", "corr-1", "第一次", "2026-09:500", "2026-10:200")
	msg := h.runExpectErr("bill", "correct", "r1", "corr-2", "第二次", "2026-09:300", "2026-10:400")
	if !strings.Contains(msg, "超过当前应付") {
		t.Fatal(msg)
	}
	// 状态不变：仍是第一次更正后的分配。
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：500 分") {
		t.Fatal(sep)
	}
	// 合法第二次更正：以当前分配（500/200）为起点。
	h.mustRun("bill", "correct", "r1", "corr-2", "第二次", "2026-09:450", "2026-10:250")
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "实收：250 分") {
		t.Fatal(oct)
	}
}

func TestUnpayAfterCorrectCancelsLatest(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "correct", "r1", "corr-1", "月份登错", "2026-09:500", "2026-10:200")

	out := h.mustRun("bill", "unpay", "r1", "退汇")
	if !strings.Contains(out, "已撤销收款") || !strings.Contains(out, "取消最新分配") ||
		!strings.Contains(out, "月份 2026-09：分配 500 分") ||
		!strings.Contains(out, "月份 2026-10：分配 200 分") {
		t.Fatal(out)
	}
	// 两月实收归零，全部历史（收款、更正、撤销）保留。
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"实收：0 分", "未收余额：500 分",
		"收款 r1：总额 700 分", "更正 corr-1", "撤销收款 r1：原因：退汇（关联收款 r1，总额 700 分，本账单分配 500 分）",
	} {
		if !strings.Contains(sep, want) {
			t.Fatalf("9 月账单缺少 %q:\n%s", want, sep)
		}
	}
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "实收：0 分") ||
		!strings.Contains(oct, "撤销收款 r1：原因：退汇（关联收款 r1，总额 700 分，本账单分配 200 分）") {
		t.Fatal(oct)
	}
}

func TestAdjustConstrainedByLatestReceived(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3") // 9月应付 500
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "100", "首笔")
	// 更正把另一笔汇款的 200 分移入 9 月：9 月最新实收变为 300。
	h.mustRun("bill", "remit", "c1", "r1", "300", "汇款", "2026-10:300")
	h.mustRun("bill", "correct", "r1", "corr-1", "改入 9 月", "2026-09:200", "2026-10:100")

	// 减免 300 后应付 200 < 最新实收 300：拒绝。
	msg := h.runExpectErr("bill", "adjust", "c1", "2026-09", "adj-1", "-300", "大额减免")
	if !strings.Contains(msg, "低于实收") {
		t.Fatal(msg)
	}
	// 减免 200 后应付 300 = 实收 300：允许。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "-200", "合理减免")
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "当前应付：300 分") || !strings.Contains(sep, "实收：300 分") {
		t.Fatal(sep)
	}
}

func TestCorrectLedgerFlow(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")                                           // 9月 500，10月 300
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300") // 序号 1
	h.mustRun("bill", "correct", "r1", "corr-1", "月份登错", "2026-09:500", "2026-10:200")  // 序号 2
	h.mustRun("bill", "unpay", "r1", "退汇")                                              // 序号 3

	// 9 月流水：收款 +400，更正 400→500（+100），撤销取消最新分配 -500。
	sep := h.mustRun("bill", "ledger", "c1", "2026-09")
	for _, want := range []string{
		"序号 1 收款 r1：实收 +400 分",
		"事后：应付 500 分，实收 400 分，未收余额 100 分",
		"序号 2 更正 corr-1：实收 +100 分",
		"关联收款 r1，本账单分配 400 分 → 500 分",
		"事后：应付 500 分，实收 500 分，未收余额 0 分",
		"序号 3 撤销收款 r1：实收 -500 分",
		"取消本账单分配 500 分",
		"截止时余额：应付 500 分（5.00 元），实收 0 分（0.00 元），未收余额 500 分（5.00 元）",
	} {
		if !strings.Contains(sep, want) {
			t.Fatalf("9 月流水缺少 %q:\n%s", want, sep)
		}
	}

	// 10 月流水：同一更正作为共用序号的单个事件出现，300→200。
	oct := h.mustRun("bill", "ledger", "c1", "2026-10")
	for _, want := range []string{
		"序号 2 更正 corr-1：实收 -100 分",
		"关联收款 r1，本账单分配 300 分 → 200 分",
		"序号 3 撤销收款 r1：实收 -200 分",
	} {
		if !strings.Contains(oct, want) {
			t.Fatalf("10 月流水缺少 %q:\n%s", want, oct)
		}
	}

	// 截止 1（更正之前）：9 月实收 400，更正与撤销不提前影响。
	cut := h.mustRun("bill", "ledger", "c1", "2026-09", "1")
	if !strings.Contains(cut, "截止时余额：应付 500 分（5.00 元），实收 400 分（4.00 元），未收余额 100 分（1.00 元）") ||
		strings.Contains(cut, "更正 corr-1") {
		t.Fatal(cut)
	}
	// 截止 2：更正已生效，撤销尚未发生。
	cut = h.mustRun("bill", "ledger", "c1", "2026-09", "2")
	if !strings.Contains(cut, "截止时余额：应付 500 分（5.00 元），实收 500 分（5.00 元），未收余额 0 分（0.00 元）") ||
		strings.Contains(cut, "撤销收款") {
		t.Fatal(cut)
	}
	// 最新与 bill show 一致。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "实收：0 分") || !strings.Contains(shown, "未收余额：500 分") {
		t.Fatal(shown)
	}
}

func TestCorrectShowKeepsMovedOutMonthHistory(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	// 把 9 月分配全部更正到 10 月……但 10 月应付只有 300，先调增 10 月应付。
	h.mustRun("bill", "adjust", "c1", "2026-10", "adj-1", "400", "补收")
	h.mustRun("bill", "correct", "r1", "corr-1", "全部归入 10 月", "2026-10:700")

	// 9 月实收归零，但收款与更正历史完整保留（曾涉及该月）。
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"实收：0 分", "未收余额：500 分",
		"收款 r1：总额 700 分", "本账单分配 400 分",
		"更正 corr-1：原因：全部归入 10 月（关联收款 r1，本账单分配 400 分 → 0 分）",
	} {
		if !strings.Contains(sep, want) {
			t.Fatalf("9 月账单缺少 %q:\n%s", want, sep)
		}
	}
	// 重复结算同样保留历史。
	again := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(again, "更正 corr-1") || !strings.Contains(again, "收款 r1") {
		t.Fatal(again)
	}
	// 10 月：实收 700 = 应付 300 + 400。
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "当前应付：700 分") || !strings.Contains(oct, "实收：700 分") ||
		!strings.Contains(oct, "本账单分配 300 分 → 700 分") {
		t.Fatal(oct)
	}
}

func TestPayReplayAfterCorrectionKeepsCurrent(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "400", "转账")
	h.mustRun("bill", "correct", "p1", "corr-1", "改期", "2026-09:300", "2026-10:100")

	// 相同内容重放原收款：返回当前状态，不恢复旧分配。
	out := h.mustRun("bill", "pay", "c1", "2026-09", "p1", "400", "转账")
	if !strings.Contains(out, "不重复计入实收") ||
		!strings.Contains(out, "当前分配（经 1 次更正，以最新为准）：2026-09:300,2026-10:100") {
		t.Fatal(out)
	}
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：300 分") {
		t.Fatal(sep)
	}
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(oct, "实收：100 分") {
		t.Fatal(oct)
	}
	// 内容不同的重放拒绝。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "p1", "500", "转账")
	h.runExpectErr("bill", "pay", "c1", "2026-10", "p1", "400", "转账")
	// remit 入口同样按首次登记内容判重。
	out = h.mustRun("bill", "remit", "c1", "p1", "400", "转账", "2026-09:400")
	if !strings.Contains(out, "不重复计入实收") {
		t.Fatal(out)
	}
}

func TestCorrectPersistsAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "correct", "r1", "corr-1", "月份登错", "2026-09:500", "2026-10:200")

	// 每次调用都重新从磁盘载入：余额、历史、顺序与幂等跨重启保持。
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：500 分") || !strings.Contains(sep, "更正 corr-1") {
		t.Fatal(sep)
	}
	out := h.mustRun("bill", "correct", "r1", "corr-1", "月份登错", "2026-09:500", "2026-10:200")
	if !strings.Contains(out, "返回原更正记录") {
		t.Fatal(out)
	}
	// 更正与收款、调整共用全局序号：后续操作序号继续递增。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "10", "补收")
	ledger := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(ledger, "序号 1 收款 r1") ||
		!strings.Contains(ledger, "序号 2 更正 corr-1") ||
		!strings.Contains(ledger, "序号 3 调整 adj-1") {
		t.Fatal(ledger)
	}
	// 存档包含更正记录。
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"corrections"`) || !strings.Contains(string(data), `"corr-1"`) {
		t.Fatal("state.json 缺少更正记录")
	}
}

func TestCorrectSameNameAsPaymentAndAdjustmentAllowed(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "pay", "c1", "2026-09", "same", "100", "收款")
	h.mustRun("bill", "adjust", "c1", "2026-09", "same", "50", "调整")
	// 更正标识与收款、调整标识独立，可同名。
	out := h.mustRun("bill", "correct", "same", "same", "同名更正", "2026-09:100")
	if !strings.Contains(out, "已登记收款分配更正") {
		t.Fatal(out)
	}
	// 各自判重互不影响。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "same", "200", "不同内容")
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "same", "60", "不同内容")
	h.runExpectErr("bill", "correct", "same", "same", "不同原因", "2026-09:100")
}

func TestOldStateFileWithoutCorrections(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")

	// 模拟旧版数据文件：删除 corrections 字段，其余不变。
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	i := strings.Index(s, "\n  \"corrections\"")
	j := strings.Index(s, "\n  \"next_seq\"")
	if i < 0 || j < 0 || j <= i {
		t.Fatal("未找到 corrections 字段")
	}
	if err := os.WriteFile(h.statePath(), []byte(s[:i]+s[j:]), 0o644); err != nil {
		t.Fatal(err)
	}

	// 旧文件直接可读：余额与历史保持，可继续更正。
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(sep, "实收：400 分") {
		t.Fatal(sep)
	}
	out := h.mustRun("bill", "correct", "r1", "corr-1", "月份登错", "2026-09:500", "2026-10:200")
	if !strings.Contains(out, "已登记收款分配更正") {
		t.Fatal(out)
	}
}

func TestCorruptCorrectionDataRejected(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3")
	h.mustRun("bill", "remit", "c1", "r1", "700", "季度汇款", "2026-09:400", "2026-10:300")
	h.mustRun("bill", "correct", "r1", "corr-1", "月份登错", "2026-09:500", "2026-10:200")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 把更正的分配合计改到与原总额不一致：按损坏处理，原文件不改写。
	broken := strings.Replace(string(good), `"amount_fen": 200`, `"amount_fen": 300`, 1)
	if broken == string(good) {
		t.Fatal("替换未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"bill", "show", "c1", "2026-09"},
		{"bill", "correct", "r1", "corr-2", "理由", "2026-09:700"},
		{"bill", "unpay", "r1", "原因"},
	} {
		msg := h.runExpectErr(args...)
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("args=%v 未报损坏: %s", args, msg)
		}
	}
	got, _ := os.ReadFile(h.statePath())
	if string(got) != broken {
		t.Fatal("损坏文件被改写")
	}

	// 更正引用不存在的收款同样按损坏处理。
	dangling := strings.Replace(string(good), `"payment_id": "r1"`, `"payment_id": "ghost"`, 1)
	if dangling == string(good) {
		t.Fatal("替换未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(dangling), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.runExpectErr("bill", "show", "c1", "2026-09")
	if !strings.Contains(msg, "损坏") {
		t.Fatal(msg)
	}
	got, _ = os.ReadFile(h.statePath())
	if string(got) != dangling {
		t.Fatal("损坏文件被改写")
	}
}

// --- 阶梯计费方案 ---

func TestPlanAddShowListAndPersistence(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("plan", "add", "p1", "标准阶梯", "100:10", "500:8", "-:5")
	for _, want := range []string{"方案标识：p1", "方案名称：标准阶梯", "第 1 档：累计上限 100，单价 10 分", "第 2 档：累计上限 500，单价 8 分", "第 3 档：累计数量无上限，单价 5 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan add 输出缺少 %q:\n%s", want, out)
		}
	}
	// 查询与列表（每次调用重新从磁盘载入，天然验证持久化）。
	show := h.mustRun("plan", "show", "p1")
	if !strings.Contains(show, "第 3 档：累计数量无上限，单价 5 分") {
		t.Fatalf("plan show 输出异常:\n%s", show)
	}
	list := h.mustRun("plan", "list")
	if !strings.Contains(list, "已登记阶梯计费方案 1 个") || !strings.Contains(list, "p1（标准阶梯）：月费 0 分，3 档，规则 100:10 500:8 -:5") {
		t.Fatalf("plan list 输出异常:\n%s", list)
	}
	// 单档方案（仅无上限档）合法。
	h.mustRun("plan", "add", "p0", "单档", "-:7")
	if msg := h.runExpectErr("plan", "show", "ghost"); !strings.Contains(msg, "不存在") {
		t.Fatal(msg)
	}
}

func TestPlanAddValidation(t *testing.T) {
	h := newHarness(t)
	cases := [][]string{
		{"plan", "add", "p1", "名称"},                          // 缺少阶梯（用法错误）
		{"plan", "add", "p1", "名称", "abc"},                   // 格式非法
		{"plan", "add", "p1", "名称", "100:1", "100:2", "-:3"}, // 上限未严格递增
		{"plan", "add", "p1", "名称", "200:1", "100:2", "-:3"}, // 上限倒退
		{"plan", "add", "p1", "名称", "0:1", "-:2"},            // 上限非正
		{"plan", "add", "p1", "名称", "-5:1", "-:2"},           // 上限为负
		{"plan", "add", "p1", "名称", "100:-1", "-:2"},         // 单价为负
		{"plan", "add", "p1", "名称", "100:1", "200:2"},        // 最后一档有上限
		{"plan", "add", "p1", "名称", "-:1", "100:2"},          // 无上限档不在最后
		{"plan", "add", "p1", "名称", "100", "-:2"},            // 缺少冒号
		{"plan", "add", "p1", "名称", "100:abc", "-:2"},        // 单价非整数
		{"plan", "add", "p1", "", "100:1", "-:2"},            // 空名称
		{"plan", "add", "", "名称", "100:1", "-:2"},            // 空标识
	}
	for _, args := range cases {
		h.runExpectErr(args...)
	}
	// 失败的新增不占标识：同一标识随后可正常登记。
	if msg := h.runExpectErr("plan", "show", "p1"); !strings.Contains(msg, "不存在") {
		t.Fatal(msg)
	}
	h.mustRun("plan", "add", "p1", "名称", "100:1", "-:2")
	// 重复方案标识拒绝，方案创建后不可修改。
	if msg := h.runExpectErr("plan", "add", "p1", "另一个", "-:9"); !strings.Contains(msg, "已存在") {
		t.Fatal(msg)
	}
	show := h.mustRun("plan", "show", "p1")
	if !strings.Contains(show, "规则") || !strings.Contains(show, "累计上限 100，单价 1 分") {
		t.Fatalf("方案被重复登记改写:\n%s", show)
	}
}

func TestCustomerAddPlan(t *testing.T) {
	h := newHarness(t)
	// 方案不存在时拒绝。
	if msg := h.runExpectErr("customer", "add-plan", "c1", "客户一", "ghost"); !strings.Contains(msg, "不存在") {
		t.Fatal(msg)
	}
	h.mustRun("plan", "add", "p1", "标准阶梯", "100:10", "-:5")
	out := h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	if !strings.Contains(out, `绑定阶梯计费方案 "p1"`) {
		t.Fatalf("add-plan 输出异常:\n%s", out)
	}
	// 标识和名称规则不变。
	h.runExpectErr("customer", "add-plan", "", "客户", "p1")
	h.runExpectErr("customer", "add-plan", "c2", "", "p1")
	h.runExpectErr("customer", "add-plan", "c2", "客户", "")
	// 重复客户标识拒绝（两个入口都不可复用），绑定不可变更。
	h.runExpectErr("customer", "add-plan", "c1", "客户一", "p1")
	h.runExpectErr("customer", "add", "c1", "客户一", "10")
	// 固定单价客户同样不能再登记为方案客户。
	h.mustRun("customer", "add", "c9", "客户九", "10")
	h.runExpectErr("customer", "add-plan", "c9", "客户九", "p1")
}

func TestTieredSettleCrossTierAndSegments(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "标准阶梯", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-01T00:00:00Z,60\n"+
		"u2,c1,2026-09-02T00:00:00Z,80\n")
	h.mustRun("usage", "import", f)

	out1 := h.mustRun("bill", "settle", "c1", "2026-09")
	for _, want := range []string{
		"计价类型：阶梯计费",
		"方案：p1（标准阶梯）",
		"第 1 档：累计上限 100，单价 10 分",
		"第 2 档：累计数量无上限，单价 5 分",
		"总数量：140",
		"总金额：1200 分",
		"第 1 档：数量 100，单价 10 分，金额 1000 分",
		"第 2 档：数量 40，单价 5 分，金额 200 分",
		"用量标识=u1 时间=2026-09-01T00:00:00Z 数量=60 小计=600 分",
		"分段 1：第 1 档 数量=60 单价=10 分 小计=600 分",
		"用量标识=u2 时间=2026-09-02T00:00:00Z 数量=80 小计=600 分",
		"分段 1：第 1 档 数量=40 单价=10 分 小计=400 分",
		"分段 2：第 2 档 数量=40 单价=5 分 小计=200 分",
	} {
		if !strings.Contains(out1, want) {
			t.Fatalf("阶梯账单输出缺少 %q:\n%s", want, out1)
		}
	}
	// 阶梯账单不伪造统一单价。
	if strings.Contains(out1, "\n单价：") {
		t.Fatalf("阶梯账单不应出现统一单价行:\n%s", out1)
	}

	// bill show 与重复 bill settle 返回相同计费明细。
	idx := strings.Index(out1, "\n\n")
	if idx < 0 {
		t.Fatalf("结算输出缺少账单部分:\n%s", out1)
	}
	billText := out1[idx+2:]
	out2 := h.mustRun("bill", "settle", "c1", "2026-09")
	show := h.mustRun("bill", "show", "c1", "2026-09")
	if show != billText || !strings.HasSuffix(out2, billText) {
		t.Fatalf("重复结算/查询明细不一致:\nsettle1=%q\nsettle2=%q\nshow=%q", out1, out2, show)
	}

	// 固定单价客户与阶梯客户互不影响。
	h.mustRun("customer", "add", "c9", "客户九", "10")
	f9 := h.writeFile("u9.csv", csvHeader+"u9,c9,2026-09-01T00:00:00Z,7\n")
	h.mustRun("usage", "import", f9)
	out9 := h.mustRun("bill", "settle", "c9", "2026-09")
	if !strings.Contains(out9, "单价：10 分") || strings.Contains(out9, "计价类型") {
		t.Fatalf("固定单价账单展示异常:\n%s", out9)
	}
}

func TestTieredSettleOrderingAndTieBreak(t *testing.T) {
	h := newHarness(t)
	// 单价可升：同一时间按标识字典序计价。
	h.mustRun("plan", "add", "p2", "递增阶梯", "10:1", "-:100")
	h.mustRun("customer", "add-plan", "c2", "客户二", "p2")
	f := h.writeFile("u2.csv", csvHeader+
		"u-b,c2,2026-09-10T00:00:00Z,10\n"+
		"u-a,c2,2026-09-10T00:00:00Z,10\n")
	h.mustRun("usage", "import", f)
	out := h.mustRun("bill", "settle", "c2", "2026-09")
	ia, ib := strings.Index(out, "用量标识=u-a"), strings.Index(out, "用量标识=u-b")
	if ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("同一时间未按标识字典序计价:\n%s", out)
	}
	if !strings.Contains(out, "用量标识=u-a 时间=2026-09-10T00:00:00Z 数量=10 小计=10 分") ||
		!strings.Contains(out, "用量标识=u-b 时间=2026-09-10T00:00:00Z 数量=10 小计=1000 分") ||
		!strings.Contains(out, "总金额：1010 分") {
		t.Fatalf("同时间字典序计价结果异常:\n%s", out)
	}

	// 按解析后的时间点升序计价，与导入顺序无关。
	h.mustRun("plan", "add", "p3", "递减阶梯", "5:10", "-:1")
	h.mustRun("customer", "add-plan", "c3", "客户三", "p3")
	f3 := h.writeFile("u3.csv", csvHeader+
		"u-later,c3,2026-09-20T00:00:00Z,5\n"+
		"u-earlier,c3,2026-09-01T00:00:00Z,10\n")
	h.mustRun("usage", "import", f3)
	out3 := h.mustRun("bill", "settle", "c3", "2026-09")
	// u-earlier 先计价：5×10 + 5×1 = 55；u-later 后计价：5×1 = 5；合计 60。
	if !strings.Contains(out3, "用量标识=u-earlier 时间=2026-09-01T00:00:00Z 数量=10 小计=55 分") ||
		!strings.Contains(out3, "用量标识=u-later 时间=2026-09-20T00:00:00Z 数量=5 小计=5 分") ||
		!strings.Contains(out3, "总金额：60 分") {
		t.Fatalf("按时间升序计价结果异常:\n%s", out3)
	}
}

func TestTieredImportPrecheckFromZero(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "天价阶梯", "-:9223372036854775807")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	// 单条数量从零计价即溢出：整批拒绝，不留新用量。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-01T00:00:00Z,2\n")
	msg := h.runExpectErr("usage", "import", f)
	if !strings.Contains(msg, "溢出") || !strings.Contains(msg, "整批未生效") {
		t.Fatal(msg)
	}
	// 数量 1 不溢出，可正常导入。
	f1 := h.writeFile("u1.csv", csvHeader+"u1,c1,2026-09-01T00:00:00Z,1\n")
	h.mustRun("usage", "import", f1)
}

func TestTieredSettleOverflowRejectsWithoutSealing(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "单档", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-01T00:00:00Z,9223372036854775807\n"+
		"u2,c1,2026-09-02T00:00:00Z,9223372036854775807\n")
	h.mustRun("usage", "import", f)
	msg := h.runExpectErr("bill", "settle", "c1", "2026-09")
	if !strings.Contains(msg, "溢出") || !strings.Contains(msg, "不封账") {
		t.Fatal(msg)
	}
	// 未封账：无账单，重复用量导入仍按重复跳过（说明状态未变）。
	if msg := h.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "尚无账单") {
		t.Fatal(msg)
	}
	out := h.mustRun("usage", "import", f)
	if !strings.Contains(out, "新增 0 条，重复跳过 2 条") {
		t.Fatalf("结算失败后状态发生变化:\n%s", out)
	}
}

func TestTieredZeroFeeBillSeals(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "免费阶梯", "100:0", "-:0")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-01T00:00:00Z,50\n")
	h.mustRun("usage", "import", f)
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "总金额：0 分") || !strings.Contains(out, "已封账") {
		t.Fatalf("零费用账单未正常封账:\n%s", out)
	}
	// 封账后新用量不得进入。
	f2 := h.writeFile("u2.csv", csvHeader+"u2,c1,2026-09-02T00:00:00Z,1\n")
	h.runExpectErr("usage", "import", f2)
}

func TestTieredAdjustPayLedgerFlow(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "10:2", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-01T00:00:00Z,15\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09") // 10×2 + 5×1 = 25

	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "5", "补收")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "30", "转账")
	out := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{"计价类型：阶梯计费", "总金额：25 分", "当前应付：30 分", "实收：30 分", "未收余额：0 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("阶梯账单账后操作展示缺少 %q:\n%s", want, out)
		}
	}
	ledger := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(ledger, "截止时余额：应付 30 分") {
		t.Fatalf("阶梯账单流水异常:\n%s", ledger)
	}
	// 超额收款拒绝：0 ≤ 实收 ≤ 应付 对阶梯账单同样成立。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "pay-2", "1", "超额")
}

func TestTieredCorruptStateRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "标准阶梯", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-01T00:00:00Z,60\n"+
		"u2,c1,2026-09-02T00:00:00Z,80\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ old, new string }{
		{`"line_fee_fen": 600`, `"line_fee_fen": 601`},     // 明细小计与计价规则不符
		{`"fee_fen": 400`, `"fee_fen": 401`},               // 分段小计被篡改
		{`"quantity": 100`, `"quantity": 101`},             // 分档合计被篡改
		{`"total_fee_fen": 1200`, `"total_fee_fen": 1201`}, // 总金额被篡改
		{`"plan_id": "p1"`, `"plan_id": "ghost"`},          // 方案引用失效
		{`"plan_name": "标准阶梯"`, `"plan_name": "改名"`},       // 方案名称快照不符
		{`"limit": 100`, `"limit": 101`},                   // 方案规则快照不符
	}
	for _, tc := range cases {
		broken := strings.Replace(string(good), tc.old, tc.new, 1)
		if broken == string(good) {
			t.Fatalf("替换 %q 未生效", tc.old)
		}
		if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := h.runExpectErr("bill", "show", "c1", "2026-09")
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("篡改 %q 未报损坏: %s", tc.old, msg)
		}
		got, _ := os.ReadFile(h.statePath())
		if string(got) != broken {
			t.Fatalf("篡改 %q 后文件被改写", tc.old)
		}
	}
	// 恢复完好存档后一切正常。
	if err := os.WriteFile(h.statePath(), good, 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("bill", "show", "c1", "2026-09")
}

func TestOldStateFileWithoutPlans(t *testing.T) {
	h := newHarness(t)
	// 旧格式存档：无 plans 字段，客户与账单均为固定单价。
	old := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "老客户", "price_fen": 10}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-01T00:00:00Z", "quantity": 3}},
  "bills": {}
}
`
	if err := os.WriteFile(h.statePath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "单价：10 分") || !strings.Contains(out, "总金额：30 分") {
		t.Fatalf("旧存档固定单价结算异常:\n%s", out)
	}
	// 重启（重新载入）后方案、绑定、账单与幂等保持。
	h.mustRun("plan", "add", "p1", "新方案", "10:1", "-:2")
	h.mustRun("customer", "add-plan", "c2", "新客户", "p1")
	show := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(show, "总金额：30 分") {
		t.Fatalf("旧账单重启后异常:\n%s", show)
	}
}

// --- 阶梯客户按月生效的方案变更 ---

func TestPlanChangeHappyPathAndSchedule(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "标准阶梯", "100:10", "-:5")
	h.mustRun("plan", "add", "p2", "续期阶梯", "50:20", "-:8")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")

	// 变更前查询：仅初始方案。
	out := h.mustRun("plan", "schedule", "c1")
	for _, want := range []string{"初始方案：p1（标准阶梯）", "方案变更：无"} {
		if !strings.Contains(out, want) {
			t.Fatalf("变更前安排查询缺少 %q:\n%s", want, out)
		}
	}

	out = h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期采用新价格")
	for _, want := range []string{"已登记方案变更", "生效月：2026-11", "目标方案：p2（续期阶梯）", "原因：续期采用新价格"} {
		if !strings.Contains(out, want) {
			t.Fatalf("登记变更输出缺少 %q:\n%s", want, out)
		}
	}

	// 安排查询：初始方案 + 按生效月排列的变更及原因。
	out = h.mustRun("plan", "schedule", "c1")
	if !strings.Contains(out, "初始方案：p1（标准阶梯）") ||
		!strings.Contains(out, "自 2026-11 起改用 p2（续期阶梯）") ||
		!strings.Contains(out, "原因：续期采用新价格") {
		t.Fatalf("安排查询输出异常:\n%s", out)
	}
	// 指定月份说明该月有效方案：生效月（含）起用新方案，之前月份不受影响。
	if out = h.mustRun("plan", "schedule", "c1", "2026-10"); !strings.Contains(out, "月份 2026-10 的有效方案：p1（标准阶梯）") {
		t.Fatalf("生效月前的有效方案异常:\n%s", out)
	}
	if out = h.mustRun("plan", "schedule", "c1", "2026-11"); !strings.Contains(out, "月份 2026-11 的有效方案：p2（续期阶梯）") {
		t.Fatalf("生效月的有效方案异常:\n%s", out)
	}
	if out = h.mustRun("plan", "schedule", "c1", "2027-03"); !strings.Contains(out, "月份 2027-03 的有效方案：p2（续期阶梯）") {
		t.Fatalf("生效月后的有效方案异常:\n%s", out)
	}
	// 固定单价客户无方案安排；不存在的客户拒绝。
	h.mustRun("customer", "add", "c9", "客户九", "10")
	if out = h.mustRun("plan", "schedule", "c9"); !strings.Contains(out, "固定单价客户") {
		t.Fatalf("固定单价客户安排查询异常:\n%s", out)
	}
	h.runExpectErr("plan", "schedule", "ghost")
	h.runExpectErr("plan", "schedule", "c1", "2026-13")
}

func TestPlanChangeSettlementAcrossMonths(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "旧阶梯", "100:10", "-:5")
	h.mustRun("plan", "add", "p2", "新阶梯", "100:1", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-10-05T00:00:00Z,60\n"+
		"u2,c1,2026-11-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)

	// 先按初始方案结算 2026-10 并封账：60×10=600。
	out := h.mustRun("bill", "settle", "c1", "2026-10")
	if !strings.Contains(out, "方案：p1（旧阶梯）") || !strings.Contains(out, "总金额：600 分") {
		t.Fatalf("变更前结算异常:\n%s", out)
	}
	// 登记自 2026-11 起的变更；生效月晚于已封账的 2026-10，合法。
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	// 2026-11 按新方案结算：60×1=60；账单保存实际使用方案的快照。
	out = h.mustRun("bill", "settle", "c1", "2026-11")
	for _, want := range []string{"方案：p2（新阶梯）", "第 1 档：累计上限 100，单价 1 分", "总金额：60 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("变更后结算缺少 %q:\n%s", want, out)
		}
	}
	// 历史账单不重算：bill show 与重复 settle 保留原快照、金额。
	show10 := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(show10, "方案：p1（旧阶梯）") || !strings.Contains(show10, "总金额：600 分") {
		t.Fatalf("历史账单被变更影响:\n%s", show10)
	}
	// 账后行为保持：调整、收款与流水在新旧账单上均正常。
	h.mustRun("bill", "adjust", "c1", "2026-11", "adj-1", "40", "补收")
	h.mustRun("bill", "pay", "c1", "2026-11", "pay-1", "100", "转账")
	ledger := h.mustRun("bill", "ledger", "c1", "2026-11")
	if !strings.Contains(ledger, "截止时余额：应付 100 分") {
		t.Fatalf("变更后账单流水异常:\n%s", ledger)
	}
}

func TestPlanChangeSettleOldMonthAfterChange(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "旧阶梯", "-:10")
	h.mustRun("plan", "add", "p2", "新阶梯", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-10-05T00:00:00Z,10\n"+
		"u2,c1,2026-12-05T00:00:00Z,10\n")
	h.mustRun("usage", "import", f)
	// 先登记变更，再结算生效月之前的月份：仍按初始方案，之前月份不受影响。
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	out := h.mustRun("bill", "settle", "c1", "2026-10")
	if !strings.Contains(out, "方案：p1（旧阶梯）") || !strings.Contains(out, "总金额：100 分") {
		t.Fatalf("变更后结算之前月份未用初始方案:\n%s", out)
	}
	out = h.mustRun("bill", "settle", "c1", "2026-12")
	if !strings.Contains(out, "方案：p2（新阶梯）") || !strings.Contains(out, "总金额：10 分") {
		t.Fatalf("变更后结算生效月之后月份未用新方案:\n%s", out)
	}
}

func TestPlanChangeIdempotentAndConflict(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "旧阶梯", "-:10")
	h.mustRun("plan", "add", "p2", "新阶梯", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")

	// 相同目标方案和原因的重放返回原记录，不重复生效。
	out := h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	if !strings.Contains(out, "内容相同，返回原记录") {
		t.Fatalf("重放未幂等返回:\n%s", out)
	}
	// 追加后续变更、封账之后重放仍返回原记录。
	h.mustRun("plan", "change", "c1", "2027-01", "p1", "恢复旧价")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-11")
	out = h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	if !strings.Contains(out, "内容相同，返回原记录") {
		t.Fatalf("封账后重放未幂等返回:\n%s", out)
	}
	// 同一项内容不同（目标方案或原因不同）拒绝，已有变更不可改写。
	if msg := h.runExpectErr("plan", "change", "c1", "2026-11", "p1", "续期新价"); !strings.Contains(msg, "不可改写") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("plan", "change", "c1", "2026-11", "p2", "另一个原因"); !strings.Contains(msg, "不可改写") {
		t.Fatal(msg)
	}
	// 其他客户可使用相同月份。
	h.mustRun("customer", "add-plan", "c2", "客户二", "p1")
	h.mustRun("plan", "change", "c2", "2026-11", "p2", "其他客户同期变更")
	// 安排不被重放与冲突影响。
	out = h.mustRun("plan", "schedule", "c1")
	if !strings.Contains(out, "自 2026-11 起改用 p2") || !strings.Contains(out, "自 2027-01 起改用 p1") {
		t.Fatalf("安排异常:\n%s", out)
	}
}

func TestPlanChangeValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "旧阶梯", "-:10")
	h.mustRun("plan", "add", "p2", "新阶梯", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "add", "c9", "客户九", "10")

	h.runExpectErr("plan", "change", "c1", "2026-13", "p2", "原因")    // 月份非法
	h.runExpectErr("plan", "change", "c1", "2026-1", "p2", "原因")     // 月份格式
	h.runExpectErr("plan", "change", "c1", "2026-11", "p2", "  ")    // 空原因
	h.runExpectErr("plan", "change", "ghost", "2026-11", "p2", "原因") // 客户不存在
	h.runExpectErr("plan", "change", "c1", "2026-11", "ghost", "原因") // 目标方案不存在
	// 仅限已绑定阶梯方案的客户。
	if msg := h.runExpectErr("plan", "change", "c9", "2026-11", "p2", "原因"); !strings.Contains(msg, "固定单价") {
		t.Fatal(msg)
	}
	// 失败不占用客户月份，可重试。
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	// 新变更只能按生效月递增追加。
	if msg := h.runExpectErr("plan", "change", "c1", "2026-10", "p1", "回退"); !strings.Contains(msg, "递增追加") {
		t.Fatal(msg)
	}
	// 生效月须晚于所有已封账月份。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-12-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-12")
	if msg := h.runExpectErr("plan", "change", "c1", "2026-12", "p1", "太晚"); !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
	// 晚于所有已封账月份且按生效月递增的变更合法。
	h.mustRun("plan", "change", "c1", "2027-01", "p1", "再次变更")
}

func TestPlanChangeOverflowPrecheck(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "cheap", "低价", "-:1")
	h.mustRun("plan", "add", "spike", "天价", "-:9223372036854775807")
	h.mustRun("customer", "add-plan", "c1", "客户一", "cheap")
	// 生效月起已导入的未封账用量按新方案单条从零计价溢出：整项拒绝并指出用量。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	msg := h.runExpectErr("plan", "change", "c1", "2026-11", "spike", "涨价")
	if !strings.Contains(msg, "u1") || !strings.Contains(msg, "溢出") || !strings.Contains(msg, "整项变更拒绝") {
		t.Fatal(msg)
	}
	// 用量未被修改，变更未生效：重复导入仍按重复跳过，安排无变更。
	if out := h.mustRun("usage", "import", f); !strings.Contains(out, "新增 0 条，重复跳过 1 条") {
		t.Fatalf("预检失败后用量状态变化:\n%s", out)
	}
	if out := h.mustRun("plan", "schedule", "c1"); !strings.Contains(out, "方案变更：无") {
		t.Fatalf("预检失败后变更已生效:\n%s", out)
	}
	// 生效月之前的用量不参与预检：2026-12 起变更合法（u1 在 2026-11）。
	h.mustRun("plan", "change", "c1", "2026-12", "spike", "涨价")
}

func TestPlanChangeImportUsesEffectivePlan(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "spike", "天价", "-:9223372036854775807")
	h.mustRun("plan", "add", "cheap", "低价", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "spike")
	h.mustRun("plan", "change", "c1", "2026-11", "cheap", "续期降价")
	// 后续导入按记录时间换算的 UTC 月份选择方案：2026-11 起用低价方案。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	// 生效月之前仍按初始（天价）方案预检：单条溢出，整批拒绝。
	f2 := h.writeFile("u2.csv", csvHeader+"u2,c1,2026-10-05T00:00:00Z,2\n")
	if msg := h.runExpectErr("usage", "import", f2); !strings.Contains(msg, "溢出") || !strings.Contains(msg, "整批未生效") {
		t.Fatal(msg)
	}
}

func TestPlanChangeDoesNotConsumeSeq(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("plan", "add", "p2", "新阶梯", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")
	// 方案变更不产生账后流水事件、不占用全局操作序号。
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "5", "补收")
	ledger := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(ledger, "存档全局序号上限：1") || !strings.Contains(ledger, "序号 1 调整 adj-1") {
		t.Fatalf("方案变更占用了操作序号:\n%s", ledger)
	}
}

func TestPlanChangeCorruptStateRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "旧阶梯", "-:10")
	h.mustRun("plan", "add", "p2", "新阶梯", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-11")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 篡改点都锚定在 plan_changes 节（键 + 6 空格缩进与账单字段区分）。
	cases := []struct{ old, new string }{
		{"\"month\": \"2026-11\",\n      \"plan_id\": \"p2\"",
			"\"month\": \"2026-11\",\n      \"plan_id\": \"ghost\""}, // 变更引用失效的方案
		{"\"c1|2026-11\": {\n      \"customer_id\": \"c1\",\n      \"month\": \"2026-11\"",
			"\"c1|2026-11\": {\n      \"customer_id\": \"c1\",\n      \"month\": \"2026-13\""}, // 变更生效月非法
		{`"reason": "续期新价"`, `"reason": "  "`}, // 变更原因为空
		{"\"c1|2026-11\": {\n      \"customer_id\": \"c1\"",
			"\"c1|2026-11\": {\n      \"customer_id\": \"ghost\""}, // 变更引用失效的客户
	}
	for _, tc := range cases {
		broken := strings.Replace(string(good), tc.old, tc.new, 1)
		if broken == string(good) {
			t.Fatalf("替换 %q 未生效", tc.old)
		}
		if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := h.runExpectErr("plan", "schedule", "c1")
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("篡改 %q 未报损坏: %s", tc.old, msg)
		}
		got, _ := os.ReadFile(h.statePath())
		if string(got) != broken {
			t.Fatalf("篡改 %q 后文件被改写", tc.old)
		}
	}
	// 账单方案须符合账期安排：把变更移到 2026-12 后，2026-11 的安排回到
	// 初始方案 p1，而已封账账单仍是 p2，不一致即损坏。
	broken := strings.Replace(string(good),
		"\"c1|2026-11\": {\n      \"customer_id\": \"c1\",\n      \"month\": \"2026-11\"",
		"\"c1|2026-12\": {\n      \"customer_id\": \"c1\",\n      \"month\": \"2026-12\"", 1)
	if broken == string(good) {
		t.Fatal("变更月份替换未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-11"); !strings.Contains(msg, "损坏") {
		t.Fatalf("账单方案与账期安排不一致未报损坏: %s", msg)
	}
	// 恢复完好存档后一切正常。
	if err := os.WriteFile(h.statePath(), good, 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("plan", "schedule", "c1")
	h.mustRun("bill", "show", "c1", "2026-11")
}

func TestOldStateFileWithoutPlanChanges(t *testing.T) {
	h := newHarness(t)
	// 旧格式存档：有方案与阶梯客户，但无 plan_changes 字段，沿用原绑定。
	old := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "老客户", "price_fen": 0, "plan_id": "p1"}},
  "plans": {"p1": {"id": "p1", "name": "阶梯", "tiers": [{"limit": 0, "price_fen": 10}]}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-01T00:00:00Z", "quantity": 3}},
  "bills": {}
}
`
	if err := os.WriteFile(h.statePath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("plan", "schedule", "c1")
	if !strings.Contains(out, "初始方案：p1（阶梯）") || !strings.Contains(out, "方案变更：无") {
		t.Fatalf("旧存档安排查询异常:\n%s", out)
	}
	out = h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "方案：p1（阶梯）") || !strings.Contains(out, "总金额：30 分") {
		t.Fatalf("旧存档结算异常:\n%s", out)
	}
	// 重启（重新载入）后安排、幂等与历史账单校验保持。
	h.mustRun("plan", "add", "p2", "新阶梯", "-:1")
	h.mustRun("plan", "change", "c1", "2026-10", "p2", "续期新价")
	show := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(show, "方案：p1（阶梯）") || !strings.Contains(show, "总金额：30 分") {
		t.Fatalf("旧账单在变更后异常:\n%s", show)
	}
}

// --- 按客户账期清单整批结算（bill settle-batch） ---

// 混合清单：固定单价、阶梯、零费用客户与多月配对，含一项预先单笔结算的
// 已有账单；整批一次确认，同时生成账单并封账。
func TestSettleBatchMixedHappyPath(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "150")
	h.mustRun("customer", "add", "c2", "零费", "0")
	h.mustRun("plan", "add", "p1", "阶梯", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c3", "丙方", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-10T00:00:00Z,2\n"+ // c1 9月 300
		"u2,c1,2026-10-05T00:00:00Z,3\n"+ // c1 10月 450
		"u3,c3,2026-09-15T10:00:00Z,150\n"+ // c3 9月 100*10+50*5=1250
		"u4,c2,2026-09-01T00:00:00Z,7\n", // c2 9月 0（零费用正常封账）
	)
	h.mustRun("usage", "import", f)

	// 预先单笔结算一项，批量中应作为“已有”返回且不重新计费。
	single := h.mustRun("bill", "settle", "c1", "2026-10")
	singleID := extractBillID(single)

	out := h.mustRun("bill", "settle-batch",
		"c1", "2026-09", "c3", "2026-09", "c2", "2026-09", "c1", "2026-10")
	for _, want := range []string{
		"共 4 项", "新增账单 3 张", "已有账单 1 张",
		"1. 客户 c1 月份 2026-09", "原总金额 300 分", "[新增（已封账）]",
		"2. 客户 c3 月份 2026-09", "原总金额 1250 分",
		"3. 客户 c2 月份 2026-09", "原总金额 0 分",
		"4. 客户 c1 月份 2026-10", "原总金额 450 分", "[已有（返回原账单，不重新计费）]",
		singleID,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("批量结算输出缺少 %q:\n%s", want, out)
		}
	}

	// 账单标识与单笔结算一致（由客户与月份确定），完整明细可读回。
	if id := extractBillID(h.mustRun("bill", "settle", "c1", "2026-09")); id == "" || id == singleID {
		t.Fatalf("账单标识异常: %q", id)
	}
	show := h.mustRun("bill", "show", "c3", "2026-09")
	for _, want := range []string{"计价类型：阶梯计费", "方案：p1（阶梯）", "分段 1：第 1 档 数量=100", "分段 2：第 2 档 数量=50", "总金额：1250 分"} {
		if !strings.Contains(show, want) {
			t.Fatalf("阶梯账单明细缺少 %q:\n%s", want, show)
		}
	}

	// 新封账月份拒绝新用量；相同用量重放仍跳过。
	more := h.writeFile("more.csv", csvHeader+
		"u1,c1,2026-09-10T00:00:00Z,2\n"+ // 重放，跳过
		"u9,c1,2026-09-20T00:00:00Z,5\n", // 新用量进入已封账月份，拒绝
	)
	msg := h.runExpectErr("usage", "import", more)
	if !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
	replay := h.writeFile("replay.csv", csvHeader+"u1,c1,2026-09-10T00:00:00Z,2\n")
	if out := h.mustRun("usage", "import", replay); !strings.Contains(out, "重复跳过 1 条") {
		t.Fatalf("封账月重放应跳过:\n%s", out)
	}

	// 批量结算不占用操作序号：第一笔账后操作序号为 1。
	adj := h.mustRun("bill", "adjust", "c1", "2026-09", "a1", "60", "补收")
	if !strings.Contains(adj, "已登记补收调整") {
		t.Fatal(adj)
	}
	ledger := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(ledger, "存档全局序号上限：1") || !strings.Contains(ledger, "序号 1 调整 a1") {
		t.Fatalf("批量结算不应占用操作序号:\n%s", ledger)
	}
}

// 清单任一项非法（月份无效、客户不存在、同客户同月重复、无用量、计价溢出）
// 都拒绝整批：不留下任何新账单，也不封闭任何原本未封账的月份。
func TestSettleBatchRejectsWholeOnAnyProblem(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "150")
	h.mustRun("customer", "add", "big", "大量", "1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-10T00:00:00Z,2\n"+
		"u2,big,2026-09-10T00:00:00Z,9223372036854775807\n"+
		"u3,big,2026-09-11T00:00:00Z,1\n", // 汇总数量溢出
	)
	h.mustRun("usage", "import", f)

	// 清单中 c1 2026-09 完全合法，但其余各项非法，整批必须拒绝。
	msg := h.runExpectErr("bill", "settle-batch",
		"c1", "2026-09", // 合法
		"c1", "2026-13", // 月份无效
		"ghost", "2026-09", // 客户不存在
		"c1", "2026-10", // 无用量
		"big", "2026-09", // 计价溢出
		"c1", "2026-09", // 与第 1 项重复
	)
	for _, want := range []string{
		"整批未生效", "第 1 项", "第 2 项", "月份无效", "第 3 项", "客户不存在",
		"第 4 项", "没有用量", "第 5 项", "溢出", "第 6 项", "重复",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("拒绝报告缺少 %q:\n%s", want, msg)
		}
	}

	// 不留下任何新账单、不封闭任何月份：合法项仍可单独结算。
	h.runExpectErr("bill", "show", "c1", "2026-09")
	h.mustRun("bill", "settle", "c1", "2026-09")
	// 溢出项单独结算同样拒绝且不封账。
	h.runExpectErr("bill", "settle", "big", "2026-09")
}

// 空清单、奇数参数为用法错误（退出码 2）。
func TestSettleBatchUsageErrors(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"bill", "settle-batch"},
		{"bill", "settle-batch", "c1"},
		{"bill", "settle-batch", "c1", "2026-09", "c2"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}
}

// 重复提交与清单换序不重建账单；全部已结算时成功返回且不改写存档。
func TestSettleBatchIdempotentAndOrderIndependent(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	h.mustRun("customer", "add", "c2", "乙方", "200")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-10T00:00:00Z,1\n"+
		"u2,c2,2026-09-10T00:00:00Z,1\n",
	)
	h.mustRun("usage", "import", f)

	first := h.mustRun("bill", "settle-batch", "c1", "2026-09", "c2", "2026-09")
	if !strings.Contains(first, "新增账单 2 张") {
		t.Fatal(first)
	}
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 换序重复提交：全部已有，成功返回且不改写存档。
	second := h.mustRun("bill", "settle-batch", "c2", "2026-09", "c1", "2026-09")
	if !strings.Contains(second, "新增账单 0 张") || !strings.Contains(second, "已有账单 2 张") ||
		!strings.Contains(second, "未改写存档") {
		t.Fatalf("重复提交应全部已有:\n%s", second)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("全部已结算时不应改写存档")
	}
	// 换序后账单标识不变（按清单位置逐项列出，标识由客户与月份确定）。
	if !strings.Contains(second, "1. 客户 c2 月份 2026-09") || !strings.Contains(second, "2. 客户 c1 月份 2026-09") {
		t.Fatalf("换序后清单顺序异常:\n%s", second)
	}
}

// 阶梯客户跨方案变更月份整批结算：各账期独立采用当月有效方案，从零累计。
func TestSettleBatchTieredAcrossPlanChange(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "旧价", "100:10", "-:8")
	h.mustRun("plan", "add", "p2", "新价", "50:20", "-:15")
	h.mustRun("customer", "add-plan", "c1", "阶梯客户", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-10T00:00:00Z,150\n"+ // 9月旧价：100*10+50*8=1400
		"u2,c1,2026-10-10T00:00:00Z,60\n", // 10月新价：50*20+10*15=1150
	)
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "change", "c1", "2026-10", "p2", "续期新价")

	out := h.mustRun("bill", "settle-batch", "c1", "2026-09", "c1", "2026-10")
	if !strings.Contains(out, "新增账单 2 张") ||
		!strings.Contains(out, "1. 客户 c1 月份 2026-09") || !strings.Contains(out, "原总金额 1400 分") ||
		!strings.Contains(out, "2. 客户 c1 月份 2026-10") || !strings.Contains(out, "原总金额 1150 分") {
		t.Fatalf("跨方案变更批量结算异常:\n%s", out)
	}
	sep := h.mustRun("bill", "show", "c1", "2026-09")
	oct := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(sep, "方案：p1（旧价）") || !strings.Contains(oct, "方案：p2（新价）") {
		t.Fatalf("各账期应采用当月有效方案:\n9月:\n%s\n10月:\n%s", sep, oct)
	}
}

// 保存失败整批不生效：原有账单、用量与其他状态不变，排除故障后原清单可重试。
func TestSettleBatchSaveFailureRollsBack(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	h.mustRun("customer", "add", "c2", "乙方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-10T00:00:00Z,1\n"+
		"u2,c2,2026-09-10T00:00:00Z,1\n",
	)
	h.mustRun("usage", "import", f)
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 数据目录置为只读使保存失败。
	if err := os.Chmod(h.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	msg := h.runExpectErr("bill", "settle-batch", "c1", "2026-09", "c2", "2026-09")
	if !strings.Contains(msg, "保存失败") {
		t.Fatalf("应报告保存失败: %s", msg)
	}
	if err := os.Chmod(h.dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 原状态不变，无部分结算。
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("保存失败不应改变存档")
	}
	h.runExpectErr("bill", "show", "c1", "2026-09")
	h.runExpectErr("bill", "show", "c2", "2026-09")

	// 排除故障后原清单可重试并成功。
	out := h.mustRun("bill", "settle-batch", "c1", "2026-09", "c2", "2026-09")
	if !strings.Contains(out, "新增账单 2 张") {
		t.Fatalf("重试应成功:\n%s", out)
	}
}

// 完整 JSON 之后的任何非空白内容（包括单独的 ] 或 }）都按损坏拒绝并保留
// 原文件；合法尾随空白仍可读取。
func TestTrailingContentAfterJSONRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	for _, suffix := range []string{"]", "}", "{}", " null", "\n]\n"} {
		if err := os.WriteFile(h.statePath(), append(append([]byte{}, good...), suffix...), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := h.runExpectErr("bill", "show", "c1", "2026-09")
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("尾随 %q 未报损坏: %s", suffix, msg)
		}
		got, err := os.ReadFile(h.statePath())
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(append(append([]byte{}, good...), suffix...)) {
			t.Fatalf("尾随 %q 的损坏文件被改写", suffix)
		}
	}

	// 合法尾随空白仍可读取。
	if err := os.WriteFile(h.statePath(), append(append([]byte{}, good...), " \t\n\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("plan", "list")
}

// --- 阶梯方案的固定月费 ---

func TestPlanAddFeeCreateShowList(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "100:10", "-:5")
	for _, want := range []string{"方案标识：sub", "方案名称：订阅阶梯", "月费：1000 分", "第 1 档：累计上限 100，单价 10 分", "第 2 档：累计数量无上限，单价 5 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan add-fee 输出缺少 %q:\n%s", want, out)
		}
	}
	show := h.mustRun("plan", "show", "sub")
	if !strings.Contains(show, "月费：1000 分") {
		t.Fatalf("plan show 未展示月费:\n%s", show)
	}
	list := h.mustRun("plan", "list")
	if !strings.Contains(list, "sub（订阅阶梯）：月费 1000 分，2 档，规则 100:10 -:5") {
		t.Fatalf("plan list 未展示月费:\n%s", list)
	}
	// 省略月费的 plan add 视为月费 0。
	h.mustRun("plan", "add", "plain", "普通阶梯", "-:5")
	if show = h.mustRun("plan", "show", "plain"); !strings.Contains(show, "月费：0 分") {
		t.Fatalf("plan add 省略月费应为 0:\n%s", show)
	}
	// 客户方案安排展示月费。
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	sched := h.mustRun("plan", "schedule", "c1")
	if !strings.Contains(sched, "初始方案：sub（订阅阶梯）：月费 1000 分") {
		t.Fatalf("plan schedule 未展示月费:\n%s", sched)
	}
}

func TestPlanAddFeeValidation(t *testing.T) {
	h := newHarness(t)
	cases := [][]string{
		{"plan", "add-fee", "p1", "名称", "-1", "-:5"},         // 月费为负
		{"plan", "add-fee", "p1", "名称", "abc", "-:5"},        // 月费非整数
		{"plan", "add-fee", "p1", "名称", "1.5", "-:5"},        // 月费非整数
		{"plan", "add-fee", "p1", "名称", "100"},               // 缺少阶梯
		{"plan", "add-fee", "p1", "名称", "100", "5:1"},        // 最后一档有上限
		{"plan", "add-fee", "", "名称", "100", "-:5"},          // 空标识
		{"plan", "add-fee", "p1", "", "100", "-:5"},          // 空名称
		{"plan", "add-fee", "p1", "名称", "100", "-:5", "bad"}, // 阶梯格式非法
	}
	for _, args := range cases {
		h.runExpectErr(args...)
	}
	// 失败的新增不占标识。
	if msg := h.runExpectErr("plan", "show", "p1"); !strings.Contains(msg, "不存在") {
		t.Fatal(msg)
	}
	h.mustRun("plan", "add-fee", "p1", "名称", "100", "-:5")
	// 重复方案标识拒绝（两个入口都不可复用），方案创建后不可修改。
	h.runExpectErr("plan", "add-fee", "p1", "另一个", "200", "-:9")
	h.runExpectErr("plan", "add", "p1", "另一个", "-:9")
	show := h.mustRun("plan", "show", "p1")
	if !strings.Contains(show, "月费：100 分") {
		t.Fatalf("方案被重复登记改写:\n%s", show)
	}
}

func TestMonthlyFeeSettleWithUsage(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-01T00:00:00Z,60\n"+
		"u2,c1,2026-09-02T00:00:00Z,80\n")
	h.mustRun("usage", "import", f)

	// 原总金额 = 月费 1000 + 全月用量费 1200 = 2200；月费不计入数量与各档金额。
	out1 := h.mustRun("bill", "settle", "c1", "2026-09")
	for _, want := range []string{
		"计价类型：阶梯计费",
		"方案：sub（订阅阶梯）",
		"总数量：140",
		"月费：1000 分",
		"用量费：1200 分",
		"原总金额：2200 分",
		"第 1 档：数量 100，单价 10 分，金额 1000 分",
		"第 2 档：数量 40，单价 5 分，金额 200 分",
		"用量标识=u1 时间=2026-09-01T00:00:00Z 数量=60 小计=600 分",
	} {
		if !strings.Contains(out1, want) {
			t.Fatalf("含月费账单输出缺少 %q:\n%s", want, out1)
		}
	}

	// bill show 与重复 bill settle 返回相同明细（含月费、用量费、原总金额）。
	idx := strings.Index(out1, "\n\n")
	if idx < 0 {
		t.Fatalf("结算输出缺少账单部分:\n%s", out1)
	}
	billText := out1[idx+2:]
	out2 := h.mustRun("bill", "settle", "c1", "2026-09")
	show := h.mustRun("bill", "show", "c1", "2026-09")
	if show != billText || !strings.HasSuffix(out2, billText) {
		t.Fatalf("重复结算/查询明细不一致:\nsettle1=%q\nsettle2=%q\nshow=%q", out1, out2, show)
	}

	// 调整、收款与截止流水以包含月费的原总金额为基础。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "100", "补收")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "2300", "转账")
	show = h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{"原总金额：2200 分", "当前应付：2300 分", "实收：2300 分", "未收余额：0 分"} {
		if !strings.Contains(show, want) {
			t.Fatalf("账后信息缺少 %q:\n%s", want, show)
		}
	}
	ledger := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(ledger, "原总金额：2200 分") || !strings.Contains(ledger, "初始余额：应付 2200 分") ||
		!strings.Contains(ledger, "截止时余额：应付 2300 分") || !strings.Contains(ledger, "未收余额 0 分") {
		t.Fatalf("含月费账单流水异常:\n%s", ledger)
	}
	// 超额收款拒绝：0 ≤ 实收 ≤ 应付 以含月费的原总金额为基础。
	h.runExpectErr("bill", "pay", "c1", "2026-09", "pay-2", "1", "超额")
}

func TestMonthlyFeeSettleWithoutUsage(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "500", "-:5")
	h.mustRun("plan", "add", "free", "无月费阶梯", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "add-plan", "c2", "客户二", "free")
	h.mustRun("customer", "add", "c3", "客户三", "10")

	// 月费大于 0：无用量也生成账单并封账，总数量 0、空明细、用量费 0。
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	for _, want := range []string{"已封账", "总数量：0", "月费：500 分", "用量费：0 分", "原总金额：500 分", "明细：无"} {
		if !strings.Contains(out, want) {
			t.Fatalf("无用量月费账单输出缺少 %q:\n%s", want, out)
		}
	}
	// 重复结算幂等返回原账单。
	out2 := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out2, "已结算，返回原账单") || !strings.Contains(out2, "原总金额：500 分") {
		t.Fatalf("重复结算异常:\n%s", out2)
	}
	// 封账后新用量不得进入。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-01T00:00:00Z,1\n")
	if msg := h.runExpectErr("usage", "import", f); !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
	// 月费为 0 的方案与固定单价客户仍在无用量时拒绝且不封账。
	if msg := h.runExpectErr("bill", "settle", "c2", "2026-09"); !strings.Contains(msg, "没有用量") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "settle", "c3", "2026-09"); !strings.Contains(msg, "没有用量") {
		t.Fatal(msg)
	}
}

func TestMonthlyFeeSettleBatch(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "500", "-:5")
	h.mustRun("plan", "add", "free", "无月费阶梯", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "add-plan", "c2", "客户二", "free")
	h.mustRun("customer", "add", "c3", "客户三", "10")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c2,2026-09-01T00:00:00Z,7\n"+
		"u2,c3,2026-09-01T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c3", "2026-09")

	// 混合清单：含月费方案（无用量）、旧方案（有用量）与已有账单。
	out := h.mustRun("bill", "settle-batch", "c1", "2026-09", "c2", "2026-09", "c3", "2026-09")
	for _, want := range []string{"新增账单 2 张", "已有账单 1 张", "客户 c1 月份 2026-09", "原总金额 500 分", "原总金额 35 分", "原总金额 30 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("批量结算输出缺少 %q:\n%s", want, out)
		}
	}
	show := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(show, "月费：500 分") || !strings.Contains(show, "明细：无") {
		t.Fatalf("批量生成的月费账单异常:\n%s", show)
	}

	// 任一项失败整批不生效：c2 的 2026-10 无用量且月费为 0，整批拒绝，
	// c1 的 2026-10（本可出账）也不封账。
	msg := h.runExpectErr("bill", "settle-batch", "c1", "2026-10", "c2", "2026-10")
	if !strings.Contains(msg, "整批未生效") || !strings.Contains(msg, "没有用量") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-10"); !strings.Contains(msg, "尚无账单") {
		t.Fatal(msg)
	}
	// 修正清单后可原样重试。
	h.mustRun("bill", "settle-batch", "c1", "2026-10")
}

func TestMonthlyFeeOverflowRejectsWithoutSealing(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "天价月费", "9223372036854775807", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-01T00:00:00Z,1\n")
	h.mustRun("usage", "import", f)
	// 月费加全月用量费溢出：结算拒绝且不封账。
	msg := h.runExpectErr("bill", "settle", "c1", "2026-09")
	if !strings.Contains(msg, "溢出") || !strings.Contains(msg, "不封账") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "尚无账单") {
		t.Fatal(msg)
	}
	// 批量结算同样拒绝且整批不生效。
	msg = h.runExpectErr("bill", "settle-batch", "c1", "2026-09")
	if !strings.Contains(msg, "溢出") || !strings.Contains(msg, "整批未生效") {
		t.Fatal(msg)
	}
}

func TestMonthlyFeePlanChange(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "old", "旧订阅", "100", "-:10")
	h.mustRun("plan", "add-fee", "new", "新订阅", "200", "-:20")
	h.mustRun("customer", "add-plan", "c1", "客户一", "old")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-01T00:00:00Z,2\n"+
		"u2,c1,2026-11-01T00:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "change", "c1", "2026-11", "new", "续期涨价")

	// 方案变更同时切换月费与阶梯规则；历史账单不重算。
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "月费：100 分") || !strings.Contains(out, "用量费：20 分") || !strings.Contains(out, "原总金额：120 分") {
		t.Fatalf("变更前月份账单异常:\n%s", out)
	}
	out = h.mustRun("bill", "settle", "c1", "2026-11")
	if !strings.Contains(out, "月费：200 分") || !strings.Contains(out, "用量费：40 分") || !strings.Contains(out, "原总金额：240 分") {
		t.Fatalf("变更后月份账单异常:\n%s", out)
	}
	// 无变更覆盖的月份仍按初始方案月费出账（无用量也须出账）。
	out = h.mustRun("bill", "settle", "c1", "2026-10")
	if !strings.Contains(out, "月费：100 分") || !strings.Contains(out, "原总金额：100 分") || !strings.Contains(out, "明细：无") {
		t.Fatalf("中间月份账单异常:\n%s", out)
	}
	// 安排查询展示各方案月费。
	sched := h.mustRun("plan", "schedule", "c1", "2026-11")
	if !strings.Contains(sched, "初始方案：old（旧订阅）：月费 100 分") ||
		!strings.Contains(sched, "自 2026-11 起改用 new（新订阅）：月费 200 分") ||
		!strings.Contains(sched, "月份 2026-11 的有效方案：new（新订阅）：月费 200 分") {
		t.Fatalf("安排查询未展示月费:\n%s", sched)
	}
	// 历史账单保持原快照。
	show := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(show, "月费：100 分") || !strings.Contains(show, "原总金额：120 分") {
		t.Fatalf("历史账单被重算:\n%s", show)
	}
}

func TestMonthlyFeeCorruptStateRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "add", "c2", "客户二", "10")
	f := h.writeFile("u.csv", csvHeader+"u1,c2,2026-09-01T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09") // 无用量月费账单：月费 1000，空明细
	h.mustRun("bill", "settle", "c2", "2026-09") // 固定单价账单：30 分

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 每条替换针对存档中唯一或首个出现的片段（方案在账单之前序列化）。
	cases := []struct{ old, new string }{
		{`"monthly_fee_fen": 1000`, `"monthly_fee_fen": -1`},    // 方案月费为负
		{`"total_fee_fen": 1000`, `"total_fee_fen": 1001`},      // 总金额不等于月费加用量费
		{`"monthly_fee_fen": 1000,`, `"monthly_fee_fen": 999,`}, // 方案月费被改动，账单快照与之不符
	}
	for _, tc := range cases {
		broken := strings.Replace(string(good), tc.old, tc.new, 1)
		if broken == string(good) {
			t.Fatalf("替换 %q 未生效", tc.old)
		}
		if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := h.runExpectErr("bill", "show", "c1", "2026-09")
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("篡改 %q 未报损坏: %s", tc.old, msg)
		}
		got, _ := os.ReadFile(h.statePath())
		if string(got) != broken {
			t.Fatalf("篡改 %q 后文件被改写", tc.old)
		}
	}
	if err := os.WriteFile(h.statePath(), good, 0o644); err != nil {
		t.Fatal(err)
	}

	// 手工构造的非法存档：月费为 0 却空明细；固定单价账单携带月费。
	manual := []string{
		// 月费为 0 的账单不允许空用量明细。
		`{"version":1,"customers":{"c1":{"id":"c1","name":"客户一","price_fen":0,"plan_id":"p1"}},
		  "plans":{"p1":{"id":"p1","name":"阶梯","tiers":[{"limit":0,"price_fen":5}]}},
		  "usage":{},"bills":{"c1|2026-09":{"id":"B1","customer_id":"c1","month":"2026-09","pricing":"tiered",
		  "plan_id":"p1","plan_name":"阶梯","plan_tiers":[{"limit":0,"price_fen":5}],
		  "tier_totals":[{"quantity":0,"fee_fen":0}],"total_quantity":0,"unit_price_fen":0,
		  "total_fee_fen":0,"lines":[],"created_at":"2026-10-01T00:00:00Z"}}}`,
		// 固定单价账单不得携带月费。
		`{"version":1,"customers":{"c1":{"id":"c1","name":"客户一","price_fen":10}},
		  "plans":{},"usage":{"u1":{"id":"u1","customer_id":"c1","time":"2026-09-01T00:00:00Z","quantity":3}},
		  "bills":{"c1|2026-09":{"id":"B1","customer_id":"c1","month":"2026-09","monthly_fee_fen":5,
		  "total_quantity":3,"unit_price_fen":10,"total_fee_fen":35,
		  "lines":[{"usage_id":"u1","time":"2026-09-01T00:00:00Z","quantity":3,"line_fee_fen":30}],
		  "created_at":"2026-10-01T00:00:00Z"}}}`,
	}
	for i, bad := range manual {
		if err := os.WriteFile(h.statePath(), []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := h.runExpectErr("bill", "show", "c1", "2026-09")
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("手工非法存档 %d 未报损坏: %s", i, msg)
		}
	}
	if err := os.WriteFile(h.statePath(), good, 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("bill", "show", "c1", "2026-09")
}

func TestMonthlyFeeOldStateFile(t *testing.T) {
	h := newHarness(t)
	// 旧格式存档：方案与账单均无 monthly_fee_fen 字段，按 0 读取。
	old := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "老客户", "price_fen": 0, "plan_id": "p1"}},
  "plans": {"p1": {"id": "p1", "name": "旧阶梯", "tiers": [{"limit": 0, "price_fen": 5}]}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-01T00:00:00Z", "quantity": 3}},
  "bills": {}
}
`
	if err := os.WriteFile(h.statePath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "月费：0 分") || !strings.Contains(out, "用量费：15 分") || !strings.Contains(out, "原总金额：15 分") {
		t.Fatalf("旧存档结算异常:\n%s", out)
	}
	// 月费按 0 读取：无用量月份仍拒绝结算。
	if msg := h.runExpectErr("bill", "settle", "c1", "2026-10"); !strings.Contains(msg, "没有用量") {
		t.Fatal(msg)
	}
	// 重启后快照与幂等保持。
	show := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(show, "原总金额：15 分") {
		t.Fatalf("旧账单重启后异常:\n%s", show)
	}
}

// --- 订阅暂停 ---

func TestPauseAddAndList(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "add-plan", "c2", "客户二", "sub")
	h.mustRun("customer", "add", "c9", "客户九", "10")

	// 查询为空：全部月份正常服务。
	out := h.mustRun("pause", "list", "c1")
	if !strings.Contains(out, "暂停区间：无") || !strings.Contains(out, "全部月份正常服务") {
		t.Fatalf("空暂停查询异常:\n%s", out)
	}
	if out = h.mustRun("pause", "list", "c1", "2027-01"); !strings.Contains(out, "月份 2027-01：正常服务") {
		t.Fatalf("月份查询异常:\n%s", out)
	}

	// 登记暂停区间 [2027-01, 2027-03)。
	out = h.mustRun("pause", "add", "c1", "2027-01", "2027-03", "客户停用设备")
	for _, want := range []string{"已登记订阅暂停", "客户：c1", "暂停区间：[2027-01, 2027-03)", "原因：客户停用设备"} {
		if !strings.Contains(out, want) {
			t.Fatalf("登记暂停输出缺少 %q:\n%s", want, out)
		}
	}
	// 相接的第二段视为连续暂停，合法。
	h.mustRun("pause", "add", "c1", "2027-03", "2027-05", "延长停用")

	// 列表按起月升序展示全部区间与原因。
	out = h.mustRun("pause", "list", "c1")
	i1 := strings.Index(out, "[2027-01, 2027-03)，原因：客户停用设备")
	i2 := strings.Index(out, "[2027-03, 2027-05)，原因：延长停用")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("暂停列表未按起月升序或缺少原因:\n%s", out)
	}
	// 月份查询：含起月、不含结束月，区间外正常服务。
	if out = h.mustRun("pause", "list", "c1", "2027-01"); !strings.Contains(out, "月份 2027-01：暂停中") {
		t.Fatalf("起月应暂停:\n%s", out)
	}
	if out = h.mustRun("pause", "list", "c1", "2027-04"); !strings.Contains(out, "月份 2027-04：暂停中") {
		t.Fatalf("相接区间内应暂停:\n%s", out)
	}
	if out = h.mustRun("pause", "list", "c1", "2027-05"); !strings.Contains(out, "月份 2027-05：正常服务") {
		t.Fatalf("结束月（不含）应正常服务:\n%s", out)
	}
	if out = h.mustRun("pause", "list", "c1", "2026-12"); !strings.Contains(out, "月份 2026-12：正常服务") {
		t.Fatalf("区间前应正常服务:\n%s", out)
	}
	// 固定单价客户不适用暂停；其他客户互不影响。
	if out = h.mustRun("pause", "list", "c9"); !strings.Contains(out, "固定单价客户") || !strings.Contains(out, "不适用订阅暂停") {
		t.Fatalf("固定单价客户查询异常:\n%s", out)
	}
	if out = h.mustRun("pause", "list", "c2", "2027-02"); !strings.Contains(out, "月份 2027-02：正常服务") {
		t.Fatalf("其他客户不应受影响:\n%s", out)
	}
	// 非法输入：不存在的客户、非法月份、结束月不晚于起月、空原因。
	h.runExpectErr("pause", "list", "ghost")
	h.runExpectErr("pause", "list", "c1", "2027-13")
	h.runExpectErr("pause", "add", "ghost", "2027-06", "2027-08", "原因")
	h.runExpectErr("pause", "add", "c9", "2027-06", "2027-08", "原因") // 固定单价客户
	h.runExpectErr("pause", "add", "c1", "2027-13", "2027-08", "原因")
	h.runExpectErr("pause", "add", "c1", "2027-06", "2027-06", "原因")
	h.runExpectErr("pause", "add", "c1", "2027-06", "2027-05", "原因")
	h.runExpectErr("pause", "add", "c1", "2027-06", "2027-08", "  ")
	// 失败不占用客户起月：修正后可原样重试。
	h.mustRun("pause", "add", "c1", "2027-06", "2027-08", "再次停用")
}

func TestPauseOverlapAndSealedRules(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "add-plan", "c2", "客户二", "p1")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")

	// 起月须晚于所有已封账月份。
	if msg := h.runExpectErr("pause", "add", "c1", "2026-09", "2026-11", "停用"); !strings.Contains(msg, "已封账") {
		t.Fatalf("起月等于封账月应拒绝: %s", msg)
	}
	if msg := h.runExpectErr("pause", "add", "c1", "2026-08", "2026-10", "停用"); !strings.Contains(msg, "已封账") {
		t.Fatalf("起月早于封账月应拒绝: %s", msg)
	}
	h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")

	// 同一客户区间不得重叠；可以相接。
	h.runExpectErr("pause", "add", "c1", "2026-11", "2027-02", "重叠")
	h.runExpectErr("pause", "add", "c1", "2026-09", "2026-11", "重叠")
	h.runExpectErr("pause", "add", "c1", "2026-10", "2026-12", "相同区间不同原因")
	h.mustRun("pause", "add", "c1", "2026-12", "2027-01", "相接延长")
	// 其他客户相同区间互不影响。
	h.mustRun("pause", "add", "c2", "2026-10", "2026-12", "另一客户")
}

func TestPauseIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "500", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")

	// 相同结束月与原因的重放返回原记录，不写盘。
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatalf("重放应幂等返回:\n%s", out)
	}
	after, _ := os.ReadFile(h.statePath())
	if string(before) != string(after) {
		t.Fatal("幂等重放改写了存档")
	}
	// 内容不同（结束月或原因不同）拒绝。
	h.runExpectErr("pause", "add", "c1", "2026-10", "2026-11", "停用")
	h.runExpectErr("pause", "add", "c1", "2026-10", "2026-12", "别的")

	// 结算恢复后的月份（2026-12，结束月不含，正常出账）后重放仍成功。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-12-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-12")
	out = h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatalf("结算恢复月后重放应成功:\n%s", out)
	}
}

func TestPauseRejectsWhenUsageInInterval(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-10-05T00:00:00Z,3\n"+
		"u2,c1,2026-11-20T08:00:00+08:00,5\n") // UTC 2026-11-20T00:00:00Z
	h.mustRun("usage", "import", f)

	// 区间覆盖已有用量：拒绝并指出冲突记录，不删除或改写用量。
	msg := h.runExpectErr("pause", "add", "c1", "2026-10", "2026-12", "停用")
	if !strings.Contains(msg, "u1") || !strings.Contains(msg, "u2") {
		t.Fatalf("应指出全部冲突用量: %s", msg)
	}
	// 区间不含已有用量（结束月不含）则合法。
	h.mustRun("pause", "add", "c1", "2026-12", "2027-02", "停用")
	// 用量仍在，可正常结算。
	out := h.mustRun("bill", "settle", "c1", "2026-10")
	if !strings.Contains(out, "总金额：30 分") {
		t.Fatalf("已有用量不应被暂停登记改写:\n%s", out)
	}
}

func TestPauseImportBlocked(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")

	// 暂停月的新用量使整批导入失败并指出行号，合法项也不留下。
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-05T00:00:00Z,3\n"+ // 第 2 行：合法
		"u2,c1,2026-10-05T00:00:00Z,4\n"+ // 第 3 行：暂停月
		"u3,c1,2026-12-05T00:00:00Z,5\n") // 第 4 行：合法（结束月不含）
	msg := h.runExpectErr("usage", "import", f)
	if !strings.Contains(msg, "第 3 行") || !strings.Contains(msg, "暂停区间 [2026-10, 2026-12)") {
		t.Fatalf("应指出暂停月行号与区间: %s", msg)
	}
	// 合法项也不留下：重新导入只含合法行的文件，全部新增。
	f2 := h.writeFile("u2.csv", csvHeader+
		"u1,c1,2026-09-05T00:00:00Z,3\n"+
		"u3,c1,2026-12-05T00:00:00Z,5\n")
	out := h.mustRun("usage", "import", f2)
	if !strings.Contains(out, "新增 2 条") {
		t.Fatalf("失败批次不应留下任何用量:\n%s", out)
	}
	// 暂停月用量的重放仍失败（该记录从未入库，不是重复）。
	f3 := h.writeFile("u3.csv", csvHeader+"u2,c1,2026-10-05T00:00:00Z,4\n")
	h.runExpectErr("usage", "import", f3)
}

func TestPauseSettleRejected(t *testing.T) {
	h := newHarness(t)
	// 月费大于 0 的方案：暂停月也不产生零金额账单。
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "add-plan", "c2", "客户二", "sub")
	h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")

	msg := h.runExpectErr("bill", "settle", "c1", "2026-10")
	if !strings.Contains(msg, "暂停区间 [2026-10, 2026-12)") || !strings.Contains(msg, "不封账") {
		t.Fatalf("暂停月单笔结算应拒绝: %s", msg)
	}
	h.runExpectErr("bill", "settle", "c1", "2026-11")
	if msg := h.runExpectErr("bill", "show", "c1", "2026-10"); !strings.Contains(msg, "尚无账单") {
		t.Fatalf("暂停月不应生成账单: %s", msg)
	}

	// 批量清单包含暂停月时整批拒绝：不留下任何新账单或封账。
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-12-05T00:00:00Z,3\n"+
		"u2,c2,2026-10-05T00:00:00Z,4\n")
	h.mustRun("usage", "import", f)
	msg = h.runExpectErr("bill", "settle-batch", "c1", "2026-12", "c2", "2026-10", "c1", "2026-10")
	if !strings.Contains(msg, "整批未生效") || !strings.Contains(msg, "暂停区间") {
		t.Fatalf("批量含暂停月应整批拒绝: %s", msg)
	}
	for _, m := range []string{"2026-10", "2026-12"} {
		if msg := h.runExpectErr("bill", "show", "c1", m); !strings.Contains(msg, "尚无账单") {
			t.Fatalf("整批拒绝不应留下 %s 账单: %s", m, msg)
		}
	}
	if msg := h.runExpectErr("bill", "show", "c2", "2026-10"); !strings.Contains(msg, "尚无账单") {
		t.Fatalf("整批拒绝不应封账其他客户: %s", msg)
	}

	// 恢复服务后：按账期有效方案收取一次整月月费，用量从零累计，不补收暂停月。
	out := h.mustRun("bill", "settle", "c1", "2026-12")
	if !strings.Contains(out, "月费：1000 分") || !strings.Contains(out, "用量费：30 分") ||
		!strings.Contains(out, "原总金额：1030 分") {
		t.Fatalf("恢复月结算异常:\n%s", out)
	}
}

func TestPauseDoesNotConsumeSeq(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-09-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")
	// 暂停登记不产生账后流水事件、不占用全局操作序号。
	h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "5", "补收")
	ledger := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(ledger, "存档全局序号上限：1") || !strings.Contains(ledger, "序号 1 调整 adj-1") {
		t.Fatalf("暂停登记占用了操作序号:\n%s", ledger)
	}
}

func TestPausePlanChangeUnaffected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "旧阶梯", "-:10")
	h.mustRun("plan", "add", "p2", "新阶梯", "-:1")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")
	// 暂停不改动方案变更限制：变更生效月仍按递增追加、晚于封账月。
	h.mustRun("plan", "change", "c1", "2026-11", "p2", "续期新价")
	// 恢复月按账期有效方案计价（新方案单价 1）。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-12-05T00:00:00Z,6\n")
	h.mustRun("usage", "import", f)
	out := h.mustRun("bill", "settle", "c1", "2026-12")
	if !strings.Contains(out, "方案：p2（新阶梯）") || !strings.Contains(out, "总金额：6 分") {
		t.Fatalf("恢复月应按有效方案计价:\n%s", out)
	}
	// 暂停月仍拒绝结算。
	h.runExpectErr("bill", "settle", "c1", "2026-11")
}

func TestPauseCorruptStateRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ old, new string }{
		{`"end_month": "2026-12"`, `"end_month": "2026-10"`},    // 结束月不晚于起月
		{`"end_month": "2026-12"`, `"end_month": "2026-13"`},    // 非法月份
		{`"reason": "停用"`, `"reason": "  "`},                    // 原因为空
		{`"customer_id": "c1"`, `"customer_id": "ghost"`},       // 客户引用失效
		{`"start_month": "2026-10"`, `"start_month": "2026-1"`}, // 起月格式非法
	}
	for _, tc := range cases {
		broken := strings.Replace(string(good), tc.old, tc.new, 1)
		if broken == string(good) {
			t.Fatalf("替换 %q 未生效", tc.old)
		}
		if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := h.runExpectErr("pause", "list", "c1")
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("篡改 %q 未报损坏: %s", tc.old, msg)
		}
		got, _ := os.ReadFile(h.statePath())
		if string(got) != broken {
			t.Fatalf("篡改 %q 后文件被改写", tc.old)
		}
	}
	// 同客户区间重叠。
	overlap := strings.Replace(string(good),
		`"pauses": {`,
		`"pauses": {
    "c1|2026-11": {
      "customer_id": "c1",
      "start_month": "2026-11",
      "end_month": "2027-01",
      "reason": "重叠",
      "created_at": "2026-10-01T00:00:00Z"
    },`, 1)
	if overlap == string(good) {
		t.Fatal("重叠替换未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(overlap), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("pause", "list", "c1"); !strings.Contains(msg, "重叠") {
		t.Fatalf("区间重叠未报损坏: %s", msg)
	}
	// 暂停月存在用量。
	withUsage := strings.Replace(string(good),
		`"usage": {}`,
		`"usage": {
    "u1": {
      "id": "u1",
      "customer_id": "c1",
      "time": "2026-10-05T00:00:00Z",
      "quantity": 3
    }
  }`, 1)
	if withUsage == string(good) {
		t.Fatal("用量替换未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(withUsage), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("pause", "list", "c1"); !strings.Contains(msg, "损坏") {
		t.Fatalf("暂停月存在用量未报损坏: %s", msg)
	}
	// 恢复完好存档后一切正常。
	if err := os.WriteFile(h.statePath(), good, 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("pause", "list", "c1")
}

func TestPauseOldStateFileWithoutPauses(t *testing.T) {
	h := newHarness(t)
	// 旧格式存档：无 pauses 字段，视为正常服务。
	old := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "老客户", "price_fen": 0, "plan_id": "p1"}},
  "plans": {"p1": {"id": "p1", "name": "旧阶梯", "tiers": [{"limit": 0, "price_fen": 5}]}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-01T00:00:00Z", "quantity": 3}},
  "bills": {}
}
`
	if err := os.WriteFile(h.statePath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("pause", "list", "c1")
	if !strings.Contains(out, "暂停区间：无") {
		t.Fatalf("旧存档应视为无暂停:\n%s", out)
	}
	// 重启保持：登记后区间、幂等与暂停限制跨进程保持。
	h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")
	h.mustRun("pause", "add", "c1", "2026-10", "2026-12", "停用")
	h.runExpectErr("bill", "settle", "c1", "2026-10")
	out = h.mustRun("pause", "list", "c1", "2026-11")
	if !strings.Contains(out, "月份 2026-11：暂停中") {
		t.Fatalf("重启后暂停限制未保持:\n%s", out)
	}
	// 旧用量（非暂停月）结算不受影响。
	out = h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "总金额：15 分") {
		t.Fatalf("旧存档结算异常:\n%s", out)
	}
}
