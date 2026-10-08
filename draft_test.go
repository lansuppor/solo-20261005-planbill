package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// --- 结算草案（bill draft / draft-show / draft-confirm）测试 ---

// stateBytes 读取存档原始字节，用于断言只读与重放命令不写盘。
func stateBytes(t *testing.T, h *harness) []byte {
	t.Helper()
	b, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixedDraftSetup(t *testing.T) *harness {
	h := newHarness(t)
	h.mustRun("customer", "add", "acme", "月球咖啡馆", "150")
	uf := h.writeFile("u.csv", csvHeader+
		"u-001,acme,2026-10-15T10:00:00Z,3\n"+
		"u-002,acme,2026-10-16T10:00:00Z,2\n")
	h.mustRun("usage", "import", uf)
	return h
}

func TestDraftFixedCreateShowConfirm(t *testing.T) {
	h := fixedDraftSetup(t)

	out := h.mustRun("bill", "draft", "d1", "acme", "2026-10")
	for _, want := range []string{
		"已创建结算草案 \"d1\"", "待确认", "固定单价",
		"单价：150 分", "总数量：5", "总金额：750 分",
		"用量标识=u-001", "用量标识=u-002", "不生成账单、不封账",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("草案创建输出缺少 %q:\n%s", want, out)
		}
	}
	// 创建后不生成账单、不封账、不占账后序号。
	if loadState(t, h).sealed("acme", "2026-10") {
		t.Fatal("草案创建后不应封账")
	}
	if got := loadState(t, h).NextSeq; got != 0 {
		t.Fatalf("草案创建不应占用账后序号，NextSeq=%d", got)
	}

	// 只读查询。
	out = h.mustRun("bill", "draft-show", "d1")
	if !strings.Contains(out, "待确认") || !strings.Contains(out, "草案标识：d1") {
		t.Fatalf("草案查询输出异常:\n%s", out)
	}

	// 确认封账。
	out = h.mustRun("bill", "draft-confirm", "d1")
	for _, want := range []string{"草案 \"d1\" 已确认", "BILL-", "账单、封账与确认状态一次原子保存"} {
		if !strings.Contains(out, want) {
			t.Fatalf("确认输出缺少 %q:\n%s", want, out)
		}
	}
	s := loadState(t, h)
	if !s.sealed("acme", "2026-10") {
		t.Fatal("确认后应已封账")
	}
	d := s.Drafts["d1"]
	if !d.Confirmed || d.BillID == "" || d.ConfirmedAt == "" {
		t.Fatal("确认状态与账单关联未持久化")
	}
	b := s.Bills[billKey("acme", "2026-10")]
	if b.ID != d.BillID || b.TotalFee != 750 || b.TotalQty != 5 {
		t.Fatalf("关联账单与快照不一致: %+v", b)
	}
	if s.NextSeq != 0 {
		t.Fatalf("确认不应占用账后序号，NextSeq=%d", s.NextSeq)
	}

	// bill show 与重复结算照常可用，且重复结算返回同一账单。
	out = h.mustRun("bill", "show", "acme", "2026-10")
	if !strings.Contains(out, d.BillID) {
		t.Fatal("bill show 应展示正式账单")
	}
	out = h.mustRun("bill", "settle", "acme", "2026-10")
	if !strings.Contains(out, "已结算，返回原账单") || !strings.Contains(out, d.BillID) {
		t.Fatalf("重复结算应返回原账单:\n%s", out)
	}
}

