package main

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var monthRe = regexp.MustCompile(`^(\d{4})-(0[1-9]|1[0-2])$`)

// parseUsageTime 按 RFC3339 解析时间（如 2026-01-15T08:30:00+08:00），
// 允许秒后的小数部分。
func parseUsageTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("时间 %q 不是合法 RFC3339 时间: %v", s, err)
	}
	return t, nil
}

// normalizeUTC 将时间点规范化为 UTC 的 RFC3339Nano 文本，
// 使同一时刻的不同写法可逐字比较。
func normalizeUTC(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseMonth 严格解析 YYYY-MM。
func parseMonth(m string) (int, int, error) {
	match := monthRe.FindStringSubmatch(m)
	if match == nil {
		return 0, 0, fmt.Errorf("月份 %q 无效，应为 YYYY-MM 形式（如 2026-01）", m)
	}
	year, _ := strconv.Atoi(match[1])
	mon, _ := strconv.Atoi(match[2])
	return year, mon, nil
}

// monthRange 返回 UTC 自然月的左闭右开区间 [start, end)。
func monthRange(m string) (time.Time, time.Time, error) {
	year, mon, err := parseMonth(m)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start := time.Date(year, time.Month(mon), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	return start, end, nil
}

// utcMonth 返回某时间点所在的 UTC 自然月 YYYY-MM。
func utcMonth(t time.Time) string {
	return t.UTC().Format("2006-01")
}
