package util

import (
	"testing"
	"time"

	"github.com/medasset/medasset/internal/constants"
)

func TestDeriveCalibrationStatus(t *testing.T) {
	// 固定时点：2026-09-20 10:00（本地时区）。
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.Local)
	day := func(offsetDays int) *time.Time {
		d := time.Date(2026, 9, 20+offsetDays, 9, 0, 0, 0, time.Local)
		return &d
	}

	cases := []struct {
		name   string
		result string
		next   *time.Time
		want   string
	}{
		{"30天外合格", constants.CalibrationResultQualified, day(31), constants.CalibrationStatusNormal},
		{"第30天即将到期", constants.CalibrationResultQualified, day(30), constants.CalibrationStatusDue},
		{"明天即将到期", constants.CalibrationResultQualified, day(1), constants.CalibrationStatusDue},
		{"计量日当天仍为即将到期", constants.CalibrationResultQualified, day(0), constants.CalibrationStatusDue},
		{"逾期一天已过期", constants.CalibrationResultQualified, day(-1), constants.CalibrationStatusExpired},
		{"不合格始终不合格(下次计量在未来)", constants.CalibrationResultUnqualified, day(120), constants.CalibrationStatusUnqualified},
		{"不合格始终不合格(已逾期)", constants.CalibrationResultUnqualified, day(-5), constants.CalibrationStatusUnqualified},
		{"无下次计量日按合格", constants.CalibrationResultQualified, nil, constants.CalibrationStatusNormal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveCalibrationStatus(tc.result, tc.next, now)
			if got != tc.want {
				t.Fatalf("DeriveCalibrationStatus(%s, %v) = %s, want %s", tc.result, tc.next, got, tc.want)
			}
		})
	}
}

func TestCalibrationDeadlineBounds(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 30, 0, 0, time.Local)
	expiredBefore, dueBefore := CalibrationDeadlineBounds(now)
	if expiredBefore != time.Date(2026, 9, 20, 0, 0, 0, 0, time.Local) {
		t.Errorf("expiredBefore = %v", expiredBefore)
	}
	if dueBefore != time.Date(2026, 10, 21, 0, 0, 0, 0, time.Local) {
		t.Errorf("dueBefore = %v, want 2026-10-21", dueBefore)
	}
}

func TestSameCalendarDay(t *testing.T) {
	a := time.Date(2026, 9, 20, 1, 0, 0, 0, time.Local)
	b := time.Date(2026, 9, 20, 23, 59, 0, 0, time.Local)
	c := time.Date(2026, 9, 21, 0, 1, 0, 0, time.Local)
	if !SameCalendarDay(a, b) {
		t.Error("a,b should be same day")
	}
	if SameCalendarDay(a, c) {
		t.Error("a,c should be different days")
	}
}
