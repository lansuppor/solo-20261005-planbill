package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// --- 暂停按月提前恢复（customer resume）测试 ---

func TestResumeHappyPathAndQuery(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "店面装修暂停营业")

	out := h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工")
	for _, want := range []string{
		"已登记暂停提前恢复",
		"原暂停区间：2026-11（含）至 2027-03（不含", "原暂停原因：店面装修暂停营业",
		"提前恢复：自 2027-01（含", "恢复原因：提前复工",
		"当前有效区间：2026-11（含）至 2027-01（不含",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("提前恢复输出缺少 %q:\n%s", want, out)
		}
	}

	// 列表展示原区间、提前恢复信息与当前有效区间。
	out = h.mustRun("customer", "suspensions", "c1")
	for _, want := range []string{
		"1. 2026-11（含）至 2027-03（不含），原因：店面装修暂停营业",
		"提前恢复：自 2027-01（含", "恢复原因：提前复工",
		"当前有效区间：2026-11（含）至 2027-01（不含",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("暂停列表缺少 %q:\n%s", want, out)
		}
	}

	// 月份状态按当前有效区间：恢复月起不再暂停，原结束月照常恢复。
	for month, suspended := range map[string]bool{
		"2026-10": false,
		"2026-11": true,  // 仍暂停
		"2026-12": true,  // 仍暂停
		"2027-01": false, // 提前恢复释放
		"2027-02": false, // 提前恢复释放（原本也在暂停原区间内）
		"2027-03": false,
	} {
		out = h.mustRun("customer", "suspensions", "c1", month)
		got := strings.Contains(out, "已暂停")
		if got != suspended {
			t.Fatalf("月份 %s 暂停状态=%v，期望 %v:\n%s", month, got, suspended, out)
		}
		if !suspended {
			if !strings.Contains(out, "未暂停") {
				t.Fatalf("月份 %s 未说明未暂停:\n%s", month, out)
			}
		}
	}
	// 释放月明确说明来自提前恢复。
	if out := h.mustRun("customer", "suspensions", "c1", "2027-01"); !strings.Contains(out, "已登记提前恢复") {
		t.Fatalf("恢复月状态说明异常:\n%s", out)
	}
}

func TestResumeValidation(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "add", "c9", "固定客户", "10")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")

	h.runExpectErr("customer", "resume", "c1", "2026-13", "2026-12", "原因") // 原起月非法
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-3", "原因")  // 恢复月格式错
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2026-11", "原因") // 恢复月=原起月
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-03", "原因") // 恢复月=原结束月
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-04", "原因") // 恢复月晚于原结束月
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-01", "  ") // 空原因
	h.runExpectErr("customer", "resume", "ghost", "2026-11", "2027-01", "原因")
	if msg := h.runExpectErr("customer", "resume", "c9", "2026-11", "2027-01", "原因"); !strings.Contains(msg, "固定单价") {
		t.Fatal(msg)
	}
	// 目标暂停必须存在。
	if msg := h.runExpectErr("customer", "resume", "c1", "2026-12", "2027-01", "原因"); !strings.Contains(msg, "暂停不存在") {
		t.Fatal(msg)
	}
	// 参数数量错误是用法错误（退出码 2）。
	for _, args := range [][]string{
		{"customer", "resume", "c1", "2026-11", "2027-01"},
		{"customer", "resume", "c1", "2026-11"},
	} {
		_, err := h.run(args...)
		var ue usageErrorf
		if !errors.As(err, &ue) {
			t.Fatalf("args=%v 应为用法错误(2)，得到 %v", args, err)
		}
	}
	// 全部失败均不占用恢复登记，合法请求可原样重试成功。
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工")

	// 每项暂停只能登记一次：恢复月不同或原因不同均拒绝，不可改写或撤销。
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-02", "提前复工")
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-01", "其他原因")
}

