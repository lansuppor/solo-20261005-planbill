package main

import (
	"os"
	"strings"
	"testing"
)

// 账后对账共享计算的回归验证：连续更正跨范围迁移、零差额与移出后撤销的
// 展示差异、更正后部分退款及再收款、历史截止、完整流水异常与整数边界。
// 两类查询（bill ledger / bill reconcile）共用同一套业务归属与金额影响
// 计算，这里按业务预期核对事件关联、逐月余额与报表汇总。

// setupPostbillMonths 登记客户 c1 并结算三个月：2026-01=1000 分、
// 2026-02=2000 分、2026-03=1000 分（固定单价 100）。
func setupPostbillMonths(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-01-15T10:00:00Z,10\n"+
		"u2,c1,2026-02-10T10:00:00Z,20\n"+
		"u3,c1,2026-03-05T10:00:00Z,10\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-01")
	h.mustRun("bill", "settle", "c1", "2026-02")
	h.mustRun("bill", "settle", "c1", "2026-03")
	return h
}

// 连续更正跨范围迁移：一笔汇款经多次更正在范围内/外月份间迁移，报表每次
// 只按当时前后分配计范围差额，逐月余额与相同截止的 ledger 一致。
func TestPostbillConsecutiveCorrectionsAcrossRange(t *testing.T) {
	h := setupPostbillMonths(t)
	// 1 收款 pay-1 600：2026-01:200（范围内）、2026-03:400（范围外）。
	h.mustRun("bill", "remit", "c1", "pay-1", "600", "汇款", "2026-01:200", "2026-03:400")
	// 2 更正 cor-1：2026-01:200 → 2026-02:200（范围内迁移，差额合计 0）。
	h.mustRun("bill", "correct", "pay-1", "cor-1", "第一次更正", "2026-02:200", "2026-03:400")
	// 3 更正 cor-2：2026-02:200 → 2026-01:200（范围内回迁）。
	h.mustRun("bill", "correct", "pay-1", "cor-2", "第二次更正", "2026-01:200", "2026-03:400")
	// 4 更正 cor-3：全部转出范围到 2026-03。
	h.mustRun("bill", "correct", "pay-1", "cor-3", "转出范围", "2026-03:600")

	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	for _, want := range []string{
		"序号 1 收款 pay-1：备注：汇款；汇款总额 600 分（6.00 元）；范围内变化：2026-01 实收 +200 分（分配 200 分）",
		"事后汇总：应付 3000 分，实收 200 分，未收余额 2800 分",
		"序号 2 更正 cor-1（关联收款 pay-1）：原因：第一次更正；范围内变化：2026-01 实收 -200 分（分配 200 分 → 0 分），2026-02 实收 +200 分（分配 0 分 → 200 分）",
		"序号 3 更正 cor-2（关联收款 pay-1）：原因：第二次更正；范围内变化：2026-01 实收 +200 分（分配 0 分 → 200 分），2026-02 实收 -200 分（分配 200 分 → 0 分）",
		"序号 4 更正 cor-3（关联收款 pay-1）：原因：转出范围；范围内变化：2026-01 实收 -200 分（分配 200 分 → 0 分）",
		"终点合计：应付 3000 分（30.00 元），实收 0 分（0.00 元），未收余额 3000 分（30.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q：\n%s", want, out)
		}
	}

	// 逐月 ledger：2026-01 经历 +200、-200、+200、-200；2026-02 经历 +200、-200。
	ledger1 := h.mustRun("bill", "ledger", "c1", "2026-01")
	for _, want := range []string{
		"序号 1 收款 pay-1：实收 +200 分",
		"序号 2 更正 cor-1：实收 -200 分（-2.00 元，关联收款 pay-1，本账单分配 200 分 → 0 分）",
		"序号 3 更正 cor-2：实收 +200 分（2.00 元，关联收款 pay-1，本账单分配 0 分 → 200 分）",
		"序号 4 更正 cor-3：实收 -200 分（-2.00 元，关联收款 pay-1，本账单分配 200 分 → 0 分）",
		"截止时余额：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）",
	} {
		if !strings.Contains(ledger1, want) {
			t.Fatalf("2026-01 流水缺少 %q：\n%s", want, ledger1)
		}
	}
	ledger2 := h.mustRun("bill", "ledger", "c1", "2026-02")
	for _, want := range []string{
		"序号 2 更正 cor-1：实收 +200 分",
		"序号 3 更正 cor-2：实收 -200 分",
		"截止时余额：应付 2000 分（20.00 元），实收 0 分（0.00 元），未收余额 2000 分（20.00 元）",
	} {
		if !strings.Contains(ledger2, want) {
			t.Fatalf("2026-02 流水缺少 %q：\n%s", want, ledger2)
		}
	}

	// 历史截止：截止 2 时报表与 ledger 逐月一致；截止后的更正不提前影响。
	cut := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02", "0", "2")
	for _, want := range []string{
		"终点合计：应付 3000 分（30.00 元），实收 200 分（2.00 元），未收余额 2800 分（28.00 元）",
		"月份 2026-01", "终点：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）",
		"月份 2026-02", "终点：应付 2000 分（20.00 元），实收 200 分（2.00 元），未收余额 1800 分（18.00 元）",
	} {
		if !strings.Contains(cut, want) {
			t.Fatalf("截止 2 报表缺少 %q：\n%s", want, cut)
		}
	}
	l2 := h.mustRun("bill", "ledger", "c1", "2026-02", "2")
	if !strings.Contains(l2, "截止时余额：应付 2000 分（20.00 元），实收 200 分（2.00 元），未收余额 1800 分（18.00 元）") {
		t.Fatalf("ledger 截止 2 与报表不一致：\n%s", l2)
	}
}

