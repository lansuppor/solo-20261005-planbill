package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// remit-auto：按最早欠款账期自动分配收款。

// settleFixed 是测试辅助：登记固定单价客户并导入单月用量后结算。
func settleFixedCust(h *harness, custID, price string, usage map[string]string) {
	h.t.Helper()
	h.mustRun("customer", "add", custID, "客户"+custID, price)
	var sb strings.Builder
	sb.WriteString(csvHeader)
	i := 0
	for month, qty := range usage {
		i++
		id := custID + "-u" + string(rune('a'+i))
		sb.WriteString(id + "," + custID + "," + month + "-15T10:00:00Z," + qty + "\n")
	}
	f := h.writeFile("usage-"+custID+".csv", sb.String())
	h.mustRun("usage", "import", f)
	for month := range usage {
		h.mustRun("bill", "settle", custID, month)
	}
}

func loadState(t *testing.T, h *harness) *state {
	t.Helper()
	s, err := loadStore(h.dir)
	if err != nil {
		t.Fatalf("载入存档失败: %v", err)
	}
	return s
}

func TestRemitAutoHappyPathPartialLastMonth(t *testing.T) {
	h := newHarness(t)
	// 三个月账单：2026-09 欠 500，2026-10 欠 300，2026-11 欠 200（单价 100 分）。
	settleFixedCust(h, "c1", "100", map[string]string{
		"2026-09": "5", "2026-10": "3", "2026-11": "2",
	})

	out := h.mustRun("bill", "remit-auto", "c1", "pay-a1", "900", "季度汇款")
	for _, want := range []string{
		"收款标识：pay-a1",
		"收款总额：900 分",
		"首次分配（登记时按最早欠款账期自动确定，永久保留）：2026-09:500,2026-10:300,2026-11:100",
		"最新分配：2026-09:500,2026-10:300,2026-11:100（与首次分配相同）",
		"当前状态：实收中",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("自动登记输出缺少 %q:\n%s", want, out)
		}
	}
	// 最后一月部分偿还：11 月未收余额 100。
	if !strings.Contains(out, "月份 2026-11：首次分配 100 分，最新分配 100 分；当前应付 200 分") {
		t.Fatalf("最后一月应部分偿还:\n%s", out)
	}

	// 占用一个账后全局序号，不按月份拆成多笔。
	s := loadState(t, h)
	if s.NextSeq != 1 {
		t.Fatalf("应只占用一个序号，NextSeq=%d", s.NextSeq)
	}
	p := s.Payments["pay-a1"]
	if p == nil || !p.Auto || p.Seq != 1 || len(p.Allocations) != 3 {
		t.Fatalf("收款记录异常: %+v", p)
	}

	// bill show 可追溯：各月实收与未收余额。
	out = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "实收：500 分") || !strings.Contains(out, "未收余额：0 分") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "show", "c1", "2026-11")
	if !strings.Contains(out, "实收：100 分") || !strings.Contains(out, "未收余额：100 分") {
		t.Fatal(out)
	}
	// ledger：跨月登记只作为一次操作（同一序号的单个收款事件）。
	out = h.mustRun("bill", "ledger", "c1", "2026-10")
	if !strings.Contains(out, "序号 1 收款 pay-a1") || !strings.Contains(out, "本账单分配 300 分") {
		t.Fatal(out)
	}
	// reconcile：跨账期报表可追溯。
	out = h.mustRun("bill", "reconcile", "c1", "2026-09", "2026-11")
	if !strings.Contains(out, "pay-a1") {
		t.Fatal(out)
	}
	// 重复结算仍返回原账单，不受收款影响。
	out = h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "已结算，返回原账单") || !strings.Contains(out, "实收：500 分") {
		t.Fatal(out)
	}
}

func TestRemitAutoSkipsZeroBalanceMonths(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{
		"2026-09": "5", "2026-10": "3", "2026-11": "2",
	})
	// 先用显式收款把 2026-09 还清，自动分配应跳过它。
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-x", "500", "银行转账")

	out := h.mustRun("bill", "remit-auto", "c1", "pay-a1", "400", "汇款")
	if !strings.Contains(out, "首次分配（登记时按最早欠款账期自动确定，永久保留）：2026-10:300,2026-11:100") {
		t.Fatalf("应跳过余额为 0 的 2026-09:\n%s", out)
	}
}