func TestResumeReplayNoWriteAndDoesNotExtend(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 相同恢复月与原因重放：返回保存信息与当前有效区间、不写盘。
	out := h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工")
	if !strings.Contains(out, "内容相同，返回保存信息与当前有效区间（不写盘）") ||
		!strings.Contains(out, "当前有效区间：2026-11（含）至 2027-01（不含") {
		t.Fatalf("恢复重放输出异常:\n%s", out)
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("恢复相同重放改写了存档")
	}
	// 原 customer suspend 仍按原结束月与原因判重：相同重放成功、不写盘，
	// 且不能延长有效暂停（2027-01 仍为恢复月）。
	out = h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	if !strings.Contains(out, "内容相同，返回原记录") {
		t.Fatalf("暂停重放异常:\n%s", out)
	}
	after, _ = os.ReadFile(h.statePath())
	if string(after) != string(before) {
		t.Fatal("暂停相同重放在提前恢复后改写了存档")
	}
	if out := h.mustRun("customer", "suspensions", "c1", "2027-01"); strings.Contains(out, "已暂停") {
		t.Fatalf("相同重放延长了有效暂停:\n%s", out)
	}

	// 后续封账、方案变更、新增暂停都不阻止两类相同重放，也不撤销后来的暂停。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2027-01-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("plan", "add-fee", "new", "新订阅", "2000", "-:2")
	h.mustRun("plan", "change", "c1", "2027-01", "new", "续期换方案")
	h.mustRun("bill", "settle", "c1", "2027-01")
	// 释放月之后再登记一段新暂停（与缩短后的有效区间相接）。
	h.mustRun("customer", "suspend", "c1", "2027-02", "2027-03", "恢复后再暂停")

	out = h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工")
	if !strings.Contains(out, "不写盘") {
		t.Fatalf("封账与新增暂停后恢复重放失败:\n%s", out)
	}
	out = h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	if !strings.Contains(out, "内容相同，返回原记录") {
		t.Fatalf("封账与新增暂停后暂停重放失败:\n%s", out)
	}
	// 后来的新暂停仍然独立生效，重放不撤销它。
	if out := h.mustRun("customer", "suspensions", "c1", "2027-02"); !strings.Contains(out, "已暂停") {
		t.Fatalf("后来的暂停被重放撤销:\n%s", out)
	}
}

func TestResumeReleasesMonthForUsageAndSettle(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "old", "旧订阅", "1000", "100:10", "-:5")
	h.mustRun("plan", "add-fee", "new", "新订阅", "2000", "-:2")
	h.mustRun("customer", "add-plan", "c1", "客户一", "old")

	// 其他月份已封账不妨碍提前恢复登记。
	f0 := h.writeFile("u10.csv", csvHeader+"u10,c1,2026-10-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f0)
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	// 恢复月起采用新方案（方案变更限制不变）。
	h.mustRun("plan", "change", "c1", "2027-01", "new", "续期换方案")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工")

	// 释放的恢复月可导入用量；仍暂停的月份拒绝导入（整批失败）。
	f := h.writeFile("u01.csv", csvHeader+"u01,c1,2027-01-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)
	// 释放月允许接收更正用量（原记录所在月未封账、未暂停）：60 → 70。
	h.mustRun("usage", "correct", "u01", "u01-fix", "c1", "2027-01-06T10:00:00Z", "70", "登记更正")
	fbad := h.writeFile("ubad.csv", csvHeader+
		"ubad,c1,2026-11-05T00:00:00Z,1\n"+
		"u02,c1,2027-02-05T00:00:00Z,1\n") // 2026-11 仍暂停，整批失败
	if msg := h.runExpectErr("usage", "import", fbad); !strings.Contains(msg, "2026-11 处于暂停区间") {
		t.Fatal(msg)
	}

	// 恢复月结算：按当月有效方案收取一次整月月费 2000，用量从零累计分档，
	// 更正后 70 在无上限档（新方案单价 2）= 140，总额 2140；
	// 不补收仍暂停月份的月费。
	out := h.mustRun("bill", "settle", "c1", "2027-01")
	for _, want := range []string{
		"方案：new（新订阅）", "月费：2000 分", "用量费：140 分", "原总金额：2140 分",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("恢复月结算缺少 %q:\n%s", want, out)
		}
	}
	// 已封账账单、用量与账后余额不因提前恢复改变：历史账单保持。
	show := h.mustRun("bill", "show", "c1", "2026-10")
	if !strings.Contains(show, "原总金额：1600 分") {
		t.Fatalf("历史账单被改动:\n%s", show)
	}
	// 仍暂停月份不可结算（单笔与批量），批量整批失败不留账单。
	h.runExpectErr("bill", "settle", "c1", "2026-11")
	h.runExpectErr("bill", "settle", "c1", "2026-12")
	msg := h.runExpectErr("bill", "settle-batch", "c1", "2026-12", "c1", "2027-02")
	if !strings.Contains(msg, "整批未生效") || !strings.Contains(msg, "暂停区间") {
		t.Fatalf("恢复后批量暂停月报错异常:\n%s", msg)
	}
}

func TestResumeReleasedMonthFeeOnlyBill(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2026-12", "提前复工")

	// 释放月无用量也按有效方案收取一次整月月费并封账。
	out := h.mustRun("bill", "settle", "c1", "2026-12")
	for _, want := range []string{"月费：1000 分", "用量费：0 分", "原总金额：1000 分", "明细：无"} {
		if !strings.Contains(out, want) {
			t.Fatalf("释放月仅月费账单缺少 %q:\n%s", want, out)
		}
	}
	// 仍暂停月份不补收月费。
	h.runExpectErr("bill", "settle", "c1", "2026-11")
}

