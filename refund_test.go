package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// --- 部分退款登记测试 ---

// refundSetup 构造两个月账单并登记一笔跨月汇款：2026-09 应付 1000、
// 2026-10 应付 2000；pay1 总额 2500（2026-09:1000，2026-10:1500）。
func refundSetup(h *harness) {
	h.mustRun("customer", "add", "c1", "甲方", "100")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-09-15T10:00:00Z,10\n"+
		"u2,c1,2026-10-15T10:00:00Z,20\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.mustRun("bill", "remit", "c1", "pay1", "2500", "九月十月汇款", "2026-09:1000", "2026-10:1500")
}

func TestRefundHappyPath(t *testing.T) {
	h := newHarness(t)
	refundSetup(h)

	out := h.mustRun("bill", "refund", "pay1", "r1", "多收退回", "2026-09:400", "2026-10:500")
	for _, want := range []string{
		"已登记退款", "退款标识：r1", "关联收款：pay1", "原因：多收退回",
		"月份 2026-09：本次退款 400 分", "剩余可退 600 分",
		"月份 2026-10：本次退款 500 分", "剩余可退 1000 分",
		"剩余可退额：2026-09:600,2026-10:1000（合计 1600 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("退款输出缺少 %q:\n%s", want, out)
		}
	}

	// 退款只减少实收、增加未收，不改变应付。
	out = h.mustRun("bill", "show", "c1", "2026-09")
	for _, want := range []string{
		"当前应付：1000 分", "实收：600 分", "未收余额：400 分",
		"退款历史（按成功操作顺序，不可撤销）：",
		"退款 r1：关联收款 pay1，本账单退款 400 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("bill show 缺少 %q:\n%s", want, out)
		}
	}
	// 重复结算保留原信息并展示退款历史。
	out = h.mustRun("bill", "settle", "c1", "2026-09")
	if !strings.Contains(out, "已结算，返回原账单") || !strings.Contains(out, "退款 r1") {
		t.Fatalf("重复结算应保留原信息并展示退款历史:\n%s", out)
	}
}

func TestRefundIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	refundSetup(h)
	h.mustRun("bill", "refund", "pay1", "r1", "多收退回", "2026-09:400", "2026-10:500")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 清单顺序无关：相同内容重放返回原记录且不写盘。
	out := h.mustRun("bill", "refund", "pay1", "r1", "多收退回", "2026-10:500", "2026-09:400")
	if !strings.Contains(out, "已存在且内容相同，返回原记录") {
		t.Fatalf("相同重放未幂等返回:\n%s", out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("相同重放改写了存档")
	}

	// 退尽后相同重放仍返回原记录且不写盘。
	h.mustRun("bill", "refund", "pay1", "r2", "退尽", "2026-09:600", "2026-10:1000")
	before, _ = os.ReadFile(h.statePath())
	out = h.mustRun("bill", "refund", "pay1", "r1", "多收退回", "2026-09:400", "2026-10:500")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatalf("退尽后相同重放应成功:\n%s", out)
	}
	after, _ = os.ReadFile(h.statePath())
	if string(after) != string(before) {
		t.Fatal("退尽后相同重放改写了存档")
	}

	// 内容不同（金额、原因、目标收款任一不同）拒绝。
	if msg := h.runExpectErr("bill", "refund", "pay1", "r1", "多收退回", "2026-09:401", "2026-10:500"); !strings.Contains(msg, "内容不同") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "refund", "pay1", "r1", "换原因", "2026-09:400", "2026-10:500"); !strings.Contains(msg, "内容不同") {
		t.Fatal(msg)
	}
	h.mustRun("customer", "add", "c2", "乙方", "100")
	if msg := h.runExpectErr("bill", "refund", "ghost", "r1", "多收退回", "2026-09:400", "2026-10:500"); !strings.Contains(msg, "不存在") {
		t.Fatal(msg)
	}
}

