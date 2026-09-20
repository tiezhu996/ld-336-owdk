package repository

import (
	"testing"
	"time"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/model"
)

func TestCalibrationSyncDerivedStatuses(t *testing.T) {
	db := newTestDB(t)
	repo := NewCalibrationRepository(db)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.Local)

	mk := func(no string, next time.Time, result string) {
		status := constants.CalibrationStatusNormal
		if result == constants.CalibrationResultUnqualified {
			status = constants.CalibrationStatusUnqualified
		}
		c := &model.CalibrationRecord{
			InstrumentNo: no, DeviceID: 1, DeviceName: "监护仪",
			CalibrationCycleMonths: 12, NextCalibrationDate: &next, Result: result,
			Status: status,
		}
		if err := repo.Create(c); err != nil {
			t.Fatalf("create %s: %v", no, err)
		}
	}
	mkExpired := time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local)
	mkDue := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	mkNormal := time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local)
	mk("C-EXPIRED", mkExpired, constants.CalibrationResultQualified)
	mk("C-DUE", mkDue, constants.CalibrationResultQualified)
	mk("C-NORMAL", mkNormal, constants.CalibrationResultQualified)
	// 不合格终态：即使下次计量日早已过期，也不能被刷成 expired。
	mk("C-UNQ", mkExpired, constants.CalibrationResultUnqualified)

	expiredN, dueN, normalN, err := repo.SyncDerivedStatuses(now)
	if err != nil {
		t.Fatalf("SyncDerivedStatuses: %v", err)
	}
	if expiredN != 1 || dueN != 1 || normalN != 1 {
		t.Errorf("sync counts expired=%d due=%d normal=%d, want 1/1/1", expiredN, dueN, normalN)
	}

	got, err := repo.FindByID(4)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Status != constants.CalibrationStatusUnqualified {
		t.Errorf("unqualified record status overwritten: %s", got.Status)
	}

	// 列表按刷新后的状态筛选。
	expiredList, total, err := repo.List(1, 10, 0, constants.CalibrationStatusExpired)
	if err != nil || total != 1 || len(expiredList) != 1 || expiredList[0].InstrumentNo != "C-EXPIRED" {
		t.Errorf("expired filter list=%v total=%d err=%v", expiredList, total, err)
	}

	// 预警清单只含 due/expired，不含不合格。
	warn, err := repo.ListDue(now)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	if len(warn) != 2 {
		t.Fatalf("due list len = %d, want 2", len(warn))
	}
	if warn[0].InstrumentNo != "C-EXPIRED" {
		t.Errorf("due list should be ordered by next date asc, got %s first", warn[0].InstrumentNo)
	}
	for _, w := range warn {
		if w.Status == constants.CalibrationStatusUnqualified {
			t.Errorf("unqualified record must not appear in due list: %s", w.InstrumentNo)
		}
	}

	// 幂等：再次刷新后各记录状态保持不变（SQLite 按匹配行计数，MySQL 按变更行计数，因此校验状态而非影响行数）。
	if _, _, _, err := repo.SyncDerivedStatuses(now); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	wantStatus := map[string]string{
		"C-EXPIRED": constants.CalibrationStatusExpired,
		"C-DUE":     constants.CalibrationStatusDue,
		"C-NORMAL":  constants.CalibrationStatusNormal,
		"C-UNQ":     constants.CalibrationStatusUnqualified,
	}
	var all []model.CalibrationRecord
	if err := db.Order("id ASC").Find(&all).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	for _, c := range all {
		if c.Status != wantStatus[c.InstrumentNo] {
			t.Errorf("%s status = %s, want %s after second sync", c.InstrumentNo, c.Status, wantStatus[c.InstrumentNo])
		}
	}
}
