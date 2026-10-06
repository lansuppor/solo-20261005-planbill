package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// --- 按月暂停区间测试 ---

func TestSuspensionRegisterHappyPathAndQuery(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")

	out := h.mustRun("customer", "suspend", "c1", "2026-11", "2027-02", "店面装修暂停营业")
	for _, want := range []string{
		"已登记暂停区间", "客户：c1", "暂停区间：2026-11（含）至 2027-02（不含", "原因：店面装修暂停营业",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("登记暂停输出缺少 %q:\n%s", want, out)
		}
	}

	// 查询全部区间：按起月升序展示区间与原因。
	out = h.mustRun("customer", "suspensions", "c1")
	for _, want := range []string{
		"客户：c1（客户一）", "暂停区间（按起月升序",
		"2026-11（含）至 2027-02（不含）", "原因：店面装修暂停营业",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("暂停查询输出缺少 %q:\n%s", want, out)
		}
	}

	// 指定月份：起月含、结束月不含，区间外正常服务。
	cases := map[string]bool{
		"2026-10": false, // 区间之前
		"2026-11": true,  // 起月（含）
		"2026-12": true,
		"2027-01": true,
		"2027-02": false, // 结束月（不含）= 恢复服务
	}
	for month, suspended := range cases {
		out = h.mustRun("customer", "suspensions", "c1", month)
		got := strings.Contains(out, "已暂停")
		if got != suspended {
			t.Fatalf("月份 %s 暂停状态=%v，期望 %v:\n%s", month, got, suspended, out)
		}
	}
}

func TestSuspensionValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "add", "c9", "固定客户", "10")

	h.runExpectErr("customer", "suspend", "c1", "2026-13", "2027-01", "原因") // 起月非法
	h.runExpectErr("customer", "suspend", "c1", "2026-11", "2026-1", "原因")  // 结束月格式错
	h.runExpectErr("customer", "suspend", "c1", "2026-11", "2026-11", "原因") // 结束月=起月
	h.runExpectErr("customer", "suspend", "c1", "2026-11", "2026-10", "原因") // 结束月早于起月
	h.runExpectErr("customer", "suspend", "c1", "2026-11", "2027-01", "  ") // 空原因
	h.runExpectErr("customer", "suspend", "ghost", "2026-11", "2027-01", "原因")
	// 仅限已绑定阶梯方案的客户。
	if msg := h.runExpectErr("customer", "suspend", "c9", "2026-11", "2027-01", "原因"); !strings.Contains(msg, "固定单价") {
		t.Fatal(msg)
	}
	// 参数数量错误是用法错误（退出码 2）；月份非法由命令返回业务错误（退出码 1）。
	for _, args := range [][]string{
		{"customer", "suspend", "c1", "2026-11", "2027-01"},
		{"customer", "suspend", "c1", "2026-11"},
		{"customer", "suspensions"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}
	h.runExpectErr("customer", "suspensions", "c1", "2026-13")
	// 查询固定单价客户：说明不适用暂停（含/不含月份两种形式）。
	if out := h.mustRun("customer", "suspensions", "c9"); !strings.Contains(out, "不适用按月暂停") {
		t.Fatalf("固定客户查询异常:\n%s", out)
	}
	if out := h.mustRun("customer", "suspensions", "c9", "2026-11"); !strings.Contains(out, "不适用暂停") {
		t.Fatalf("固定客户按月查询异常:\n%s", out)
	}
	// 查询不存在的客户失败但不改写存档。
	h.runExpectErr("customer", "suspensions", "ghost")
	// 全部失败后起月未被占用，可原样登记成功。
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "首次暂停")
}

