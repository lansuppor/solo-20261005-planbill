package main

import (
	"os"
	"strings"
	"testing"
)

// 共享账后计算（postops.go）的回归测试：覆盖连续更正跨范围迁移、零差额与
// 移出后撤销在两类查询中的展示差异、更正后部分退款及再收款、历史截止一致、
// 完整流水异常两类查询均拒绝、整数边界。两类查询的逐月余额、事件关联与
// 汇总必须一致——它们来自同一份共享事件流。

// setupPostopsMonths 登记单价 100 分的客户，按 map 导入用量并结算给定月份
// （每月一条用量，数量由 qtyByMonth 给出，原总金额 = 数量×100 分）。
func setupPostopsMonths(t *testing.T, h *harness, qtyByMonth map[string]string) {
	t.Helper()
	h.mustRun("customer", "add", "c1", "甲方", "100")
	var body strings.Builder
	body.WriteString(csvHeader)
	for _, m := range []string{"2026-01", "2026-02", "2026-03"} {
		qty, ok := qtyByMonth[m]
		if !ok {
			continue
		}
		body.WriteString("u" + m[5:] + ",c1," + m + "-15T10:00:00Z," + qty + "\n")
	}
	h.mustRun("usage", "import", h.writeFile("u.csv", body.String()))
	for _, m := range []string{"2026-01", "2026-02", "2026-03"} {
		if _, ok := qtyByMonth[m]; ok {
			h.mustRun("bill", "settle", "c1", m)
		}
	}
}

// 连续更正把同一笔汇款在范围内/外月份间多次迁移：两类查询的事件关联、逐月
// 余额与报表汇总必须来自同一推导。
func TestSharedOpsConsecutiveCorrectionsAcrossRange(t *testing.T) {
	h := newHarness(t)
	setupPostopsMonths(t, h, map[string]string{"2026-01": "10", "2026-02": "20", "2026-03": "10"})
	// 1 汇款 1000：01:600、03:400（03 在报表范围外）。
	h.mustRun("bill", "remit", "c1", "p1", "1000", "季度汇款", "2026-01:600", "2026-03:400")
	// 2 更正 c1：01:300、02:300、03:400（300 由 01 迁入范围内的 02）。
	h.mustRun("bill", "correct", "p1", "c1", "第一次迁移", "2026-01:300", "2026-02:300", "2026-03:400")
	// 3 更正 c2：02:700、03:300（01 再移出 300、03 移出 100，全部并入 02）。
	h.mustRun("bill", "correct", "p1", "c2", "第二次迁移", "2026-02:700", "2026-03:300")

	out := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	for _, want := range []string{
		"序号 1 收款 p1：备注：季度汇款；汇款总额 1000 分（10.00 元）；范围内变化：2026-01 实收 +600 分（分配 600 分）",
		"事后汇总：应付 3000 分，实收 600 分，未收余额 2400 分",
		"序号 2 更正 c1（关联收款 p1）：原因：第一次迁移；范围内变化：2026-01 实收 -300 分（分配 600 分 → 300 分），2026-02 实收 +300 分（分配 0 分 → 300 分）",
		"事后汇总：应付 3000 分，实收 600 分，未收余额 2400 分",
		"序号 3 更正 c2（关联收款 p1）：原因：第二次迁移；范围内变化：2026-01 实收 -300 分（分配 300 分 → 0 分），2026-02 实收 +400 分（分配 300 分 → 700 分）",
		"事后汇总：应付 3000 分，实收 700 分，未收余额 2300 分",
		"终点合计：应付 3000 分（30.00 元），实收 700 分（7.00 元），未收余额 2300 分（23.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("报表缺少 %q：\n%s", want, out)
		}
	}
	// 每次更正只出现一次。
	if strings.Count(out, "更正 c1（关联收款 p1）") != 1 || strings.Count(out, "更正 c2（关联收款 p1）") != 1 {
		t.Fatalf("更正未各按一次操作展示：\n%s", out)
	}

	// 逐月 ledger：01 三次实收变化 600→300→0；02 只经历两次更正 0→300→700。
	led01 := h.mustRun("bill", "ledger", "c1", "2026-01")
	for _, want := range []string{
		"序号 1 收款 p1：实收 +600 分",
		"序号 2 更正 c1：实收 -300 分", "本账单分配 600 分 → 300 分",
		"序号 3 更正 c2：实收 -300 分", "本账单分配 300 分 → 0 分",
		"截止时余额：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）",
	} {
		if !strings.Contains(led01, want) {
			t.Fatalf("01 流水缺少 %q：\n%s", want, led01)
		}
	}
	led02 := h.mustRun("bill", "ledger", "c1", "2026-02")
	for _, want := range []string{
		"序号 2 更正 c1：实收 +300 分", "本账单分配 0 分 → 300 分",
		"序号 3 更正 c2：实收 +400 分", "本账单分配 300 分 → 700 分",
		"截止时余额：应付 2000 分（20.00 元），实收 700 分（7.00 元），未收余额 1300 分（13.00 元）",
	} {
		if !strings.Contains(led02, want) {
			t.Fatalf("02 流水缺少 %q：\n%s", want, led02)
		}
	}
	// 最新余额与 bill show 一致。
	show02 := h.mustRun("bill", "show", "c1", "2026-02")
	if !strings.Contains(show02, "实收：700 分") || !strings.Contains(show02, "未收余额：1300 分") {
		t.Fatalf("bill show 与报表终点不一致：\n%s", show02)
	}

	// 历史截止：区间 (2,3] 只含第二次更正，两端逐月余额与相同截止的 ledger 一致。
	sub := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02", "2", "3")
	for _, want := range []string{
		"起点合计：应付 3000 分（30.00 元），实收 600 分（6.00 元），未收余额 2400 分（24.00 元）",
		"终点合计：应付 3000 分（30.00 元），实收 700 分（7.00 元），未收余额 2300 分（23.00 元）",
		"序号 3 更正 c2",
	} {
		if !strings.Contains(sub, want) {
			t.Fatalf("(2,3] 报表缺少 %q：\n%s", want, sub)
		}
	}
	if strings.Contains(sub, "序号 1 收款") || strings.Contains(sub, "序号 2 更正") {
		t.Fatalf("区间外操作混入：\n%s", sub)
	}
	cut2_01 := h.mustRun("bill", "ledger", "c1", "2026-01", "2")
	if !strings.Contains(cut2_01, "截止时余额：应付 1000 分（10.00 元），实收 300 分（3.00 元），未收余额 700 分（7.00 元）") {
		t.Fatalf("01 截止 2 余额异常：\n%s", cut2_01)
	}
	if !strings.Contains(sub, "起点：应付 1000 分（10.00 元），实收 300 分（3.00 元），未收余额 700 分（7.00 元）") {
		t.Fatalf("报表起点与 ledger 截止 2 不一致：\n%s", sub)
	}
}