func TestDraftTieredSnapshotAndConfirm(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅", "1000", "100:10", "500:8", "-:5")
	h.mustRun("customer", "add-plan", "beta", "贝塔", "sub")
	uf := h.writeFile("u.csv", csvHeader+
		"u-1,beta,2026-10-10T10:00:00Z,80\n"+
		"u-2,beta,2026-10-20T10:00:00Z,150\n")
	h.mustRun("usage", "import", uf)

	out := h.mustRun("bill", "draft", "da", "beta", "2026-10")
	// 80 落第 1 档；第二条 20 落第 1 档、130 落第 2 档；用量费
	// 800+200+1040=2040，月费 1000，总额 3040。
	for _, want := range []string{
		"月费：1000 分", "用量费：2040 分", "原总金额：3040 分", "总数量：230",
		"第 1 档：数量 100，单价 10 分，金额 1000 分",
		"第 2 档：数量 130，单价 8 分，金额 1040 分",
		"分段 1：第 1 档 数量=80", "分段 1：第 1 档 数量=20", "分段 2：第 2 档 数量=130",
		"方案：sub（订阅）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("阶梯草案输出缺少 %q:\n%s", want, out)
		}
	}

	h.mustRun("bill", "draft-confirm", "da")
	b := loadState(t, h).Bills[billKey("beta", "2026-10")]
	if b == nil || b.TotalFee != 3040 || b.MonthlyFee != 1000 || b.PlanID != "sub" {
		t.Fatalf("确认生成的阶梯账单与快照不一致: %+v", b)
	}
}

func TestDraftCreateRejections(t *testing.T) {
	h := fixedDraftSetup(t)
	// 固定单价无用量月份拒绝且不占标识。
	if err := h.runExpectErr("bill", "draft", "dz", "acme", "2026-09"); !strings.Contains(err, "没有用量") {
		t.Fatalf("无用量月份应拒绝: %v", err)
	}
	// 失败创建不占标识：同一标识随后可成功创建。
	u9 := h.writeFile("u9.csv", csvHeader+"u-009,acme,2026-09-01T10:00:00Z,1\n")
	h.mustRun("usage", "import", u9)
	h.mustRun("bill", "draft", "dz", "acme", "2026-09")

	// 客户不存在。
	if err := h.runExpectErr("bill", "draft", "dx", "nope", "2026-10"); !strings.Contains(err, "不存在") {
		t.Fatalf("不存在客户应拒绝: %v", err)
	}
	// 空标识、坏月份。
	if err := h.runExpectErr("bill", "draft", " ", "acme", "2026-10"); !strings.Contains(err, "草案标识不能为空") {
		t.Fatalf("空标识应拒绝: %v", err)
	}
	var ue usageErrorf
	// 坏月份与其他账单命令一致，是业务错误(1)；参数个数不对才是用法错误(2)。
	if err := h.runExpectErr("bill", "draft", "dx", "acme", "2026-99"); !strings.Contains(err, "月份") {
		t.Fatalf("坏月份应拒绝: %v", err)
	}
	if _, err := h.run("bill", "draft", "only", "three"); err == nil || !errors.As(err, &ue) {
		t.Fatalf("参数个数不对应为用法错误(2): %v", err)
	}
	if _, err := h.run("bill", "draft-confirm"); err == nil || !errors.As(err, &ue) {
		t.Fatalf("draft-confirm 缺参数应为用法错误(2): %v", err)
	}
	if _, err := h.run("bill", "draft-show"); err == nil || !errors.As(err, &ue) {
		t.Fatalf("draft-show 缺参数应为用法错误(2): %v", err)
	}
	// 封账后不能再创建草案。
	h.mustRun("bill", "settle", "acme", "2026-10")
	if err := h.runExpectErr("bill", "draft", "d2", "acme", "2026-10"); !strings.Contains(err, "已封账") {
		t.Fatalf("已封账月份不应再创建草案: %v", err)
	}
}

