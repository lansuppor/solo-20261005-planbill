package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// --- 按月提前恢复（customer resume）测试 ---

// setupResumeCustomer 登记一个含月费的阶梯方案与阶梯客户，返回 harness。
func setupResumeCustomer(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.mustRun("plan", "add-fee", "sub", "订阅阶梯", "1000", "100:10", "-:5")
	h.mustRun("customer", "add-plan", "c1", "客户一", "sub")
	return h
}

func TestResumeHappyPathAndEffectiveRange(t *testing.T) {
	h := setupResumeCustomer(t)
	// 暂停 2026-11、12，2027-01、02（原结束月 2027-03，不含）。
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "店面装修暂停营业")
	out := h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "装修完成提前复业")
	for _, want := range []string{
		"已登记提前恢复",
		"暂停区间：2026-11（含）至 2027-03（不含", // 原区间永久保留
		"原因：店面装修暂停营业",                 // 原原因永久保留
		"提前恢复：2027-01（含）起恢复服务",
		"原因：装修完成提前复业",
		"当前有效区间：2026-11（含）至 2027-01（不含",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("提前恢复登记输出缺少 %q:\n%s", want, out)
		}
	}

	// 查询：展示原区间、提前恢复信息与当前有效区间。
	out = h.mustRun("customer", "suspensions", "c1")
	for _, want := range []string{
		"2026-11（含）至 2027-03（不含），原因：店面装修暂停营业",
		"提前恢复：2027-01（含）起恢复服务（早于原结束月 2027-03）",
		"当前有效区间 2026-11（含）至 2027-01（不含）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("暂停查询输出缺少 %q:\n%s", want, out)
		}
	}

	// 给定月份按当前有效区间判定：11、12 仍暂停；01（恢复月）、02 已释放。
	for month, suspended := range map[string]bool{
		"2026-10": false,
		"2026-11": true,
		"2026-12": true,
		"2027-01": false, // 恢复月（含）起恢复服务
		"2027-02": false,
		"2027-03": false, // 原结束月原本即正常
	} {
		out = h.mustRun("customer", "suspensions", "c1", month)
		got := strings.Contains(out, "已暂停")
		if got != suspended {
			t.Fatalf("恢复后月份 %s 暂停状态=%v，期望 %v:\n%s", month, got, suspended, out)
		}
	}
	// 仍暂停月份说明处于当前有效区间；释放月份说明正常服务。
	if out := h.mustRun("customer", "suspensions", "c1", "2026-12"); !strings.Contains(out, "当前有效区间") {
		t.Fatalf("仍暂停月份说明异常:\n%s", out)
	}
	if out := h.mustRun("customer", "suspensions", "c1", "2027-01"); !strings.Contains(out, "未暂停") {
		t.Fatalf("恢复月说明异常:\n%s", out)
	}
}

func TestResumeValidation(t *testing.T) {
	h := setupResumeCustomer(t)
	h.mustRun("customer", "add", "c9", "固定客户", "10")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")

	h.runExpectErr("customer", "resume", "c1", "2026-13", "2027-01", "原因") // 原起月非法
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-13", "原因") // 恢复月非法
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2026-11", "原因") // 恢复月=原起月
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-03", "原因") // 恢复月=原结束月
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2026-10", "原因") // 恢复月早于原起月
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-04", "原因") // 恢复月晚于原结束月
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-01", "  ") // 空原因
	h.runExpectErr("customer", "resume", "ghost", "2026-11", "2027-01", "原因")
	// 目标暂停缺失（阶梯客户但无该原起月的暂停）。
	if msg := h.runExpectErr("customer", "resume", "c1", "2026-09", "2026-10", "原因"); !strings.Contains(msg, "暂停不存在") {
		t.Fatalf("目标缺失报错异常: %s", msg)
	}
	// 仅限阶梯客户。
	if msg := h.runExpectErr("customer", "resume", "c9", "2026-11", "2026-12", "原因"); !strings.Contains(msg, "固定单价") {
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
	// 全部失败后该客户该原起月的恢复登记未被占用，可原样登记成功。
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "装修完成")
}

