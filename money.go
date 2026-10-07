package main

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"strconv"
)

// 金额、单价、数量均使用有符号 64 位整数（金额单位：人民币分），
// 全程整数运算，不经过浮点数。

var errOverflow = errors.New("数量或金额运算超出有符号 64 位整数范围")

// mul64 返回 a*b，溢出时返回错误。a、b 均非负。
func mul64(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, errOverflow
	}
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b {
		return 0, errOverflow
	}
	return a * b, nil
}

// add64 返回 a+b，溢出时返回错误。a、b 均非负。
func add64(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, errOverflow
	}
	return a + b, nil
}

// addSigned64 返回 a+b（允许负数），溢出时返回错误。
func addSigned64(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, errOverflow
	}
	return a + b, nil
}

// neg64 返回 -a；a 为 math.MinInt64 时无法表示，返回错误。
func neg64(a int64) (int64, error) {
	if a == math.MinInt64 {
		return 0, errOverflow
	}
	return -a, nil
}

// sumSigned64 精确求和一组有符号 64 位整数（内部按 128 位累加，
// 与求和顺序无关），最终结果超出有符号 64 位范围时返回错误。
func sumSigned64(vals ...int64) (int64, error) {
	var hi, lo uint64
	for _, v := range vals {
		var carry uint64
		lo, carry = bits.Add64(lo, uint64(v), 0)
		// 符号扩展 v 的高位后连同进位一起累加。
		sign := uint64(0)
		if v < 0 {
			sign = math.MaxUint64
		}
		hi, _ = bits.Add64(hi, sign, carry)
	}
	switch {
	case hi == 0 && lo <= math.MaxInt64:
		return int64(lo), nil
	case hi == math.MaxUint64 && lo >= 1<<63:
		return int64(lo), nil
	default:
		return 0, errOverflow
	}
}

// moneyFen 将“分”渲染为人民币金额，如 1234 -> "12.34 元"，5 -> "0.05 元"。
func moneyFen(fen int64) string {
	neg := fen < 0
	// 用无符号数取绝对值，避免 math.MinInt64 取负溢出。
	u := uint64(fen)
	if neg {
		u = uint64(-(fen + 1)) + 1
	}
	s := strconv.FormatUint(u, 10)
	if len(s) < 3 {
		s = "00" + s
		s = s[len(s)-3:]
	}
	whole, frac := s[:len(s)-2], s[len(s)-2:]
	out := whole + "." + frac + " 元"
	if neg {
		out = "-" + out
	}
	return out
}

// int128 是有符号 128 位整数（二进制补码：hi 为高 64 位、lo 为低 64 位）。
// 单账单的应付/实收/未收余额保证在有符号 64 位内，但跨账期报表的多账单
// 合计可能超出有符号 64 位上限，汇总必须精确输出，故用 128 位整数累加；
// 全程整数运算，不经过浮点（math/big 仅用于十进制格式化）。
type int128 struct {
	hi int64
	lo uint64
}

// int128Of 把有符号 64 位整数符号扩展为 128 位。
func int128Of(v int64) int128 {
	if v < 0 {
		return int128{hi: -1, lo: uint64(v)}
	}
	return int128{hi: 0, lo: uint64(v)}
}

// add 返回 a+b（二进制补码加法，精确）。账单数量级（每月一张）远不足以
// 让合法余额的合计溢出 128 位，故无需溢出检查。
func (a int128) add(b int128) int128 {
	lo, carry := bits.Add64(a.lo, b.lo, 0)
	hi, _ := bits.Add64(uint64(a.hi), uint64(b.hi), carry)
	return int128{hi: int64(hi), lo: lo}
}

// neg 返回 -a（二进制补码取负）。
func (a int128) neg() int128 {
	lo, carry := bits.Add64(^a.lo, 1, 0)
	hi, _ := bits.Add64(uint64(^a.hi), 0, carry)
	return int128{hi: int64(hi), lo: lo}
}

// sub 返回 a-b。
func (a int128) sub(b int128) int128 { return a.add(b.neg()) }

// big 转换为 big.Int 以便十进制格式化（仅用于输出，不参与计算）。
func (a int128) big() *big.Int {
	v := new(big.Int).SetInt64(a.hi)
	v.Lsh(v, 64)
	return v.Add(v, new(big.Int).SetUint64(a.lo))
}

func (a int128) String() string { return a.big().String() }

// moneyFen128 将 128 位“分”渲染为人民币金额，规则与 moneyFen 相同。
func moneyFen128(fen int128) string {
	b := fen.big()
	neg := b.Sign() < 0
	if neg {
		b.Neg(b)
	}
	whole, frac := new(big.Int).QuoRem(b, big.NewInt(100), new(big.Int))
	out := fmt.Sprintf("%s.%02d 元", whole.String(), frac.Int64())
	if neg {
		out = "-" + out
	}
	return out
}
