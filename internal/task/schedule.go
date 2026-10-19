package task

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 循环规则的调度单位（与前端「循环方式」下拉一一对应）。
const (
	UnitMinute  = "minute"
	UnitHourly  = "hourly"
	UnitDaily   = "daily"
	UnitWeekly  = "weekly"
	UnitMonthly = "monthly"
	UnitYearly  = "yearly"
)

// ScheduleRule 是一条循环规则的结构化形态：界面按它渲染控件、编辑时按它回填，
// 落盘与排程用的仍是编译出的 5 段 cron（Task.Cron 是引擎的唯一事实源）。
//
// 之所以存这一份而不是每次反解 cron：人写的表达式（如 "0 9 * * 1,3,5"）能编译，
// 却不一定能可靠地还原成"每周周一三五 9 点"这句语义（同一条 cron 有多种等价写法），
// 反解失败就会把用户推回手敲表达式。
type ScheduleRule struct {
	// Unit 调度单位，决定下面哪些字段生效。
	Unit string `json:"unit"`
	// Interval 每 N 个单位触发一次，仅 minute/hourly 支持 N>1；空或 1 = 每个单位。
	Interval int `json:"interval"`
	// Hour 触发小时（0-23），daily/weekly/monthly/yearly 生效。
	Hour int `json:"hour"`
	// Minute 触发分钟（0-59），hourly 及以上单位生效。
	Minute int `json:"minute"`
	// Weekdays 星期几集合（0=周日 … 6=周六），weekly 必填。
	Weekdays []int `json:"weekdays,omitempty"`
	// MonthDays 每月几号集合（1-31），monthly/yearly 必填。
	MonthDays []int `json:"monthDays,omitempty"`
	// Months 月份集合（1-12），yearly 必填。
	Months []int `json:"months,omitempty"`
}

// ValidUnits 报告单位是否是已知值。
func ValidUnits() []string {
	return []string{UnitMinute, UnitHourly, UnitDaily, UnitWeekly, UnitMonthly, UnitYearly}
}

// Validate 校验规则并归一化数字集合（去重、升序），让同一条语义只有一种 cron 写法。
func (r *ScheduleRule) Validate() error {
	if !contains(ValidUnits(), r.Unit) {
		return fmt.Errorf("循环单位 %q 不支持（可选 %s）", r.Unit, strings.Join(ValidUnits(), "/"))
	}
	if r.Interval == 0 {
		r.Interval = 1
	}
	switch r.Unit {
	case UnitMinute:
		if r.Interval < 1 || r.Interval > 59 {
			return fmt.Errorf("每 N 分钟触发要求 N 在 1-59，当前 %d", r.Interval)
		}
	case UnitHourly:
		if r.Interval < 1 || r.Interval > 23 {
			return fmt.Errorf("每 N 小时触发要求 N 在 1-23，当前 %d", r.Interval)
		}
	default:
		// 天级以上的"每 N 天"需要锚点（cron 的 */N 会在每月 1 号重置），
		// 排程器没有锚点概念，故明确不支持而不是给出一个悄悄错位的表达式。
		if r.Interval != 1 {
			return fmt.Errorf("%s 暂不支持每 N 个（N>1）触发，请改用具体时刻或指定星期几/几号", r.Unit)
		}
	}
	if r.Minute < 0 || r.Minute > 59 {
		return fmt.Errorf("分钟须在 0-59，当前 %d", r.Minute)
	}
	if r.Unit == UnitMinute {
		return nil
	}
	if r.Hour < 0 || r.Hour > 23 {
		return fmt.Errorf("小时须在 0-23，当前 %d", r.Hour)
	}
	switch r.Unit {
	case UnitWeekly:
		if err := normalizeList(&r.Weekdays, 0, 6, "星期几（0=周日 … 6=周六）"); err != nil {
			return err
		}
	case UnitMonthly:
		if err := normalizeList(&r.MonthDays, 1, 31, "每月几号"); err != nil {
			return err
		}
	case UnitYearly:
		if err := normalizeList(&r.Months, 1, 12, "月份"); err != nil {
			return err
		}
		if err := normalizeList(&r.MonthDays, 1, 31, "几号"); err != nil {
			return err
		}
	}
	return nil
}

// ToCron 编译成 5 段 cron（分 时 日 月 周）。
func (r *ScheduleRule) ToCron() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	switch r.Unit {
	case UnitMinute:
		if r.Interval == 1 {
			return "* * * * *", nil
		}
		return fmt.Sprintf("*/%d * * * *", r.Interval), nil
	case UnitHourly:
		if r.Interval == 1 {
			return fmt.Sprintf("%d * * * *", r.Minute), nil
		}
		return fmt.Sprintf("%d */%d * * *", r.Minute, r.Interval), nil
	case UnitDaily:
		return fmt.Sprintf("%d %d * * *", r.Minute, r.Hour), nil
	case UnitWeekly:
		return fmt.Sprintf("%d %d * * %s", r.Minute, r.Hour, joinList(r.Weekdays)), nil
	case UnitMonthly:
		return fmt.Sprintf("%d %d %s * *", r.Minute, r.Hour, joinList(r.MonthDays)), nil
	default: // UnitYearly
		return fmt.Sprintf("%d %d %s %s *", r.Minute, r.Hour, joinList(r.MonthDays), joinList(r.Months)), nil
	}
}

// normalizeList 校验数字集合落在 [lo,hi]、去重并升序回填。
func normalizeList(list *[]int, lo, hi int, label string) error {
	if len(*list) == 0 {
		return fmt.Errorf("%s 不能为空", label)
	}
	seen := make(map[int]bool, len(*list))
	out := make([]int, 0, len(*list))
	for _, n := range *list {
		if n < lo || n > hi {
			return fmt.Errorf("%s须在 %d-%d，出现了 %d", label, lo, hi, n)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	*list = out
	return nil
}

func joinList(list []int) string {
	parts := make([]string, len(list))
	for i, n := range list {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
