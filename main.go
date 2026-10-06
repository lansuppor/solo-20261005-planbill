package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const appName = "planbill"

const defaultDataDir = "planbill-data"

const helpText = appName + ` — 离线按量月结账单管理

用法:
  go run . [选项]
  go run . [选项] <命令> [命令参数]

选项:
      --data-dir <目录>   本地数据目录（默认: ./` + defaultDataDir + `）
  -h, --help              显示本帮助

命令:
  customer add <标识> <名称> <单价分>
                          登记固定单价客户；单价为非负整数人民币分，
                          创建后不可修改
  customer add-plan <标识> <名称> <方案标识>
                          登记绑定阶梯计费方案的客户；标识和名称规则同上，
                          绑定创建后不可变更，已有客户不得换方案
  plan add <标识> <名称> <上限:单价分>... <-:单价分>
                          登记按月累计用量的阶梯计费方案：至少一档，有限
                          上限为严格递增的正整数，最后一档用 - 表示无上限；
                          单价为非负整数分，可升可降；创建后不可修改
  plan show <标识>        查询阶梯计费方案的完整规则
  plan list               列出全部已登记的阶梯计费方案
  usage import <文件>     整批导入用量记录（CSV，UTF-8；- 表示标准输入）
  bill settle <客户标识> <YYYY-MM>
                          按 UTC 自然月（左闭右开）月结并封账；
                          再次结算同一客户月份返回原账单，不重新计费
  bill show <客户标识> <YYYY-MM>
                          查询账单：稳定账单标识、客户、月份、总数量、
                          单价、总金额、调整净额、当前应付、实收、未收余额、
                          每条用量明细与小计，以及按操作顺序排列的
                          调整/撤销与收款/撤销历史
  bill adjust <客户标识> <YYYY-MM> <调整标识> <金额分> <原因>
                          对已存在账单追加费用调整：正数补收、负数减免，
                          金额为非零整数分；相同标识内容相同幂等返回，
                          内容不同（含跨客户/跨月份复用）拒绝
  bill revoke <调整标识> <原因>
                          撤销一笔调整的金额影响，保留原记录与撤销原因；
                          相同原因重复撤销幂等，改用其他原因拒绝
  bill pay <客户标识> <YYYY-MM> <收款标识> <金额分> <备注>
                          对已存在账单登记一笔实收：正整数分，可分笔；
                          等价于只含一项分配的汇款，与 bill remit 共用
                          全局收款标识
  bill remit <客户标识> <收款标识> <总金额分> <备注> <YYYY-MM:金额分>...
                          登记一笔汇款，按分配列表计入同一客户的多个
                          已结算月份：每项月份不得重复且须已有账单，
                          分配合计须等于总额，每项不得超过对应未收余额，
                          全部合法才整笔生效
  bill correct <收款标识> <更正标识> <原因> <YYYY-MM:金额分>...
                          更正一笔未撤销收款的分配（修正入账月份，不重复
                          收钱）：客户、总额、备注与原账单不变，新列表
                          非空、月份不重复且须已有账单，各项为正整数分、
                          合计等于原总额；以当前最新分配为起点整笔替换，
                          替换后各月仍满足 0 ≤ 实收 ≤ 应付才生效；
                          相同标识按目标收款、原因及新月份-金额关系判重
                          （顺序无关），相同重放返回原更正记录
  bill unpay <收款标识> <原因>
                          整笔撤销一笔收款的全部分配（不接受部分撤销），
                          取消的是撤销时最新分配的实收；永久保留原记录、
                          分配、更正与撤销原因，不改变应付、其他收款或
                          调整；相同原因重复撤销幂等，改用其他原因拒绝
  bill ledger <客户标识> <YYYY-MM> [截止操作序号]
                          已结算账单的账后对账流水（只读，不改写数据）：
                          以原总金额为初始应付、零实收为起点，按全局操作
                          序号合并回放该账单的调整/收款及其撤销，逐事件
                          给出应付、实收、未收余额；省略截止序号表示最新，
                          0 表示仅初始余额，截止包含该序号

说明:
  - 所有数据保存在数据目录的 state.json，原子写入，跨进程持久化，不依赖外部服务。
  - 数量、单价、金额均为有符号 64 位整数（金额单位：分），全程整数运算。
  - 阶梯计费：绑定方案的客户按 UTC 自然月从零累计用量，各档仅对落入本档的
    数量收费；账单保存方案标识、名称与完整规则，展示各档合计与每条用量的
    跨档分段，不伪造统一单价；固定单价客户的计费与展示保持不变。
  - 当前应付 = 原总金额 + 未撤销调整净额；实收 = 未撤销收款按最新分配在该月
    计入之和；未收余额 = 当前应付 - 实收，始终满足 0 ≤ 实收 ≤ 当前应付。
  - 收款标识全局唯一（收款与调整可同名，各自判重与撤销）；按客户、总额、
    备注及月份-金额对应关系判重，分配顺序不影响身份。首次登记内容永久保留，
    分配更正不改变收款身份与判重结果。
  - 更正标识全局唯一，与收款、调整标识相互独立、可同名；更正与收款、调整
    及其撤销共用同一个全局操作序号。
  - 用量文件格式与更多示例见 README.md。
`

// stdout 是全部正常输出的目的地；测试中会临时替换以捕获输出。
var stdout io.Writer = os.Stdout

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, appName+":", err)
		os.Exit(exitCode(err))
	}
}

// exitCode 区分用法错误（2）与业务错误（1），成功为 0。
func exitCode(err error) int {
	var ue usageErrorf
	if errors.As(err, &ue) {
		return 2
	}
	return 1
}

func run(args []string) error {
	if len(args) == 0 {
		// 保留无参数入口：输出应用名与帮助。
		fmt.Fprint(stdout, helpText)
		return nil
	}

	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dataDir := fs.String("data-dir", defaultDataDir, "本地数据目录")
	helpShort := fs.Bool("h", false, "显示帮助")
	helpLong := fs.Bool("help", false, "显示帮助")
	if err := fs.Parse(args); err != nil {
		return usageError("参数错误：%v；用 --help 查看用法", err)
	}
	if *helpShort || *helpLong {
		if fs.NArg() > 0 {
			return usageError("--help 不能与命令同时使用")
		}
		fmt.Fprint(stdout, helpText)
		return nil
	}
	if fs.NArg() == 0 {
		fmt.Fprint(stdout, helpText)
		return nil
	}
	return runCmd(fs.Args(), *dataDir)
}
