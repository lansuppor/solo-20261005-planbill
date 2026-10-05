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
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "200", "漏算用量")     // 序号 2
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "300", "银行转账")        // 序号 3
	h.mustRun("bill", "pay", "c2", "2026-09", "pay-c2", "100", "其他客户收款")   // 序号 4
	h.mustRun("bill", "revoke", "adj-1", "录入错误")                              // 序号 5
	h.mustRun("bill", "unpay", "pay-1", "账号登记错误")                           // 序号 6

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
	settleTwoMonths(h, "c1", "100", "5", "3") // 2026-09: 500，2026-10: 300
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
	settleBill(h, "c1", "100", "10") // 总金额 1000
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
	h.mustRun("bill", "unpay", "pay-1", "退回")                           // 序号 3
	h.mustRun("bill", "revoke", "adj-1", "撤回")                          // 序号 4
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