func TestRemitAutoNetOfAdjustRevokeRefund(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{
		"2026-09": "5", "2026-10": "3",
	})
	// 09 月：调整 +100 后撤销，再减免 -50 → 应付 450；登记收款 200 并退 50 → 实收 150。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "100", "补收")
	h.mustRun("bill", "revoke", "adj-1", "录错")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "-50", "减免")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-x", "200", "转账")
	h.mustRun("bill", "refund", "pay-x", "rf-1", "多收", "2026-09:50")
	// 09 月未收 = 450 - 150 = 300；自动登记 400 → 09:300、10:100。
	out := h.mustRun("bill", "remit-auto", "c1", "pay-a1", "400", "汇款")
	if !strings.Contains(out, "首次分配（登记时按最早欠款账期自动确定，永久保留）：2026-09:300,2026-10:100") {
		t.Fatalf("未收应按调整、撤销及退款后的净额计算:\n%s", out)
	}
}

func TestRemitAutoRejectNoDebtAndExcess(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5"})

	// 总金额超过全部欠款（500）：整笔拒绝。
	errText := h.runExpectErr("bill", "remit-auto", "c1", "pay-a1", "501", "汇款")
	if !strings.Contains(errText, "超过") || !strings.Contains(errText, "欠款合计 500 分") {
		t.Fatal(errText)
	}
	// 失败后不占标识、不占序号，可原样重试。
	out := h.mustRun("bill", "remit-auto", "c1", "pay-a1", "500", "汇款")
	if !strings.Contains(out, "首次分配（登记时按最早欠款账期自动确定，永久保留）：2026-09:500") {
		t.Fatal(out)
	}
	if s := loadState(t, h); s.NextSeq != 1 {
		t.Fatalf("失败尝试不得占用序号，NextSeq=%d", s.NextSeq)
	}

	// 无欠款：整笔拒绝。
	errText = h.runExpectErr("bill", "remit-auto", "c1", "pay-a2", "1", "汇款")
	if !strings.Contains(errText, "没有欠款") {
		t.Fatal(errText)
	}
	// 客户没有任何账单也视为无欠款。
	h.mustRun("customer", "add", "c2", "客户c2", "100")
	errText = h.runExpectErr("bill", "remit-auto", "c2", "pay-a3", "1", "汇款")
	if !strings.Contains(errText, "没有欠款") {
		t.Fatal(errText)
	}
}

func TestRemitAutoIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{
		"2026-09": "5", "2026-10": "3",
	})
	h.mustRun("bill", "remit-auto", "c1", "pay-a1", "600", "季度汇款")

	// 相同请求（客户、总金额、备注相同）：返回原收款及当前状态，不写盘、
	// 不重新分配、不增加序号——即使随后新增了账单。
	settleFixedCust(h, "c1x", "100", map[string]string{"2026-09": "1"}) // 其他客户干扰
	h.mustRun("usage", "import", h.writeFile("u-new.csv", csvHeader+"unew,c1,2026-11-15T10:00:00Z,4\n"))
	h.mustRun("bill", "settle", "c1", "2026-11")
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("bill", "remit-auto", "c1", "pay-a1", "600", "季度汇款")
	if !strings.Contains(out, "已存在且内容相同") ||
		!strings.Contains(out, "首次分配（登记时按最早欠款账期自动确定，永久保留）：2026-09:500,2026-10:100") {
		t.Fatalf("重放应返回原收款且不重新分配:\n%s", out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("重放不得写盘")
	}
	if s := loadState(t, h); s.NextSeq != 1 {
		t.Fatalf("重放不得增加序号，NextSeq=%d", s.NextSeq)
	}

	// 内容不同（总额、备注、客户任一不同）拒绝。
	h.runExpectErr("bill", "remit-auto", "c1", "pay-a1", "601", "季度汇款")
	h.runExpectErr("bill", "remit-auto", "c1", "pay-a1", "600", "另一备注")
	h.runExpectErr("bill", "remit-auto", "c1x", "pay-a1", "600", "季度汇款")
}

