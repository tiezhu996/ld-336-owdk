package util

import (
	"time"

	"github.com/medasset/medasset/internal/constants"
)

// CalibrationDueWindowDays 计量到期预警窗口：下次计量日前 30 天显示“即将到期”。
const CalibrationDueWindowDays = 30

// startOfDay 截断到本地时区的当日零点，保证列表筛选、预警、总览按同一“日”粒度比较。
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// CalibrationDeadlineBounds 返回计量到期判定的两个时点（均为当日零点）：
// expiredBefore：下次计量日早于该时点（即 < 今日零点）即为“已过期”；
// dueBefore：下次计量日早于该时点（即 < 今日+30 天零点）即为“即将到期”。
// 列表筛选、到期预警、统计总览必须复用同一组边界，确保同一时点计算结果一致。
func CalibrationDeadlineBounds(now time.Time) (expiredBefore, dueBefore time.Time) {
	today := startOfDay(now)
	return today, today.AddDate(0, 0, CalibrationDueWindowDays+1)
}

// DeriveCalibrationStatus 按统一时点推导计量状态：
// 结果不合格始终显示“不合格”；逾期显示“已过期”；下次计量日前 30 天内显示“即将到期”；其余为“合格”。
// next 为 nil（尚未安排下次计量日）时按“合格”处理。
func DeriveCalibrationStatus(result string, next *time.Time, now time.Time) string {
	if result == constants.CalibrationResultUnqualified {
		return constants.CalibrationStatusUnqualified
	}
	if next == nil {
		return constants.CalibrationStatusNormal
	}
	expiredBefore, dueBefore := CalibrationDeadlineBounds(now)
	switch {
	case next.Before(expiredBefore):
		return constants.CalibrationStatusExpired
	case next.Before(dueBefore):
		return constants.CalibrationStatusDue
	default:
		return constants.CalibrationStatusNormal
	}
}

// SameCalendarDay 判断两个时间是否处于同一日历日（用于重复登记判定）。
func SameCalendarDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
