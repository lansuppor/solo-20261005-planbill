package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 阶梯计费方案与阶梯账单的行为测试。沿用 main_test.go 的 harness：
// 每个用例独立临时数据目录，每次 run 重新从磁盘载入。

func TestPlanAddValidation(t *testing.T) {
	h := newHarness(t)

	out := h.mustRun("plan", "add", "p1", "标准阶梯", "100:10", "1000:8", "5")
	if !strings.Contains(out, "已登记阶梯计费方案") || !strings.Contains(out, "档 3") {
		t.Fatal(out)
	}

	// 重复标识拒绝，且失败新增不占标识（p2 规则非法后仍可用合法规则创建）。
	h.runExpectErr("plan", "add", "p1", "另一个名称", "10")
	h.runExpectErr("plan", "add", "p2", "非法", "100:10", "50:8", "5") // 上限未严格递增
	h.mustRun("plan", "add", "p2", "合法", "100:10", "5")

	// 空标识 / 空名称拒绝。
	h.runExpectErr("plan", "add", "", "名称", "10")
	h.runExpectErr("plan", "add", "   ", "名称", "10")
	h.runExpectErr("plan", "add", "p3", "", "10")
	h.runExpectErr("plan", "add", "p3", "   ", "10")

	// 阶梯规则非法：无档、最后一档带上限、中间档无上限、零/负上限、
	// 负单价、非整数、上限相等。
	h.runExpectErr("plan", "add", "p3", "名称")
	h.runExpectErr("plan", "add", "p3", "名称", "100:10", "200:5")
	h.runExpectErr("plan", "add", "p3", "名称", "10", "5")
	h.runExpectErr("plan", "add", "p3", "名称", "0:10", "5")
	h.runExpectErr("plan", "add", "p3", "名称", "-3:10", "5")
	h.runExpectErr("plan", "add", "p3", "名称", "100:-1", "5")
	h.runExpectErr("plan", "add", "p3", "名称", "100:10", "-5")
	h.runExpectErr("plan", "add", "p3", "名称", "abc:10", "5")
	h.runExpectErr("plan", "add", "p3", "名称", "100:1.5", "5")
	h.runExpectErr("plan", "add", "p3", "名称", "100:10", "100:8", "5")
	h.runExpectErr("plan", "add", "p3", "名称", "99999999999999999999999:10", "5")

	// 单档（仅无上限单价）与零单价均合法。
	h.mustRun("plan", "add", "p3", "单档", "10")
	h.mustRun("plan", "add", "p4", "免费", "0")
}

func TestPlanShowAndImmutable(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "标准阶梯", "100:10", "1000:8", "5")

	out := h.mustRun("plan", "show", "p1")
	for _, want := range []string{"方案标识：p1", "名称：标准阶梯", "档 1（累计 ≤ 100）：单价 10 分", "档 2（累计 ≤ 1000）：单价 8 分", "档 3（累计超过 1000（无上限））：单价 5 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan show 缺少 %q:\n%s", want, out)
		}
	}

	// 不存在的方案查询失败；重复登记（即使内容相同）拒绝——方案创建后不可修改。
	h.runExpectErr("plan", "show", "ghost")
	h.runExpectErr("plan", "add", "p1", "标准阶梯", "100:10", "1000:8", "5")
}

func TestCustomerAddPlan(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "标准阶梯", "100:10", "5")

	out := h.mustRun("customer", "add-plan", "c1", "阶梯客户", "p1")
	if !strings.Contains(out, "绑定阶梯计费方案") {
		t.Fatal(out)
	}

	// 方案不存在、空标识、空名称、空方案标识均拒绝，失败不占标识。
	h.runExpectErr("customer", "add-plan", "c2", "客户", "ghost")
	h.runExpectErr("customer", "add-plan", "", "客户", "p1")
	h.runExpectErr("customer", "add-plan", "c2", "", "p1")
	h.runExpectErr("customer", "add-plan", "c2", "客户", "")
	h.mustRun("customer", "add-plan", "c2", "客户二", "p1")

	// 已有客户不得换方案：重复标识拒绝（无论走哪个入口）。
	h.runExpectErr("customer", "add-plan", "c1", "改名", "p1")
	h.runExpectErr("customer", "add", "c1", "改名", "10")
	h.mustRun("customer", "add", "c3", "固定客户", "10")
	h.runExpectErr("customer", "add-plan", "c3", "换方案", "p1")
}

