package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const helpText = `planbill

Usage: go run . [--help]

订阅与用量账单管理：客户登记、用量导入、单客户按月结算与账单查询。
数据保存在本地数据目录，跨进程持久化，不依赖外部服务。

运行命令时可选用本地数据目录：
  go run . [--data-dir 目录] <命令> [参数...]

全局选项：
  --data-dir 目录   指定本地数据目录（也可用环境变量 PLANBILL_DATA_DIR），
                    默认为当前目录下的 .planbill
  -h, --help        显示本帮助

命令：
  register <客户标识> <客户名称> <单价(分)>
      登记客户。单价为非负整数分，创建后不可修改。
      示例: go run . register c1 示例客户 100

  import <用量文件>
      整批导入用量记录（JSONL，每行一条 JSON）。先校验再生效，
      报告新增与重复数量。文件格式见 README。
      示例: go run . import usage.jsonl

  settle <客户标识> <YYYY-MM>
      对客户某 UTC 自然月（左闭右开）结算，生成一张账单并封账。
      重复结算返回原账单，不重新计费。
      示例: go run . settle c1 2026-01

  bill <客户标识> <YYYY-MM>
      查询账单及其全部用量明细。
      示例: go run . bill c1 2026-01
`

// runCLI 解析参数并执行命令，返回退出码。错误信息写入 stderr。
func runCLI(args []string, stdout, stderr io.Writer) int {
	// 无参数与 --help/-h：显示帮助并以 0 退出。
	if len(args) == 0 {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, helpText)
		return 0
	}

	// 提取全局选项与命令。
	dataDir := os.Getenv("PLANBILL_DATA_DIR")
	rest := args
	for len(rest) > 0 {
		a := rest[0]
		switch {
		case a == "--data-dir":
			if len(rest) < 2 || rest[1] == "" {
				return fail(stderr, "--data-dir 需要一个目录参数")
			}
			dataDir = rest[1]
			rest = rest[2:]
		case strings.HasPrefix(a, "--data-dir="):
			dataDir = strings.TrimPrefix(a, "--data-dir=")
			if dataDir == "" {
				return fail(stderr, "--data-dir 的目录不能为空")
			}
			rest = rest[1:]
		case a == "--help" || a == "-h":
			return fail(stderr, "--help 只能单独使用")
		default:
			goto dispatch
		}
	}
dispatch:
	if len(rest) == 0 {
		return fail(stderr, "缺少命令；可用命令: register, import, settle, bill（使用 --help 查看说明）")
	}
	cmd, cmdArgs := rest[0], rest[1:]
	if strings.HasPrefix(cmd, "-") {
		return fail(stderr, "未知选项 %q；使用 --help 查看帮助", cmd)
	}
	if dataDir == "" {
		dataDir = ".planbill"
	}

	store, err := NewStore(dataDir)
	if err != nil {
		return fail(stderr, "%v", err)
	}

	switch cmd {
	case "register":
		return cmdRegister(store, cmdArgs, stdout, stderr)
	case "import":
		return cmdImport(store, cmdArgs, stdout, stderr)
	case "settle":
		return cmdSettle(store, cmdArgs, stdout, stderr)
	case "bill":
		return cmdBill(store, cmdArgs, stdout, stderr)
	default:
		return fail(stderr, "未知命令 %q；可用命令: register, import, settle, bill", cmd)
	}
}

func cmdRegister(s *Store, args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 {
		return fail(stderr, "register 需要 3 个参数: <客户标识> <客户名称> <单价(分)>")
	}
	id, name, priceText := args[0], args[1], args[2]
	if id == "" {
		return fail(stderr, "客户标识不能为空")
	}
	if name == "" {
		return fail(stderr, "客户名称不能为空")
	}
	price, err := strconv.ParseInt(priceText, 10, 64)
	if err != nil {
		return fail(stderr, "单价 %q 不是合法整数: %v", priceText, err)
	}
	if price < 0 {
		return fail(stderr, "单价必须为非负整数分，得到 %d", price)
	}
	if err := s.Register(id, name, price); err != nil {
		return fail(stderr, "%v", err)
	}
	fmt.Fprintf(stdout, "已登记客户: 标识=%s 名称=%s 单价=%s\n", id, name, formatMoney(price))
	return 0
}

func cmdImport(s *Store, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return fail(stderr, "import 需要 1 个参数: <用量文件>")
	}
	res, err := s.ImportUsage(args[0])
	if err != nil {
		return fail(stderr, "%v", err)
	}
	fmt.Fprintf(stdout, "导入完成: 新增 %d 条，重复跳过 %d 条\n", res.Added, res.Duplicated)
	return 0
}

func cmdSettle(s *Store, args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		return fail(stderr, "settle 需要 2 个参数: <客户标识> <YYYY-MM>")
	}
	b, created, err := s.Settle(args[0], args[1])
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if created {
		fmt.Fprintf(stdout, "结算成功并已封账: 客户=%s 月份=%s\n", b.CustomerID, b.Month)
	} else {
		fmt.Fprintf(stdout, "该客户月份已结算，返回原账单（不重新计费）: 客户=%s 月份=%s\n", b.CustomerID, b.Month)
	}
	printBill(stdout, s, b)
	return 0
}

func cmdBill(s *Store, args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		return fail(stderr, "bill 需要 2 个参数: <客户标识> <YYYY-MM>")
	}
	b, err := s.GetBill(args[0], args[1])
	if err != nil {
		return fail(stderr, "%v", err)
	}
	printBill(stdout, s, b)
	return 0
}

func printBill(w io.Writer, s *Store, b *Bill) {
	custName := ""
	if c := s.findCustomer(b.CustomerID); c != nil {
		custName = c.Name
	}
	fmt.Fprintf(w, "账单标识: %s\n", b.ID)
	fmt.Fprintf(w, "客户: %s（%s）\n", b.CustomerID, custName)
	fmt.Fprintf(w, "月份: %s（UTC 自然月）\n", b.Month)
	fmt.Fprintf(w, "单价: %s\n", formatMoney(b.UnitPrice))
	fmt.Fprintf(w, "总数量: %d\n", b.TotalQty)
	fmt.Fprintf(w, "总金额: %s\n", formatMoney(b.TotalAmt))
	fmt.Fprintf(w, "明细（共 %d 条）:\n", len(b.Lines))
	for _, l := range b.Lines {
		fmt.Fprintf(w, "  - 用量标识=%s 时间=%s 数量=%d 小计=%s\n",
			l.RecID, l.Time, l.Quantity, formatMoney(l.Subtotal))
	}
}

// formatMoney 将分格式化为“N 分（X.YY 元）”，换算只用整数，不用浮点数。
func formatMoney(cents int64) string {
	neg := ""
	if cents < 0 {
		neg = "-"
		cents = -cents
	}
	yuan := cents / 100
	rem := cents % 100
	return fmt.Sprintf("%s%d 分（%s%d.%02d 元）", neg, cents, neg, yuan, rem)
}

// fail 向 stderr 输出错误原因并返回退出码 2。
func fail(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "planbill: %s\n", fmt.Sprintf(format, a...))
	return 2
}
