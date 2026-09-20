package service

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/dto"
	"github.com/medasset/medasset/internal/model"
	"github.com/medasset/medasset/internal/repository"
	"github.com/medasset/medasset/internal/util"
)

func newCalibrationTestEnv(t *testing.T) (*CalibrationService, *repository.CalibrationRepository, *repository.DeviceRepository) {
	env := newTestServiceEnv(t)
	calRepo := repository.NewCalibrationRepository(env.db)
	devRepo := repository.NewDeviceRepository(env.db)
	svc := NewCalibrationService(calRepo, devRepo, env.audit, env.logger)
	return svc, calRepo, devRepo
}

func seedCalibrationDevice(t *testing.T, devRepo *repository.DeviceRepository, status string) *model.Device {
	t.Helper()
	d := &model.Device{AssetCode: "MA-CAL-" + status + "-" + t.Name(), Name: "输液泵", Status: status,
		Department: "心内科", Category: "生命支持"}
	if err := devRepo.Create(d); err != nil {
		t.Fatalf("create device: %v", err)
	}
	return d
}

func seedCalibrationRecord(t *testing.T, calRepo *repository.CalibrationRepository, deviceID uint, next time.Time) *model.CalibrationRecord {
	t.Helper()
	c := &model.CalibrationRecord{
		InstrumentNo: "INS-" + t.Name(), DeviceID: deviceID, DeviceName: "输液泵",
		CalibrationCycleMonths: 12, NextCalibrationDate: &next,
		Status: constants.CalibrationStatusNormal, Result: constants.CalibrationResultQualified,
	}
	if err := calRepo.Create(c); err != nil {
		t.Fatalf("create calibration: %v", err)
	}
	return c
}

func TestRecordResultUnqualifiedDisablesDeviceInTx(t *testing.T) {
	svc, calRepo, devRepo := newCalibrationTestEnv(t)
	d := seedCalibrationDevice(t, devRepo, constants.DeviceStatusInUse)
	future := time.Now().AddDate(0, 6, 0)
	c := seedCalibrationRecord(t, calRepo, d.ID, future)

	out, err := svc.RecordResult(c.ID, &dto.CalibrationResultReq{Result: constants.CalibrationResultUnqualified}, "tester")
	if err != nil {
		t.Fatalf("RecordResult: %v", err)
	}
	if out.Status != constants.CalibrationStatusUnqualified || out.Result != constants.CalibrationResultUnqualified {
		t.Errorf("record status=%s result=%s", out.Status, out.Result)
	}
	if out.ResultRegisteredAt == nil {
		t.Error("result_registered_at should be set")
	}
	gotDev, err := devRepo.FindByID(d.ID)
	if err != nil {
		t.Fatalf("FindByID device: %v", err)
	}
	if gotDev.Status != constants.DeviceStatusDisabled {
		t.Errorf("device should be disabled in same transaction, got %s", gotDev.Status)
	}
}

func TestRecordResultDuplicateSameDayRejected(t *testing.T) {
	svc, calRepo, devRepo := newCalibrationTestEnv(t)
	d := seedCalibrationDevice(t, devRepo, constants.DeviceStatusInUse)
	future := time.Now().AddDate(0, 6, 0)
	c := seedCalibrationRecord(t, calRepo, d.ID, future)

	if _, err := svc.RecordResult(c.ID, &dto.CalibrationResultReq{Result: constants.CalibrationResultQualified}, "tester"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// 当日重复登记：必须 409，且不能改变既有终态/设备状态。
	_, err := svc.RecordResult(c.ID, &dto.CalibrationResultReq{Result: constants.CalibrationResultUnqualified}, "tester2")
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != http.StatusConflict {
		t.Fatalf("duplicate register should be 409, got %v", err)
	}
	got, _ := calRepo.FindByID(c.ID)
	if got.Result != constants.CalibrationResultQualified {
		t.Errorf("terminal result changed by duplicate register: %s", got.Result)
	}
	gotDev, _ := devRepo.FindByID(d.ID)
	if gotDev.Status != constants.DeviceStatusInUse {
		t.Errorf("device status changed by rejected duplicate register: %s", gotDev.Status)
	}
}

func TestRecordResultScrappedDeviceRollsBack(t *testing.T) {
	svc, calRepo, devRepo := newCalibrationTestEnv(t)
	d := seedCalibrationDevice(t, devRepo, constants.DeviceStatusScrapped)
	future := time.Now().AddDate(0, 6, 0)
	c := seedCalibrationRecord(t, calRepo, d.ID, future)

	_, err := svc.RecordResult(c.ID, &dto.CalibrationResultReq{Result: constants.CalibrationResultUnqualified}, "tester")
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != http.StatusConflict {
		t.Fatalf("scrapped device unqualified result should conflict, got %v", err)
	}
	// 失败不得部分更新：计量结果回滚。
	got, _ := calRepo.FindByID(c.ID)
	if got.ResultRegisteredAt != nil || got.Status == constants.CalibrationStatusUnqualified {
		t.Errorf("calibration partially updated on failure: registered=%v status=%s", got.ResultRegisteredAt, got.Status)
	}
}

func TestCalibrationListRefreshesStatusBeforeFilter(t *testing.T) {
	svc, calRepo, devRepo := newCalibrationTestEnv(t)
	d := seedCalibrationDevice(t, devRepo, constants.DeviceStatusInUse)

	expiredDate := time.Now().AddDate(0, 0, -10)
	expired := seedCalibrationRecord(t, calRepo, d.ID, expiredDate)
	expired.InstrumentNo = "INS-EXPIRED"
	if err := calRepo.Update(expired); err != nil {
		t.Fatalf("update instrument no: %v", err)
	}
	dueDate := time.Now().AddDate(0, 0, 10)
	due := seedCalibrationRecord(t, calRepo, d.ID, dueDate)
	due.InstrumentNo = "INS-DUE"
	if err := calRepo.Update(due); err != nil {
		t.Fatalf("update instrument no: %v", err)
	}

	// 两条记录初值都是 normal，列表读取时必须先按时点刷新再按状态筛选。
	page, err := svc.List(1, 10, 0, constants.CalibrationStatusExpired)
	if err != nil {
		t.Fatalf("List expired: %v", err)
	}
	if page.Total != 1 {
		t.Errorf("expired total = %d, want 1", page.Total)
	}
	pageDue, err := svc.List(1, 10, 0, constants.CalibrationStatusDue)
	if err != nil || pageDue.Total != 1 {
		t.Errorf("due total = %d err=%v, want 1", pageDue.Total, err)
	}
	// 刷新后可回读：直接查库状态也已落库。
	got, _ := calRepo.FindByID(expired.ID)
	if got.Status != constants.CalibrationStatusExpired {
		t.Errorf("expired status not persisted after list: %s", got.Status)
	}

	if _, err := svc.List(1, 10, 0, "bad_status"); err == nil {
		t.Error("invalid status filter should return error")
	}

	dueList, err := svc.DueList()
	if err != nil {
		t.Fatalf("DueList: %v", err)
	}
	if len(dueList) != 2 {
		t.Errorf("due list len = %d, want 2", len(dueList))
	}
}