func TestResumeNewSuspensionInReleasedMonth(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	// 原暂停 2026-11..2027-03，提前恢复到 2027-01：有效区间缩短为 [11,01)。
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "第一段装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工")

	// 与缩短后的有效区间重叠仍拒绝；仅缩短目标区间，其他暂停独立生效。
	if msg := h.runExpectErr("customer", "suspend", "c1", "2026-12", "2027-02", "重叠"); !strings.Contains(msg, "重叠") {
		t.Fatal(msg)
	}
	// 释放月起另登记暂停：与当前有效区间相接，合法。
	h.mustRun("customer", "suspend", "c1", "2027-01", "2027-02", "恢复后再暂停")
	out := h.mustRun("customer", "suspensions", "c1")
	if !strings.Contains(out, "1. 2026-11（含）至 2027-03（不含），原因：第一段装修") ||
		!strings.Contains(out, "2. 2027-01（含）至 2027-02（不含），原因：恢复后再暂停") ||
		!strings.Contains(out, "当前有效区间：2026-11（含）至 2027-01（不含") {
		t.Fatalf("释放月新暂停展示异常:\n%s", out)
	}
	// 月份实际状态：12 仍暂停（第一项有效区间），01 被新暂停覆盖，02 正常服务。
	for month, suspended := range map[string]bool{
		"2026-11": true, "2026-12": true, "2027-01": true, "2027-02": false,
	} {
		if out := h.mustRun("customer", "suspensions", "c1", month); strings.Contains(out, "已暂停") != suspended {
			t.Fatalf("月份 %s 状态异常（期望暂停=%v）:\n%s", month, suspended, out)
		}
	}
}

func TestResumeDoesNotConsumeSeq(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-10-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2026-12", "提前复工")
	h.mustRun("bill", "adjust", "c1", "2026-10", "adj-1", "5", "补收")
	ledger := h.mustRun("bill", "ledger", "c1", "2026-10")
	if !strings.Contains(ledger, "存档全局序号上限：1") || !strings.Contains(ledger, "序号 1 调整 adj-1") {
		t.Fatalf("提前恢复占用了操作序号:\n%s", ledger)
	}
}

func TestResumePersistenceAcrossRestart(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工")

	// 跨进程保持有效区间、判重与释放月规则。
	if out := h.mustRun("customer", "suspensions", "c1", "2026-12"); !strings.Contains(out, "已暂停") {
		t.Fatalf("重启后有效区间丢失:\n%s", out)
	}
	if out := h.mustRun("customer", "suspensions", "c1", "2027-01"); strings.Contains(out, "已暂停") {
		t.Fatalf("重启后恢复月仍暂停:\n%s", out)
	}
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工") // 幂等不写盘
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2026-11-05T00:00:00Z,3\n")
	if msg := h.runExpectErr("usage", "import", f); !strings.Contains(msg, "暂停区间") {
		t.Fatalf("重启后仍暂停月份限制丢失:\n%s", msg)
	}
	f2 := h.writeFile("u2.csv", csvHeader+"u2,c1,2027-01-05T00:00:00Z,3\n")
	if out := h.mustRun("usage", "import", f2); !strings.Contains(out, "新增 1 条") {
		t.Fatalf("重启后释放月不能导入:\n%s", out)
	}
}