func TestSuspensionIdempotentReplayAndImmutable(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 相同结束月与原因的重放返回原记录且不写盘。
	out := h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")
	if !strings.Contains(out, "内容相同，返回原记录") {
		t.Fatalf("重放未幂等返回:\n%s", out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("相同重放改写了存档")
	}
	// 结束月不同 / 原因不同均拒绝，记录不可改写。
	h.runExpectErr("customer", "suspend", "c1", "2026-11", "2027-02", "装修")
	h.runExpectErr("customer", "suspend", "c1", "2026-11", "2027-01", "其他原因")

	// 即使后来结算了恢复后的月份（2027-01 为结束月，正常服务，月费 > 0 可出账），
	// 相同重放仍须成功且不写盘。
	h.mustRun("bill", "settle", "c1", "2027-01")
	out = h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")
	if !strings.Contains(out, "内容相同，返回原记录") {
		t.Fatalf("结算恢复月后重放失败:\n%s", out)
	}
}

func TestSuspensionOverlapRejectedButAdjacentAllowed(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "第一段")

	// 各种重叠形态均拒绝。
	if msg := h.runExpectErr("customer", "suspend", "c1", "2026-12", "2027-02", "内含重叠"); !strings.Contains(msg, "重叠") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("customer", "suspend", "c1", "2026-10", "2026-12", "左侧重叠"); !strings.Contains(msg, "重叠") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("customer", "suspend", "c1", "2026-10", "2027-02", "整体包围"); !strings.Contains(msg, "重叠") {
		t.Fatal(msg)
	}
	// 相接合法：第二段自前一段结束月起，相接视为连续暂停。
	h.mustRun("customer", "suspend", "c1", "2027-01", "2027-03", "第二段（相接）")
	out := h.mustRun("customer", "suspensions", "c1")
	if !strings.Contains(out, "1. 2026-11（含）至 2027-01（不含）") ||
		!strings.Contains(out, "2. 2027-01（含）至 2027-03（不含）") {
		t.Fatalf("相接区间展示异常:\n%s", out)
	}
	// 相接边界月连续暂停；2026-10 与 2027-03 正常服务。
	for month, suspended := range map[string]bool{
		"2026-12": true, "2027-01": true, "2027-02": true,
		"2026-10": false, "2027-03": false,
	} {
		out = h.mustRun("customer", "suspensions", "c1", month)
		if strings.Contains(out, "已暂停") != suspended {
			t.Fatalf("相接场景月份 %s 状态异常:\n%s", month, out)
		}
	}
}

func TestSuspensionStartAfterSealedMonths(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-10-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-10")

	// 起月早于/等于已封账月份，或区间覆盖已封账月份，均拒绝。
	if msg := h.runExpectErr("customer", "suspend", "c1", "2026-10", "2026-12", "起月即封账月"); !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("customer", "suspend", "c1", "2026-09", "2026-11", "起月更早"); !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("customer", "suspend", "c1", "2026-09", "2027-01", "覆盖封账月"); !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
	// 起月严格晚于所有已封账月份：合法。
	h.mustRun("customer", "suspend", "c1", "2026-11", "2026-12", "封账之后")
}

func TestSuspensionRejectsExistingUsageInRange(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-11-05T00:00:00Z,3\n"+
		"u2,c1,2026-12-05T00:00:00Z,2\n")
	h.mustRun("usage", "import", f)

	// 区间内已有用量：拒绝并指出冲突记录，不删除用量。
	msg := h.runExpectErr("customer", "suspend", "c1", "2026-11", "2027-01", "装修")
	for _, want := range []string{"已存在 2 条用量", "u1", "u2", "2026-11", "2026-12", "不改写用量"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("冲突报错缺少 %q:\n%s", want, msg)
		}
	}
	// 用量保持不变：重放导入仍全部识别为重复。
	if out := h.mustRun("usage", "import", f); !strings.Contains(out, "重复跳过 2 条") {
		t.Fatalf("拒绝暂停后用量被改写:\n%s", out)
	}
	// 区间不覆盖用量即可登记：只覆盖 2027-01。
	h.mustRun("customer", "suspend", "c1", "2027-01", "2027-02", "无冲突")
	// 结束月（不含）内已有用量不冲突。
	h.mustRun("customer", "suspend", "c1", "2026-09", "2026-11", "结束月有用量")
}

