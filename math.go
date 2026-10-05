package main

import (
	"errors"
	"math"
	"math/bits"
)

var errOverflow = errors.New("有符号 64 位整数运算溢出")

// addInt64 返回 a+b，结果超出 int64 范围时返回 errOverflow。
func addInt64(a, b int64) (int64, error) {
	// 非负求和的快路径（本程序所有金额、数量累加均为非负）。
	if a >= 0 && b >= 0 {
		s, carry := bits.Add64(uint64(a), uint64(b), 0)
		if carry != 0 || s > math.MaxInt64 {
			return 0, errOverflow
		}
		return int64(s), nil
	}
	if b > 0 && a > math.MaxInt64-b {
		return 0, errOverflow
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, errOverflow
	}
	return a + b, nil
}

// mulInt64 返回 a*b，结果超出 int64 范围时返回 errOverflow。全程整数运算。
func mulInt64(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	// 非负相乘的快路径：数量、单价均非负。
	if a >= 0 && b >= 0 {
		hi, lo := bits.Mul64(uint64(a), uint64(b))
		if hi != 0 || lo > math.MaxInt64 {
			return 0, errOverflow
		}
		return int64(lo), nil
	}
	// 通用有符号路径（当前业务不会走到，保留以防误用）。
	if a == math.MinInt64 || b == math.MinInt64 {
		return 0, errOverflow
	}
	p := a * b
	if p/a != b {
		return 0, errOverflow
	}
	return p, nil
}