func TestDraftSuspendTerminateZeroFeeRejections(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅", "0", "-:10")
	h.mustRun("plan", "add-fee", "fee", "付费", "1000", "-:10")
	h.mustRun("customer", "add-plan", "z", "零月费", "sub")
	h.mustRun("customer", "add-plan", "f", "正月费", "fee")

	// 零月费方案无用量月拒绝。
	if err := h.runExpectErr("bill", "draft", "d0", "z", "2026-11"); !strings.Contains(err, "没有用量") {
		t.Fatalf("零月费无用量月应拒绝: %v", err)
	}
	// 正月费方案无用量月可创建仅月费草案并确认。
	out := h.mustRun("bill", "draft", "df", "f", "2026-11")
	if !strings.Contains(out, "原总金额：1000 分") || !strings.Contains(out, "明细：无") {
		t.Fatalf("仅月费草案输出异常:\n%s", out)
	}
	h.mustRun("bill", "draft-confirm", "df")
	if !loadState(t, h).sealed("f", "2026-11") {
		t.Fatal("仅月费草案确认后应封账")
	}

	// 暂停月拒绝（正月费也不例外）。
	h.mustRun("customer", "suspend", "f", "2027-01", "2027-03", "装修")
	if err := h.runExpectErr("bill", "draft", "ds", "f", "2027-01"); !strings.Contains(err, "暂停") {
		t.Fatalf("暂停月应拒绝创建草案: %v", err)
	}
	// 终止月（含）起拒绝。
	h.mustRun("customer", "terminate", "z", "2027-06", "停业")
	if err := h.runExpectErr("bill", "draft", "dt", "z", "2027-06"); !strings.Contains(err, "终止") {
		t.Fatalf("终止月应拒绝创建草案: %v", err)
	}
}

func TestDraftReplayByIdentity(t *testing.T) {
	h := fixedDraftSetup(t)
	h.mustRun("bill", "draft", "d1", "acme", "2026-10")
	before := stateBytes(t, h)

	// 相同客户月份重放：不重新计算、不写盘。
	out := h.mustRun("bill", "draft", "d1", "acme", "2026-10")
	if !strings.Contains(out, "返回原快照与当前确认状态") {
		t.Fatalf("相同重放应返回原快照:\n%s", out)
	}
	if got := stateBytes(t, h); string(got) != string(before) {
		t.Fatal("相同重放不应写盘")
	}

	// 客户或月份不同：拒绝复用。
	if err := h.runExpectErr("bill", "draft", "d1", "acme", "2026-09"); !strings.Contains(err, "指向不同客户月份") {
		t.Fatalf("同标识指向不同月份应拒绝: %v", err)
	}
	h.mustRun("customer", "add", "other", "其他", "100")
	if err := h.runExpectErr("bill", "draft", "d1", "other", "2026-10"); !strings.Contains(err, "指向不同客户月份") {
		t.Fatalf("同标识指向不同客户应拒绝: %v", err)
	}

	// 后来封账也不阻止同客户同月份重放，仍不写盘。
	h.mustRun("bill", "draft-confirm", "d1")
	after := stateBytes(t, h)
	out = h.mustRun("bill", "draft", "d1", "acme", "2026-10")
	if !strings.Contains(out, "已确认") || !strings.Contains(out, "BILL-") {
		t.Fatalf("封账后的相同重放应返回原快照与已确认状态:\n%s", out)
	}
	if got := stateBytes(t, h); string(got) != string(after) {
		t.Fatal("封账后的相同重放不应写盘")
	}
}

func TestDraftMultipleSameMonthAndNotClaimBill(t *testing.T) {
	h := fixedDraftSetup(t)
	h.mustRun("bill", "draft", "da", "acme", "2026-10")
	// 不同草案可指向同一客户月份。
	h.mustRun("bill", "draft", "db", "acme", "2026-10")

	// 确认 da；db 的确认因月份已封账被拒绝，不认领 da 的账单。
	h.mustRun("bill", "draft-confirm", "da")
	err := h.runExpectErr("bill", "draft-confirm", "db")
	if !strings.Contains(err, "已由其他结算出账") || !strings.Contains(err, "不认领已有账单") {
		t.Fatalf("月份已由其他草案出账时应拒绝且不认领: %v", err)
	}
	// db 仍为待确认，且只有一张账单。
	s := loadState(t, h)
	if s.Drafts["db"].Confirmed {
		t.Fatal("被拒草案不应被确认")
	}
	if len(s.Bills) != 1 {
		t.Fatalf("被拒确认不应新增账单，账单数=%d", len(s.Bills))
	}

	// 由原结算命令先出账时同样拒绝。
	vf := h.writeFile("v.csv", csvHeader+"v-1,acme,2026-08-01T10:00:00Z,4\n")
	h.mustRun("usage", "import", vf)
	h.mustRun("bill", "draft", "dc", "acme", "2026-08")
	h.mustRun("bill", "settle", "acme", "2026-08")
	err = h.runExpectErr("bill", "draft-confirm", "dc")
	if !strings.Contains(err, "已由其他结算出账") {
		t.Fatalf("原结算命令已出账时确认应拒绝: %v", err)
	}
}