// 零差额更正：同一月份前后分配相同，两类查询都保留该月零差额明细（事件仍
// 出现），而前后均不涉及的月份不列更正。
func TestPostbillZeroDeltaCorrectionDetail(t *testing.T) {
	h := setupPostbillMonths(t)
	// 1 收款 pay-1 300：2026-01:200、2026-02:100。
	h.mustRun("bill", "remit", "c1", "pay-1", "300", "汇款", "2026-01:200", "2026-02:100")
	// 2 更正 cor-1：2026-01 仍 200（零差额），2026-02:100 → 2026-03:100。
	h.mustRun("bill", "correct", "pay-1", "cor-1", "修正", "2026-01:200", "2026-03:100")

	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	// 2026-01 零差额明细保留；2026-02 -100；2026-03 不在范围、不出现在该更正。
	line := "序号 2 更正 cor-1（关联收款 pay-1）：原因：修正；范围内变化：2026-01 实收 +0 分（分配 200 分 → 200 分），2026-02 实收 -100 分（分配 100 分 → 0 分）"
	if !strings.Contains(out, line) {
		t.Fatalf("报表应保留零差额明细且不含 2026-03：\n%s", out)
	}
	if strings.Contains(out, "2026-03 实收") {
		t.Fatalf("前后均不涉及范围的月份不应列更正：\n%s", out)
	}

	// ledger 2026-01 同样保留零差额更正事件。
	ledger := h.mustRun("bill", "ledger", "c1", "2026-01")
	if !strings.Contains(ledger, "序号 2 更正 cor-1：实收 +0 分（0.00 元，关联收款 pay-1，本账单分配 200 分 → 200 分）") {
		t.Fatalf("流水应保留零差额更正：\n%s", ledger)
	}
	// ledger 2026-02 有移出事件；2026-03 有移入事件。
	if l2 := h.mustRun("bill", "ledger", "c1", "2026-02"); !strings.Contains(l2, "序号 2 更正 cor-1：实收 -100 分") {
		t.Fatalf("2026-02 应有移出更正：\n%s", l2)
	}
	if l3 := h.mustRun("bill", "ledger", "c1", "2026-03"); !strings.Contains(l3, "序号 2 更正 cor-1：实收 +100 分") {
		t.Fatalf("2026-03 应有移入更正：\n%s", l3)
	}
}

