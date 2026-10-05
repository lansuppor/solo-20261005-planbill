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

// settleOne 是调整相关用例的公共准备：导入一条用量并结算，
// 得到数量 qty、单价为登记价的账单（客户须已登记）。
func settleOne(h *harness, cust, month, qty string) {
	h.t.Helper()
	f := h.writeFile("seed-"+cust+month+".csv", csvHeader+
		"u-"+cust+month+","+cust+","+month+"-15T10:00:00Z,"+qty+"\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", cust, month)
}

func TestAdjustHappyPathAndShow(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "150")
	settleOne(h, "c1", "2026-09", "6") // 总金额 900

	out := h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补录最低消费")
	if !strings.Contains(out, "补收") || !strings.Contains(out, "当前应付 1400 分") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "-200", "服务补偿")
	if !strings.Contains(out, "减免") || !strings.Contains(out, "当前应付 1200 分") {
		t.Fatal(out)
	}

	// bill show：原字段保留，补充调整净额、当前应付与按顺序排列的历史。
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"总金额：900 分", "总数量：6", "单价：150 分",
		"调整净额：300 分", "当前应付：1200 分",
		"调整标识=adj-1", "金额=+500 分", "补录最低消费", "状态=生效中",
		"调整标识=adj-2", "金额=-200 分", "服务补偿",
	} {
		if !strings.Contains(shown, want) {
			t.Fatalf("账单展示缺少 %q:\n%s", want, shown)
		}
	}
	// 历史按成功操作顺序排列：adj-1 在 adj-2 之前。
	if strings.Index(shown, "adj-1") > strings.Index(shown, "adj-2") {
		t.Fatalf("调整历史顺序错误:\n%s", shown)
	}

	// 重复结算返回原账单并展示调整，不创建新账单或调整。
	again := h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(again, "幂等") || !strings.Contains(again, "当前应付：1200 分") {
		t.Fatal(again)
	}
}

func TestAdjustRequiresExistingBillAndCustomer(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")

	// 未结算月份不得调整。
	msg := h.runExpectErr("bill", "adjust", "c1", "2026-09", "adj-1", "100", "原因")
	if !strings.Contains(msg, "尚无账单") {
		t.Fatal(msg)
	}
	// 客户不存在。
	h.runExpectErr("bill", "adjust", "ghost", "2026-09", "adj-1", "100", "原因")

	// 失败的首次新增不占用调整标识：结算后同一标识可正常登记。
	settleOne(h, "c1", "2026-09", "1")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "100", "原因")
}

func TestAdjustInputValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	settleOne(h, "c1", "2026-09", "2") // 总金额 200

	h.runExpectErr("bill", "adjust", "c1", "2026-9", "a1", "100", "原因")                      // 月份格式
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "", "100", "原因")                       // 空标识
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "  ", "100", "原因")                     // 纯空白标识
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "0", "原因")                       // 零金额
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "1.5", "原因")                     // 非整数
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "abc", "原因")                     // 非数字
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "99999999999999999999999", "原因") // 超 int64
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "100", "")                       // 空原因
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "a1", "100", "   ")                    // 纯空白原因

	// 全部失败后标识 a1 未被占用，应付不变。
	h.mustRun("bill", "adjust", "c1", "2026-09", "a1", "50", "首次成功")
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "当前应付：250 分") {
		t.Fatal(out)
	}
}

func TestAdjustIdempotentReplayAndConflict(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	settleOne(h, "c1", "2026-09", "2") // 总金额 200
	h.mustRun("customer", "add", "c2", "乙方", "100")
	settleOne(h, "c2", "2026-09", "10") // 另一客户

	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补录")

	// 内容完全相同的重放：幂等成功，不重复计入。
	out := h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补录")
	if !strings.Contains(out, "幂等") || !strings.Contains(out, "状态=生效中") {
		t.Fatal(out)
	}
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：700 分") {
		t.Fatalf("重放导致重复计入:\n%s", shown)
	}

	// 任一字段不同均拒绝：金额、原因、跨客户、跨月份。
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "adj-1", "600", "补录")
	h.runExpectErr("bill", "adjust", "c1", "2026-09", "adj-1", "500", "其他原因")
	h.runExpectErr("bill", "adjust", "c2", "2026-09", "adj-1", "500", "补录")
	settleOne(h, "c1", "2026-10", "1")
	h.runExpectErr("bill", "adjust", "c1", "2026-10", "adj-1", "500", "补录")
}

func TestAdjustPayableBounds(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	settleOne(h, "c1", "2026-09", "2") // 总金额 200

	// 减免使应付变负：拒绝，且不占用标识。
	msg := h.runExpectErr("bill", "adjust", "c1", "2026-09", "adj-neg", "-201", "超额减免")
	if !strings.Contains(msg, "小于 0") {
		t.Fatal(msg)
	}
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-neg", "-200", "全额减免") // 恰好为 0 合法
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "当前应付：0 分") {
		t.Fatal(out)
	}

	// 补收使应付溢出 int64：拒绝。
	h.mustRun("customer", "add", "rich", "巨款客户", "9223372036854775800")
	settleOne(h, "rich", "2026-09", "1")
	msg = h.runExpectErr("bill", "adjust", "rich", "2026-09", "adj-big", "100", "补收")
	if !strings.Contains(msg, "溢出") {
		t.Fatal(msg)
	}
	// 界内最大补收合法。
	h.mustRun("bill", "adjust", "rich", "2026-09", "adj-big", "7", "补收")
	out = h.mustRun("bill", "show", "rich", "2026-09")
	if !strings.Contains(out, "当前应付：9223372036854775807 分") {
		t.Fatal(out)
	}
}