// tieredSetup 登记方案 100:10 / 1000:8 / 5 与阶梯客户 tc。
func tieredSetup(h *harness) {
	h.mustRun("plan", "add", "tp", "测试阶梯", "100:10", "1000:8", "5")
	h.mustRun("customer", "add-plan", "tc", "阶梯客户", "tp")
}

func TestTieredSettleCrossTier(t *testing.T) {
	h := newHarness(t)
	tieredSetup(h)
	// u-001 跨档：100×10 + 50×8 = 1400；u-002 全部落在第二档：60×8 = 480。
	f := h.writeFile("usage.csv", csvHeader+
		"u-001,tc,2026-09-15T10:00:00Z,150\n"+
		"u-002,tc,2026-09-16T10:00:00Z,60\n")
	h.mustRun("usage", "import", f)

	out := h.mustRun("bill", "settle", "tc", "2026-09")
	for _, want := range []string{
		"计价类型：阶梯计价（按月累计用量）",
		"方案：tp（测试阶梯）",
		"档 1（累计 ≤ 100）：数量 100 × 单价 10 分 = 1000 分",
		"档 2（累计 ≤ 1000）：数量 110 × 单价 8 分 = 880 分",
		"档 3（累计超过 1000（无上限））：数量 0 × 单价 5 分 = 0 分",
		"总数量：210",
		"总金额：1880 分",
		"用量标识=u-001 时间=2026-09-15T10:00:00Z 数量=150 小计=1400 分",
		"档 1 数量 100 × 单价 10 分 = 1000 分（10.00 元）；档 2 数量 50 × 单价 8 分 = 400 分",
		"用量标识=u-002 时间=2026-09-16T10:00:00Z 数量=60 小计=480 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("settle 输出缺少 %q:\n%s", want, out)
		}
	}
	// 阶梯账单不伪造统一单价。
	if strings.Contains(out, "单价：") {
		t.Fatalf("阶梯账单不应展示统一单价:\n%s", out)
	}

	// bill show 与重复 bill settle 返回相同计费明细。
	show := h.mustRun("bill", "show", "tc", "2026-09")
	again := h.mustRun("bill", "settle", "tc", "2026-09")
	if !strings.Contains(again, "已结算，返回原账单") {
		t.Fatal(again)
	}
	for _, want := range []string{"总金额：1880 分", "档 2 数量 50 × 单价 8 分 = 400 分（4.00 元）", "档 2（累计 ≤ 1000）：数量 110 × 单价 8 分 = 880 分"} {
		if !strings.Contains(show, want) || !strings.Contains(again, want) {
			t.Fatalf("show/重复 settle 明细不一致，缺少 %q:\nshow:\n%s\nagain:\n%s", want, show, again)
		}
	}
}

func TestTieredSettleOrdersByInstantAndID(t *testing.T) {
	h := newHarness(t)
	// 两档方案：前 10 个 1 分，之后 100 分。同一时刻两条记录按标识字典序计价：
	// u-a 先计价落在第一档，u-b 落入第二档。
	h.mustRun("plan", "add", "tp", "测试阶梯", "10:1", "100")
	h.mustRun("customer", "add-plan", "tc", "阶梯客户", "tp")
	f := h.writeFile("usage.csv", csvHeader+
		"u-b,tc,2026-09-15T10:00:00Z,10\n"+
		"u-a,tc,2026-09-15T18:00:00+08:00,10\n")
	h.mustRun("usage", "import", f)

	out := h.mustRun("bill", "settle", "tc", "2026-09")
	ia := strings.Index(out, "用量标识=u-a")
	ib := strings.Index(out, "用量标识=u-b")
	if ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("明细应按时间点升序、同一时间按标识字典序排列:\n%s", out)
	}
	if !strings.Contains(out, "用量标识=u-a 时间=2026-09-15T18:00:00+08:00 数量=10 小计=10 分") {
		t.Fatalf("u-a 应先计价落入第一档:\n%s", out)
	}
	if !strings.Contains(out, "用量标识=u-b 时间=2026-09-15T10:00:00Z 数量=10 小计=1000 分") {
		t.Fatalf("u-b 应落入第二档:\n%s", out)
	}
	if !strings.Contains(out, "总金额：1010 分") {
		t.Fatalf("总金额应为 1010 分:\n%s", out)
	}
}