func TestResumeOldStateFileWithoutResumesKeepsOriginalInterval(t *testing.T) {
	h := newHarness(t)
	// 手工构造有暂停区间但无 suspension_resumes 字段的旧版数据文件：
	// 缺少恢复记录时暂停沿用原区间。
	legacy := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 0, "plan_id": "p1"}},
  "plans": {"p1": {"id": "p1", "name": "阶梯", "tiers": [{"limit": 0, "price_fen": 10}]}},
  "usage": {},
  "bills": {},
  "suspensions": {"c1|2026-11": {
    "customer_id": "c1", "start_month": "2026-11", "end_month": "2027-01",
    "reason": "装修", "created_at": "2026-10-01T00:00:00Z"
  }}
}`
	if err := os.WriteFile(h.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("customer", "suspensions", "c1", "2026-12")
	if !strings.Contains(out, "已暂停") {
		t.Fatalf("旧存档未沿用原暂停区间:\n%s", out)
	}
}

func TestResumeCorruptStateRejected(t *testing.T) {
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工")
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
			"恢复月越界（等于原结束月）",
			`"resume_month": "2027-01"`,
			`"resume_month": "2027-03"`,
		},
		{
			"恢复原因为空",
			`"reason": "提前复工"`,
			`"reason": "   "`,
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

	// 恢复引用缺失：只有恢复记录、没有目标暂停。
	missingTarget := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 0, "plan_id": "p1"}},
  "plans": {"p1": {"id": "p1", "name": "阶梯", "tiers": [{"limit": 0, "price_fen": 10}]}},
  "usage": {},
  "bills": {},
  "suspension_resumes": {"c1|2026-11": {
    "customer_id": "c1", "start_month": "2026-11", "resume_month": "2026-12",
    "reason": "提前复工", "created_at": "2026-10-01T00:00:00Z"
  }}
}`
	if err := os.WriteFile(h.statePath(), []byte(missingTarget), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("customer", "suspensions", "c1"); !strings.Contains(msg, "损坏") || !strings.Contains(msg, "恢复目标缺失") {
		t.Fatalf("恢复引用缺失未按损坏拒绝: %s", msg)
	}

	// 有效区间重叠：第一项有效区间 [11,01)，把相接的第二项起月改为 2026-12。
	h2 := newHarness(t)
	h2.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h2.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	h2.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "第一段")
	h2.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复工")
	h2.mustRun("customer", "suspend", "c1", "2027-01", "2027-02", "第二段相接")
	good2, err := os.ReadFile(h2.statePath())
	if err != nil {
		t.Fatal(err)
	}
	broken2 := strings.ReplaceAll(string(good2), `"c1|2027-01"`, `"c1|2026-12"`)
	broken2 = strings.ReplaceAll(broken2, `"start_month": "2027-01"`, `"start_month": "2026-12"`)
	if broken2 == string(good2) {
		t.Fatal("有效区间重叠用例替换未生效")
	}
	if err := os.WriteFile(h2.statePath(), []byte(broken2), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h2.runExpectErr("customer", "suspensions", "c1"); !strings.Contains(msg, "损坏") || !strings.Contains(msg, "重叠") {
		t.Fatalf("有效区间重叠未按损坏拒绝: %s", msg)
	}
}

func TestResumeCorruptUsageOrBillInEffectiveRange(t *testing.T) {
	// 有效暂停月（恢复月之前）存在未撤回用量：损坏；释放月存在用量：正常。
	h := newHarness(t)
	h.mustRun("plan", "add", "p1", "阶梯", "-:10")
	h.mustRun("customer", "add-plan", "c1", "客户一", "p1")
	f := h.writeFile("u11.csv", csvHeader+"u11,c1,2026-11-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	good, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 暂停原区间 11..03、恢复月 2027-01：有效区间 [11,01) 覆盖 11 月用量。
	withSus := strings.Replace(string(good), `"plan_changes": {}`,
		`"plan_changes": {},
  "suspensions": {
    "c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "end_month": "2027-03",
      "reason": "装修", "created_at": "2026-10-01T00:00:00Z"
    }
  },
  "suspension_resumes": {
    "c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "resume_month": "2027-01",
      "reason": "提前复工", "created_at": "2026-10-02T00:00:00Z"
    }
  }`, 1)
	if withSus == string(good) {
		t.Fatal("注入暂停与恢复未生效")
	}
	if err := os.WriteFile(h.statePath(), []byte(withSus), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := h.runExpectErr("customer", "suspensions", "c1"); !strings.Contains(msg, "损坏") || !strings.Contains(msg, "u11") {
		t.Fatalf("有效暂停月存在用量未报损坏: %s", msg)
	}

	// 同样的释放月（恢复月及以后）存在已封账账单：合法，不按损坏处理。
	h2 := newHarness(t)
	h2.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "-:10")
	h2.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	// 释放月 2026-12 出仅月费账单并封账；11 月无账单。
	h2.mustRun("bill", "settle", "c1", "2026-12")
	goodBill, err := os.ReadFile(h2.statePath())
	if err != nil {
		t.Fatal(err)
	}
	withResume := strings.Replace(string(goodBill), `"plan_changes": {}`,
		`"plan_changes": {},
  "suspensions": {
    "c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "end_month": "2027-03",
      "reason": "装修", "created_at": "2026-10-01T00:00:00Z"
    }
  },
  "suspension_resumes": {
    "c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "resume_month": "2026-12",
      "reason": "提前复工", "created_at": "2026-10-02T00:00:00Z"
    }
  }`, 1)
	if withResume == string(goodBill) {
		t.Fatal("注入暂停与恢复未生效")
	}
	if err := os.WriteFile(h2.statePath(), []byte(withResume), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h2.mustRun("bill", "show", "c1", "2026-12")
	if !strings.Contains(out, "原总金额：1000 分") {
		t.Fatalf("释放月账单读取异常:\n%s", out)
	}
	if out := h2.mustRun("customer", "suspensions", "c1", "2026-12"); strings.Contains(out, "已暂停") {
		t.Fatalf("释放月被误判暂停:\n%s", out)
	}
	if msg := h2.runExpectErr("bill", "show", "c1", "2026-11"); !strings.Contains(msg, "尚无账单") {
		t.Fatalf("仍暂停月份不应存在账单: %s", msg)
	}
}