func TestRevokeFlow(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	settleOne(h, "c1", "2026-09", "10") // 总金额 1000
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补录")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "-100", "补偿")

	// 撤销目标不存在。
	h.runExpectErr("bill", "revoke", "adj-ghost", "原因")
	// 空撤销原因。
	h.runExpectErr("bill", "revoke", "adj-1", "")
	h.runExpectErr("bill", "revoke", "adj-1", "  ")

	// 撤销 adj-1：应付 1500 -> 900，其他调整不受影响。
	out := h.mustRun("bill", "revoke", "adj-1", "重复录入")
	if !strings.Contains(out, "当前应付 900 分") {
		t.Fatal(out)
	}
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"调整净额：-100 分", "当前应付：900 分",
		"调整标识=adj-1", "状态=已撤销（撤销原因=\"重复录入\"）",
		"调整标识=adj-2", "状态=生效中",
	} {
		if !strings.Contains(shown, want) {
			t.Fatalf("账单展示缺少 %q:\n%s", want, shown)
		}
	}

	// 相同标识 + 相同原因重复撤销：幂等成功，不新增记录。
	out = h.mustRun("bill", "revoke", "adj-1", "重复录入")
	if !strings.Contains(out, "幂等") {
		t.Fatal(out)
	}
	// 改用其他原因：拒绝。
	h.runExpectErr("bill", "revoke", "adj-1", "换个理由")

	// 撤销后重放原调整：返回已撤销状态，不重新生效。
	out = h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补录")
	if !strings.Contains(out, "幂等") || !strings.Contains(out, "状态=已撤销") {
		t.Fatal(out)
	}
	shown = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：900 分") {
		t.Fatalf("已撤销调整被重新生效:\n%s", shown)
	}
}

func TestRevokeOutOfBoundsRejectedAndRetryable(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "1")
	settleOne(h, "c1", "2026-09", "100")                                // 总金额 100
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-a", "50", "补收")   // 应付 150
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-b", "-120", "减免") // 应付 30

	// 撤销 adj-a 后应付将为 -20：拒绝，adj-a 保持有效。
	msg := h.runExpectErr("bill", "revoke", "adj-a", "撤销补收")
	if !strings.Contains(msg, "越界") || !strings.Contains(msg, "仍有效") {
		t.Fatal(msg)
	}
	shown := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(shown, "当前应付：30 分") {
		t.Fatalf("被拒的撤销改变了应付:\n%s", shown)
	}

	// 其他合法调整改变余额后可重试：+100 后应付 130，撤销 adj-a 得 80。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-c", "100", "补收")
	out := h.mustRun("bill", "revoke", "adj-a", "撤销补收")
	if !strings.Contains(out, "当前应付 80 分") {
		t.Fatal(out)
	}
}

func TestAdjustPersistenceAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	settleOne(h, "c1", "2026-09", "10")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补录")
	h.mustRun("bill", "revoke", "adj-1", "录入错误")

	// 全新 harness 指向同一目录：记录、顺序、重复判断与撤销约束保持。
	h2 := &harness{t: t, dir: h.dir}
	prev := stdout
	stdout = &h2.buf
	defer func() { stdout = prev }()

	out, err := h2.run("bill", "show", "c1", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "状态=已撤销") || !strings.Contains(out, "当前应付：1000 分") {
		t.Fatal(out)
	}
	// 跨“进程”重复判断仍生效。
	if _, err := h2.run("bill", "adjust", "c1", "2026-09", "adj-1", "600", "补录"); err == nil {
		t.Fatal("跨进程标识冲突未被拒绝")
	}
	if _, err := h2.run("bill", "revoke", "adj-1", "其他原因"); err == nil {
		t.Fatal("跨进程撤销原因冲突未被拒绝")
	}
}

func TestOldStateFileWithoutAdjustmentsLoads(t *testing.T) {
	h := newHarness(t)
	// 手写一份不含调整字段的旧格式数据文件（一次完整结算后的状态）。
	old := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2}},
  "bills": {"c1|2026-09": {
    "id": "BILL-old", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 2, "unit_price_fen": 100, "total_fee_fen": 200,
    "lines": [{"usage_id": "u1", "time": "2026-09-15T10:00:00Z", "quantity": 2, "line_fee_fen": 200}],
    "created_at": "2026-10-01T00:00:00Z"
  }}
}
`
	if err := os.WriteFile(h.statePath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	// 旧数据直接可读：视为零调整。
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "调整净额：0 分") ||
		!strings.Contains(out, "当前应付：200 分") ||
		!strings.Contains(out, "调整记录：无") {
		t.Fatal(out)
	}

	// 旧库上可正常登记调整并持久化。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "50", "补录")
	out = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "当前应付：250 分") {
		t.Fatal(out)
	}
}

func TestCorruptAdjustmentDataRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	settleOne(h, "c1", "2026-09", "2")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "50", "补录")
	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}

	// 调整引用失效（指向不存在的账单）：按损坏处理，原文件保留。
	broken := strings.Replace(string(good), `"month": "2026-09"`, `"month": "2027-01"`, 1)
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
	got, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != broken {
		t.Fatal("损坏文件被改写")
	}
}