func TestTieredImportPrecheckAndBatchRules(t *testing.T) {
	h := newHarness(t)
	// 单价为最大值的方案：单条数量 2 从零计价即溢出。
	h.mustRun("plan", "add", "tp", "贵方案", "1000:9223372036854775807", "1")
	h.mustRun("customer", "add-plan", "tc", "阶梯客户", "tp")

	f := h.writeFile("bad.csv", csvHeader+"u-001,tc,2026-09-15T10:00:00Z,2\n")
	err := h.runExpectErr("usage", "import", f)
	if !strings.Contains(err, "从零阶梯计价溢出") {
		t.Fatal(err)
	}
	// 整批未生效：合法记录也不入库。
	f = h.writeFile("mixed.csv", csvHeader+
		"u-ok,tc,2026-09-15T10:00:00Z,1\n"+
		"u-bad,tc,2026-09-15T11:00:00Z,2\n")
	h.runExpectErr("usage", "import", f)
	h.runExpectErr("bill", "settle", "tc", "2026-09") // 无用量，说明 u-ok 未入库

	// 去重规则不变：相同内容重放跳过，内容不同拒绝整批。
	f = h.writeFile("ok.csv", csvHeader+"u-ok,tc,2026-09-15T10:00:00Z,1\n")
	h.mustRun("usage", "import", f)
	out := h.mustRun("usage", "import", f)
	if !strings.Contains(out, "重复跳过 1 条") {
		t.Fatal(out)
	}
	f = h.writeFile("conflict.csv", csvHeader+"u-ok,tc,2026-09-15T10:00:00Z,2\n")
	h.runExpectErr("usage", "import", f)
}

func TestTieredSettleOverflowRejectsWithoutSealing(t *testing.T) {
	h := newHarness(t)
	// 免费方案（单价 0）：金额不溢出，但月累计数量可溢出。
	h.mustRun("plan", "add", "tp", "免费", "0")
	h.mustRun("customer", "add-plan", "tc", "阶梯客户", "tp")
	f := h.writeFile("usage.csv", csvHeader+
		"u-1,tc,2026-09-01T00:00:00Z,9223372036854775807\n"+
		"u-2,tc,2026-09-02T00:00:00Z,1\n")
	h.mustRun("usage", "import", f)

	err := h.runExpectErr("bill", "settle", "tc", "2026-09")
	if !strings.Contains(err, "溢出") {
		t.Fatal(err)
	}
	// 不封账：show 仍无账单，新用量仍可进入该月。
	h.runExpectErr("bill", "show", "tc", "2026-09")
	f = h.writeFile("more.csv", csvHeader+"u-3,tc,2026-09-03T00:00:00Z,1\n")
	h.mustRun("usage", "import", f)
}