func TestRemitAutoCrossReuseRejected(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{
		"2026-09": "5", "2026-10": "3",
	})
	h.mustRun("bill", "remit-auto", "c1", "pay-auto", "500", "自动")
	h.mustRun("bill", "pay", "c1", "2026-10", "pay-exp", "100", "显式")

	// 自动标识在显式入口复用拒绝（即使内容碰巧相同也拒绝）。
	h.runExpectErr("bill", "pay", "c1", "2026-10", "pay-auto", "100", "显式")
	h.runExpectErr("bill", "remit", "c1", "pay-auto", "500", "自动", "2026-10:500")
	// 显式标识在自动入口复用拒绝。
	h.runExpectErr("bill", "remit-auto", "c1", "pay-exp", "100", "显式")
	// pay 与 remit 之间原有判重保持：相同内容重放成功。
	out := h.mustRun("bill", "remit", "c1", "pay-exp", "100", "显式", "2026-10:100")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatal(out)
	}
	// 收款标识仍与调整、更正、退款标识独立：同名不冲突。
	h.mustRun("bill", "adjust", "c1", "2026-09", "pay-auto", "10", "同名调整")
	h.mustRun("bill", "refund", "pay-exp", "pay-auto", "同名退款", "2026-10:10")
}

func TestRemitAutoCorrectUnpayRefund(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{
		"2026-09": "5", "2026-10": "3",
	})
	h.mustRun("bill", "remit-auto", "c1", "pay-a1", "600", "汇款") // 09:500 10:100

	// 分配更正：把 09 月的 100 挪到 10 月。
	out := h.mustRun("bill", "correct", "pay-a1", "corr-1", "入账月份错误", "2026-09:400", "2026-10:200")
	if !strings.Contains(out, "已登记收款分配更正") {
		t.Fatal(out)
	}
	// 重放自动登记：首次分配不变，最新分配为更正后。
	out = h.mustRun("bill", "remit-auto", "c1", "pay-a1", "600", "汇款")
	if !strings.Contains(out, "首次分配（登记时按最早欠款账期自动确定，永久保留）：2026-09:500,2026-10:100") ||
		!strings.Contains(out, "最新分配（经更正，以最新为准）：2026-09:400,2026-10:200") {
		t.Fatalf("重放应展示首次与最新分配:\n%s", out)
	}

	// 部分退款后最新分配固定：拒绝更正与整笔撤销。
	h.mustRun("bill", "refund", "pay-a1", "rf-1", "多收退回", "2026-10:50")
	h.runExpectErr("bill", "correct", "pay-a1", "corr-2", "再更正", "2026-09:600")
	h.runExpectErr("bill", "unpay", "pay-a1", "撤销")
	out = h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(out, "实收：150 分") || !strings.Contains(out, "未收余额：150 分") {
		t.Fatal(out)
	}
}

func TestRemitAutoUnpayThenBalancesFreed(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{
		"2026-09": "5", "2026-10": "3",
	})
	h.mustRun("bill", "remit-auto", "c1", "pay-a1", "600", "汇款")
	h.mustRun("bill", "unpay", "pay-a1", "登记错误")
	// 撤销后欠款恢复，可再次自动登记（新标识）。
	out := h.mustRun("bill", "remit-auto", "c1", "pay-a2", "800", "重新汇款")
	if !strings.Contains(out, "首次分配（登记时按最早欠款账期自动确定，永久保留）：2026-09:500,2026-10:300") {
		t.Fatal(out)
	}
	// 原收款重放返回已撤销状态，不恢复实收。
	out = h.mustRun("bill", "remit-auto", "c1", "pay-a1", "600", "汇款")
	if !strings.Contains(out, "当前状态：已撤销") {
		t.Fatal(out)
	}
}

