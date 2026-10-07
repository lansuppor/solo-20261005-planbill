package main

import (
	"os"
	"strings"
	"testing"
)

// bill remit-import：收款文件的整批自动分配导入。

const remitCSVHeader = "payment_id,customer_id,total_fen,note\n"

// writeRemitFile 在数据目录写入收款 CSV，返回路径。
func (h *harness) writeRemitFile(name, content string) string {
	return h.writeFile(name, content)
}

// runWithStdin 临时把 os.Stdin 替换为给定内容文件后执行命令。
func (h *harness) runWithStdin(path string, args ...string) (string, error) {
	h.t.Helper()
	oldStdin := os.Stdin
	f, err := os.Open(path)
	if err != nil {
		h.t.Fatal(err)
	}
	os.Stdin = f
	defer func() {
		f.Close()
		os.Stdin = oldStdin
	}()
	return h.run(args...)
}

func (h *harness) mustRunStdin(path string, args ...string) string {
	h.t.Helper()
	out, err := h.runWithStdin(path, args...)
	if err != nil {
		h.t.Fatalf("run %v 意外失败: %v", args, err)
	}
	return out
}

// settleTwoCustomers 准备两个固定单价客户：acme 2026-09 欠 500、2026-10 欠 300；
// globex 2026-09 欠 200。
func settleTwoCustomers(h *harness) {
	settleFixedCust(h, "acme", "100", map[string]string{
		"2026-09": "5", "2026-10": "3",
	})
	settleFixedCust(h, "globex", "100", map[string]string{"2026-09": "2"})
}

func TestRemitImportHappyPathMultiCustomerMultiRow(t *testing.T) {
	h := newHarness(t)
	settleTwoCustomers(h)

	// 同一客户多笔汇款 + 多客户混合：
	// pay-01 acme 600  → 09:500,10:100
	// pay-02 globex 200 → 09:200
	// pay-03 acme 200   → 基于批内余额，10 月尚欠 200 → 10:200
	csv := remitCSVHeader +
		"pay-01,acme,600,季度汇款\n" +
		"pay-02,globex,200,货款\n" +
		"pay-03,acme,200,尾款\n"
	f := h.writeRemitFile("remits.csv", csv)
	out := h.mustRun("bill", "remit-import", f)
	for _, want := range []string{
		"新增 3 笔，重复跳过 0 行",
		"第 2 行 新增：收款标识 pay-01",
		"第 3 行 新增：收款标识 pay-02",
		"第 4 行 新增：收款标识 pay-03",
		"首次分配：2026-09:500,2026-10:100",
		"首次分配：2026-09:200",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out)
		}
	}

	s := loadState(t, h)
	// 按首次出现顺序各占一个连续递增序号，不按月份拆笔。
	if s.NextSeq != 3 {
		t.Fatalf("NextSeq=%d，应为 3", s.NextSeq)
	}
	p1, p2, p3 := s.Payments["pay-01"], s.Payments["pay-02"], s.Payments["pay-03"]
	if p1 == nil || !p1.Auto || p1.Seq != 1 || formatAllocations(p1.Allocations) != "2026-09:500,2026-10:100" {
		t.Fatalf("pay-01 异常: %+v", p1)
	}
	if p2 == nil || !p2.Auto || p2.Seq != 2 || formatAllocations(p2.Allocations) != "2026-09:200" {
		t.Fatalf("pay-02 异常: %+v", p2)
	}
	if p3 == nil || !p3.Auto || p3.Seq != 3 || formatAllocations(p3.Allocations) != "2026-10:200" {
		t.Fatalf("pay-03 应基于批内余额分配 10 月: %+v", p3)
	}
	// 不跨客户：acme 两月全部还清，globex 还清。
	out = h.mustRun("bill", "show", "acme", "2026-10")
	if !strings.Contains(out, "实收：300 分") || !strings.Contains(out, "未收余额：0 分") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "show", "globex", "2026-09")
	if !strings.Contains(out, "实收：200 分") || !strings.Contains(out, "未收余额：0 分") {
		t.Fatal(out)
	}
	// ledger 能按逐笔序号追溯。
	out = h.mustRun("bill", "ledger", "acme", "2026-10")
	if !strings.Contains(out, "序号 1 收款 pay-01") || !strings.Contains(out, "序号 3 收款 pay-03") {
		t.Fatal(out)
	}
	// reconcile 合并展示。
	out = h.mustRun("bill", "reconcile", "acme", "2026-09", "2026-10")
	if !strings.Contains(out, "pay-01") || !strings.Contains(out, "pay-03") {
		t.Fatal(out)
	}
}

