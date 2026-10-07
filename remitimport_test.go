package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// bill remit-import：整批自动分配收款导入。

const remitCSVHeader = "customer_id,payment_id,total_fen,note\n"

// withStdin 临时把全局 stdin 替换为给定内容。
func withStdin(h *harness, content string) {
	h.t.Helper()
	prev := stdin
	stdin = strings.NewReader(content)
	h.t.Cleanup(func() { stdin = prev })
}

// remitFile 写一个带固定表头的收款导入文件并返回路径。
func remitFile(h *harness, name, body string) string {
	h.t.Helper()
	return h.writeFile(name, remitCSVHeader+body)
}

func TestRemitImportMultiCustomerSequentialBalance(t *testing.T) {
	h := newHarness(t)
	// c1：09 欠 500、10 欠 300、11 欠 200；c2：09 欠 700。
	settleFixedCust(h, "c1", "100", map[string]string{
		"2026-09": "5", "2026-10": "3", "2026-11": "2",
	})
	settleFixedCust(h, "c2", "100", map[string]string{"2026-09": "7"})

	f := remitFile(h, "remits.csv",
		"c1,pay-1,900,季度汇款\n"+
			"c2,pay-2,700,c2 汇款\n"+
			"c1,pay-3,100,尾款\n") // 批内第二笔 c1：前两月已清，进入 11 月
	out := h.mustRun("bill", "remit-import", f)
	for _, want := range []string{
		"新增 3 笔，重复跳过 0 行",
		"第 2 行：", "新增 收款 \"pay-2\"",
		"首次分配：2026-09:500,2026-10:300,2026-11:100", // pay-1
		"首次分配：2026-09:700",                         // pay-2
		"首次分配：2026-11:100",                         // pay-3 基于批内前笔形成的余额
		"最新分配：2026-09:500,2026-10:300,2026-11:100（与首次分配相同）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("导入输出缺少 %q:\n%s", want, out)
		}
	}

	// 每笔各占一个连续递增序号，按首次出现顺序，不按月份拆笔。
	s := loadState(t, h)
	if s.NextSeq != 3 {
		t.Fatalf("三笔新增应占序号 1..3，NextSeq=%d", s.NextSeq)
	}
	for id, wantSeq := range map[string]int64{"pay-1": 1, "pay-2": 2, "pay-3": 3} {
		p := s.Payments[id]
		if p == nil || !p.Auto || p.Seq != wantSeq {
			t.Fatalf("收款 %s 异常: %+v", id, p)
		}
	}
	if s.Payments["pay-3"].CustomerID != "c1" {
		t.Fatal("不跨客户：pay-3 必须属于 c1")
	}

	// bill show 逐笔可追溯。
	out = h.mustRun("bill", "show", "c1", "2026-11")
	if !strings.Contains(out, "实收：200 分") || !strings.Contains(out, "未收余额：0 分") {
		t.Fatal(out)
	}
}

