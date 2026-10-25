package app

import (
	"testing"
	"time"

	"songloft/internal/database"
)

// TestAlarmDue 覆盖星期掩码、分钟去重与一次性闹钟。
func TestAlarmDue(t *testing.T) {
	// 2026-10-26 是周一。
	mon0730 := time.Date(2026, 10, 26, 7, 30, 10, 0, time.Local)

	base := &database.AlarmRow{
		ID: "a", TargetID: "g", TimeOfDay: "07:30", DaysMask: 127, Enabled: true,
	}
	if !alarmDue(base, mon0730) {
		t.Fatal("每天 07:30 的闹钟在周一 07:30 应触发")
	}

	// 不在同一分钟：不触发。
	if alarmDue(base, mon0730.Add(time.Minute)) {
		t.Fatal("07:31 不应触发 07:30 的闹钟")
	}

	// 同一分钟已经响过：不重复触发。
	fired := *base
	fired.LastFiredAt = alarmMinuteKey(mon0730)
	if alarmDue(&fired, mon0730) {
		t.Fatal("同一分钟已触发过，不应重复")
	}

	// 只在周二的掩码，周一不触发。
	tue := *base
	tue.DaysMask = 1 << 1
	if alarmDue(&tue, mon0730) {
		t.Fatal("仅周二的闹钟周一不应触发")
	}

	// 周日（bit6）匹配。
	sun := *base
	sun.DaysMask = 1 << 6
	sunAt := time.Date(2026, 10, 25, 7, 30, 0, 0, time.Local) // 周日
	if !alarmDue(&sun, sunAt) {
		t.Fatal("周日闹钟在周日 07:30 应触发")
	}

	// days_mask=0 表示"只今天一次"，不受星期限制。
	once := *base
	once.DaysMask = 0
	if !alarmDue(&once, mon0730) {
		t.Fatal("days_mask=0 应在匹配时刻触发")
	}

	// 关闭的闹钟永不触发；时间格式非法也不触发。
	off := *base
	off.Enabled = false
	if alarmDue(&off, mon0730) {
		t.Fatal("enabled=false 不应触发")
	}
	bad := *base
	bad.TimeOfDay = "7:30:00"
	if alarmDue(&bad, mon0730) {
		t.Fatal("非法时间格式不应触发")
	}
}

// TestFadeVolumeSteps 渐强序列单调递增、起点不低于 1、终点等于目标。
func TestFadeVolumeSteps(t *testing.T) {
	if steps := fadeVolumeSteps(40, 0); steps != nil {
		t.Fatalf("fade=0 应返回 nil，实际 %v", steps)
	}
	steps := fadeVolumeSteps(40, 10)
	if len(steps) != 11 {
		t.Fatalf("期望 11 级（含首尾），实际 %d", len(steps))
	}
	if steps[0] < 1 || steps[0] >= 40 {
		t.Fatalf("起点应在 1..39 之间，实际 %d", steps[0])
	}
	if steps[len(steps)-1] != 40 {
		t.Fatalf("终点应为 40，实际 %d", steps[len(steps)-1])
	}
	for i := 1; i < len(steps); i++ {
		if steps[i] < steps[i-1] {
			t.Fatalf("音量应单调不降: %v", steps)
		}
	}
	// 目标音量很小、渐强很长时，级数被目标值封顶，避免长时间停在 1。
	if many := fadeVolumeSteps(3, 60); len(many) > 4 {
		t.Fatalf("级数应被目标音量封顶，实际 %d", len(many))
	}
}

// TestParseHHMM 覆盖正常与非法输入。
func TestParseHHMM(t *testing.T) {
	cases := []struct {
		in   string
		h, m int
		ok   bool
	}{
		{"07:30", 7, 30, true},
		{"00:00", 0, 0, true},
		{"23:59", 23, 59, true},
		{" 6:05 ", 6, 5, true},
		{"24:00", 0, 0, false},
		{"07:60", 0, 0, false},
		{"0730", 0, 0, false},
		{"07:3a", 0, 0, false},
	}
	for _, c := range cases {
		h, m, ok := parseHHMM(c.in)
		if ok != c.ok || (ok && (h != c.h || m != c.m)) {
			t.Fatalf("parseHHMM(%q) = %d,%d,%v，期望 %d,%d,%v", c.in, h, m, ok, c.h, c.m, c.ok)
		}
	}
}