func TestRemitAutoOverflowSumNotMisrejected(t *testing.T) {
	h := newHarness(t)
	// 两张账单各欠 MaxInt64 分，欠款合计远超 64 位上限；
	// 合法总额 MaxInt64 不得被误拒，应全部分配到最早月。
	settleFixedCust(h, "c1", "9223372036854775807", map[string]string{
		"2026-09": "1", "2026-10": "1",
	})
	out := h.mustRun("bill", "remit-auto", "c1", "pay-big", "9223372036854775807", "巨额汇款")
	if !strings.Contains(out, "首次分配（登记时按最早欠款账期自动确定，永久保留）：2026-09:9223372036854775807") {
		t.Fatalf("合计超 64 位不得误拒合法金额:\n%s", out)
	}
	// 再登记 1 分也应进入次月。
	out = h.mustRun("bill", "remit-auto", "c1", "pay-big2", "1", "尾款")
	if !strings.Contains(out, "首次分配（登记时按最早欠款账期自动确定，永久保留）：2026-10:1") {
		t.Fatal(out)
	}
	// 超过剩余欠款（MaxInt64 - 1）时整笔拒绝。
	errText := h.runExpectErr("bill", "remit-auto", "c1", "pay-big3", "9223372036854775807", "超额")
	if !strings.Contains(errText, "超过") {
		t.Fatal(errText)
	}
}

func TestRemitAutoInvalidInput(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{"2026-09": "5"})

	// 总金额必须为正整数分。
	h.runExpectErr("bill", "remit-auto", "c1", "p1", "0", "备注")
	h.runExpectErr("bill", "remit-auto", "c1", "p1", "-100", "备注")
	h.runExpectErr("bill", "remit-auto", "c1", "p1", "1.5", "备注")
	h.runExpectErr("bill", "remit-auto", "c1", "p1", "abc", "备注")
	h.runExpectErr("bill", "remit-auto", "c1", "p1", "99999999999999999999999", "备注")
	// 标识与备注非空。
	h.runExpectErr("bill", "remit-auto", "c1", "", "100", "备注")
	h.runExpectErr("bill", "remit-auto", "c1", "   ", "100", "备注")
	h.runExpectErr("bill", "remit-auto", "c1", "p1", "100", "")
	h.runExpectErr("bill", "remit-auto", "c1", "p1", "100", "   ")
	// 客户缺失。
	h.runExpectErr("bill", "remit-auto", "ghost", "p1", "100", "备注")
	// 参数个数。
	if _, err := h.run("bill", "remit-auto", "c1", "p1", "100"); err == nil {
		t.Fatal("缺少参数应失败")
	}
	// 全部失败均不产生状态。
	s := loadState(t, h)
	if len(s.Payments) != 0 || s.NextSeq != 0 {
		t.Fatalf("非法输入不得改变业务状态: %+v", s.Payments)
	}
}

func TestRemitAutoOldFormatPaymentReadAsExplicit(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{
		"2026-09": "5", "2026-10": "3",
	})
	// 手工写入旧版单月格式收款（month + amount_fen，无 allocations/auto）。
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-old", "100", "旧格式")
	raw, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	rec := doc["payments"].(map[string]any)["pay-old"].(map[string]any)
	delete(rec, "allocations")
	rec["month"] = "2026-09"
	rec["amount_fen"] = 100
	raw, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.statePath(), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// 旧格式收款按显式分配登记读取：自动入口复用其标识拒绝。
	h.runExpectErr("bill", "remit-auto", "c1", "pay-old", "100", "旧格式")
	// 显式入口按原判重规则幂等。
	out := h.mustRun("bill", "pay", "c1", "2026-09", "pay-old", "100", "旧格式")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatal(out)
	}
	// 载入后归一化为一项分配，写回新格式且不带 auto 标记。
	s := loadState(t, h)
	p := s.Payments["pay-old"]
	if p.Auto || len(p.Allocations) != 1 || p.Allocations[0].Month != "2026-09" || p.Allocations[0].Amount != 100 {
		t.Fatalf("旧格式收款归一化异常: %+v", p)
	}
}

func TestRemitAutoPersistsAcrossRestart(t *testing.T) {
	h := newHarness(t)
	settleFixedCust(h, "c1", "100", map[string]string{
		"2026-09": "5", "2026-10": "3",
	})
	h.mustRun("bill", "remit-auto", "c1", "pay-a1", "600", "汇款")
	// 每次 run 都重新从磁盘载入，模拟重启：判重与余额保持。
	out := h.mustRun("bill", "remit-auto", "c1", "pay-a1", "600", "汇款")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(out, "实收：100 分") {
		t.Fatal(out)
	}
	s := loadState(t, h)
	if !s.Payments["pay-a1"].Auto {
		t.Fatal("自动登记身份应跨重启保持")
	}
}