func TestRemitImportBatchBalanceSequential(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "acme", "100", map[string]string{
		"2026-09": "5", "2026-10": "3", "2026-11": "2",
	}) // 欠款 500/300/200
	// 三笔各 500：依次 09:500 → 10:300,11:200 → 11 无剩余，第三笔无欠款应整批拒绝。
	csv := remitCSVHeader +
		"p1,acme,500,第一笔\n" +
		"p2,acme,500,第二笔\n" +
		"p3,acme,500,第三笔\n"
	f := h.writeRemitFile("r.csv", csv)
	errText := h.runExpectErr("bill", "remit-import", f)
	if !strings.Contains(errText, "第 4 行") || !strings.Contains(errText, "没有欠款") {
		t.Fatalf("第三笔无欠款应带行号整批拒绝:\n%s", errText)
	}
	s := loadState(t, h)
	if len(s.Payments) != 0 || s.NextSeq != 0 {
		t.Fatalf("整批失败不得留下部分新增: %+v", s.Payments)
	}

	// 去掉第三笔：前两笔按批内余额联动成功。
	csv = remitCSVHeader +
		"p1,acme,500,第一笔\n" +
		"p2,acme,500,第二笔\n"
	f = h.writeRemitFile("r2.csv", csv)
	out := h.mustRun("bill", "remit-import", f)
	if !strings.Contains(out, "首次分配：2026-09:500") ||
		!strings.Contains(out, "首次分配：2026-10:300,2026-11:200") {
		t.Fatal(out)
	}
}

