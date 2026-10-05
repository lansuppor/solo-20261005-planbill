package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// settleLedgerBill 是账后流水测试辅助：登记客户、导入一条 2026-09 用量
// （数量 qty，单价 price 分）并结算，返回账单原总金额（= qty*price）。
func settleLedgerBill(h *harness, customerID, price, qty string) int64 {
	h.t.Helper()
	h.mustRun("customer", "add", customerID, "客户"+customerID, price)
	f := h.writeFile("ul-"+customerID+".csv", csvHeader+
		"ul-"+customerID+","+customerID+",2026-09-15T10:00:00Z,"+qty+"\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", customerID, "2026-09")
	p, _ := parsePrice(price)
	q, _ := parsePrice(qty) // 仅用于整数解析
	total, err := mul64(p, q)
	if err != nil {
		h.t.Fatal(err)
	}
	return total
}

func TestLedgerEmptyBill(t *testing.T) {
	h := newHarness(t)
	settleLedgerBill(h, "c1", "150", "6") // 原总金额 900

	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	for _, want := range []string{
		"账后对账流水",
		"原总金额：900 分",
		"存档全局序号上限：0",
		"查询截止序号：0（最新）",
		"流水：空",
		"应付：900 分", "实收：0 分", "未收余额：900 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("空流水输出缺少 %q:\n%s", want, out)
		}
	}

	// 显式截止 0：同样只有初始余额。
	out0 := h.mustRun("bill", "ledger", "c1", "2026-09", "0")
	if !strings.Contains(out0, "流水：空") || !strings.Contains(out0, "0：只返回初始余额") ||
		!strings.Contains(out0, "应付：900 分") {
		t.Fatal(out0)
	}

	// 最新余额须与 bill show 一致。
	show := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(show, "当前应付：900 分") || !strings.Contains(show, "实收：0 分") ||
		!strings.Contains(show, "未收余额：900 分") {
		t.Fatal(show)
	}
}

func TestLedgerFullFlowMatchesShow(t *testing.T) {
	h := newHarness(t)
	settleLedgerBill(h, "c1", "150", "6") // 900

	// 序号顺序：1 调整+500；2 收款500；3 调整-100；4 撤销收款；5 撤销调整。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "漏算用量")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "500", "银行转账")
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-2", "-100", "客户补偿")
	h.mustRun("bill", "unpay", "pay-1", "账号登记错误")
	h.mustRun("bill", "revoke", "adj-1", "录入错误")

	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	lines := strings.Split(out, "\n")
	var evLines []string
	for _, ln := range lines {
		if strings.Contains(ln, "序号=") {
			evLines = append(evLines, ln)
		}
	}
	if len(evLines) != 5 {
		t.Fatalf("应有 5 个事件，实际 %d 条:\n%s", len(evLines), out)
	}
	wantSeq := []string{
		"序号=1 类型=调整 原记录=adj-1 金额影响：应付 +500 分",
		"序号=2 类型=收款 原记录=pay-1 汇款总额=500 分",
		"序号=3 类型=调整 原记录=adj-2 金额影响：应付 -100 分",
		"序号=4 类型=收款撤销 原记录=pay-1",
		"序号=5 类型=调整撤销 原记录=adj-1",
	}
	for i, want := range wantSeq {
		if !strings.Contains(evLines[i], want) {
			t.Fatalf("事件 %d 缺少 %q，实际：%s", i+1, want, evLines[i])
		}
	}
	// 逐事件后的余额。
	wantPost := []string{
		"应付=1400 分 实收=0 分 未收=1400 分",
		"应付=1400 分 实收=500 分 未收=900 分",
		"应付=1300 分 实收=500 分 未收=800 分",
		"应付=1300 分 实收=0 分 未收=1300 分",
		"应付=800 分 实收=0 分 未收=800 分",
	}
	for i, want := range wantPost {
		if !strings.Contains(evLines[i], want) {
			t.Fatalf("事件 %d 后期望 %q，实际：%s", i+1, want, evLines[i])
		}
	}
	// 撤销关联。
	if !strings.Contains(evLines[0], "已于序号 5 撤销") ||
		!strings.Contains(evLines[1], "已于序号 4 撤销") {
		t.Fatalf("原操作缺少撤销关联:\n%s", out)
	}
	if !strings.Contains(evLines[3], "抵消序号 2 的收款") ||
		!strings.Contains(evLines[4], "抵消序号 1 的调整") {
		t.Fatalf("撤销事件缺少关联:\n%s", out)
	}
	// 原因/备注与汇款信息。
	if !strings.Contains(out, "原因：漏算用量") || !strings.Contains(out, "备注：银行转账") ||
		!strings.Contains(out, "撤销原因：账号登记错误") ||
		!strings.Contains(out, "本账单分配=500 分") {
		t.Fatal(out)
	}
	// 截止时三项余额与 bill show 一致（adj-1 已撤销、pay-1 已撤销，
	// 仅剩 adj-2 的 -100：应付 800，实收 0，未收 800）。
	for _, want := range []string{"应付：800 分", "实收：0 分", "未收余额：800 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ledger 缺少 %q:\n%s", want, out)
		}
	}
	show := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(show, "当前应付：800 分") || !strings.Contains(show, "实收：0 分") ||
		!strings.Contains(show, "未收余额：800 分") {
		t.Fatal(show)
	}
}

