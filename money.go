package main

import (
	"errors"
	"math"
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

// addSigned64 返回 a+b，允许负数，溢出时返回错误。
func addSigned64(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, errOverflow
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, errOverflow
	}
	return a + b, nil
}

// moneyFen 将“分”渲染为人民币金额，如 1234 -> "12.34 元"，5 -> "0.05 元"。
func moneyFen(fen int64) string {
	neg := fen < 0
	if neg {
		fen = -fen
	}
	s := strconv.FormatInt(fen, 10)
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