func TestDraftStaleConfirmRejections(t *testing.T) {
	// 撤回用量使有效用量变化 → 确认拒绝并逐项说明差异，草案保留待确认。
	h := fixedDraftSetup(t)
	h.mustRun("bill", "draft", "d1", "acme", "2026-10")
	h.mustRun("usage", "withdraw", "u-001", "误导入")
	err := h.runExpectErr("bill", "draft-confirm", "d1")
	for _, want := range []string{"不一致，确认被拒绝", "有效用量条数变化", "原总金额变化", "逐条明细条数变化"} {
		if !strings.Contains(err, want) {
			t.Fatalf("过时确认错误应包含 %q: %v", want, err)
		}
	}
	s := loadState(t, h)
	if s.Drafts["d1"].Confirmed || s.sealed("acme", "2026-10") {
		t.Fatal("失败确认应保留待确认草案且不封账")
	}
	// draft-show 仍可读，状态待确认。
	if out := h.mustRun("bill", "draft-show", "d1"); !strings.Contains(out, "待确认") {
		t.Fatalf("过时草案查询应仍为待确认:\n%s", out)
	}

	// 方案变化使阶梯草案过时。
	h2 := newHarness(t)
	h2.mustRun("plan", "add", "sub", "旧", "100:10", "-:5")
	h2.mustRun("plan", "add", "pro", "新", "100:8", "-:4")
	h2.mustRun("customer", "add-plan", "beta", "贝塔", "sub")
	wf := h2.writeFile("w.csv", csvHeader+"w-1,beta,2026-11-10T10:00:00Z,50\n")
	h2.mustRun("usage", "import", wf)
	h2.mustRun("bill", "draft", "dp", "beta", "2026-11")
	h2.mustRun("plan", "change", "beta", "2026-11", "pro", "续期降价")
	err = h2.runExpectErr("bill", "draft-confirm", "dp")
	if !strings.Contains(err, "月有效方案变化") || !strings.Contains(err, "阶梯规则变化") {
		t.Fatalf("方案变更后确认应逐项拒绝: %v", err)
	}
	if loadState(t, h2).sealed("beta", "2026-11") {
		t.Fatal("过时拒绝不应封账")
	}

	// 新增有效用量使总数量与金额变化 → 确认拒绝。
	h3 := fixedDraftSetup(t)
	h3.mustRun("bill", "draft", "d3", "acme", "2026-10")
	mf := h3.writeFile("more.csv", csvHeader+"u-003,acme,2026-10-17T10:00:00Z,1\n")
	h3.mustRun("usage", "import", mf)
	err = h3.runExpectErr("bill", "draft-confirm", "d3")
	if !strings.Contains(err, "有效用量条数变化") || !strings.Contains(err, "总数量变化") {
		t.Fatalf("新增用量后确认应拒绝: %v", err)
	}
}

func TestDraftPendingDoesNotRestrictLaterOperations(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "sub", "方案", "100:10", "-:5")
	h.mustRun("plan", "add", "pro", "续期", "-:4")
	h.mustRun("customer", "add-plan", "beta", "贝塔", "sub")
	wf := h.writeFile("w.csv", csvHeader+"w-1,beta,2026-11-10T10:00:00Z,50\n")
	h.mustRun("usage", "import", wf)
	h.mustRun("bill", "draft", "dp", "beta", "2026-11")

	// 待确认草案不限制后续用量、撤回、方案变更等操作。
	w2 := h.writeFile("w2.csv", csvHeader+"w-2,beta,2026-11-11T10:00:00Z,5\n")
	h.mustRun("usage", "import", w2)
	h.mustRun("usage", "withdraw", "w-2", "撤一条")
	h.mustRun("plan", "change", "beta", "2027-01", "pro", "后续续期")
	// 原结算命令照常可用于其他月份。
	w3 := h.writeFile("w3.csv", csvHeader+"w-3,beta,2026-12-10T10:00:00Z,10\n")
	h.mustRun("usage", "import", w3)
	h.mustRun("bill", "settle", "beta", "2026-12")
}