func TestResumeIdempotentReplayNoWriteAndConflict(t *testing.T) {
	h := setupResumeCustomer(t)
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")

	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	// 相同恢复月与原因重放：返回保存信息与当前有效区间，不写盘。
	out := h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")
	for _, want := range []string{"内容相同，返回保存信息与当前有效区间", "当前有效区间：2026-11（含）至 2027-01（不含"} {
		if !strings.Contains(out, want) {
			t.Fatalf("恢复重放输出缺少 %q:\n%s", want, out)
		}
	}
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("相同恢复重放改写了存档")
	}
	// 恢复月不同 / 原因不同均拒绝，恢复记录不可改写。
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-02", "提前复业")
	h.runExpectErr("customer", "resume", "c1", "2026-11", "2027-01", "其他原因")

	// 后续封账、方案变更、新增暂停都不阻止相同重放，也不撤销后来的暂停。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2027-01-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2027-01")              // 恢复月已封账
	h.mustRun("plan", "change", "c1", "2027-02", "sub", "续期") // 同方案变更
	h.mustRun("customer", "suspend", "c1", "2027-02", "2027-03", "释放月又暂停")
	out = h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")
	if !strings.Contains(out, "内容相同，返回保存信息与当前有效区间") {
		t.Fatalf("后续变化后恢复重放失败:\n%s", out)
	}
	// 后来新增的暂停不被撤销：2027-02 仍暂停。
	if out := h.mustRun("customer", "suspensions", "c1", "2027-02"); !strings.Contains(out, "已暂停") {
		t.Fatalf("后来的暂停被恢复重放撤销:\n%s", out)
	}
	// 原 customer suspend 仍按原结束月与原原因判重，相同重放成功且不延长有效暂停。
	out = h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	if !strings.Contains(out, "内容相同，返回原记录") {
		t.Fatalf("原暂停重放失败:\n%s", out)
	}
	if out := h.mustRun("customer", "suspensions", "c1", "2027-01"); strings.Contains(out, "已暂停") {
		t.Fatalf("原暂停重放延长了有效暂停:\n%s", out)
	}
}

func TestResumeReleasedMonthUsageAndSettle(t *testing.T) {
	h := setupResumeCustomer(t)
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")

	// 释放月（2027-01）可导入用量；仍暂停月（2026-12）导入被整批拒绝。
	fRel := h.writeFile("rel.csv", csvHeader+
		"u1,c1,2027-01-05T00:00:00Z,60\n"+
		"u2,c1,2026-12-05T00:00:00Z,2\n")
	msg := h.runExpectErr("usage", "import", fRel)
	if !strings.Contains(msg, "2026-12 处于暂停区间") || !strings.Contains(msg, "整批未生效") {
		t.Fatalf("仍暂停月导入报错异常: %s", msg)
	}
	fOK := h.writeFile("ok.csv", csvHeader+"u1,c1,2027-01-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", fOK)

	// 单笔结算释放月：按当月有效方案收一次整月月费 1000，用量从零累计
	// （60 落第 1 档 60×10=600），原总金额 1600。
	out := h.mustRun("bill", "settle", "c1", "2027-01")
	for _, want := range []string{"月费：1000 分", "用量费：600 分", "原总金额：1600 分"} {
		if !strings.Contains(out, want) {
			t.Fatalf("释放月结算缺少 %q:\n%s", want, out)
		}
	}
	// 仍暂停月份不补收月费：不能结算、不封账、无账单。
	h.runExpectErr("bill", "settle", "c1", "2026-12")
	h.runExpectErr("bill", "show", "c1", "2026-12")

	// 批量清单混入仍暂停月：整批失败，释放月也不留账单（u3 已导入但不封账）。
	f2 := h.writeFile("u2.csv", csvHeader+"u3,c1,2027-02-05T00:00:00Z,10\n")
	h.mustRun("usage", "import", f2)
	msg = h.runExpectErr("bill", "settle-batch", "c1", "2027-02", "c1", "2026-12")
	if !strings.Contains(msg, "整批未生效") || !strings.Contains(msg, "暂停区间") {
		t.Fatalf("批量混入暂停月报错异常: %s", msg)
	}
	h.runExpectErr("bill", "show", "c1", "2027-02")
	// 去除暂停月后批量成功：u3 仍在（整批结算失败不删用量），2027-02 出账，
	// 月费 1000 + 10×10=100 = 1100。
	out = h.mustRun("bill", "settle-batch", "c1", "2027-02")
	if !strings.Contains(out, "新增账单 1 张") || !strings.Contains(out, "1100 分") {
		t.Fatalf("释放月批量结算异常:\n%s", out)
	}
}