func TestLedgerCutoffInclusiveAndFutureRevokeHidden(t *testing.T) {
	h := newHarness(t)
	settleLedgerBill(h, "c1", "150", "6") // 900
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj-1", "500", "补收")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay-1", "500", "转账")
	h.mustRun("bill", "unpay", "pay-1", "撤销原因")
	// 序号 1 调整、2 收款、3 撤销收款。

	// 截止 2：包含序号 2 的收款；序号 3 的撤销不混入流水、不影响历史余额。
	out := h.mustRun("bill", "ledger", "c1", "2026-09", "2")
	if strings.Contains(out, "序号=3") || strings.Contains(out, "类型=收款撤销") {
		t.Fatalf("截止之后的撤销混入了流水:\n%s", out)
	}
	if !strings.Contains(out, "查询截止序号：2") {
		t.Fatal(out)
	}
	// 历史视图不泄露截止之后的撤销：原收款不显示撤销关联。
	if strings.Contains(out, "撤销关联") {
		t.Fatalf("截止之后的撤销信息泄露进历史视图:\n%s", out)
	}
	if !strings.Contains(out, "应付：1400 分") || !strings.Contains(out, "实收：500 分") ||
		!strings.Contains(out, "未收余额：900 分") {
		t.Fatalf("截止 2 的历史余额不正确:\n%s", out)
	}
	// 事件 2 后余额：收款仍计入。
	if !strings.Contains(out, "序号=2") || !strings.Contains(out, "应付=1400 分 实收=500 分") {
		t.Fatal(out)
	}

	// 截止 1：只剩调整；收款事件也不出现。
	out1 := h.mustRun("bill", "ledger", "c1", "2026-09", "1")
	if strings.Contains(out1, "序号=2") || strings.Contains(out1, "类型=收款") {
		t.Fatalf("截止 1 不应包含收款事件:\n%s", out1)
	}
	if !strings.Contains(out1, "应付：1400 分") || !strings.Contains(out1, "实收：0 分") {
		t.Fatal(out1)
	}
}

