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
                          登记客户；单价为非负整数人民币分，创建后不可修改
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
                          收款标识全局唯一（与调整标识允许同名，各自判重），
                          超过未收余额或零应付账单登记正额均拒绝
  bill remit <客户标识> <收款标识> <总额分> <备注> <YYYY-MM> <金额分> [ <YYYY-MM> <金额分> ... ]
                          登记一笔汇款，分配到同一客户多个已结算月份：
                          每项为月份与正整数分，月份不得重复且须已有账单，
                          分配合计须等于总额，每项不得超过对应未收余额；
                          与 bill pay 共用收款标识，单账单收款视为一项分配
  bill unpay <收款标识> <原因>
                          整笔撤销一笔收款的全部实收分配，永久保留原记录、
                          分配与撤销原因，不改变应付、其他收款或调整；
                          相同原因重复撤销幂等，改用其他原因拒绝

说明:
  - 所有数据保存在数据目录的 state.json，原子写入，跨进程持久化，不依赖外部服务。
  - 数量、单价、金额均为有符号 64 位整数（金额单位：分），全程整数运算。
  - 当前应付 = 原总金额 + 未撤销调整净额；实收 = 未撤销收款分配到该月的金额之和；
    未收余额 = 当前应付 - 实收，始终满足 0 ≤ 实收 ≤ 当前应付。
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