// 移出后撤销的展示差异：收款被更正全部移出某月后再整笔撤销——
//   - 单账单流水保留该月零金额撤销事件（曾涉及）；
//   - 跨账期报表只按撤销当时分配判断，不因曾经涉及而增加操作。
func TestPostbillMovedOutRevokeDisplayDifference(t *testing.T) {
	h := setupPostbillMonths(t)
	// 1 收款 pay-1 200 全部落在 2026-01。
	h.mustRun("bill", "pay", "c1", "2026-01", "pay-1", "200", "收款")
	// 2 更正 cor-1：全部移到 2026-03。
	h.mustRun("bill", "correct", "pay-1", "cor-1", "移出", "2026-03:200")
	// 3 撤销：取消的是撤销时最新分配（2026-03:200）。
	h.mustRun("bill", "unpay", "pay-1", "撤销")

	// 2026-01 流水：收款、更正移出，仍保留零金额撤销事件。
	l1 := h.mustRun("bill", "ledger", "c1", "2026-01")
	for _, want := range []string{
		"序号 1 收款 pay-1：实收 +200 分",
		"序号 2 更正 cor-1：实收 -200 分",
		"序号 3 撤销收款 pay-1：实收 +0 分（0.00 元，关联序号 1 的收款 pay-1，汇款总额 200 分，取消本账单分配 0 分）",
		"截止时余额：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）",
	} {
		if !strings.Contains(l1, want) {
			t.Fatalf("2026-01 流水缺少 %q：\n%s", want, l1)
		}
	}

	// 报表范围 2026-01..2026-02：撤销时最新分配全在 2026-03，不纳入报表。
	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	if !strings.Contains(out, "序号 1 收款 pay-1") || !strings.Contains(out, "序号 2 更正 cor-1") {
		t.Fatalf("收款与移出更正应在报表中：\n%s", out)
	}
	if strings.Contains(out, "序号 3 撤销收款 pay-1") {
		t.Fatalf("撤销只按当时最新分配判断，不应因曾涉及而纳入报表：\n%s", out)
	}
	if !strings.Contains(out, "终点合计：应付 3000 分（30.00 元），实收 0 分（0.00 元），未收余额 3000 分（30.00 元）") {
		t.Fatalf("报表终点汇总异常：\n%s", out)
	}

	// 范围包含 2026-03 时撤销按最新分配正常出现一次。
	all := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-03")
	if !strings.Contains(all, "序号 3 撤销收款 pay-1（关联序号 1 的收款 pay-1）：原因：撤销；汇款总额 200 分（2.00 元）；范围内变化：2026-03 实收 -200 分（取消分配 200 分）") {
		t.Fatalf("范围内撤销应取消最新分配：\n%s", all)
	}
}

// 更正后部分退款及再收款：退款固定最新分配、只减对应月实收；退款释放的
// 未收余额可再收款；多月退款在报表中作为一次操作展示。
func TestPostbillRefundAfterCorrectionThenRepay(t *testing.T) {
	h := setupPostbillMonths(t)
	// 1 收款 pay-1 400：2026-01:300、2026-02:100。
	h.mustRun("bill", "remit", "c1", "pay-1", "400", "汇款", "2026-01:300", "2026-02:100")
	// 2 更正 cor-1：2026-01:100、2026-02:300。
	h.mustRun("bill", "correct", "pay-1", "cor-1", "更正", "2026-01:100", "2026-02:300")
	// 3 退款 rf-1：从最新分配退 2026-01:100（退尽）、2026-02:200。
	h.mustRun("bill", "refund", "pay-1", "rf-1", "多收退回", "2026-01:100", "2026-02:200")
	// 退款释放未收：2026-02 现实收 100、应付 2000，可再收 100。
	h.mustRun("bill", "pay", "c1", "2026-02", "pay-2", "100", "补款") // 序号 4

	// ledger 2026-02：收款 100、更正 +200(→300)、退款 -200、再收款 +100。
	l2 := h.mustRun("bill", "ledger", "c1", "2026-02")
	for _, want := range []string{
		"序号 1 收款 pay-1：实收 +100 分",
		"序号 2 更正 cor-1：实收 +200 分（2.00 元，关联收款 pay-1，本账单分配 100 分 → 300 分）",
		"序号 3 退款 rf-1：实收 -200 分（-2.00 元，关联收款 pay-1，本账单退款 200 分）",
		"序号 4 收款 pay-2：实收 +100 分",
		"截止时余额：应付 2000 分（20.00 元），实收 200 分（2.00 元），未收余额 1800 分（18.00 元）",
	} {
		if !strings.Contains(l2, want) {
			t.Fatalf("2026-02 流水缺少 %q：\n%s", want, l2)
		}
	}
	// ledger 2026-01：收款 300、更正 -200(→100)、退款 -100（退尽）→ 实收 0。
	l1 := h.mustRun("bill", "ledger", "c1", "2026-01")
	for _, want := range []string{
		"序号 3 退款 rf-1：实收 -100 分",
		"截止时余额：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）",
	} {
		if !strings.Contains(l1, want) {
			t.Fatalf("2026-01 流水缺少 %q：\n%s", want, l1)
		}
	}

	// 报表：跨月退款是一次操作，只计范围内各月退款额；再收款独立列示。
	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	if !strings.Contains(out, "序号 3 退款 rf-1（关联收款 pay-1）：原因：多收退回；范围内变化：2026-01 实收 -100 分（退款 100 分），2026-02 实收 -200 分（退款 200 分）") {
		t.Fatalf("跨月退款应作为一次操作：\n%s", out)
	}
	if !strings.Contains(out, "序号 4 收款 pay-2") ||
		!strings.Contains(out, "终点合计：应付 3000 分（30.00 元），实收 200 分（2.00 元），未收余额 2800 分（28.00 元）") {
		t.Fatalf("再收款或终点汇总异常：\n%s", out)
	}
	// 最新余额与 bill show 一致。
	for month, bal := range map[string]string{
		"2026-01": "当前应付：1000 分",
		"2026-02": "实收：200 分",
	} {
		show := h.mustRun("bill", "show", "c1", month)
		if !strings.Contains(show, bal) {
			t.Fatalf("bill show %s 缺少 %q：\n%s", month, bal, show)
		}
	}

	// 退款后最新分配固定：再更正、整笔撤销都被拒绝。
	if msg := h.runExpectErr("bill", "correct", "pay-1", "cor-9", "再更正", "2026-01:400"); !strings.Contains(msg, "最新分配已固定") {
		t.Fatalf("退款后应拒绝更正：%s", msg)
	}
	if msg := h.runExpectErr("bill", "unpay", "pay-1", "整笔撤销"); !strings.Contains(msg, "最新分配已固定") {
		t.Fatalf("退款后应拒绝整笔撤销：%s", msg)
	}
}