func TestLedgerSeqGapsFromOtherCustomersAndMonths(t *testing.T) {
	h := newHarness(t)
	settleTwoMonths(h, "c1", "100", "5", "3") // c1：9月500、10月300
	// 另一个客户也结算，占用序号以外的操作不影响；先在 c1 9月登记一笔。
	h.mustRun("customer", "add", "c2", "客户c2", "100")
	f := h.writeFile("u-c2.csv", csvHeader+"u-c2,c2,2026-09-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c2", "2026-09")

	// c1 9月收款（序号 1），c2 9月收款（序号 2，与 c1 流水无关），
	// c1 的多月汇款（序号 3：9月100 + 10月300）。
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "400", "首笔")
	h.mustRun("bill", "pay", "c2", "2026-09", "p2", "100", "其他客户")
	h.mustRun("bill", "remit", "c1", "r1", "400", "汇款", "2026-09:100", "2026-10:300")

	// c1 9月流水只有序号 1 与 3：空档（序号 2 属于其他客户）合法。
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if strings.Contains(out, "p2") {
		t.Fatalf("其他客户的收款混入流水:\n%s", out)
	}
	if !strings.Contains(out, "序号=1 类型=收款 原记录=p1") ||
		!strings.Contains(out, "序号=3 类型=收款 原记录=r1 汇款总额=400 分") {
		t.Fatal(out)
	}
	// 多月汇款只按本账单分配改变实收：9月分配100。
	if !strings.Contains(out, "本账单分配=100 分") {
		t.Fatal(out)
	}
	if !strings.Contains(out, "应付：500 分") || !strings.Contains(out, "实收：500 分") ||
		!strings.Contains(out, "未收余额：0 分") {
		t.Fatalf("9 月余额不正确:\n%s", out)
	}

	// c1 10月：只有多月汇款的 300 分配。
	outOct := h.mustRun("bill", "ledger", "c1", "2026-10")
	if strings.Contains(outOct, "p1") || strings.Contains(outOct, "p2") {
		t.Fatalf("10 月流水混入无关收款:\n%s", outOct)
	}
	if !strings.Contains(outOct, "序号=3 类型=收款 原记录=r1 汇款总额=400 分（4.00 元） 本账单分配=300 分") {
		t.Fatal(outOct)
	}
	if !strings.Contains(outOct, "应付：300 分") || !strings.Contains(outOct, "实收：300 分") ||
		!strings.Contains(outOct, "未收余额：0 分") {
		t.Fatal(outOct)
	}

	// 整笔撤销多月汇款：同一序号取消两张账单上的分配。
	h.mustRun("bill", "unpay", "r1", "整笔撤销")
	outRev := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(outRev, "序号=4 类型=收款撤销 原记录=r1") ||
		!strings.Contains(outRev, "本账单分配=100 分") ||
		!strings.Contains(outRev, "金额影响：实收 -100 分") {
		t.Fatalf("9 月撤销事件不正确:\n%s", outRev)
	}
	if !strings.Contains(outRev, "实收：400 分") { // 仅剩 p1 的 400
		t.Fatal(outRev)
	}
	outOctRev := h.mustRun("bill", "ledger", "c1", "2026-10")
	if !strings.Contains(outOctRev, "金额影响：实收 -300 分") ||
		!strings.Contains(outOctRev, "实收：0 分") {
		t.Fatal(outOctRev)
	}

	// 截止序号落在其他客户造成的空档（2）上同样合法。
	gap := h.mustRun("bill", "ledger", "c1", "2026-09", "2")
	if !strings.Contains(gap, "序号=1") || strings.Contains(gap, "序号=3") {
		t.Fatal(gap)
	}
}

func TestLedgerSameNameAdjustmentAndPaymentLinkedSeparately(t *testing.T) {
	h := newHarness(t)
	settleLedgerBill(h, "c1", "100", "10") // 1000
	// 调整与收款同名 x1：按类型分别关联、各自撤销。
	h.mustRun("bill", "adjust", "c1", "2026-09", "x1", "200", "同名调整")
	h.mustRun("bill", "pay", "c1", "2026-09", "x1", "300", "同名收款")
	h.mustRun("bill", "revoke", "x1", "撤销调整")
	h.mustRun("bill", "unpay", "x1", "撤销收款")

	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "序号=1 类型=调整 原记录=x1") ||
		!strings.Contains(out, "序号=2 类型=收款 原记录=x1") ||
		!strings.Contains(out, "序号=3 类型=调整撤销 原记录=x1") ||
		!strings.Contains(out, "序号=4 类型=收款撤销 原记录=x1") {
		t.Fatal(out)
	}
	// 调整撤销抵消应付 +200（影响 -200）；收款撤销抵消实收 300。
	if !strings.Contains(out, "序号=3 类型=调整撤销 原记录=x1 金额影响：应付 -200 分") ||
		!strings.Contains(out, "序号=4 类型=收款撤销 原记录=x1") {
		t.Fatal(out)
	}
	if !strings.Contains(out, "应付：1000 分") || !strings.Contains(out, "实收：0 分") {
		t.Fatal(out)
	}
}