func TestResumeCorrectUsageIntoReleasedMonth(t *testing.T) {
	h := setupResumeCustomer(t)
	// 更正前先在未封账、未暂停的 2026-10 放一条错误用量。
	f := h.writeFile("u.csv", csvHeader+"u-wrong,c1,2026-10-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f)
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")

	// 更正到仍暂停月（2026-12）拒绝；更正到释放月（2027-01）成功。
	h.runExpectErr("usage", "correct", "u-wrong", "u-fix1", "c1", "2026-12-05T00:00:00Z", "5", "时间更正")
	h.mustRun("usage", "correct", "u-wrong", "u-fix", "c1", "2027-01-05T00:00:00Z", "5", "时间更正")
	out := h.mustRun("bill", "settle", "c1", "2027-01")
	// 月费 1000 + 5×10=50 = 1050。
	if !strings.Contains(out, "原总金额：1050 分") {
		t.Fatalf("释放月更正用量结算异常:\n%s", out)
	}
}

func TestResumeReleasedMonthNewSuspension(t *testing.T) {
	h := setupResumeCustomer(t)
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "第一段")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")

	// 与目标仍生效部分 [11,01) 重叠：拒绝。
	if msg := h.runExpectErr("customer", "suspend", "c1", "2026-12", "2027-02", "重叠仍暂停部分"); !strings.Contains(msg, "重叠") {
		t.Fatal(msg)
	}
	// 相接于有效区间结束月 2027-01：合法（虽与原区间 [11,03) 历史重叠）。
	h.mustRun("customer", "suspend", "c1", "2027-01", "2027-02", "释放月再暂停（相接）")
	if out := h.mustRun("customer", "suspensions", "c1", "2027-01"); !strings.Contains(out, "已暂停") {
		t.Fatalf("新暂停未生效:\n%s", out)
	}
	// 目标有效区间与新暂停独立：2026-11 仍由第一项暂停，2027-01 由第二项暂停。
	out := h.mustRun("customer", "suspensions", "c1")
	if !strings.Contains(out, "1. 2026-11（含）至 2027-03（不含），原因：第一段") ||
		!strings.Contains(out, "2. 2027-01（含）至 2027-02（不含），原因：释放月再暂停（相接）") ||
		!strings.Contains(out, "当前有效区间 2026-11（含）至 2027-01（不含）") {
		t.Fatalf("两项暂停展示异常:\n%s", out)
	}

	// 释放月另登记暂停仍守封账限制：结算并封账 2027-02 后不能再起暂停于该月。
	h.mustRun("bill", "settle", "c1", "2027-02") // 正月费无用量也出账
	if msg := h.runExpectErr("customer", "suspend", "c1", "2027-02", "2027-03", "封账月暂停"); !strings.Contains(msg, "已封账") {
		t.Fatal(msg)
	}
}

func TestResumeOtherSuspensionIndependent(t *testing.T) {
	h := setupResumeCustomer(t)
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "第一段")
	h.mustRun("customer", "suspend", "c1", "2027-03", "2027-05", "第二段相接")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")
	// 仅缩短目标区间：01、02 释放；第二段 03、04 仍暂停。
	for month, suspended := range map[string]bool{
		"2026-11": true, "2026-12": true,
		"2027-01": false, "2027-02": false,
		"2027-03": true, "2027-04": true, "2027-05": false,
	} {
		out := h.mustRun("customer", "suspensions", "c1", month)
		if strings.Contains(out, "已暂停") != suspended {
			t.Fatalf("月份 %s 状态异常（期望暂停=%v）:\n%s", month, suspended, out)
		}
	}
}

func TestResumeSealedMonthsAndBillsDoNotBlock(t *testing.T) {
	h := setupResumeCustomer(t)
	// 暂停前封账 2026-10（正月费无用量账单）；原结束月 2027-03 也可先封账。
	h.mustRun("bill", "settle", "c1", "2026-10")
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("bill", "settle", "c1", "2027-03") // 原结束月正常服务，正月费出账
	// 其他月份已封账不妨碍提前恢复登记。
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")
	// 释放月封账后，已有账单不变；再登记一笔调整，确认恢复不占账后序号。
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2027-01-05T00:00:00Z,60\n")
	h.mustRun("usage", "import", f)
	h.mustRun("bill", "settle", "c1", "2027-01")
	h.mustRun("bill", "adjust", "c1", "2027-01", "adj-1", "7", "补收")
	ledger := h.mustRun("bill", "ledger", "c1", "2027-01")
	// 2026-10 与 2027-03 的账单不含账后事件；2027-01 只有一笔调整，序号 1。
	if !strings.Contains(ledger, "存档全局序号上限：1") || !strings.Contains(ledger, "序号 1 调整 adj-1") {
		t.Fatalf("提前恢复占用了操作序号:\n%s", ledger)
	}
	// 封账月的原账单不被恢复改动。
	if out := h.mustRun("bill", "show", "c1", "2026-10"); !strings.Contains(out, "原总金额：1000 分") {
		t.Fatalf("已有账单被改动:\n%s", out)
	}
}