func TestRefundValidation(t *testing.T) {
	h := newHarness(t)
	refundSetup(h)

	// 非法输入：空标识、空原因、月份非法/重复、金额非正、清单为空。
	h.runExpectErr("bill", "refund", "pay1", "", "原因", "2026-09:100")
	h.runExpectErr("bill", "refund", "pay1", "r1", "  ", "2026-09:100")
	h.runExpectErr("bill", "refund", "pay1", "r1", "原因", "2026-9:100")
	h.runExpectErr("bill", "refund", "pay1", "r1", "原因", "2026-09:100", "2026-09:50")
	h.runExpectErr("bill", "refund", "pay1", "r1", "原因", "2026-09:0")
	h.runExpectErr("bill", "refund", "pay1", "r1", "原因", "2026-09:-5")
	h.runExpectErr("bill", "refund", "pay1", "r1", "原因")
	h.runExpectErr("bill", "refund", "ghost", "r1", "原因", "2026-09:100")

	// 月份不在最新分配中；超过剩余可退额；整笔拒绝后标识与序号不被占用。
	if msg := h.runExpectErr("bill", "refund", "pay1", "r1", "原因", "2026-11:100"); !strings.Contains(msg, "不在收款") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "refund", "pay1", "r1", "原因", "2026-09:1001"); !strings.Contains(msg, "超过剩余可退额") {
		t.Fatal(msg)
	}
	// 同一笔内一项非法整笔拒绝：合法月也不生效。
	h.runExpectErr("bill", "refund", "pay1", "r1", "原因", "2026-09:100", "2026-10:1501")
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "实收：1000 分") || !strings.Contains(out, "退款历史：无") {
		t.Fatalf("整笔拒绝后状态不应改变:\n%s", out)
	}
	// 退款标识未被占用，可原样重试成功。
	h.mustRun("bill", "refund", "pay1", "r1", "原因", "2026-09:100")

	// 多次部分退款累计约束：不能超过分配减累计退款。
	if msg := h.runExpectErr("bill", "refund", "pay1", "r2", "原因", "2026-09:901"); !strings.Contains(msg, "超过剩余可退额 900 分") {
		t.Fatal(msg)
	}
	h.mustRun("bill", "refund", "pay1", "r2", "原因", "2026-09:900") // 退尽该月
	if msg := h.runExpectErr("bill", "refund", "pay1", "r3", "原因", "2026-09:1"); !strings.Contains(msg, "剩余可退额 0 分") {
		t.Fatal(msg)
	}

	// 已撤销收款不能退款。
	h.mustRun("bill", "pay", "c1", "2026-10", "pay2", "100", "补缴")
	h.mustRun("bill", "unpay", "pay2", "误登记")
	if msg := h.runExpectErr("bill", "refund", "pay2", "r3", "原因", "2026-10:1"); !strings.Contains(msg, "已撤销") {
		t.Fatal(msg)
	}
}

func TestRefundFreezesPayment(t *testing.T) {
	h := newHarness(t)
	refundSetup(h)

	// 退款前可更正；退款后固定最新分配，拒绝更正与整笔撤销。
	h.mustRun("bill", "correct", "pay1", "cor1", "修正月份", "2026-09:1000", "2026-10:1500")
	h.mustRun("bill", "refund", "pay1", "r1", "多收退回", "2026-09:100")
	if msg := h.runExpectErr("bill", "correct", "pay1", "cor2", "再改", "2026-09:2500"); !strings.Contains(msg, "已固定") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "unpay", "pay1", "撤销"); !strings.Contains(msg, "已固定") {
		t.Fatal(msg)
	}
	// 已有更正的相同重放仍成功，不恢复旧分配或已退金额。
	out := h.mustRun("bill", "correct", "pay1", "cor1", "修正月份", "2026-10:1500", "2026-09:1000")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatalf("已有更正的相同重放应成功:\n%s", out)
	}
	// 已有收款的相同重放仍成功，不恢复已退金额。
	out = h.mustRun("bill", "remit", "c1", "pay1", "2500", "九月十月汇款", "2026-10:1500", "2026-09:1000")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatalf("已有收款的相同重放应成功:\n%s", out)
	}
	out = h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "实收：900 分") {
		t.Fatalf("重放不应恢复已退金额:\n%s", out)
	}

	// 无退款收款仍按原规则处理：可更正、可整笔撤销。
	h.mustRun("bill", "pay", "c1", "2026-10", "pay2", "100", "补缴")
	h.mustRun("bill", "correct", "pay2", "cor3", "改月", "2026-09:100")
	h.mustRun("bill", "unpay", "pay2", "误登记")
}