func TestLedgerRevokedRecordCountsBeforeRevoke(t *testing.T) {
	h := newHarness(t)
	settleLedgerBill(h, "c1", "100", "10") // 1000
	// 补收 500 后又撤销；收款 800 在撤销之前成立（应付 1500 ≥ 800）。
	h.mustRun("bill", "adjust", "c1", "2026-09", "a1", "500", "先补收")
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "800", "先收款")
	// 直接撤销补收会因应付(1500-500=1000) ≥ 实收(800) 而合法。
	h.mustRun("bill", "revoke", "a1", "再撤销")

	// 截止 2：补收虽最终已撤销，但在序号 1 仍计入应付；不能按当前撤销状态删除。
	out := h.mustRun("bill", "ledger", "c1", "2026-09", "2")
	if !strings.Contains(out, "序号=1") || !strings.Contains(out, "应付=1500 分 实收=0 分") {
		t.Fatal(out)
	}
	if !strings.Contains(out, "应付：1500 分") || !strings.Contains(out, "实收：800 分") ||
		!strings.Contains(out, "未收余额：700 分") {
		t.Fatalf("撤销前的历史余额必须保留补收影响:\n%s", out)
	}
	// 最新：补收已抵消，应付 1000、实收 800。
	latest := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(latest, "应付：1000 分") || !strings.Contains(latest, "实收：800 分") ||
		!strings.Contains(latest, "未收余额：200 分") {
		t.Fatal(latest)
	}
}

func TestLedgerGrossVolumeCanExceedMaxInt64(t *testing.T) {
	h := newHarness(t)
	// 零单价账单：原总金额 0，便于反复把应付推到 MaxInt64 再收回。
	settleLedgerBill(h, "c1", "0", "1")
	const max64 = "9223372036854775807"
	// 累计补收发生额 = 2*MaxInt64+MaxInt64，但每一步余额都合法。
	h.mustRun("bill", "adjust", "c1", "2026-09", "a1", max64, "补收一")
	h.mustRun("bill", "adjust", "c1", "2026-09", "a2", "-"+max64, "减免一")
	h.mustRun("bill", "adjust", "c1", "2026-09", "a3", max64, "补收二")
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "应付："+max64+" 分") {
		t.Fatal(out)
	}

	// 累计收款发生额同理：收满、整笔撤销、再收满，每步 0 ≤ 实收 ≤ 应付。
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", max64, "首笔")
	h.mustRun("bill", "unpay", "p1", "撤销首笔")
	h.mustRun("bill", "pay", "c1", "2026-09", "p2", max64, "次笔")
	out = h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "应付："+max64+" 分") ||
		!strings.Contains(out, "实收："+max64+" 分") ||
		!strings.Contains(out, "未收余额：0 分") {
		t.Fatal(out)
	}
	// 每一步都合法：序号 4 收款后实收=MaxInt64，序号 5 撤销后实收=0。
	if !strings.Contains(out, "序号=4") || !strings.Contains(out, "实收="+max64+" 分") ||
		!strings.Contains(out, "序号=5") {
		t.Fatal(out)
	}
}