func TestDraftConfirmReplayReflectsPostbill(t *testing.T) {
	h := fixedDraftSetup(t)
	h.mustRun("bill", "draft", "d1", "acme", "2026-10")
	h.mustRun("bill", "draft-confirm", "d1")
	h.mustRun("bill", "adjust", "acme", "2026-10", "adj1", "100", "补收")
	h.mustRun("bill", "pay", "acme", "2026-10", "pay1", "300", "现金")

	before := stateBytes(t, h)
	seqBefore := loadState(t, h).NextSeq

	// 已确认草案的确认重放：返回关联账单及当前账后状态，不写盘、不重新收费、
	// 不占序号。
	out := h.mustRun("bill", "draft-confirm", "d1")
	for _, want := range []string{"已确认", "当前应付：850 分", "实收：300 分", "未收余额：550 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("已确认重放输出缺少 %q:\n%s", want, out)
		}
	}
	if got := stateBytes(t, h); string(got) != string(before) {
		t.Fatal("已确认重放不应写盘")
	}
	if seq := loadState(t, h).NextSeq; seq != seqBefore {
		t.Fatalf("已确认重放不应占用序号，%d -> %d", seqBefore, seq)
	}
	// draft-show 也展示关联账单当前账后状态。
	out = h.mustRun("bill", "draft-show", "d1")
	if !strings.Contains(out, "已确认") || !strings.Contains(out, "关联账单当前账后状态") {
		t.Fatalf("已确认草案查询应展示账后状态:\n%s", out)
	}
}

func TestDraftReadOnlyShowNeverWrites(t *testing.T) {
	h := fixedDraftSetup(t)
	if err := h.runExpectErr("bill", "draft-show", "missing"); !strings.Contains(err, "不存在") {
		t.Fatalf("查询不存在草案应报错: %v", err)
	}
	h.mustRun("bill", "draft", "d1", "acme", "2026-10")
	before := stateBytes(t, h)
	out := h.mustRun("bill", "draft-show", "d1")
	if !strings.Contains(out, "待确认") {
		t.Fatalf("查询输出异常:\n%s", out)
	}
	if got := stateBytes(t, h); string(got) != string(before) {
		t.Fatal("只读查询不应写盘")
	}
}

func TestDraftRestartKeepsSnapshotAndLink(t *testing.T) {
	h := fixedDraftSetup(t)
	h.mustRun("bill", "draft", "d1", "acme", "2026-10")

	// 重启（新进程视角）：待确认草案快照与判重保持。
	h2 := newHarness(t)
	h2.dir = h.dir
	out := h2.mustRun("bill", "draft", "d1", "acme", "2026-10")
	if !strings.Contains(out, "返回原快照与当前确认状态") {
		t.Fatalf("重启后相同创建应判重返回:\n%s", out)
	}
	h2.mustRun("bill", "draft-confirm", "d1")

	// 再次重启：已确认关联与确认重放保持。
	h3 := newHarness(t)
	h3.dir = h2.dir
	out = h3.mustRun("bill", "draft-confirm", "d1")
	if !strings.Contains(out, "已确认") || !strings.Contains(out, "BILL-") {
		t.Fatalf("重启后已确认重放应返回关联账单:\n%s", out)
	}
}