// 零差额更正（范围内转移、汇总变化为 0）在报表中保留；收款被更正移出范围后
// 再撤销：单账单流水保留零金额撤销事件，跨账期报表只按撤销当时分配判断，
// 不因曾经涉及而增加操作。
func TestSharedOpsZeroDeltaAndMovedOutRevokeDisplay(t *testing.T) {
	h := newHarness(t)
	setupPostopsMonths(t, h, map[string]string{"2026-01": "10", "2026-02": "10"})
	// 1 收款 p1：01:1000；2 全部更正到 02；3 整笔撤销（取消的是 02 最新分配）。
	h.mustRun("bill", "pay", "c1", "2026-01", "p1", "1000", "一月收款")
	h.mustRun("bill", "correct", "p1", "c1", "改到二月", "2026-02:1000")
	h.mustRun("bill", "unpay", "p1", "撤销")

	// 单账单流水（01）：收款 +1000、更正 1000→0、以及移出后的零金额撤销事件。
	led01 := h.mustRun("bill", "ledger", "c1", "2026-01")
	for _, want := range []string{
		"序号 1 收款 p1：实收 +1000 分",
		"序号 2 更正 c1：实收 -1000 分", "本账单分配 1000 分 → 0 分",
		"序号 3 撤销收款 p1：实收 +0 分", "取消本账单分配 0 分",
		"截止时余额：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）",
	} {
		if !strings.Contains(led01, want) {
			t.Fatalf("01 流水缺少 %q：\n%s", want, led01)
		}
	}
	// 02 流水：更正 0→1000、撤销 -1000。
	led02 := h.mustRun("bill", "ledger", "c1", "2026-02")
	for _, want := range []string{
		"序号 2 更正 c1：实收 +1000 分", "本账单分配 0 分 → 1000 分",
		"序号 3 撤销收款 p1：实收 -1000 分", "取消本账单分配 1000 分",
	} {
		if !strings.Contains(led02, want) {
			t.Fatalf("02 流水缺少 %q：\n%s", want, led02)
		}
	}

	// 报表范围只有 01：撤销当时最新分配全在 02（范围外），撤销不纳入报表；
	// 但 01 的收款与更正（按前后分配涉及范围）仍各出现一次。
	rep01 := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-01")
	if strings.Contains(rep01, "撤销收款 p1") {
		t.Fatalf("撤销当时分配在范围外，报表不应因曾经涉及而增加撤销操作：\n%s", rep01)
	}
	for _, want := range []string{
		"序号 1 收款 p1",
		"序号 2 更正 c1（关联收款 p1）：原因：改到二月；范围内变化：2026-01 实收 -1000 分（分配 1000 分 → 0 分）",
		"终点合计：应付 1000 分（10.00 元），实收 0 分（0.00 元），未收余额 1000 分（10.00 元）",
	} {
		if !strings.Contains(rep01, want) {
			t.Fatalf("单月报表缺少 %q：\n%s", want, rep01)
		}
	}

	// 范围含两个月：零差额的范围内转移（汇总变化为 0）保留在 (1,2] 区间内。
	zero := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02", "1", "2")
	for _, want := range []string{
		"序号 2 更正 c1（关联收款 p1）",
		"2026-01 实收 -1000 分（分配 1000 分 → 0 分），2026-02 实收 +1000 分（分配 0 分 → 1000 分）",
		"事后汇总：应付 2000 分，实收 1000 分，未收余额 1000 分",
	} {
		if !strings.Contains(zero, want) {
			t.Fatalf("零差额更正应保留：\n%s\n缺少 %q", zero, want)
		}
	}
	// 同样的单月范围：移出操作按实际差额 -1000 计入，事件仍保留。
	moveOut := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-01", "1", "2")
	if !strings.Contains(moveOut, "序号 2 更正 c1") || !strings.Contains(moveOut, "2026-01 实收 -1000 分") {
		t.Fatalf("跨范围转出应按实际差额计入：\n%s", moveOut)
	}
}