func TestLedgerCutoffArgumentValidation(t *testing.T) {
	h := newHarness(t)
	settleLedgerBill(h, "c1", "100", "1") // 100，next_seq 仍为 0

	// 参数个数错误：用法错误（退出码 2）。
	for _, args := range [][]string{
		{"bill", "ledger"},
		{"bill", "ledger", "c1"},
		{"bill", "ledger", "c1", "2026-09", "1", "extra"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误，得到 %v", args, err)
		}
	}

	// 负数、非整数拒绝（业务错误，退出码 1）。
	for _, bad := range []string{"-1", "1.5", "abc", "1e3", "  ", "9223372036854775808"} {
		msg := h.runExpectErr("bill", "ledger", "c1", "2026-09", bad)
		if !strings.Contains(msg, "截止操作序号") {
			t.Fatalf("截止参数 %q 报错信息异常: %s", bad, msg)
		}
	}
	// 超过存档全局序号上限拒绝。
	msg := h.runExpectErr("bill", "ledger", "c1", "2026-09", "1")
	if !strings.Contains(msg, "超过存档全局序号上限 0") {
		t.Fatal(msg)
	}

	// 客户/账单不存在、月份非法。
	h.runExpectErr("bill", "ledger", "nope", "2026-09")
	h.runExpectErr("bill", "ledger", "c1", "2026-11")
	h.runExpectErr("bill", "ledger", "c1", "2026-9")
	h.runExpectErr("bill", "ledger", "c1", "2026-13")
}