func TestResumePersistenceAcrossRestart(t *testing.T) {
	h := setupResumeCustomer(t)
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")

	// 跨进程（每次 run 重新载入）：有效区间、释放月限制与幂等保持。
	if out := h.mustRun("customer", "suspensions", "c1", "2026-12"); !strings.Contains(out, "已暂停") {
		t.Fatalf("重启后有效区间丢失:\n%s", out)
	}
	if out := h.mustRun("customer", "suspensions", "c1", "2027-01"); strings.Contains(out, "已暂停") {
		t.Fatalf("重启后恢复月仍暂停:\n%s", out)
	}
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业") // 幂等
	f := h.writeFile("u.csv", csvHeader+"u1,c1,2027-01-05T00:00:00Z,3\n")
	h.mustRun("usage", "import", f) // 释放月可导入
	fBad := h.writeFile("ub.csv", csvHeader+"u2,c1,2026-12-05T00:00:00Z,3\n")
	if msg := h.runExpectErr("usage", "import", fBad); !strings.Contains(msg, "暂停区间") {
		t.Fatalf("重启后仍暂停月限制丢失: %s", msg)
	}
}

func TestResumeQueryNeverWrites(t *testing.T) {
	h := setupResumeCustomer(t)
	h.mustRun("customer", "suspend", "c1", "2026-11", "2027-03", "装修")
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")
	before, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	h.mustRun("customer", "suspensions", "c1")
	h.mustRun("customer", "suspensions", "c1", "2027-01")
	h.runExpectErr("customer", "suspensions", "ghost")
	after, err := os.ReadFile(h.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("暂停查询改写了存档")
	}
}