func TestSuspensionImportRejectedWholeBatch(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "add-plan", "c2", "客户二", "p1")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")

	// 第 2 行落入暂停月，第 3 行合法：整批失败并指出行号，合法项也不留下。
	f := h.writeFile("u.csv", csvHeader+
		"u1,c1,2026-11-05T00:00:00Z,3\n"+
		"u2,c1,2026-10-05T00:00:00Z,2\n")
	msg := h.runExpectErr("usage", "import", f)
	if !strings.Contains(msg, "第 2 行") || !strings.Contains(msg, "2026-11 处于暂停区间") ||
		!strings.Contains(msg, "整批未生效") {
		t.Fatalf("暂停月导入报错异常:\n%s", msg)
	}
	// 合法项未留下：再次仅导入 u2 时应报新增 1 条。
	f2 := h.writeFile("u2.csv", csvHeader+"u2,c1,2026-10-05T00:00:00Z,2\n")
	if out := h.mustRun("usage", "import", f2); !strings.Contains(out, "新增 1 条") {
		t.Fatalf("整批失败后合法行被保留:\n%s", out)
	}
	// 按记录时间换算 UTC 月判断：本地 +08:00 的 11 月 1 日凌晨仍是 UTC 10 月，放行。
	f3 := h.writeFile("u3.csv", csvHeader+"u3,c1,2026-11-01T07:00:00+08:00,2\n")
	h.mustRun("usage", "import", f3)
	// 本地 11 月 1 日 09 点（+08:00）= UTC 11 月 1 日，拒绝。
	f4 := h.writeFile("u4.csv", csvHeader+"u4,c1,2026-11-01T09:00:00+08:00,2\n")
	if msg := h.runExpectErr("usage", "import", f4); !strings.Contains(msg, "第 2 行") {
		t.Fatal(msg)
	}
	// 其他客户同一月份不受影响。
	f5 := h.writeFile("u5.csv", csvHeader+"u5,c2,2026-11-05T00:00:00Z,4\n")
	h.mustRun("usage", "import", f5)
}

func TestSuspensionSettleRejectedNoBillNoSeal(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")

	// 单笔结算：暂停月无论月费多少都拒绝，不封账、不生成零金额账单。
	msg := h.runExpectErr("bill", "settle", "c1", "2026-11")
	if !strings.Contains(msg, "暂停区间") || !strings.Contains(msg, "拒绝结算且不封账") {
		t.Fatal(msg)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-11"); !strings.Contains(msg, "尚无账单") {
		t.Fatalf("暂停月生成了账单:\n%s", msg)
	}
	// 重复拒绝仍然不封账（不存在“原账单”可幂等返回）。
	h.runExpectErr("bill", "settle", "c1", "2026-12")

	// 批量清单包含暂停月：整批拒绝，同月清单里的合法未结算项也不留账单。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-10-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	msg = h.runExpectErr("bill", "settle-batch", "c1", "2026-11", "c1", "2026-10")
	if !strings.Contains(msg, "整批未生效") || !strings.Contains(msg, "暂停区间") {
		t.Fatalf("批量暂停月报错异常:\n%s", msg)
	}
	if msg := h.runExpectErr("bill", "show", "c1", "2026-10"); !strings.Contains(msg, "尚无账单") {
		t.Fatalf("整批失败后合法项被封账:\n%s", msg)
	}
	// 去除暂停月后批量结算正常（结束月 2027-01 无用量但月费 > 0，同样出账）。
	out := h.mustRun("bill", "settle-batch", "c1", "2026-10", "c1", "2027-01")
	if !strings.Contains(out, "新增账单 2 张") {
		t.Fatalf("恢复月批量结算异常:\n%s", out)
	}
}

func TestSuspensionRecoveryBillingAndPlanChange(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "old", "旧订阅", "1000", "100:10", "-:5")
	h.mustRun("plan", "add-fee", "new", "新订阅", "2000", "-:2")
	h.mustRun("customer", "add-plan", "c1", "客户一", "old")

	// 暂停前导入并结算 2026-10：用量 60，费用 60×10=600 + 月费 1000 = 1600。
	f := h.writeFile("u10.csv", csvHeader+"u10,c1,2026-10-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-10")
	// 暂停 2026-11、2026-12 两个月。
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")
	// 暂停期间允许登记生效月落在暂停月的方案变更（限制与预检不变）。
	h.mustRun("plan", "change", "c1", "2026-12", "new", "续期换方案")

	// 恢复月 2027-01 导入用量：阶梯从零累计（60 仍在第 1 档，按新方案单价 2）。
	f = h.writeFile("u01.csv", csvHeader+"u01,c1,2027-01-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)
	out := h.mustRun("bill", "settle", "c1", "2027-01")
	// 按恢复后账期有效方案收一次整月月费 2000，用量费 120，从零累计分档；
	// 不补收暂停月月费。
	for _, want := range []string{
		"方案：new（新订阅）", "月费：2000 分", "用量费：120 分", "原总金额：2120 分",
		"累计数量无上限，单价 2 分", "明细：",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("恢复月结算缺少 %q:\n%s", want, out)
		}
	}
	// 历史账单不因暂停或变更改动。
	show := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(show, "方案：old（旧订阅）") || !strings.Contains(show, "原总金额：1600 分") {
		t.Fatalf("历史账单被改动:\n%s", show)
	}
	// 暂停月仍不补收：无法结算 2026-11/2026-12。
	h.runExpectErr("bill", "settle", "c1", "2026-11")
	h.runExpectErr("bill", "settle", "c1", "2026-12")
}