// 历史截止回放：截止包含该序号，截止之后的更正、撤销都不能提前影响历史
// 余额，也不混入流水或报表；报表变动区间为起点之后、终点以内，逐月余额
// 两类查询一致。
func TestPostbillHistoricalCutoff(t *testing.T) {
	h := setupPostbillMonths(t)
	h.mustRun("bill", "pay", "c1", "2026-01", "pay-1", "400", "收款")                    // 1
	h.mustRun("bill", "adjust", "c1", "2026-01", "adj-1", "100", "补收")                 // 2
	h.mustRun("bill", "correct", "pay-1", "cor-1", "更正", "2026-01:100", "2026-02:300") // 3
	h.mustRun("bill", "revoke", "adj-1", "撤回补收")                                       // 4
	h.mustRun("bill", "unpay", "pay-1", "撤销")                                          // 5

	// 单账单流水在各截止的余额：截止包含该序号，后续操作不提前影响。
	cases := []struct {
		cutoff string
		want   string
	}{
		{"0", "截止时余额：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）"},
		{"1", "截止时余额：应付 1000 分（10.00 元），实收 400 分（4.00 元），未收余额 600 分（6.00 元）"},
		{"2", "截止时余额：应付 1100 分（11.00 元），实收 400 分（4.00 元），未收余额 700 分（7.00 元）"},
		{"3", "截止时余额：应付 1100 分（11.00 元），实收 100 分（1.00 元），未收余额 1000 分（10.00 元）"},
		{"4", "截止时余额：应付 1000 分（10.00 元），实收 100 分（1.00 元），未收余额 900 分（9.00 元）"},
		{"5", "截止时余额：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）"},
	}
	for _, c := range cases {
		out := h.mustRun("bill", "ledger", "c1", "2026-01", c.cutoff)
		if !strings.Contains(out, c.want) {
			t.Fatalf("2026-01 截止 %s 期望 %q：\n%s", c.cutoff, c.want, out)
		}
	}

	// 截止 2 的流水不得混入序号 3 之后的更正与撤销。
	cut2 := h.mustRun("bill", "ledger", "c1", "2026-01", "2")
	if strings.Contains(cut2, "更正") || strings.Contains(cut2, "撤销") {
		t.Fatalf("截止之后的操作混入流水：\n%s", cut2)
	}

	// 报表区间 (1, 3]：起点含收款与初始应付，终点含补收与更正；区间内只
	// 出现序号 2、3，逐月终点与 ledger 截止 3 一致。
	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02", "1", "3")
	for _, want := range []string{
		"操作序号区间：起点 1、终点 3（指定）",
		"起点合计：应付 3000 分（30.00 元），实收 400 分（4.00 元），未收余额 2600 分（26.00 元）",
		"终点合计：应付 3100 分（31.00 元），实收 400 分（4.00 元），未收余额 2700 分（27.00 元）",
		"序号 2 调整 adj-1",
		"序号 3 更正 cor-1（关联收款 pay-1）",
		"月份 2026-01", "终点：应付 1100 分（11.00 元），实收 100 分（1.00 元），未收余额 1000 分（10.00 元）",
		"月份 2026-02", "终点：应付 2000 分（20.00 元），实收 300 分（3.00 元），未收余额 1700 分（17.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("区间 (1,3] 报表缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "序号 1 收款") || strings.Contains(out, "撤销") {
		t.Fatalf("区间外操作混入报表：\n%s", out)
	}

	// 终点为最新时报表逐月余额与 bill show 一致（撤销后全部归零）。
	final := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	if !strings.Contains(final, "终点合计：应付 3000 分（30.00 元），实收 0 分（0.00 元），未收余额 3000 分（30.00 元）") {
		t.Fatalf("最新终点汇总异常：\n%s", final)
	}
}