// 更正后部分退款（跨月退款作为一次操作）释放未收余额，再登记新收款：
// 两类查询逐月余额一致，最新与 bill show 一致。
func TestSharedOpsRefundAfterCorrectionThenRepay(t *testing.T) {
	h := newHarness(t)
	setupPostopsMonths(t, h, map[string]string{"2026-01": "10", "2026-02": "10"})
	// 1 汇款 800：01:500、02:300；2 更正为 01:300、02:500；
	// 3 跨月退款 01:200、02:100；4 在退款释放的 01 未收余额上再收 200。
	h.mustRun("bill", "remit", "c1", "p1", "800", "汇款", "2026-01:500", "2026-02:300")
	h.mustRun("bill", "correct", "p1", "c1", "更正", "2026-01:300", "2026-02:500")
	h.mustRun("bill", "refund", "p1", "r1", "多收退回", "2026-01:200", "2026-02:100")
	h.mustRun("bill", "pay", "c1", "2026-01", "p2", "200", "退款后补款")

	rep := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-02")
	for _, want := range []string{
		"序号 3 退款 r1（关联收款 p1）：原因：多收退回；范围内变化：2026-01 实收 -200 分（退款 200 分），2026-02 实收 -100 分（退款 100 分）",
		"序号 4 收款 p2：备注：退款后补款；汇款总额 200 分（2.00 元）；范围内变化：2026-01 实收 +200 分（分配 200 分）",
		"终点合计：应付 2000 分（20.00 元），实收 700 分（7.00 元），未收余额 1300 分（13.00 元）",
	} {
		if !strings.Contains(rep, want) {
			t.Fatalf("报表缺少 %q：\n%s", want, rep)
		}
	}
	// 跨月退款只作为一次操作展示。
	if strings.Count(rep, "退款 r1（关联收款 p1）") != 1 {
		t.Fatalf("跨月退款应只出现一次：\n%s", rep)
	}

	led01 := h.mustRun("bill", "ledger", "c1", "2026-01")
	for _, want := range []string{
		"序号 1 收款 p1：实收 +500 分",
		"序号 2 更正 c1：实收 -200 分", "本账单分配 500 分 → 300 分",
		"序号 3 退款 r1：实收 -200 分", "本账单退款 200 分",
		"序号 4 收款 p2：实收 +200 分",
		"截止时余额：应付 1000 分（10.00 元），实收 300 分（3.00 元），未收余额 700 分（7.00 元）",
	} {
		if !strings.Contains(led01, want) {
			t.Fatalf("01 流水缺少 %q：\n%s", want, led01)
		}
	}
	led02 := h.mustRun("bill", "ledger", "c1", "2026-02")
	for _, want := range []string{
		"序号 1 收款 p1：实收 +300 分",
		"序号 2 更正 c1：实收 +200 分", "本账单分配 300 分 → 500 分",
		"序号 3 退款 r1：实收 -100 分",
		"截止时余额：应付 1000 分（10.00 元），实收 400 分（4.00 元），未收余额 600 分（6.00 元）",
	} {
		if !strings.Contains(led02, want) {
			t.Fatalf("02 流水缺少 %q：\n%s", want, led02)
		}
	}
	// 历史截止：退款之前的余额不被提前影响。
	cut2 := h.mustRun("bill", "ledger", "c1", "2026-01", "2")
	if !strings.Contains(cut2, "截止时余额：应付 1000 分（10.00 元），实收 300 分（3.00 元），未收余额 700 分（7.00 元）") ||
		strings.Contains(cut2, "退款") {
		t.Fatalf("截止后的退款不应影响历史余额：\n%s", cut2)
	}
	// 最新与 bill show 一致；首次退款后业务约束不变（不得更正或整笔撤销）。
	show01 := h.mustRun("bill", "show", "c1", "2026-01")
	if !strings.Contains(show01, "实收：300 分") || !strings.Contains(show01, "未收余额：700 分") {
		t.Fatalf("bill show 01 与流水不一致：\n%s", show01)
	}
	if msg := h.runExpectErr("bill", "correct", "p1", "c9", "退款后更正", "2026-01:800"); !strings.Contains(msg, "已固定") {
		t.Fatalf("首次退款后应拒绝更正：%s", msg)
	}
	if msg := h.runExpectErr("bill", "unpay", "p1", "退款后撤销"); !strings.Contains(msg, "已固定") {
		t.Fatalf("首次退款后应拒绝整笔撤销：%s", msg)
	}
}

