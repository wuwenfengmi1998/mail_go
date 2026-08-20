package web

// 12 小时制时间格式化（上午/下午）回归测试：
// time12 / time12m / shortDate 需先按 Web 时区转换，再输出中文习惯的
// 12 小时制（「2026-08-20 下午 2:35:05」），后台页面此前直接 Format
// 输出库内 UTC 时间（比北京时间慢 8 小时）。

import (
	"fmt"
	"testing"
	"time"
)

// setWebTZForTest 临时把展示时区固定为 UTC+8，测试结束恢复。
func setWebTZForTest(t *testing.T, loc *time.Location) {
	t.Helper()
	old := webTZ
	webTZ = loc
	t.Cleanup(func() { webTZ = old })
}

func TestTime12Format(t *testing.T) {
	setWebTZForTest(t, time.FixedZone("UTC+8", 8*3600))

	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"上午", time.Date(2026, 8, 20, 1, 5, 7, 0, time.UTC), "2026-08-20 上午 9:05:07"},
		{"正午", time.Date(2026, 8, 20, 4, 0, 0, 0, time.UTC), "2026-08-20 下午 12:00:00"},
		{"下午", time.Date(2026, 8, 20, 10, 30, 45, 0, time.UTC), "2026-08-20 下午 6:30:45"},
		{"凌晨", time.Date(2026, 8, 20, 16, 0, 0, 0, time.UTC), "2026-08-21 上午 12:00:00"},
	}
	for _, tc := range cases {
		if got := time12("zh", tc.in); got != tc.want {
			t.Errorf("time12(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTime12mNoSeconds(t *testing.T) {
	setWebTZForTest(t, time.FixedZone("UTC+8", 8*3600))
	in := time.Date(2026, 8, 20, 10, 30, 45, 0, time.UTC)
	if got, want := time12m("zh", in), "2026-08-20 下午 6:30"; got != want {
		t.Fatalf("time12m = %q, want %q", got, want)
	}
}

func TestShortDate12Hour(t *testing.T) {
	setWebTZForTest(t, time.FixedZone("UTC+8", 8*3600))
	now := time.Now().In(webTZ)

	// 今天 → 「下午 2:35」（无日期）
	today := time.Date(now.Year(), now.Month(), now.Day(), 14, 35, 0, 0, webTZ).UTC()
	if got, want := shortDate("zh", today), "下午 2:35"; got != want {
		t.Fatalf("shortDate(today) = %q, want %q", got, want)
	}

	// 今年非今天 → 「06-15 上午 9:05」（避开今天，防止午夜跨日抖动）
	day := time.Date(now.Year(), 6, 15, 9, 5, 0, 0, webTZ)
	if day.YearDay() == now.YearDay() {
		day = day.AddDate(0, 0, 1)
	}
	if got, want := shortDate("zh", day.UTC()), day.Format("01-02")+" 上午 9:05"; got != want {
		t.Fatalf("shortDate(thisYear) = %q, want %q", got, want)
	}

	// 往年 → 「YYYY-MM-DD 上午 9:05」
	older := time.Date(now.Year()-1, 12, 1, 9, 5, 0, 0, webTZ).UTC()
	if got, want := shortDate("zh", older), fmt.Sprintf("%d-12-01 上午 9:05", now.Year()-1); got != want {
		t.Fatalf("shortDate(older) = %q, want %q", got, want)
	}
}