func TestTieredZeroFeeBillSeals(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "tp", "免费", "100:0", "0")
	h.mustRun("customer", "add-plan", "tc", "阶梯客户", "tp")
	f := h.writeFile("usage.csv", csvHeader+"u-1,tc,2026-09-15T10:00:00Z,150\n")
	h.mustRun("usage", "import", f)

	out := h.mustRun("bill", "settle", "tc", "2026-09")
	if !strings.Contains(out, "总金额：0 分") || !strings.Contains(out, "已封账") {
		t.Fatal(out)
	}
	// 零费用账单正常封账：新用量不得进入该月。
	f = h.writeFile("late.csv", csvHeader+"u-2,tc,2026-09-16T10:00:00Z,1\n")
	h.runExpectErr("usage", "import", f)
	// 应付为 0 的账单不能登记正额收款（与固定单价一致）。
	h.runExpectErr("bill", "pay", "tc", "2026-09", "pay-1", "1", "备注")
}

func TestTieredBillAdjustPayLedger(t *testing.T) {
	h := newHarness(t)
	tieredSetup(h)
	f := h.writeFile("usage.csv", csvHeader+"u-1,tc,2026-09-15T10:00:00Z,150\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "tc", "2026-09") // 总金额 1400

	// 调整、收款、流水在阶梯账单上的行为与固定单价一致。
	h.mustRun("bill", "adjust", "tc", "2026-09", "adj-1", "100", "补收")
	h.mustRun("bill", "pay", "tc", "2026-09", "pay-1", "1500", "转账")
	out := h.mustRun("bill", "show", "tc", "2026-09")
	for _, want := range []string{"总金额：1400 分", "调整净额：+100 分", "当前应付：1500 分", "实收：1500 分", "未收余额：0 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show 缺少 %q:\n%s", want, out)
		}
	}
	// 超额收款拒绝：0 ≤ 实收 ≤ 应付。
	h.runExpectErr("bill", "pay", "tc", "2026-09", "pay-2", "1", "超额")
	out = h.mustRun("bill", "ledger", "tc", "2026-09")
	if !strings.Contains(out, "截止时余额：应付 1500 分") {
		t.Fatal(out)
	}
}

func TestTieredPersistenceAcrossInvocations(t *testing.T) {
	h := newHarness(t)
	tieredSetup(h)
	f := h.writeFile("usage.csv", csvHeader+"u-1,tc,2026-09-15T10:00:00Z,150\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "tc", "2026-09")

	// 每次 run 都重新从磁盘载入：方案、绑定、账单与幂等跨重启保持。
	out := h.mustRun("plan", "show", "tp")
	if !strings.Contains(out, "档 3（累计超过 1000（无上限））：单价 5 分") {
		t.Fatal(out)
	}
	out = h.mustRun("bill", "settle", "tc", "2026-09")
	if !strings.Contains(out, "已结算，返回原账单") || !strings.Contains(out, "总金额：1400 分") {
		t.Fatal(out)
	}
	h.runExpectErr("plan", "add", "tp", "改名", "1")
	h.runExpectErr("customer", "add-plan", "tc", "改名", "tp")
}

func TestOldStateFileWithoutPlans(t *testing.T) {
	h := newHarness(t)
	h.mustRun("customer", "add", "c1", "固定客户", "150")
	f := h.writeFile("usage.csv", csvHeader+"u-1,c1,2026-09-15T10:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-09")

	// 抹掉 plans 字段模拟旧文件：无方案的客户与账单按固定单价读取。
	data, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "plans")
	data, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.statePath(), data, 0o644); err != nil {
		t.Fatal(err)
	}

	out := h.mustRun("bill", "show", "c1", "2026-09")
	if !strings.Contains(out, "计价类型：固定单价") || !strings.Contains(out, "总金额：450 分") {
		t.Fatal(out)
	}
	// 旧库上可继续登记方案与阶梯客户。
	h.mustRun("plan", "add", "p1", "新方案", "100:10", "5")
	h.mustRun("customer", "add-plan", "c2", "阶梯客户", "p1")
}