// 完整流水中间异常：即使查询截止在异常序号之前，ledger 与 reconcile 都整体
// 拒绝、不输出部分报告，且不改写存档。
func TestSharedOpsIntermediateAnomalyRejectsBothQueries(t *testing.T) {
	h := newHarness(t)
	h.writeFile("state.json", `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-01-15T10:00:00Z", "quantity": 10}},
  "bills": {"c1|2026-01": {
    "id": "BILL-x", "customer_id": "c1", "month": "2026-01",
    "total_quantity": 10, "unit_price_fen": 100, "total_fee_fen": 1000,
    "lines": [{"usage_id": "u1", "time": "2026-01-15T10:00:00Z", "quantity": 10, "line_fee_fen": 1000}],
    "created_at": "2026-02-01T00:00:00Z"
  }},
  "adjustments": {
    "a1": {"id": "a1", "customer_id": "c1", "month": "2026-01", "amount_fen": -1000,
      "reason": "减免", "seq": 2, "created_at": "2026-02-02T00:00:00Z"},
    "a2": {"id": "a2", "customer_id": "c1", "month": "2026-01", "amount_fen": 1000,
      "reason": "补回", "seq": 3, "created_at": "2026-02-03T00:00:00Z"}
  },
  "payments": {"p1": {"id": "p1", "customer_id": "c1", "total_fen": 500, "note": "收款",
    "allocations": [{"month": "2026-01", "amount_fen": 500}], "seq": 1, "created_at": "2026-02-01T12:00:00Z"}},
  "next_seq": 3
}`)
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 序号 2 之后实收 500 > 应付 0；最终余额合法。无截止与截止在异常之前都拒绝。
	for _, args := range [][]string{
		{"bill", "ledger", "c1", "2026-01"},
		{"bill", "ledger", "c1", "2026-01", "1"},
		{"bill", "ledger", "c1", "2026-01", "0"},
		{"bill", "reconcile", "c1", "2026-01", "2026-01"},
		{"bill", "reconcile", "c1", "2026-01", "2026-01", "0", "1"},
		{"bill", "reconcile", "c1", "2026-01", "2026-01", "0", "3"},
	} {
		msg := h.runExpectErr(args...)
		if !strings.Contains(msg, "数据异常") {
			t.Fatalf("args=%v 应按数据异常拒绝：%s", args, msg)
		}
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("查询失败后存档被改写")
	}
}