// f10Path/f01Path 占位已移除（直接使用 writeFile 返回的路径）。

func TestSuspensionOtherCustomersUnaffected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "暂停客户", "p1")
	h.mustRun("customer", "add-plan", "c2", "正常客户", "p1")
	h.mustRun("customer", "add", "c3", "固定客户", "10")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")

	f := h.writeFile("u.csv", csvHeader+
		"u1,c2,2026-11-05T00:00:00Z,3\n"+
		"u2,c3,2026-11-05T00:00:00Z,2\n")
	h.mustRun("usage", "import", f)
	out := h.mustRun("bill", "settle-batch", "c2", "2026-11", "c3", "2026-11")
	if !strings.Contains(out, "新增账单 2 张") {
		t.Fatalf("其他客户受暂停影响:\n%s", out)
	}
	// 区间查询相互独立。
	if out := h.mustRun("customer", "suspensions", "c2"); !strings.Contains(out, "暂停区间：无") {
		t.Fatalf("其他客户出现暂停区间:\n%s", out)
	}
}

func TestSuspensionDoesNotConsumeSeq(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-10-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")
	h.mustRun("bill", "adjust", "c1", "2026-10", "adj-1", "5", "补收")
	ledger := h.mustRun("bill", "ledger", "c1", "2026-10")
	if !strings.Contains(ledger, "存档全局序号上限：1") || !strings.Contains(ledger, "序号 1 调整 adj-1") {
		t.Fatalf("暂停占用了操作序号:\n%s", ledger)
	}
}

func TestSuspensionPersistenceAcrossRestart(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修")

	// 每次 run 都重新从磁盘载入：区间、幂等与暂停限制跨进程保持。
	out := h.mustRun("customer", "suspensions", "c1", "2026-11")
	if !strings.Contains(out, "已暂停") {
		t.Fatalf("重启后暂停区间丢失:\n%s", out)
	}
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-01", "装修") // 幂等
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,3\n")
	if msg := h.runExpectErr("usage", "import", f); !strings.Contains(msg, "暂停区间") {
		t.Fatalf("重启后暂停限制丢失:\n%s", msg)
	}
	h.runExpectErr("bill", "settle", "c1", "2026-12")
}

