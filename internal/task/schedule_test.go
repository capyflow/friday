package task

import (
	"strings"
	"testing"
)

func TestScheduleRuleToCron(t *testing.T) {
	cases := []struct {
		name string
		rule ScheduleRule
		want string
	}{
		{"每分钟", ScheduleRule{Unit: UnitMinute, Interval: 1}, "* * * * *"},
		{"每10分钟", ScheduleRule{Unit: UnitMinute, Interval: 10}, "*/10 * * * *"},
		{"每小时第5分", ScheduleRule{Unit: UnitHourly, Interval: 1, Minute: 5}, "5 * * * *"},
		{"每3小时的第20分", ScheduleRule{Unit: UnitHourly, Interval: 3, Minute: 20}, "20 */3 * * *"},
		{"每天8:30", ScheduleRule{Unit: UnitDaily, Hour: 8, Minute: 30}, "30 8 * * *"},
		{"每天0点", ScheduleRule{Unit: UnitDaily}, "0 0 * * *"},
		// 星期几乱序 + 重复：编译前要归一化成升序去重，同一条语义只有一种写法。
		{"每周一三五9点", ScheduleRule{Unit: UnitWeekly, Hour: 9, Weekdays: []int{5, 1, 3, 1}}, "0 9 * * 1,3,5"},
		{"每月1号和15号20点", ScheduleRule{Unit: UnitMonthly, Hour: 20, MonthDays: []int{15, 1}}, "0 20 1,15 * *"},
		{"每年10月19日7点", ScheduleRule{Unit: UnitYearly, Hour: 7, Months: []int{10}, MonthDays: []int{19}}, "0 7 19 10 *"},
	}
	for _, tc := range cases {
		got, err := tc.rule.ToCron()
		if err != nil {
			t.Fatalf("%s: ToCron: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
		if _, err := ParseCron(got); err != nil {
			t.Errorf("%s: 编译出的表达式引擎不认: %v", tc.name, err)
		}
	}
}

func TestScheduleRuleValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		rule ScheduleRule
		want string
	}{
		{"未知单位", ScheduleRule{Unit: " fortnightly "}, "不支持"},
		{"空单位", ScheduleRule{}, "不支持"},
		{"分钟间隔越界", ScheduleRule{Unit: UnitMinute, Interval: 60}, "1-59"},
		{"小时间隔越界", ScheduleRule{Unit: UnitHourly, Interval: 24}, "1-23"},
		{"每天每2天需要锚点", ScheduleRule{Unit: UnitDaily, Interval: 2}, "暂不支持每 N 个"},
		{"每小时每2天需要锚点", ScheduleRule{Unit: UnitWeekly, Interval: 2}, "暂不支持每 N 个"},
		{"分钟越界", ScheduleRule{Unit: UnitDaily, Minute: 60}, "0-59"},
		{"小时越界", ScheduleRule{Unit: UnitDaily, Hour: 24}, "0-23"},
		{"周缺星期几", ScheduleRule{Unit: UnitWeekly, Hour: 9}, "星期几"},
		{"周取值越界", ScheduleRule{Unit: UnitWeekly, Weekdays: []int{7}}, "0-6"},
		{"月缺几号", ScheduleRule{Unit: UnitMonthly, Hour: 9}, "几号"},
		{"年缺月份", ScheduleRule{Unit: UnitYearly, MonthDays: []int{1}}, "月份"},
		{"年缺日", ScheduleRule{Unit: UnitYearly, Months: []int{1}}, "几号"},
	}
	for _, tc := range cases {
		_, err := tc.rule.ToCron()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: 应报含 %q 的错误，实际 %v", tc.name, tc.want, err)
		}
	}
}

// TestScheduleRuleNormalizes 校验会把 interval 缺省补 1、数字集合去重升序回填，
// 前端回填时才能和提交的内容对得上。
func TestScheduleRuleNormalizes(t *testing.T) {
	r := ScheduleRule{Unit: UnitWeekly, Hour: 9, Weekdays: []int{3, 3, 1}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.Interval != 1 {
		t.Errorf("interval 缺省应补 1，实际 %d", r.Interval)
	}
	if len(r.Weekdays) != 2 || r.Weekdays[0] != 1 || r.Weekdays[1] != 3 {
		t.Errorf("星期几应去重升序: %v", r.Weekdays)
	}
}
