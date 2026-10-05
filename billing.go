package main

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// 业务错误。
var (
	errCustomerExists  = errors.New("客户标识已存在")
	errCustomerMissing = errors.New("客户不存在")
	errNoUsageInMonth  = errors.New("该客户该月没有用量")
	errBillNotFound    = errors.New("该客户该月尚未结算，无账单")
)

// Register 登记客户。标识、名称非空，单价为非负整数分；
// 标识重复、空标识、空名称一律拒绝。单价创建后不可修改（本程序不提供修改入口）。
func (s *Store) Register(id, name string, price int64) error {
	if id == "" {
		return errors.New("客户标识不能为空")
	}
	if name == "" {
		return errors.New("客户名称不能为空")
	}
	if price < 0 {
		return fmt.Errorf("单价不能为负: %d", price)
	}
	if s.findCustomer(id) != nil {
		return fmt.Errorf("%w: %q", errCustomerExists, id)
	}
	s.data.Customers = append(s.data.Customers, Customer{ID: id, Name: name, Price: price})
	if err := s.save(); err != nil {
		return err
	}
	return nil
}

// Settle 对客户某 UTC 自然月做按量月结。
// 已结算则原样返回既有账单（created=false），不重新计费、不产生新账单。
// 客户不存在、月份无效、当月无用量或汇总溢出时拒绝且不封账。
func (s *Store) Settle(customerID, month string) (bill *Bill, created bool, err error) {
	c := s.findCustomer(customerID)
	if c == nil {
		return nil, false, fmt.Errorf("%w: %q", errCustomerMissing, customerID)
	}
	start, end, err := monthRange(month)
	if err != nil {
		return nil, false, err
	}
	if existing, ok := s.data.Bills[billKey(customerID, month)]; ok {
		b := existing
		return &b, false, nil
	}

	// 归集 [start, end) 内的用量，同时完成全部溢出校验；
	// 任何一步失败都在写库之前返回，保持未封账状态。
	type picked struct {
		u        Usage
		t        time.Time
		subtotal int64
	}
	var items []picked
	var totalQty, totalAmt int64
	for i := range s.data.Usages {
		u := &s.data.Usages[i]
		if u.CustomerID != customerID {
			continue
		}
		t, perr := parseUsageTime(u.Time)
		if perr != nil {
			return nil, false, perr // 加载时已校验，理论上不会发生
		}
		if t.Before(start) || !t.Before(end) {
			continue
		}
		sub, perr := mulInt64(u.Quantity, c.Price)
		if perr != nil {
			return nil, false, fmt.Errorf("用量 %q 的金额（数量 %d × 单价 %d）%w", u.RecID, u.Quantity, c.Price, perr)
		}
		totalQty, perr = addInt64(totalQty, u.Quantity)
		if perr != nil {
			return nil, false, fmt.Errorf("月份 %s 总数量%w", month, perr)
		}
		totalAmt, perr = addInt64(totalAmt, sub)
		if perr != nil {
			return nil, false, fmt.Errorf("月份 %s 总金额%w", month, perr)
		}
		items = append(items, picked{u: *u, t: t, subtotal: sub})
	}
	if len(items) == 0 {
		return nil, false, fmt.Errorf("%w: 客户 %q，月份 %s", errNoUsageInMonth, customerID, month)
	}

	// 明细按时间、用量标识稳定排序，保证账单可复现。
	sort.Slice(items, func(i, j int) bool {
		if !items[i].t.Equal(items[j].t) {
			return items[i].t.Before(items[j].t)
		}
		return items[i].u.RecID < items[j].u.RecID
	})
	lines := make([]Line, 0, len(items))
	for _, it := range items {
		lines = append(lines, Line{
			RecID:    it.u.RecID,
			Time:     it.u.Time,
			Quantity: it.u.Quantity,
			Subtotal: it.subtotal,
		})
	}

	seq := s.data.NextBillSeq + 1
	b := Bill{
		ID:         fmt.Sprintf("B%06d", seq),
		CustomerID: customerID,
		Month:      month,
		TotalQty:   totalQty,
		UnitPrice:  c.Price,
		TotalAmt:   totalAmt,
		Lines:      lines,
	}
	s.data.NextBillSeq = seq
	s.data.Bills[billKey(customerID, month)] = b
	if err := s.save(); err != nil {
		return nil, false, err
	}
	return &b, true, nil
}

// GetBill 查询某客户某月的账单。
func (s *Store) GetBill(customerID, month string) (*Bill, error) {
	if s.findCustomer(customerID) == nil {
		return nil, fmt.Errorf("%w: %q", errCustomerMissing, customerID)
	}
	if _, _, err := monthRange(month); err != nil {
		return nil, err
	}
	b, ok := s.data.Bills[billKey(customerID, month)]
	if !ok {
		return nil, fmt.Errorf("%w: 客户 %q，月份 %s", errBillNotFound, customerID, month)
	}
	return &b, nil
}