func TestLedgerFailureAfterCutoffStillRejects(t *testing.T) {
	h := newHarness(t)
	// 手工构造一条“最终余额合法、但历史中间步不合法”的链：
	//   seq1 收款 100（应付100/实收100）
	//   seq2 调整 -100（应付0 < 实收100，该步非法）
	//   seq3 撤销收款（实收0）——最终 0 ≤ 0 ≤ MaxInt64，validate 放行，
	// 但逐事件回放必须在 seq2 失败；即使只查到截止 0/1 也拒绝，不出报告。
	legacy := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 1}},
  "bills": {"c1|2026-09": {
    "id": "BILL-bad-chain", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 1, "unit_price_fen": 100, "total_fee_fen": 100,
    "lines": [{"usage_id": "u1", "time": "2026-09-15T10:00:00Z", "quantity": 1, "line_fee_fen": 100}],
    "created_at": "2026-10-01T00:00:00Z"
  }},
  "adjustments": {"adj-1": {
    "id": "adj-1", "customer_id": "c1", "month": "2026-09",
    "amount_fen": -100, "reason": "中途减免", "seq": 2, "created_at": "2026-10-03T00:00:00Z"
  }},
  "payments": {"pay-1": {
    "id": "pay-1", "customer_id": "c1", "total_fen": 100, "note": "先收款",
    "allocations": [{"month": "2026-09", "amount_fen": 100}],
    "seq": 1, "created_at": "2026-10-02T00:00:00Z",
    "revoked": true, "revoke_reason": "后撤销", "revoke_seq": 3,
    "revoked_at": "2026-10-04T00:00:00Z"
  }},
  "next_seq": 3
}`
	if err := os.WriteFile(h.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cutoff := range []string{"0", "1", "3"} {
		msg := h.runExpectErr("bill", "ledger", "c1", "2026-09", cutoff)
		if !strings.Contains(msg, "完整流水核验失败") || !strings.Contains(msg, "序号 2") {
			t.Fatalf("cutoff=%s 应因完整流水核验失败而拒绝: %s", cutoff, msg)
		}
	}
	// bill show 看的是最终余额（合法），仍可展示——逐事件核验是 ledger 的要求。
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "当前应付：0 分") || !strings.Contains(out, "实收：0 分") {
		t.Fatal(out)
	}
}

func TestLedgerReadOnlyNeverRewritesState(t *testing.T) {
	h := newHarness(t)
	settleLedgerBill(h, "c1", "100", "2") // 200
	h.mustRun("bill", "adjust", "c1", "2026-09", "a1", "50", "补收")
	h.mustRun("bill", "pay", "c1", "2026-09", "p1", "100", "收款")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 成功查询（多种截止）不改写存档。
	for _, args := range [][]string{
		{"bill", "ledger", "c1", "2026-09"},
		{"bill", "ledger", "c1", "2026-09", "0"},
		{"bill", "ledger", "c1", "2026-09", "1"},
	} {
		h.mustRun(args...)
		after, err := os.ReadFile(h.statePath())
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Fatalf("查询 %v 改写了 state.json", args)
		}
	}
	// 失败查询同样不改写、不占用序号。
	h.runExpectErr("bill", "ledger", "c1", "2026-09", "99")
	h.runExpectErr("bill", "ledger", "nope", "2026-09")
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("失败查询改写了 state.json")
	}
	// 失败操作不产生流水事件：下一次成功登记的序号紧接已有序号（当前为 2）。
	h.mustRun("bill", "adjust", "c1", "2026-09", "a2", "10", "再补收")
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "存档全局序号上限：3") || !strings.Contains(out, "序号=3 类型=调整 原记录=a2") {
		t.Fatal(out)
	}
}

func TestLedgerLegacyStateFiles(t *testing.T) {
	h := newHarness(t)
	// 旧版数据：无 adjustments/payments 字段——按空历史处理。
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
}`
	if err := os.WriteFile(h.statePath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(out, "存档全局序号上限：0") || !strings.Contains(out, "流水：空") ||
		!strings.Contains(out, "应付：200 分") || !strings.Contains(out, "实收：0 分") {
		t.Fatal(out)
	}

	// 旧版单月收款（month + amount_fen）按一项分配读取并进流水。
	legacy := `{
  "version": 1,
  "customers": {"c2": {"id": "c2", "name": "乙方", "price_fen": 100}},
  "usage": {"u2": {"id": "u2", "customer_id": "c2", "time": "2026-09-15T10:00:00Z", "quantity": 3}},
  "bills": {"c2|2026-09": {
    "id": "BILL-legacy-pay", "customer_id": "c2", "month": "2026-09",
    "total_quantity": 3, "unit_price_fen": 100, "total_fee_fen": 300,
    "lines": [{"usage_id": "u2", "time": "2026-09-15T10:00:00Z", "quantity": 3, "line_fee_fen": 300}],
    "created_at": "2026-10-01T00:00:00Z"
  }},
  "adjustments": {},
  "payments": {"pay-old": {
    "id": "pay-old", "customer_id": "c2", "month": "2026-09", "amount_fen": 150,
    "note": "旧格式收款", "seq": 1, "created_at": "2026-10-02T00:00:00Z"
  }},
  "next_seq": 1
}`
	h2 := newHarness(t)
	if err := os.WriteFile(h2.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	out = h2.mustRun("bill", "ledger", "c2", "2026-09")
	if !strings.Contains(out, "序号=1 类型=收款 原记录=pay-old") ||
		!strings.Contains(out, "汇款总额=150 分") || !strings.Contains(out, "本账单分配=150 分") ||
		!strings.Contains(out, "实收：150 分") || !strings.Contains(out, "未收余额：150 分") {
		t.Fatal(out)
	}
	// 重启（重新载入）后顺序与历史余额一致。
	out = h2.mustRun("bill", "ledger", "c2", "2026-09", "1")
	if !strings.Contains(out, "实收：150 分") {
		t.Fatal(out)
	}
}

func TestLedgerCorruptAndTrailingGarbageRejected(t *testing.T) {
	h := newHarness(t)
	settleLedgerBill(h, "c1", "100", "1")

	// JSON 之后存在非空白内容：拒绝且保留原文件。
	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	garbage := string(good) + " NOT-JSON"
	if err := os.WriteFile(h.statePath(), []byte(garbage), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.runExpectErr("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(msg, "损坏") {
		t.Fatal(msg)
	}
	got, _ := os.ReadFile(h.statePath())
	if string(got) != garbage {
		t.Fatal("损坏文件被改写")
	}

	// 文件不可读（目录替代文件）同样非零退出。
	if err := os.RemoveAll(h.statePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.statePath(), 0o755); err != nil {
		t.Fatal(err)
	}
	msg = h.runExpectErr("bill", "ledger", "c1", "2026-09")
	if !strings.Contains(msg, "不可读取") && !strings.Contains(msg, "损坏") {
		t.Fatal(msg)
	}
}

func TestLedgerHelpListsCommand(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("--help")
	if !strings.Contains(out, "bill ledger <客户标识> <YYYY-MM>") {
		t.Fatal(out)
	}
}