func TestDraftCorruptStateRejected(t *testing.T) {
	// 1) 快照计价不自洽（总额被篡改）→ 载入拒绝。
	h := fixedDraftSetup(t)
	h.mustRun("bill", "draft", "d1", "acme", "2026-10")
	mutateState(t, h, func(m map[string]any) {
		drafts := m["drafts"].(map[string]any)
		d1 := drafts["d1"].(map[string]any)
		d1["total_fee_fen"] = float64(999)
	})
	if err := h.runExpectErr("bill", "draft-show", "d1"); !strings.Contains(err, "已损坏") {
		t.Fatalf("快照不自洽应按损坏拒绝: %v", err)
	}

	// 2) 草案引用缺失（引用不存在的用量）→ 载入拒绝。
	h = fixedDraftSetup(t)
	h.mustRun("bill", "draft", "d2", "acme", "2026-10")
	mutateState(t, h, func(m map[string]any) {
		drafts := m["drafts"].(map[string]any)
		d2 := drafts["d2"].(map[string]any)
		refs := d2["usage_refs"].([]any)
		refs[0].(map[string]any)["usage_id"] = "u-missing"
		lines := d2["lines"].([]any)
		lines[0].(map[string]any)["usage_id"] = "u-missing"
	})
	if err := h.runExpectErr("bill", "draft-show", "d2"); !strings.Contains(err, "已损坏") || !strings.Contains(err, "引用缺失") {
		t.Fatalf("草案引用缺失应按损坏拒绝: %v", err)
	}

	// 3) 已确认草案关联账单标识不符 → 载入拒绝。
	h = fixedDraftSetup(t)
	h.mustRun("bill", "draft", "d3", "acme", "2026-10")
	h.mustRun("bill", "settle", "acme", "2026-10") // 与快照内容一致的正式账单
	mutateState(t, h, func(m map[string]any) {
		drafts := m["drafts"].(map[string]any)
		d3 := drafts["d3"].(map[string]any)
		d3["confirmed"] = true
		d3["confirmed_at"] = "2026-10-08T00:00:00Z"
		d3["bill_id"] = "BILL-wrongwrongwr00"
	})
	if err := h.runExpectErr("bill", "draft-show", "d3"); !strings.Contains(err, "已损坏") || !strings.Contains(err, "关联账单") {
		t.Fatalf("已确认草案关联账单不符应按损坏拒绝: %v", err)
	}

	// 损坏文件必须保留。
	if _, err := os.Stat(h.statePath()); err != nil {
		t.Fatalf("损坏存档应保留: %v", err)
	}
}

func TestDraftUsageCorrectionMakesStale(t *testing.T) {
	h := fixedDraftSetup(t)
	h.mustRun("bill", "draft", "d1", "acme", "2026-10")
	// 原子更正改变用量身份（原记录撤回、替代记录全新标识）→ 快照过时。
	h.mustRun("usage", "correct", "u-001", "u-001-fix", "acme", "2026-10-15T10:00:00Z", "4", "数量录错")
	err := h.runExpectErr("bill", "draft-confirm", "d1")
	if !strings.Contains(err, "不一致，确认被拒绝") {
		t.Fatalf("用量更正后草案应过时: %v", err)
	}
	// 失败确认不封账；更正后的月份按当前用量仍可正常结算。
	if loadState(t, h).sealed("acme", "2026-10") {
		t.Fatal("过时确认失败不应封账")
	}
	out := h.mustRun("bill", "settle", "acme", "2026-10")
	if !strings.Contains(out, "总金额：900 分") { // (4+2)*150
		t.Fatalf("更正后结算金额异常:\n%s", out)
	}
}

func TestDraftConfirmFailureLeavesPendingViaPlanChange(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "sub", "旧", "-:10")
	h.mustRun("plan", "add", "pro", "新", "-:8")
	h.mustRun("customer", "add-plan", "beta", "贝塔", "sub")
	wf := h.writeFile("w.csv", csvHeader+"w-1,beta,2026-11-10T10:00:00Z,5\n")
	h.mustRun("usage", "import", wf)
	h.mustRun("bill", "draft", "dp", "beta", "2026-11")
	beforeBills := len(loadState(t, h).Bills)
	h.mustRun("plan", "change", "beta", "2026-11", "pro", "降价")
	if err := h.runExpectErr("bill", "draft-confirm", "dp"); !strings.Contains(err, "不一致") {
		t.Fatalf("方案变化后确认应失败: %v", err)
	}
	s := loadState(t, h)
	if s.Drafts["dp"].Confirmed || len(s.Bills) != beforeBills {
		t.Fatal("失败确认应保留待确认草案且不新增账单")
	}
	// 草案仍可只读查询；标识不被占用，其他命令不受影响。
	if out := h.mustRun("bill", "draft-show", "dp"); !strings.Contains(out, "待确认") {
		t.Fatalf("失败后草案应仍可查询: %s", out)
	}
}