func TestCorruptPlanDataRejected(t *testing.T) {
	h := newHarness(t)
	tieredSetup(h)
	f := h.writeFile("usage.csv", csvHeader+"u-1,tc,2026-09-15T10:00:00Z,150\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "tc", "2026-09")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(name string, fn func(m map[string]json.RawMessage)) {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(good, &m); err != nil {
			t.Fatal(err)
		}
		fn(m)
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.statePath(), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := h.run("bill", "show", "tc", "2026-09"); err == nil {
			t.Fatalf("%s：损坏数据应被拒绝，输出:\n%s", name, out)
		}
		// 损坏存档保留不被覆盖，恢复后一切正常。
		var m2 map[string]json.RawMessage
		data, err = os.ReadFile(h.statePath())
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &m2); err != nil {
			t.Fatalf("%s：损坏存档被改写: %v", name, err)
		}
		if err := os.WriteFile(h.statePath(), good, 0o644); err != nil {
			t.Fatal(err)
		}
		h.mustRun("bill", "show", "tc", "2026-09")
	}

	rawOf := func(v any) json.RawMessage {
		d, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	// 客户绑定了不存在的方案。
	mutate("客户方案引用失效", func(m map[string]json.RawMessage) {
		m["customers"] = rawOf(map[string]any{
			"tc": map[string]any{"id": "tc", "name": "阶梯客户", "price_fen": 0, "plan_id": "ghost"},
		})
	})
	// 账单引用了不存在的方案。
	mutate("账单方案引用失效", func(m map[string]json.RawMessage) {
		m["plans"] = rawOf(map[string]any{})
	})
	// 方案规则非法（上限未严格递增）。
	mutate("方案规则非法", func(m map[string]json.RawMessage) {
		m["plans"] = rawOf(map[string]any{
			"tp": map[string]any{"id": "tp", "name": "测试阶梯", "tiers": []any{
				map[string]any{"limit": 100, "price_fen": 10},
				map[string]any{"limit": 50, "price_fen": 8},
				map[string]any{"limit": 0, "price_fen": 5},
			}},
		})
	})
	// 账单明细小计与阶梯规则不符。
	mutate("明细小计不符", func(m map[string]json.RawMessage) {
		var s map[string]json.RawMessage
		if err := json.Unmarshal(good, &s); err != nil {
			t.Fatal(err)
		}
		var bills map[string]map[string]any
		if err := json.Unmarshal(s["bills"], &bills); err != nil {
			t.Fatal(err)
		}
		b := bills["tc|2026-09"]
		lines := b["lines"].([]any)
		line := lines[0].(map[string]any)
		line["line_fee_fen"] = 1399
		lines[0] = line
		b["lines"] = lines
		bills["tc|2026-09"] = b
		m["bills"] = rawOf(bills)
	})
	// 账单分段与阶梯规则不符。
	mutate("分段不符", func(m map[string]json.RawMessage) {
		var s map[string]json.RawMessage
		if err := json.Unmarshal(good, &s); err != nil {
			t.Fatal(err)
		}
		var bills map[string]map[string]any
		if err := json.Unmarshal(s["bills"], &bills); err != nil {
			t.Fatal(err)
		}
		b := bills["tc|2026-09"]
		lines := b["lines"].([]any)
		line := lines[0].(map[string]any)
		line["segments"] = []any{map[string]any{"tier": 0, "quantity": 150, "unit_price_fen": 10, "fee_fen": 1400}}
		lines[0] = line
		b["lines"] = lines
		bills["tc|2026-09"] = b
		m["bills"] = rawOf(bills)
	})
	// 分档合计与明细不符。
	mutate("分档合计不符", func(m map[string]json.RawMessage) {
		var s map[string]json.RawMessage
		if err := json.Unmarshal(good, &s); err != nil {
			t.Fatal(err)
		}
		var bills map[string]map[string]any
		if err := json.Unmarshal(s["bills"], &bills); err != nil {
			t.Fatal(err)
		}
		b := bills["tc|2026-09"]
		b["tier_totals"] = []any{
			map[string]any{"quantity": 100, "fee_fen": 1000},
			map[string]any{"quantity": 49, "fee_fen": 392},
			map[string]any{"quantity": 0, "fee_fen": 0},
		}
		bills["tc|2026-09"] = b
		m["bills"] = rawOf(bills)
	})
}