// 完整流水异常：构造最终余额合法但中间步骤实收超过应付的存档，载入时的逐步
// 回放即拒绝（任何命令都无法读取），且保留原文件、不覆盖。
func TestPostbillRejectIntermediateAnomaly(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-01-15T10:00:00Z,10\n"+
		"u2,c1,2026-02-10T10:00:00Z,20\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-01")
	h.mustRun("bill", "settle", "c1", "2026-02")
	// 序号 1 收款 1000；序号 2 减免 -1500（应付 500 < 实收 1000，中间越界）；
	// 序号 3 补回 +1000 后应付 1500 ≥ 实收 1000，最终余额合法——载入时的逐步
	// 回放仍必须拒绝。
	corrupt := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {
    "u1": {"id": "u1", "customer_id": "c1", "time": "2026-01-15T10:00:00Z", "quantity": 10},
    "u2": {"id": "u2", "customer_id": "c1", "time": "2026-02-10T10:00:00Z", "quantity": 20}
  },
  "bills": {
    "c1|2026-01": {"id": "B1", "customer_id": "c1", "month": "2026-01",
      "total_quantity": 10, "unit_price_fen": 100, "total_fee_fen": 1000,
      "lines": [{"usage_id": "u1", "time": "2026-01-15T10:00:00Z", "quantity": 10, "line_fee_fen": 1000}],
      "created_at": "2026-03-01T00:00:00Z"},
    "c1|2026-02": {"id": "B2", "customer_id": "c1", "month": "2026-02",
      "total_quantity": 20, "unit_price_fen": 100, "total_fee_fen": 2000,
      "lines": [{"usage_id": "u2", "time": "2026-02-10T10:00:00Z", "quantity": 20, "line_fee_fen": 2000}],
      "created_at": "2026-03-01T00:00:00Z"}
  },
  "adjustments": {
    "a1": {"id": "a1", "customer_id": "c1", "month": "2026-02", "amount_fen": -1500,
      "reason": "减免", "seq": 2, "created_at": "2026-03-02T00:00:00Z"},
    "a2": {"id": "a2", "customer_id": "c1", "month": "2026-02", "amount_fen": 1000,
      "reason": "补回", "seq": 3, "created_at": "2026-03-03T00:00:00Z"}
  },
  "payments": {"p1": {"id": "p1", "customer_id": "c1", "total_fen": 1000, "note": "收款",
    "allocations": [{"month": "2026-02", "amount_fen": 1000}], "seq": 1,
    "created_at": "2026-03-02T00:00:00Z"}},
  "next_seq": 3
}`
	if err := os.WriteFile(h.statePath(), []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	// 载入即按数据损坏拒绝：bill show 与任意截止的流水、报表命令都失败。
	for _, args := range [][]string{
		{"bill", "show", "c1", "2026-02"},
		{"bill", "ledger", "c1", "2026-02"},
		{"bill", "ledger", "c1", "2026-02", "1"}, // 截止早于异常序号 2 仍拒绝
		{"bill", "ledger", "c1", "2026-02", "0"},
	} {
		if msg := h.runExpectErr(args...); !strings.Contains(msg, "数据文件已损坏") {
			t.Fatalf("args=%v 应按数据文件损坏拒绝：%s", args, msg)
		}
	}
	// 报表同样整体拒绝：终点在异常之前、只查未涉及异常的 2026-01 也拒绝
	// （核验覆盖选中账单的完整流水）。
	for _, args := range [][]string{
		{"bill", "reconcile", "c1", "2026-01", "2026-02", "0", "1"},
		{"bill", "reconcile", "c1", "2026-01", "2026-02"},
	} {
		if msg := h.runExpectErr(args...); !strings.Contains(msg, "数据文件已损坏") {
			t.Fatalf("args=%v 应按数据文件损坏拒绝：%s", args, msg)
		}
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != corrupt {
		t.Fatal("载入失败改写了存档")
	}
}

// 整数边界：累计补收/收款发生额远超 64 位上限但逐步余额合法时，流水仍准确
// 输出；多账单汇总超过 64 位上限时报表以 128 位精确输出。
func TestPostbillIntegerBoundaries(t *testing.T) {
	h := newHarness(t)
	// 两张账单各为 MaxInt64（单价 MaxInt64、数量 1），合计 2*(2^63-1) 超上限。
	h.mustRun("customer", "add", "c1", "甲方", "9223372036854775807")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-01-15T10:00:00Z,1\n"+
		"u2,c1,2026-02-10T10:00:00Z,1\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-01")
	h.mustRun("bill", "settle", "c1", "2026-02")
	// 在 2026-01 上做巨额补收→收款→撤销→撤回调整的往返：累计发生额超
	// 64 位上限，但每步余额都在界内（原总金额已为 MaxInt64，不能再补收，
	// 故先用减免腾出空间）。
	const max = "9223372036854775807"
	h.mustRun("bill", "adjust", "c1", "2026-01", "adj-down", "-9223372036854775807", "腾挪") // 1 应付 0
	h.mustRun("bill", "adjust", "c1", "2026-01", "adj-up", max, "补收")                      // 2 应付 Max
	h.mustRun("bill", "pay", "c1", "2026-01", "pay-1", max, "巨额收款")                        // 3 实收 Max
	h.mustRun("bill", "unpay", "pay-1", "退回")                                              // 4 实收 0
	h.mustRun("bill", "revoke", "adj-up", "撤回补收")                                          // 5 应付 0

	l1 := h.mustRun("bill", "ledger", "c1", "2026-01")
	for _, want := range []string{
		"序号 1 调整 adj-down：应付 -9223372036854775807 分",
		"序号 2 调整 adj-up：应付 +9223372036854775807 分",
		"序号 3 收款 pay-1：实收 +9223372036854775807 分",
		"序号 4 撤销收款 pay-1：实收 -9223372036854775807 分",
		"序号 5 撤销调整 adj-up：应付 -9223372036854775807 分",
		"截止时余额：应付 0 分（0.00 元），实收 0 分（0.00 元），未收余额 0 分（0.00 元）",
	} {
		if !strings.Contains(l1, want) {
			t.Fatalf("巨额往返流水缺少 %q：\n%s", want, l1)
		}
	}

	// 报表起点/原总金额合计 2*(2^63-1)=18446744073709551614，超 64 位上限仍
	// 精确输出；巨额往返后 2026-01 应付回到 0，终点合计为单张 MaxInt64。
	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	for _, want := range []string{
		"原总金额合计：18446744073709551614 分（184467440737095516.14 元）",
		"起点合计：应付 18446744073709551614 分（184467440737095516.14 元），实收 0 分（0.00 元），未收余额 18446744073709551614 分（184467440737095516.14 元）",
		"终点合计：应付 9223372036854775807 分（92233720368547758.07 元），实收 0 分（0.00 元），未收余额 9223372036854775807 分（92233720368547758.07 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("128 位汇总缺少 %q：\n%s", want, out)
		}
	}

	// 区间中序号 2 之后范围内汇总应付一度达到 2*(2^63-1)，超 64 位上限仍精确
	// 展示（事件后汇总按 128 位累加）；逐月余额始终在 64 位内，与 ledger 一致。
	if !strings.Contains(out, "序号 2 调整 adj-up") ||
		!strings.Contains(out, "事后汇总：应付 18446744073709551614 分，实收 0 分，未收余额 18446744073709551614 分") {
		t.Fatalf("区间内 128 位事件后汇总异常：\n%s", out)
	}
}