func TestRemitImportInBatchDuplicatesAndStoreDuplicates(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5", "2026-10": "3"})
	// 存档已有一笔自动收款（pay-old：09:500），10 月仍欠 300。
	h.mustRun("bill", "remit-auto", "c1", "pay-old", "500", "历史汇款")

	body := "c1,pay-old,500,历史汇款\n" + // 存档相同记录：重复跳过
		"c1,pay-new,100,批内首现\n" + // 新增：10:100
		"c1,pay-new,100,批内首现\n" // 本批先前出现的相同记录：重复跳过
	f := remitFile(h, "remits.csv", body)
	out := h.mustRun("bill", "remit-import", f)
	if !strings.Contains(out, "新增 1 笔，重复跳过 2 行") {
		t.Fatal(out)
	}
	if strings.Count(out, "重复跳过") != 3 { // 汇总行 + 两行
		t.Fatalf("应有两行重复跳过:\n%s", out)
	}
	// 重复行不占序号：存档原有 pay-old 序号 1，新笔序号 2。
	s := loadState(t, h)
	if s.NextSeq != 2 || s.Payments["pay-new"].Seq != 2 {
		t.Fatalf("重复行不得占序号: NextSeq=%d", s.NextSeq)
	}

	// 全为重复时不写盘。
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	out = h.mustRun("bill", "remit-import", remitFile(h, "again.csv",
		"c1,pay-old,500,历史汇款\nc1,pay-new,100,批内首现\n"))
	if !strings.Contains(out, "新增 0 笔，重复跳过 2 行") || !strings.Contains(out, "未改写存档") {
		t.Fatal(out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("全为重复时不得写盘")
	}
	if s := loadState(t, h); s.NextSeq != 2 {
		t.Fatalf("重复导入不得增加序号，NextSeq=%d", s.NextSeq)
	}
}

func TestRemitImportContentDifferentRejects(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5", "2026-10": "3"})
	h.mustRun("bill", "remit-auto", "c1", "pay-a", "500", "备注甲")
	h.mustRun("bill", "pay", "c1", "2026-10", "pay-explicit", "100", "显式")

	// 内容不同（存档自动收款）：整批拒绝并指出行号。
	errText := h.runExpectErr("bill", "remit-import", remitFile(h, "d1.csv", "c1,pay-a,501,备注甲\n"))
	if !strings.Contains(errText, "第 2 行") || !strings.Contains(errText, "内容不同") {
		t.Fatal(errText)
	}
	errText = h.runExpectErr("bill", "remit-import", remitFile(h, "d2.csv", "c1,pay-a,500,备注乙\n"))
	if !strings.Contains(errText, "第 2 行") || !strings.Contains(errText, "内容不同") {
		t.Fatal(errText)
	}
	// 客户不同也属内容不同（客户须存在才走到判重；c2 先登记）。
	h.mustRun("customer", "add", "c2", "客户c2", "100")
	errText = h.runExpectErr("bill", "remit-import", remitFile(h, "d2b.csv", "c2,pay-a,500,备注甲\n"))
	if !strings.Contains(errText, "第 2 行") || !strings.Contains(errText, "内容不同") {
		t.Fatal(errText)
	}
	// 批内同标识内容不同：整批拒绝。
	errText = h.runExpectErr("bill", "remit-import", remitFile(h, "d3.csv",
		"c1,pay-x,100,备注一\nc1,pay-x,101,备注一\n"))
	if !strings.Contains(errText, "第 3 行") || !strings.Contains(errText, "内容不同") {
		t.Fatal(errText)
	}
	// 显式登记标识不能在自动导入中复用。
	errText = h.runExpectErr("bill", "remit-import", remitFile(h, "d4.csv", "c1,pay-explicit,100,显式\n"))
	if !strings.Contains(errText, "第 2 行") || !strings.Contains(errText, "显式分配登记占用") {
		t.Fatal(errText)
	}
	// 任何拒绝都不改状态、不占序号。
	if s := loadState(t, h); s.NextSeq != 2 {
		t.Fatalf("拒绝不得占序号，NextSeq=%d", s.NextSeq)
	}
}

func TestRemitImportNoDebtExcessAndAtomicity(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5"}) // 欠 500
	h.mustRun("customer", "add", "c2", "客户c2", "100")                  // 无账单

	// 第一行合法但第二行超额：整批拒绝，无部分成功。
	errText := h.runExpectErr("bill", "remit-import", remitFile(h, "x.csv",
		"c1,pay-ok,400,汇款A\nc1,pay-bad,200,汇款B\n")) // 剩余欠款仅 100
	if !strings.Contains(errText, "第 3 行") || !strings.Contains(errText, "超过") ||
		!strings.Contains(errText, "整批未生效") {
		t.Fatal(errText)
	}
	s := loadState(t, h)
	if len(s.Payments) != 0 || s.NextSeq != 0 {
		t.Fatalf("整批失败必须无任何新增: payments=%v NextSeq=%d", s.Payments, s.NextSeq)
	}

	// 无欠款（客户无账单）整批拒绝并指出行号。
	errText = h.runExpectErr("bill", "remit-import", remitFile(h, "y.csv", "c2,pay-x,1,汇款\n"))
	if !strings.Contains(errText, "第 2 行") || !strings.Contains(errText, "没有欠款") {
		t.Fatal(errText)
	}

	// 排除故障后可原样重试：400 成功，再 100 成功。
	h.mustRun("bill", "remit-import", remitFile(h, "ok.csv", "c1,pay-ok,400,汇款A\n"))
	h.mustRun("bill", "remit-import", remitFile(h, "tail.csv", "c1,pay-tail,100,尾款\n"))
	s = loadState(t, h)
	if s.NextSeq != 2 || s.Payments["pay-tail"].Allocations[0].Month != "2026-09" {
		t.Fatalf("重试异常: NextSeq=%d", s.NextSeq)
	}
}

func TestRemitImportParseAndValidationErrors(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5"})

	type tc struct {
		name    string
		path    string
		content string // 完整文件内容（自行决定是否含表头）
		want    string
	}
	cases := []tc{
		{"空文件", "empty.csv", "", "文件为空"},
		{"仅表头", "header.csv", remitCSVHeader, "至少需要一条"},
		{"错误表头", "badheader.csv", "x,y,z,w\n", "表头必须是"},
		{"字段不足", "few.csv", remitCSVHeader + "c1,p,100\n", "CSV 格式错误"},
		{"空客户", "nocust.csv", remitCSVHeader + ",p,100,备注\n", "第 2 行：客户标识不能为空"},
		{"空标识", "noid.csv", remitCSVHeader + "c1,,100,备注\n", "第 2 行：收款标识不能为空"},
		{"空备注", "nonote.csv", remitCSVHeader + "c1,p,100,\n", "第 2 行：收款备注不能为空"},
		{"零金额", "zero.csv", remitCSVHeader + "c1,p,0,备注\n", "第 2 行：总金额必须是正整数"},
		{"负金额", "neg.csv", remitCSVHeader + "c1,p,-5,备注\n", "第 2 行：总金额必须是正整数"},
		{"非数字", "nan.csv", remitCSVHeader + "c1,p,1.5,备注\n", "第 2 行：总金额"},
		{"超64位", "big.csv", remitCSVHeader + "c1,p,99999999999999999999999,备注\n", "第 2 行：总金额"},
		{"客户不存在", "ghost.csv", remitCSVHeader + "ghost,p,100,备注\n", "第 2 行：客户标识 \"ghost\" 不存在"},
		{"第二行问题指出行号", "line2.csv", remitCSVHeader + "c1,p1,100,备注\nc1,p2,0,备注\n", "第 3 行"},
	}
	for _, c := range cases {
		errText := h.runExpectErr("bill", "remit-import", h.writeFile(c.path, c.content))
		if !strings.Contains(errText, c.want) {
			t.Fatalf("%s: 错误应含 %q，实际:\n%s", c.name, c.want, errText)
		}
	}
	// 备注中的逗号按 RFC 4180 加引号合法。
	out := h.mustRun("bill", "remit-import", remitFile(h, "comma.csv",
		"c1,p1,100,\"汇款, 第一批\"\n"))
	if !strings.Contains(out, "备注：汇款, 第一批") {
		t.Fatal(out)
	}
	// 全部失败尝试都不产生状态：仅 comma.csv 一笔成功。
	if s := loadState(t, h); len(s.Payments) != 1 || s.NextSeq != 1 {
		t.Fatalf("非法导入不得改变状态: %d payments, seq=%d", len(s.Payments), s.NextSeq)
	}
}

func TestRemitImportStdin(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5"})
	withStdin(h, remitCSVHeader+"c1,pay-stdin,500,标准输入汇款\n")
	out := h.mustRun("bill", "remit-import", "-")
	if !strings.Contains(out, "标准输入") || !strings.Contains(out, "新增 1 笔") ||
		!strings.Contains(out, "首次分配：2026-09:500") {
		t.Fatal(out)
	}
}

func TestRemitImportOverflowDebtSumNotMisrejected(t *testing.T) {
	h := newHarness(t)
	// 两张账单各欠 MaxInt64 分，欠款合计超 64 位上限。
	settleFixedCust(h, "c1", "9223372036854775807", map[string]string{
		"2026-09": "1", "2026-10": "1",
	})
	out := h.mustRun("bill", "remit-import", remitFile(h, "big.csv",
		"c1,pay-big,9223372036854775807,巨额\nc1,pay-one,1,一分钱\n"))
	if !strings.Contains(out, "pay-big") ||
		!strings.Contains(out, "首次分配：2026-09:9223372036854775807") ||
		!strings.Contains(out, "首次分配：2026-10:1") {
		t.Fatalf("跨账单欠款合计超 64 位不得误拒合法汇款:\n%s", out)
	}
	// 超额仍拒绝（剩余欠款 MaxInt64-1）。
	errText := h.runExpectErr("bill", "remit-import", remitFile(h, "over.csv", "c1,pay-over,9223372036854775807,超额\n"))
	if !strings.Contains(errText, "超过") {
		t.Fatal(errText)
	}
}

func TestRemitImportSeqTraceAndCutoff(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5", "2026-10": "3"})
	h.mustRun("bill", "remit-import", remitFile(h, "r.csv",
		"c1,pay-1,500,第一笔\nc1,pay-2,300,第二笔\n"))

	// 序号 1 还清 09 月；序号 2 还 10 月。ledger 截止序号不提前计入后续记录。
	out := h.mustRun("bill", "ledger", "c1", "2026-10", "1")
	if strings.Contains(out, "pay-2") || !strings.Contains(out, "截止时余额：应付 300 分") {
		t.Fatalf("截止序号 1 不得计入序号 2:\n%s", out)
	}
	out = h.mustRun("bill", "ledger", "c1", "2026-10")
	if !strings.Contains(out, "序号 2 收款 pay-2") || !strings.Contains(out, "未收余额 0 分") {
		t.Fatal(out)
	}
	// reconcile 区间 [序号 0, 1] 只出现第一笔。
	out = h.mustRun("bill", "reconcile", "c1", "2026-09", "2026-10", "0", "1")
	if !strings.Contains(out, "pay-1") || strings.Contains(out, "pay-2") {
		t.Fatalf("reconcile 截止不得提前计入后续记录:\n%s", out)
	}
	out = h.mustRun("bill", "reconcile", "c1", "2026-09", "2026-10")
	if !strings.Contains(out, "pay-1") || !strings.Contains(out, "pay-2") {
		t.Fatal(out)
	}
}

func TestRemitImportReplayAfterLaterChanges(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5", "2026-10": "3"})
	h.mustRun("bill", "remit-import", remitFile(h, "r.csv", "c1,pay-1,500,汇款\n"))

	// 后来新增账单、更正、退款不阻止相同重放，不重新分配。
	h.mustRun("usage", "import", h.writeFile("nov.csv", csvHeader+"unov,c1,2026-11-15T10:00:00Z,2\n"))
	h.mustRun("bill", "settle", "c1", "2026-11") // 新增 200 分欠款
	h.mustRun("bill", "correct", "pay-1", "corr-1", "入错月", "2026-09:400", "2026-10:100")
	h.mustRun("bill", "refund", "pay-1", "rf-1", "退一点", "2026-10:50")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("bill", "remit-import", remitFile(h, "replay.csv", "c1,pay-1,500,汇款\n"))
	if !strings.Contains(out, "重复跳过") ||
		!strings.Contains(out, "首次分配：2026-09:500") || // 首次分配永久保留
		!strings.Contains(out, "最新分配（经更正，以最新为准）：2026-09:400,2026-10:100") {
		t.Fatalf("重放不得重新分配，应展示首次与最新分配:\n%s", out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("相同重放不得写盘")
	}

	// remit-auto 与导入共享判重身份：相同请求同样幂等。
	out = h.mustRun("bill", "remit-auto", "c1", "pay-1", "500", "汇款")
	if !strings.Contains(out, "已存在且内容相同") || !strings.Contains(out, "首次分配（登记时按最早欠款账期自动确定，永久保留）：2026-09:500") {
		t.Fatal(out)
	}
}

func TestRemitImportedPaymentSupportsPostbillRules(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5", "2026-10": "3"})
	// 部分偿还：09:500 还清，10 月仅收 100。
	h.mustRun("bill", "remit-import", remitFile(h, "r.csv", "c1,pay-1,600,汇款\n"))

	// 导入的收款就是一笔普通自动收款：可更正、可退款。
	out := h.mustRun("bill", "correct", "pay-1", "corr-1", "调整", "2026-09:300", "2026-10:300")
	if !strings.Contains(out, "已登记收款分配更正") {
		t.Fatal(out)
	}
	h.mustRun("bill", "refund", "pay-1", "rf-1", "退", "2026-10:200")
	// 首次退款后不得整笔撤销。
	h.runExpectErr("bill", "unpay", "pay-1", "撤销")
	out = h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(out, "实收：100 分") || !strings.Contains(out, "未收余额：200 分") {
		t.Fatal(out)
	}

	// 序号顺序：pay-1=1、corr=2、rf=3；再导入一笔补齐欠款（09 欠 200）。
	h.mustRun("bill", "remit-import", remitFile(h, "r2.csv", "c1,pay-2,200,补\n"))
	p2 := loadState(t, h).Payments["pay-2"]
	if p2 == nil || !p2.Auto || p2.Seq != 4 {
		t.Fatalf("pay-2 应为自动收款且占序号 4: %+v", p2)
	}
	if got := p2.Allocations[0]; got.Month != "2026-09" || got.Amount != 200 {
		t.Fatalf("pay-2 应按余额补到 09 月: %+v", p2.Allocations)
	}

	// 未退款的新收款可按现有规则整笔撤销。
	h.mustRun("bill", "unpay", "pay-2", "登记错误")
	if !loadState(t, h).Payments["pay-2"].Revoked {
		t.Fatal("pay-2 应可整笔撤销")
	}
}

func TestRemitImportPersistsAcrossRestart(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5", "2026-10": "3"})
	h.mustRun("bill", "remit-import", remitFile(h, "r.csv",
		"c1,pay-1,500,汇款\nc1,pay-2,300,汇款二\n"))
	// 每次 run 重新从磁盘载入，模拟重启：分配、余额与判重保持。
	out := h.mustRun("bill", "remit-import", remitFile(h, "r2.csv",
		"c1,pay-1,500,汇款\nc1,pay-2,300,汇款二\n"))
	if !strings.Contains(out, "新增 0 笔，重复跳过 2 行") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(out, "实收：300 分") || !strings.Contains(out, "未收余额：0 分") {
		t.Fatal(out)
	}
	s := loadState(t, h)
	if !s.Payments["pay-1"].Auto || !s.Payments["pay-2"].Auto {
		t.Fatal("自动登记身份应跨重启保持")
	}
}

func TestRemitImportCorruptStoreRejectedAndPreserved(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5"})
	p := h.statePath()
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	errText := h.runExpectErr("bill", "remit-import", remitFile(h, "r.csv", "c1,pay-1,100,汇款\n"))
	if !strings.Contains(errText, "数据文件已损坏") {
		t.Fatal(errText)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "{not json" {
		t.Fatal("损坏存档必须原样保留")
	}
}

func TestRemitImportUsageError(t *testing.T) {
	h := newHarness(t)
	// 参数个数错误属于用法错误（退出码 2）。
	_, err := h.run("bill", "remit-import")
	var ue usageErrorf
	if err == nil {
		t.Fatal("缺少文件参数应失败")
	}
	if !errors.As(err, &ue) {
		t.Fatalf("应为用法错误，得到 %v", err)
	}
}