func TestRemitImportDedupStoreAndBatch(t *testing.T) {
	h := newHarness(t)
	settleTwoCustomers(h)

	// 首次导入。
	f1 := h.writeRemitFile("r1.csv", remitCSVHeader+
		"pay-01,acme,600,季度汇款\n"+
		"pay-02,globex,200,货款\n")
	h.mustRun("bill", "remit-import", f1)

	// 混合：一个新增 + 一个存档重复，整批只新增一笔，存档重复标注来源。
	f2 := h.writeRemitFile("r2.csv", remitCSVHeader+
		"pay-03,acme,200,尾款\n"+
		"pay-01,acme,600,季度汇款\n")
	out := h.mustRun("bill", "remit-import", f2)
	if !strings.Contains(out, "新增 1 笔，重复跳过 1 行") ||
		!strings.Contains(out, "重复跳过（存档已有") {
		t.Fatal(out)
	}
	if s := loadState(t, h); s.NextSeq != 3 {
		t.Fatalf("重复行不占序号，NextSeq=%d 应为 3", s.NextSeq)
	}

	// 全为存档重复：不写盘。
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	f3 := h.writeRemitFile("r3.csv", remitCSVHeader+
		"pay-01,acme,600,季度汇款\n"+
		"pay-02,globex,200,货款\n")
	out = h.mustRun("bill", "remit-import", f3)
	if !strings.Contains(out, "新增 0 笔，重复跳过 2 行（全部为重复记录，未改写存档）") {
		t.Fatal(out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("全为重复时不得写盘")
	}

	// 本批内重复：相同记录第二次出现计为重复跳过，只占一个序号。
	// 另备有欠款的客户 c3（此时 acme、globex 均已还清）。
	settleFixedCust(h, "c3", "100", map[string]string{"2026-09": "3"})
	f4 := h.writeRemitFile("r4.csv", remitCSVHeader+
		"pay-x,c3,100,批内首现\n"+
		"pay-x,c3,100,批内首现\n")
	out = h.mustRun("bill", "remit-import", f4)
	if !strings.Contains(out, "新增 1 笔，重复跳过 1 行") ||
		!strings.Contains(out, "重复跳过（本批先前出现") {
		t.Fatal(out)
	}
	s := loadState(t, h)
	if s.NextSeq != 4 || len(s.Payments) != 4 {
		t.Fatalf("本批重复不占序号: NextSeq=%d payments=%d", s.NextSeq, len(s.Payments))
	}
}

func TestRemitImportDedupConflictRejectsWholeBatch(t *testing.T) {
	h := newHarness(t)
	settleTwoCustomers(h)
	f := h.writeRemitFile("r.csv", remitCSVHeader+
		"pay-01,acme,600,季度汇款\n")
	h.mustRun("bill", "remit-import", f)

	// 存档已有标识但内容不同：整批拒绝，同行的另一笔合法新增也不生效。
	bad := h.writeRemitFile("bad.csv", remitCSVHeader+
		"pay-9,acme,100,合法新增\n"+
		"pay-01,acme,601,季度汇款\n")
	errText := h.runExpectErr("bill", "remit-import", bad)
	if !strings.Contains(errText, "第 3 行") || !strings.Contains(errText, "内容不同") {
		t.Fatal(errText)
	}
	s := loadState(t, h)
	if _, ok := s.Payments["pay-9"]; ok || s.NextSeq != 1 {
		t.Fatalf("冲突应整批拒绝，不留下合法新增，NextSeq=%d", s.NextSeq)
	}

	// 本批内同标识内容不同：整批拒绝。
	bad2 := h.writeRemitFile("bad2.csv", remitCSVHeader+
		"pay-z,acme,100,备注甲\n"+
		"pay-z,acme,100,备注乙\n")
	errText = h.runExpectErr("bill", "remit-import", bad2)
	if !strings.Contains(errText, "第 3 行") || !strings.Contains(errText, "内容不同") {
		t.Fatal(errText)
	}
	if s := loadState(t, h); s.NextSeq != 1 {
		t.Fatalf("批内冲突不得占序号，NextSeq=%d", s.NextSeq)
	}

	// 复用显式分配登记标识：整批拒绝。（此时 acme 09 月已还清，10 月尚欠 200。）
	h.mustRun("bill", "pay", "acme", "2026-10", "pay-exp", "100", "显式")
	bad3 := h.writeRemitFile("bad3.csv", remitCSVHeader+
		"pay-exp,acme,100,显式\n")
	errText = h.runExpectErr("bill", "remit-import", bad3)
	if !strings.Contains(errText, "显式分配登记") {
		t.Fatal(errText)
	}
}

func TestRemitImportReplayNotRedistributed(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "acme", "100", map[string]string{
		"2026-09": "5", "2026-10": "3",
	}) // 500/300
	f := h.writeRemitFile("r.csv", remitCSVHeader+"pay-01,acme,500,汇款\n")
	h.mustRun("bill", "remit-import", f) // 09:500

	// 后来新增 11 月账单，重放不得重新分配、不得写盘。
	h.mustRun("usage", "import", h.writeFile("u.csv", csvHeader+"u11,acme,2026-11-15T10:00:00Z,2\n"))
	h.mustRun("bill", "settle", "acme", "2026-11")
	before, _ := os.ReadFile(h.statePath())
	out := h.mustRun("bill", "remit-import", f)
	if !strings.Contains(out, "新增 0 笔，重复跳过 1 行") ||
		!strings.Contains(out, "首次分配：2026-09:500") {
		t.Fatal(out)
	}
	after, _ := os.ReadFile(h.statePath())
	if string(before) != string(after) {
		t.Fatal("重放不得写盘")
	}
	// 11 月仍欠 200，未被重放补分配。
	out = h.mustRun("bill", "show", "acme", "2026-11")
	if !strings.Contains(out, "未收余额：200 分") {
		t.Fatal(out)
	}

	// 导入的收款是一笔普通自动收款：可更正、可退款、可撤销。
	out = h.mustRun("bill", "correct", "pay-01", "corr-1", "调账", "2026-09:400", "2026-10:100")
	if !strings.Contains(out, "已登记收款分配更正") {
		t.Fatal(out)
	}
}

func TestRemitImportNoDebtAndExcessReject(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "acme", "100", map[string]string{"2026-09": "5"}) // 500

	// 超额行整批拒绝。
	f := h.writeRemitFile("r.csv", remitCSVHeader+"pay-01,acme,501,超额\n")
	errText := h.runExpectErr("bill", "remit-import", f)
	if !strings.Contains(errText, "第 2 行") || !strings.Contains(errText, "超过") {
		t.Fatal(errText)
	}
	// 没有账单的客户：无欠款拒绝。
	h.mustRun("customer", "add", "c2", "客户c2", "100")
	f2 := h.writeRemitFile("r2.csv", remitCSVHeader+"pay-02,c2,1,无账单\n")
	errText = h.runExpectErr("bill", "remit-import", f2)
	if !strings.Contains(errText, "第 2 行") || !strings.Contains(errText, "没有欠款") {
		t.Fatal(errText)
	}
	if s := loadState(t, h); len(s.Payments) != 0 || s.NextSeq != 0 {
		t.Fatal("失败不得改变状态")
	}
}