func TestRefundPostBalanceConstraints(t *testing.T) {
	h := newHarness(t)
	refundSetup(h)
	h.mustRun("bill", "refund", "pay1", "r1", "多收退回", "2026-09:400")

	// 新收款按退款后余额约束：2026-09 未收余额变为 400。
	if msg := h.runExpectErr("bill", "pay", "c1", "2026-09", "pay2", "401", "补缴"); !strings.Contains(msg, "超过未收余额 400 分") {
		t.Fatal(msg)
	}
	h.mustRun("bill", "pay", "c1", "2026-09", "pay2", "400", "补缴")

	// 费用调整及其撤销按退款后余额约束：应付不得低于退款后实收。
	if msg := h.runExpectErr("bill", "adjust", "c1", "2026-09", "adj1", "-1", "减免"); !strings.Contains(msg, "低于实收 1000 分") {
		t.Fatal(msg)
	}
	// 2026-09 退款后实收 600：补收 +400 再收满后，撤销该补收会使应付低于实收。
	h.mustRun("bill", "adjust", "c1", "2026-09", "adj2", "400", "补收")
	h.mustRun("bill", "pay", "c1", "2026-09", "pay3", "400", "再补缴")
	if msg := h.runExpectErr("bill", "revoke", "adj2", "反悔"); !strings.Contains(msg, "低于实收") {
		t.Fatal(msg)
	}
}

func TestRefundLedgerAndReconcile(t *testing.T) {
	h := newHarness(t)
	refundSetup(h)
	h.mustRun("bill", "refund", "pay1", "r1", "多收退回", "2026-09:400", "2026-10:500")

	// 流水按退款发生序号减少实收。
	out := h.mustRun("bill", "ledger", "c1", "2026-09")
	for _, want := range []string{
		"序号 2 退款 r1：实收 -400 分", "关联收款 pay1，本账单退款 400 分",
		"截止时余额：应付 1000 分（10.00 元），实收 600 分（6.00 元），未收余额 400 分（4.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("ledger 缺少 %q:\n%s", want, out)
		}
	}
	// 截止后的退款不影响历史余额。
	out = h.mustRun("bill", "ledger", "c1", "2026-09", "1")
	if strings.Contains(out, "退款") || !strings.Contains(out, "截止时余额：应付 1000 分（10.00 元），实收 1000 分（10.00 元），未收余额 0 分（0.00 元）") {
		t.Fatalf("截止后的退款不应影响历史余额:\n%s", out)
	}

	// 对账把跨月退款作为一次操作，逐月列出变化，不重复减去整笔总额。
	out = h.mustRun("bill", "reconcile", "c1", "2026-09", "2026-10")
	for _, want := range []string{
		"序号 2 退款 r1（关联收款 pay1）：原因：多收退回",
		"2026-09 实收 -400 分（退款 400 分），2026-10 实收 -500 分（退款 500 分）",
		"终点合计：应付 3000 分（30.00 元），实收 1600 分（16.00 元），未收余额 1400 分（14.00 元）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("reconcile 缺少 %q:\n%s", want, out)
		}
	}
	// 序号区间外的退款不出现也不计入。
	out = h.mustRun("bill", "reconcile", "c1", "2026-09", "2026-10", "0", "1")
	if strings.Contains(out, "退款") || !strings.Contains(out, "终点合计：应付 3000 分（30.00 元），实收 2500 分（25.00 元），未收余额 500 分（5.00 元）") {
		t.Fatalf("区间外的退款不应计入:\n%s", out)
	}
}