func TestSuspensionOldStateFileWithoutField(t *testing.T) {
	h := newHarness(t)
	// 手工构造无 suspensions 字段的旧版数据文件：缺少暂停记录视为正常服务。
	legacy := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 100}},
  "usage": {"u1": {"id": "u1", "customer_id": "c1", "time": "2026-09-15T10:00:00Z", "quantity": 2}},
  "bills": {"c1|2026-09": {
    "id": "BILL-legacy", "customer_id": "c1", "month": "2026-09",
    "total_quantity": 2, "unit_price_fen": 100, "total_fee_fen": 200,
    "lines": [{"usage_id": "u1", "time": "2026-09-15T10:00:00Z", "quantity": 2, "line_fee_fen": 200}],
    "created_at": "2026-10-01T00:00:00Z"
  }}
}`
	if err := os.WriteFile(h.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	// 固定单价客户旧档：查询说明不适用暂停，不按损坏处理。
	if out := h.mustRun("customer", "suspensions", "c1"); !strings.Contains(out, "不适用按月暂停") {
		t.Fatal(out)
	}
}

func TestSuspensionCorruptStateRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2026-12", "第一段")
	h.mustRun("customer", "suspend", "c1", "2026-12", "2027-01", "第二段相接")

	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		old  string
		new  string
	}{
		{
			"引用失效客户",
			"\"c1|2026-11\": {\n      \"customer_id\": \"c1\"",
			"\"c1|2026-11\": {\n      \"customer_id\": \"ghost\"",
		},
		{
			"非法区间（结束月不晚于起月）",
			`"start_month": "2026-11",
      "end_month": "2026-12"`,
			`"start_month": "2026-11",
      "end_month": "2026-11"`,
		},
		{
			"同客户区间重叠（相接改包围）",
			`"end_month": "2026-12",
      "reason": "第一段"`,
			`"end_month": "2027-01",
      "reason": "第一段"`,
		},
	}
	for _, tc := range cases {
		broken := strings.Replace(string(good), tc.old, tc.new, 1)
		if broken == string(good) {
			t.Fatalf("%s：替换未生效", tc.name)
		}
		if err := os.WriteFile(h.statePath(), []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		msg := h.runExpectErr("customer", "suspensions", "c1")
		if !strings.Contains(msg, "损坏") {
			t.Fatalf("%s：未报损坏: %s", tc.name, msg)
		}
		got, _ := os.ReadFile(h.statePath())
		if string(got) != broken {
			t.Fatalf("%s：损坏文件被改写", tc.name)
		}
	}

	// 固定单价客户名下出现暂停区间：损坏。
	fixedOnly := `{
  "version": 1,
  "customers": {"c9": {"id": "c9", "name": "固定客户", "price_fen": 10}},
  "plans": {},
  "usage": {},
  "bills": {},
  "suspensions": {"c9|2026-11": {
    "customer_id": "c9", "start_month": "2026-11", "end_month": "2026-12",
    "reason": "不应存在", "created_at": "2026-10-01T00:00:00Z"
  }}
}`
	if err := os.WriteFile(h.statePath(), []byte(fixedOnly), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("customer", "suspensions", "c9"); !strings.Contains(msg, "损坏") {
		t.Fatalf("固定客户暂停存档未报损坏: %s", msg)
	}
}

func TestSuspensionCorruptStateWithUsageOrBillInRange(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")

	// 场景一：暂停月内存在未封账用量。手工把合法用量状态与暂停区间拼在一起。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-12-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	withSus := strings.Replace(string(good), `"plan_changes": {}`,
		`"plan_changes": {},
  "suspensions": {
    "c1|2026-12": {
      "customer_id": "c1",
      "start_month": "2026-12",
      "end_month": "2027-01",
      "reason": "装修",
      "created_at": "2026-10-01T00:00:00Z"
    }
  }`, 1)
	if withSus == string(good) {
		t.Fatal("注入暂停区间未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(withSus), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := h.runExpectErr("customer", "suspensions", "c1")
	if !strings.Contains(msg, "损坏") || !strings.Contains(msg, "u1") {
		t.Fatalf("暂停月有用量的存档报错异常: %s", msg)
	}

	// 场景二：暂停月内存在已封账账单（无用量、仅月费的账单，避免先撞用量检查）。
	h2 := newHarness(t)
	h2.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h2.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	// 无用量也出账收取月费：得到一张 2026-11 的空明细账单并封账。
	h2.mustRun("bill", "settle", "c1", "2026-11")
	goodBill, err := os.ReadFile(h2.statePath())
	if err != nil {
		t.Fatal(err)
	}
	withSusBill := strings.Replace(string(goodBill), `"plan_changes": {}`,
		`"plan_changes": {},
  "suspensions": {
    "c1|2026-11": {
      "customer_id": "c1",
      "start_month": "2026-11",
      "end_month": "2026-12",
      "reason": "装修",
      "created_at": "2026-10-01T00:00:00Z"
    }
  }`, 1)
	if withSusBill == string(goodBill) {
		t.Fatal("注入暂停区间未生效")
	}
	if err := os.WriteFile(h2.statePath(), []byte(withSusBill), 0o644); err != nil {
		t.Fatal(err)
	}
	msg = h2.runExpectErr("bill", "show", "c1", "2026-11")
	if !strings.Contains(msg, "损坏") || !strings.Contains(msg, "账单") {
		t.Fatalf("暂停月有账单的存档报错异常: %s", msg)
	}
}

func TestHelpListsSuspensionCommands(t *testing.T) {
	h := newHarness(t)
	out, err := h.run("--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"customer suspend <客户标识> <起月 YYYY-MM> <结束月 YYYY-MM> <原因>",
		"customer suspensions <客户标识> [YYYY-MM]",
		"按月暂停",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("帮助缺少 %q", want)
		}
	}
}