func TestRemitImportParseAndValidationErrors(t *testing.T) {
	h := newHarness(t)
	settleTwoCustomers(h)

	cases := map[string]string{
		"bad header":       "payment_id,customer_id,total_fen\npay-01,acme,100,备注\n",
		"empty":            "",
		"header only":      remitCSVHeader,
		"bad amount":       remitCSVHeader + "pay-01,acme,abc,备注\n",
		"zero amount":      remitCSVHeader + "pay-01,acme,0,备注\n",
		"negative amount":  remitCSVHeader + "pay-01,acme,-5,备注\n",
		"empty note":       remitCSVHeader + "pay-01,acme,100,\n",
		"empty id":         remitCSVHeader + ",acme,100,备注\n",
		"unknown customer": remitCSVHeader + "pay-01,ghost,100,备注\n",
		"bad fields":       remitCSVHeader + "pay-01,acme,100\n",
	}
	for name, content := range cases {
		f := h.writeRemitFile("x-"+name+".csv", content)
		if _, err := h.run("bill", "remit-import", f); err == nil {
			t.Fatalf("用例 %s 应失败", name)
		}
	}
	// 多行问题一次报告并指出各自行号。
	f := h.writeRemitFile("multi.csv", remitCSVHeader+
		"p1,acme,0,零额\n"+
		"p2,ghost,100,幽灵客户\n")
	errText := h.runExpectErr("bill", "remit-import", f)
	if !strings.Contains(errText, "第 2 行") || !strings.Contains(errText, "第 3 行") {
		t.Fatal(errText)
	}
	if s := loadState(t, h); len(s.Payments) != 0 || s.NextSeq != 0 {
		t.Fatal("解析/校验失败不得改变状态")
	}

	// 参数个数错误为用法错误（退出码 2）。
	if _, err := h.run("bill", "remit-import"); err == nil {
		t.Fatal("缺少文件参数应失败")
	}
}

func TestRemitImportStdin(t *testing.T) {
	h := newHarness(t)
	settleTwoCustomers(h)
	f := h.writeRemitFile("stdin.csv", remitCSVHeader+
		"pay-01,acme,600,季度汇款\n"+
		"pay-02,globex,200,货款\n")
	out := h.mustRunStdin(f, "bill", "remit-import", "-")
	if !strings.Contains(out, "标准输入") || !strings.Contains(out, "新增 2 笔") {
		t.Fatal(out)
	}
}

func TestRemitImportOverflowSumNotMisrejected(t *testing.T) {
	h := newHarness(t)
	// 两张账单各欠 MaxInt64 分，欠款合计超 64 位上限，合法 MaxInt64 不得误拒。
	settleFixedCust(h, "big", "9223372036854775807", map[string]string{
		"2026-09": "1", "2026-10": "1",
	})
	f := h.writeRemitFile("big.csv", remitCSVHeader+
		"pb1,big,9223372036854775807,巨额汇款\n"+
		"pb2,big,1,尾款\n")
	out := h.mustRun("bill", "remit-import", f)
	if !strings.Contains(out, "首次分配：2026-09:9223372036854775807") ||
		!strings.Contains(out, "首次分配：2026-10:1") {
		t.Fatalf("跨账单欠款合计超 64 位不得误拒:\n%s", out)
	}
	// 超额仍拒绝：第二月只剩 MaxInt64-1。
	bad := h.writeRemitFile("bad.csv", remitCSVHeader+
		"pb3,big,9223372036854775807,超额\n")
	if errText := h.runExpectErr("bill", "remit-import", bad); !strings.Contains(errText, "超过") {
		t.Fatal(errText)
	}
}

func TestRemitImportPersistsAcrossRestart(t *testing.T) {
	h := newHarness(t)
	settleTwoCustomers(h)
	f := h.writeRemitFile("r.csv", remitCSVHeader+
		"pay-01,acme,600,季度汇款\n"+
		"pay-02,globex,200,货款\n")
	h.mustRun("bill", "remit-import", f)

	// 每次 run 重新读盘：判重、自动身份与余额保持。
	out := h.mustRun("bill", "remit-import", f)
	if !strings.Contains(out, "新增 0 笔，重复跳过 2 行") {
		t.Fatal(out)
	}
	s := loadState(t, h)
	if !s.Payments["pay-01"].Auto || !s.Payments["pay-02"].Auto {
		t.Fatal("自动登记身份应跨重启保持")
	}
	out = h.mustRun("bill", "show", "acme", "2026-10")
	if !strings.Contains(out, "实收：100 分") {
		t.Fatal(out)
	}
}