func TestResumeOldStateFileWithoutField(t *testing.T) {
	h := newHarness(t)
	// 含暂停但无 suspension_resumes 字段的旧档：沿用原区间。
	legacy := `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 0, "plan_id": "p1"}},
  "plans": {"p1": {"id": "p1", "name": "阶梯", "tiers": [{"limit": 0, "price_fen": 10}]}},
  "usage": {},
  "bills": {},
  "plan_changes": {},
  "suspensions": {"c1|2026-11": {
    "customer_id": "c1", "start_month": "2026-11", "end_month": "2027-03",
    "reason": "装修", "created_at": "2026-10-01T00:00:00Z"
  }}
}`
	if err := os.WriteFile(h.statePath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	// 旧档沿用原区间：2027-01 在无恢复记录时仍暂停，且可在旧档上补登记恢复。
	if out := h.mustRun("customer", "suspensions", "c1", "2027-01"); !strings.Contains(out, "已暂停") {
		t.Fatalf("旧档未沿用原区间:\n%s", out)
	}
	h.mustRun("customer", "resume", "c1", "2026-11", "2027-01", "提前复业")
	if out := h.mustRun("customer", "suspensions", "c1", "2027-01"); strings.Contains(out, "已暂停") {
		t.Fatalf("旧档补登记恢复后仍暂停:\n%s", out)
	}
}

// resumeCorruptBase 构造用于载入损坏测试的最小数据文件内容。
func resumeCorruptBase(suspensions, resumes, usage, bills string) string {
	return `{
  "version": 1,
  "customers": {"c1": {"id": "c1", "name": "甲方", "price_fen": 0, "plan_id": "p1"}},
  "plans": {"p1": {"id": "p1", "name": "阶梯", "monthly_fee_fen": 1000, "tiers": [{"limit": 0, "price_fen": 10}]}},
  "usage": ` + usage + `,
  "bills": ` + bills + `,
  "plan_changes": {},
  "suspensions": ` + suspensions + `,
  "suspension_resumes": ` + resumes + `
}`
}

func TestResumeCorruptStateRejected(t *testing.T) {
	susOK := `{"c1|2026-11": {
    "customer_id": "c1", "start_month": "2026-11", "end_month": "2027-03",
    "reason": "装修", "created_at": "2026-10-01T00:00:00Z"
  }}`
	resumeOK := `{"c1|2026-11": {
    "customer_id": "c1", "start_month": "2026-11", "resume_month": "2027-01",
    "reason": "提前复业", "created_at": "2026-10-02T00:00:00Z"
  }}`
	emptyUsage := `{}`
	emptyBills := `{}`

	cases := []struct {
		name        string
		suspensions string
		resumes     string
		usage       string
		bills       string
		want        string
	}{
		{
			"恢复引用缺失",
			`{}`, resumeOK, emptyUsage, emptyBills, "恢复目标缺失",
		},
		{
			"恢复月等于原结束月（越界）",
			susOK,
			`{"c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "resume_month": "2027-03",
      "reason": "提前复业", "created_at": "2026-10-02T00:00:00Z"
    }}`,
			emptyUsage, emptyBills, "越界",
		},
		{
			"恢复月等于原起月（越界）",
			susOK,
			`{"c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "resume_month": "2026-11",
      "reason": "提前复业", "created_at": "2026-10-02T00:00:00Z"
    }}`,
			emptyUsage, emptyBills, "越界",
		},
		{
			"恢复原因为空",
			susOK,
			`{"c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "resume_month": "2027-01",
      "reason": "   ", "created_at": "2026-10-02T00:00:00Z"
    }}`,
			emptyUsage, emptyBills, "原因为空",
		},
		{
			"当前有效区间重叠",
			`{"c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "end_month": "2027-03",
      "reason": "第一段", "created_at": "2026-10-01T00:00:00Z"
    }, "c1|2027-01": {
      "customer_id": "c1", "start_month": "2027-01", "end_month": "2027-05",
      "reason": "第二段", "created_at": "2026-10-01T00:00:00Z"
    }}`,
			`{"c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "resume_month": "2027-02",
      "reason": "提前复业", "created_at": "2026-10-02T00:00:00Z"
    }}`,
			emptyUsage, emptyBills, "重叠",
		},
		{
			"有效暂停月内存在未撤回用量",
			susOK, resumeOK,
			`{"u1": {"id": "u1", "customer_id": "c1", "time": "2026-12-05T00:00:00Z", "quantity": 3}}`,
			emptyBills, "u1",
		},
		{
			"有效暂停月内存在账单",
			susOK, resumeOK, emptyUsage,
			`{"c1|2026-12": {
      "id": "BILL-X", "customer_id": "c1", "month": "2026-12",
      "pricing": "tiered", "plan_id": "p1", "plan_name": "阶梯",
      "plan_tiers": [{"limit": 0, "price_fen": 10}],
      "tier_totals": [{"quantity": 0, "fee_fen": 0}],
      "monthly_fee_fen": 1000, "total_quantity": 0, "total_fee_fen": 1000,
      "lines": [], "created_at": "2026-10-03T00:00:00Z"
    }}`,
			"暂停有效区间",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			content := resumeCorruptBase(tc.suspensions, tc.resumes, tc.usage, tc.bills)
			if err := os.WriteFile(h.statePath(), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			msg := h.runExpectErr("customer", "suspensions", "c1")
			if !strings.Contains(msg, "损坏") || !strings.Contains(msg, tc.want) {
				t.Fatalf("%s：报错不含 损坏/%q: %s", tc.name, tc.want, msg)
			}
			got, _ := os.ReadFile(h.statePath())
			if string(got) != content {
				t.Fatalf("%s：损坏文件被改写", tc.name)
			}
		})
	}

	// 对照组：两条原区间历史重叠的暂停，第一项提前恢复到 2027-01 后，两条
	// 当前有效区间 [11,01) 与 [01,05) 恰好相接——无用量、无账单时载入成功。
	h := newHarness(t)
	valid := resumeCorruptBase(
		`{"c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "end_month": "2027-03",
      "reason": "第一段", "created_at": "2026-10-01T00:00:00Z"
    }, "c1|2027-01": {
      "customer_id": "c1", "start_month": "2027-01", "end_month": "2027-05",
      "reason": "第二段", "created_at": "2026-10-01T00:00:00Z"
    }}`,
		`{"c1|2026-11": {
      "customer_id": "c1", "start_month": "2026-11", "resume_month": "2027-01",
      "reason": "提前复业", "created_at": "2026-10-02T00:00:00Z"
    }}`,
		emptyUsage, emptyBills)
	if err := os.WriteFile(h.statePath(), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("customer", "suspensions", "c1")
	if !strings.Contains(out, "当前有效区间 2026-11（含）至 2027-01（不含）") {
		t.Fatalf("有效区间相接的合法存档载入异常:\n%s", out)
	}
}

func TestResumeHelpListsCommand(t *testing.T) {
	h := newHarness(t)
	out, err := h.run("--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"customer resume <客户标识> <原起月 YYYY-MM> <恢复月 YYYY-MM> <原因>",
		"按月提前恢复",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("帮助缺少 %q", want)
		}
	}
}