// 整数边界：累计补收/收款发生额远超 64 位上限但逐步余额合法时，报表与流水
// 仍准确输出；两张 MaxInt64 账单的 128 位汇总精确超出 64 位上限。
func TestSharedOpsIntegerBoundaries(t *testing.T) {
	h := newHarness(t)
	// 单价 0 的 01 账单（原总金额 0）：巨额调整/收款往返，发生额累计溢出，
	// 每步余额合法。
	h.mustRun("customer", "add", "c1", "零价客户", "0")
	h.mustRun("usage", "import", h.writeFile("u.csv", csvHeader+"u1,c1,2026-01-15T10:00:00Z,5\n"))
	h.mustRun("bill", "settle", "c1", "2026-01")
	const max = "9223372036854775807"
	h.mustRun("bill", "adjust", "c1", "2026-01", "a1", max, "巨额补收") // 1
	h.mustRun("bill", "pay", "c1", "2026-01", "p1", max, "巨额收款")    // 2
	h.mustRun("bill", "unpay", "p1", "退回")                          // 3
	h.mustRun("bill", "revoke", "a1", "撤回")                         // 4
	h.mustRun("bill", "adjust", "c1", "2026-01", "a2", max, "再补收")  // 5
	h.mustRun("bill", "pay", "c1", "2026-01", "p2", max, "再收款")     // 6

	rep := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-01")
	for _, want := range []string{
		"序号 1 调整 a1：原因：巨额补收；范围内变化：2026-01 应付 +9223372036854775807 分",
		"序号 2 收款 p1：备注：巨额收款；汇款总额 9223372036854775807 分",
		"序号 6 收款 p2",
		"终点合计：应付 9223372036854775807 分（92233720368547758.07 元），实收 9223372036854775807 分（92233720368547758.07 元），未收余额 0 分（0.00 元）",
	} {
		if !strings.Contains(rep, want) {
			t.Fatalf("巨额往返报表缺少 %q：\n%s", want, rep)
		}
	}
	// 截止 3（撤销与再补收之前）：应付仍为巨额、实收已退回 0。
	cut := h.mustRun("bill", "reconcile", "c1", "2026-01", "2026-01", "0", "3")
	if !strings.Contains(cut, "终点合计：应付 9223372036854775807 分（92233720368547758.07 元），实收 0 分（0.00 元），未收余额 9223372036854775807 分") {
		t.Fatalf("截止 3 的巨额余额异常：\n%s", cut)
	}

	// 两张各为 MaxInt64 的账单：多账单汇总按 128 位精确输出，超出 64 位上限。
	h2 := newHarness(t)
	h2.mustRun("customer", "add", "c2", "巨款客户", max)
	h2.mustRun("usage", "import", h2.writeFile("u.csv", csvHeader+
		"u1,c2,2026-01-15T10:00:00Z,1\n"+
		"u2,c2,2026-02-15T10:00:00Z,1\n"))
	h2.mustRun("bill", "settle", "c2", "2026-01")
	h2.mustRun("bill", "settle", "c2", "2026-02")
	big := h2.mustRun("bill", "reconcile", "c2", "2026-01", "2026-02")
	if !strings.Contains(big, "原总金额合计：18446744073709551614 分（184467440737095516.14 元）") ||
		!strings.Contains(big, "终点合计：应付 18446744073709551614 分") {
		t.Fatalf("128 位汇总不精确：\n%s", big)
	}
}