func TestRefundPersistenceAndCorruption(t *testing.T) {
	h := newHarness(t)
	refundSetup(h)
	h.mustRun("bill", "refund", "pay1", "r1", "多收退回", "2026-09:400")

	// 重启（每条命令都是新进程）后退款、幂等及限制保持。
	out := h.mustRun("bill", "refund", "pay1", "r1", "多收退回", "2026-09:400")
	if !strings.Contains(out, "已存在且内容相同") {
		t.Fatalf("重启后相同重放应幂等:\n%s", out)
	}
	h.runExpectErr("bill", "unpay", "pay1", "撤销")
	h.runExpectErr("bill", "correct", "pay1", "cor1", "改", "2026-10:2500")

	// 篡改存档：累计超退拒绝加载，损坏文件保留。
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(data), `"amount_fen": 400`, `"amount_fen": 9999`, 1)
	if bad == string(data) {
		t.Fatal("未找到退款金额字段")
	}
	if err := os.WriteFile(h.statePath(), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "累计超退") {
		t.Fatal(msg)
	}
	// 失效引用：退款指向不存在的收款。
	var s map[string]any
	if err := json.Unmarshal([]byte(bad), &s); err != nil {
		t.Fatal(err)
	}
	s["refunds"].(map[string]any)["r1"].(map[string]any)["payment_id"] = "ghost"
	s["refunds"].(map[string]any)["r1"].(map[string]any)["allocations"].([]any)[0].(map[string]any)["amount_fen"] = 400
	raw, _ := json.Marshal(s)
	if err := os.WriteFile(h.statePath(), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-09"); !strings.Contains(msg, "退款目标失效") {
		t.Fatal(msg)
	}
	// 损坏文件保留，恢复后可用。
	if _, err := os.Stat(h.statePath()); err != nil {
		t.Fatal("损坏文件应保留")
	}
	if err := os.WriteFile(h.statePath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustRun("bill", "show", "c1", "2026-09")
}

func TestRefundOldStateWithoutRefunds(t *testing.T) {
	h := newHarness(t)
	refundSetup(h)

	// 旧文件缺少退款字段视为无退款：删除 refunds 键后一切照常。
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	delete(s, "refunds")
	raw, _ := json.Marshal(s)
	if err := os.WriteFile(h.statePath(), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "实收：1000 分") || !strings.Contains(out, "退款历史：无") {
		t.Fatalf("旧文件应视为无退款:\n%s", out)
	}
	// 旧单月收款仍可用：退款、冻结规则照常。
	h.mustRun("bill", "pay", "c1", "2026-10", "pay2", "500", "补缴")
	h.mustRun("bill", "refund", "pay2", "r1", "多收退回", "2026-10:200")
	h.runExpectErr("bill", "unpay", "pay2", "撤销")
}

func TestRefundSeqSharedAndAtomic(t *testing.T) {
	h := newHarness(t)
	refundSetup(h)

	// 退款与调整、收款、更正及其撤销共用同一个递增全局序号。
	h.mustRun("bill", "adjust", "c1", "2026-10", "adj1", "100", "补收") // 序号 2
	out := h.mustRun("bill", "refund", "pay1", "r1", "多收退回", "2026-09:400")
	if !strings.Contains(out, "操作序号：3") {
		t.Fatalf("退款应占用递增的全局序号:\n%s", out)
	}
	// 失败退款不占序号：下一笔成功操作仍取连续序号。
	h.runExpectErr("bill", "refund", "pay1", "r2", "原因", "2026-09:9999")
	out = h.mustRun("bill", "refund", "pay1", "r2", "原因", "2026-09:100")
	if !strings.Contains(out, "操作序号：4") {
		t.Fatalf("失败退款不应占用序号:\n%s", out)
	}
}
