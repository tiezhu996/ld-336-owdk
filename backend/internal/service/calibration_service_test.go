package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/dto"
	"github.com/medasset/medasset/internal/model"
	"github.com/medasset/medasset/internal/repository"
	"github.com/medasset/medasset/internal/util"
)

// newCalibrationSvc 构建计量服务及依赖仓储。
func newCalibrationSvc(env *testEnv) (*CalibrationService, *repository.CalibrationRepository, *repository.DeviceRepository) {
	calibRepo := repository.NewCalibrationRepository(env.db)
	deviceRepo := repository.NewDeviceRepository(env.db)
	return NewCalibrationService(calibRepo, deviceRepo, env.audit, env.logger), calibRepo, deviceRepo
}

func mustDevice(t *testing.T, repo *repository.DeviceRepository, assetCode string) *model.Device {
	t.Helper()
	d := &model.Device{AssetCode: assetCode, Name: "设备" + assetCode, Status: constants.DeviceStatusInUse, Department: "ICU"}
	if err := repo.Create(d); err != nil {
		t.Fatalf("create device failed: %v", err)
	}
	return d
}

func mustCalibration(t *testing.T, repo *repository.CalibrationRepository, instrumentNo string, deviceID uint, next time.Time) *model.CalibrationRecord {
	t.Helper()
	last := next.AddDate(0, -12, 0)
	c := &model.CalibrationRecord{
		InstrumentNo:           instrumentNo,
		DeviceID:               deviceID,
		DeviceName:             "测试设备",
		CalibrationCycleMonths: 12,
		LastCalibrationDate:    &last,
		NextCalibrationDate:    &next,
		Status:                 constants.CalibrationStatusNormal,
		Result:                 constants.CalibrationResultQualified,
	}
	if err := repo.Create(c); err != nil {
		t.Fatalf("create calibration failed: %v", err)
	}
	return c
}

// TestDeriveCalibrationStatus 计量状态机边界：不合格优先，30 天窗口，逾期。
func TestDeriveCalibrationStatus(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	cases := []struct {
		name   string
		result string
		next   *time.Time
		want   string
	}{
		{"不合格优先于未到期", constants.CalibrationResultUnqualified, ptrTime(now.AddDate(0, 0, 60)), constants.CalibrationStatusUnqualified},
		{"不合格优先于已过期", constants.CalibrationResultUnqualified, ptrTime(now.AddDate(0, 0, -10)), constants.CalibrationStatusUnqualified},
		{"无下次计量日期", constants.CalibrationResultQualified, nil, constants.CalibrationStatusNormal},
		{"昨天已过期", constants.CalibrationResultQualified, ptrTime(now.Add(-day)), constants.CalibrationStatusExpired},
		{"当天为即将到期", constants.CalibrationResultQualified, ptrTime(now), constants.CalibrationStatusDue},
		{"第30天为即将到期", constants.CalibrationResultQualified, ptrTime(startOfDay(now).AddDate(0, 0, constants.CalibrationDueWindowDays)), constants.CalibrationStatusDue},
		{"第31天恢复合格", constants.CalibrationResultQualified, ptrTime(startOfDay(now).AddDate(0, 0, constants.CalibrationDueWindowDays+1)), constants.CalibrationStatusNormal},
		{"远期合格", constants.CalibrationResultQualified, ptrTime(now.AddDate(0, 6, 0)), constants.CalibrationStatusNormal},
	}
	for _, tc := range cases {
		if got := deriveCalibrationStatus(tc.result, tc.next, now); got != tc.want {
			t.Errorf("%s: deriveCalibrationStatus = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestCalibrationListRefreshPersistAndFilter 列表刷新后状态持久化，可按状态筛选回读。
func TestCalibrationListRefreshPersistAndFilter(t *testing.T) {
	env := newTestServiceEnv(t)
	svc, calibRepo, deviceRepo := newCalibrationSvc(env)
	dev := mustDevice(t, deviceRepo, "MA-C-1")
	now := time.Now()

	expired := mustCalibration(t, calibRepo, "JL-E-1", dev.ID, now.AddDate(0, 0, -2))
	due := mustCalibration(t, calibRepo, "JL-D-1", dev.ID, now.AddDate(0, 0, 10))
	normal := mustCalibration(t, calibRepo, "JL-N-1", dev.ID, now.AddDate(0, 0, 90))

	res, err := svc.List(1, 10, 0, "")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if res.Total != 3 {
		t.Fatalf("total = %d, want 3", res.Total)
	}
	// 刷新后可回读：直接从仓储重读，状态已持久化。
	assertStatus := func(id uint, want string) {
		got, err := calibRepo.FindByID(id)
		if err != nil {
			t.Fatalf("FindByID failed: %v", err)
		}
		if got.Status != want {
			t.Errorf("record %d status = %q, want %q", id, got.Status, want)
		}
	}
	assertStatus(expired.ID, constants.CalibrationStatusExpired)
	assertStatus(due.ID, constants.CalibrationStatusDue)
	assertStatus(normal.ID, constants.CalibrationStatusNormal)

	// 列表筛选与刷新后的持久化状态一致。
	expiredOnly, err := svc.List(1, 10, 0, constants.CalibrationStatusExpired)
	if err != nil {
		t.Fatalf("List(expired) failed: %v", err)
	}
	if expiredOnly.Total != 1 {
		t.Errorf("expired total = %d, want 1", expiredOnly.Total)
	}
	dueOnly, err := svc.List(1, 10, 0, constants.CalibrationStatusDue)
	if err != nil {
		t.Fatalf("List(due) failed: %v", err)
	}
	if dueOnly.Total != 1 {
		t.Errorf("due total = %d, want 1", dueOnly.Total)
	}
}

// TestCalibrationDueListAndOverviewConsistency 预警清单与统计总览按同一时点口径一致。
func TestCalibrationDueListAndOverviewConsistency(t *testing.T) {
	env := newTestServiceEnv(t)
	svc, calibRepo, deviceRepo := newCalibrationSvc(env)
	dev := mustDevice(t, deviceRepo, "MA-C-2")
	now := time.Now()

	mustCalibration(t, calibRepo, "JL-E-2", dev.ID, now.AddDate(0, 0, -5))
	mustCalibration(t, calibRepo, "JL-D-2", dev.ID, now.AddDate(0, 0, 20))
	mustCalibration(t, calibRepo, "JL-N-2", dev.ID, now.AddDate(0, 0, 120))
	// 不合格记录即使日期已过期也只显示"不合格"，不计入到期预警。
	unq := mustCalibration(t, calibRepo, "JL-U-2", dev.ID, now.AddDate(0, 0, -5))
	unq.Result = constants.CalibrationResultUnqualified
	unq.Status = constants.CalibrationStatusUnqualified
	if err := calibRepo.Update(unq); err != nil {
		t.Fatalf("update failed: %v", err)
	}

	dueList, err := svc.DueList()
	if err != nil {
		t.Fatalf("DueList failed: %v", err)
	}
	if len(dueList) != 2 {
		t.Fatalf("due list len = %d, want 2", len(dueList))
	}
	for _, c := range dueList {
		if c.Status != constants.CalibrationStatusDue && c.Status != constants.CalibrationStatusExpired {
			t.Errorf("due list contains status %q", c.Status)
		}
	}

	statsSvc := NewStatsService(deviceRepo, repository.NewMaintenanceRepository(env.db),
		calibRepo, repository.NewPurchaseRepository(env.db), env.audit, env.logger)
	overview, err := statsSvc.Overview()
	if err != nil {
		t.Fatalf("Overview failed: %v", err)
	}
	if overview.CalibrationDue != int64(len(dueList)) {
		t.Errorf("overview calibration_due = %d, due list len = %d", overview.CalibrationDue, len(dueList))
	}
	// 不合格记录刷新后仍为不合格。
	after, err := calibRepo.FindByID(unq.ID)
	if err != nil {
		t.Fatalf("FindByID failed: %v", err)
	}
	if after.Status != constants.CalibrationStatusUnqualified {
		t.Errorf("unqualified record status = %q, want unqualified", after.Status)
	}
}

// TestRecordResultUnqualifiedDisablesDevice 不合格登记与设备禁用同一事务生效。
func TestRecordResultUnqualifiedDisablesDevice(t *testing.T) {
	env := newTestServiceEnv(t)
	svc, calibRepo, deviceRepo := newCalibrationSvc(env)
	dev := mustDevice(t, deviceRepo, "MA-C-3")
	rec := mustCalibration(t, calibRepo, "JL-R-1", dev.ID, time.Now().AddDate(0, 0, 90))

	updated, err := svc.RecordResult(rec.ID, &dto.CalibrationResultReq{Result: constants.CalibrationResultUnqualified}, "admin")
	if err != nil {
		t.Fatalf("RecordResult failed: %v", err)
	}
	if updated.Status != constants.CalibrationStatusUnqualified || updated.Result != constants.CalibrationResultUnqualified {
		t.Errorf("status=%q result=%q, want unqualified", updated.Status, updated.Result)
	}
	if updated.ResultRegisteredAt == nil {
		t.Error("result_registered_at should be set")
	}
	// 设备已禁用且可回读。
	d, err := deviceRepo.FindByID(dev.ID)
	if err != nil {
		t.Fatalf("FindByID(device) failed: %v", err)
	}
	if d.Status != constants.DeviceStatusDisabled {
		t.Errorf("device status = %q, want disabled", d.Status)
	}
	// 刷新后不合格状态不被日期规则覆盖。
	if _, err := svc.List(1, 10, 0, ""); err != nil {
		t.Fatalf("List failed: %v", err)
	}
	after, err := calibRepo.FindByID(rec.ID)
	if err != nil {
		t.Fatalf("FindByID failed: %v", err)
	}
	if after.Status != constants.CalibrationStatusUnqualified {
		t.Errorf("status after refresh = %q, want unqualified", after.Status)
	}
}

// TestRecordResultDuplicateConflict 重复登记（含并发重试）只落一个终态，第二次冲突拒绝。
func TestRecordResultDuplicateConflict(t *testing.T) {
	env := newTestServiceEnv(t)
	svc, calibRepo, deviceRepo := newCalibrationSvc(env)
	dev := mustDevice(t, deviceRepo, "MA-C-4")
	rec := mustCalibration(t, calibRepo, "JL-R-2", dev.ID, time.Now().AddDate(0, 0, 90))

	if _, err := svc.RecordResult(rec.ID, &dto.CalibrationResultReq{Result: constants.CalibrationResultQualified}, "admin"); err != nil {
		t.Fatalf("first RecordResult failed: %v", err)
	}
	_, err := svc.RecordResult(rec.ID, &dto.CalibrationResultReq{Result: constants.CalibrationResultUnqualified}, "admin")
	if err == nil {
		t.Fatal("expected duplicate registration conflict")
	}
	appErr, ok := err.(*util.AppError)
	if !ok || appErr.Code != http.StatusConflict {
		t.Fatalf("expected 409 AppError, got %v", err)
	}
	if appErr.Message != constants.MsgDuplicateCalibrationResult {
		t.Errorf("message = %q", appErr.Message)
	}
	// 终态唯一：仍为第一次登记的合格结果，设备未被禁用。
	after, err := calibRepo.FindByID(rec.ID)
	if err != nil {
		t.Fatalf("FindByID failed: %v", err)
	}
	if after.Result != constants.CalibrationResultQualified {
		t.Errorf("result = %q, want qualified (first terminal state)", after.Result)
	}
	d, err := deviceRepo.FindByID(dev.ID)
	if err != nil {
		t.Fatalf("FindByID(device) failed: %v", err)
	}
	if d.Status == constants.DeviceStatusDisabled {
		t.Error("device should not be disabled by rejected duplicate")
	}
}

// TestRecordResultRollbackOnFailure 登记失败整体回滚，不得部分更新。
func TestRecordResultRollbackOnFailure(t *testing.T) {
	env := newTestServiceEnv(t)
	svc, calibRepo, deviceRepo := newCalibrationSvc(env)
	dev := mustDevice(t, deviceRepo, "MA-C-5")
	rec := mustCalibration(t, calibRepo, "JL-R-3", dev.ID, time.Now().AddDate(0, 0, 90))

	// 删除设备使"不合格联动禁用"失败，验证计量记录不被部分更新。
	if err := deviceRepo.Delete(dev.ID); err != nil {
		t.Fatalf("delete device failed: %v", err)
	}
	if _, err := svc.RecordResult(rec.ID, &dto.CalibrationResultReq{Result: constants.CalibrationResultUnqualified}, "admin"); err == nil {
		t.Fatal("expected error when device missing")
	}
	after, err := calibRepo.FindByID(rec.ID)
	if err != nil {
		t.Fatalf("FindByID failed: %v", err)
	}
	if after.Result != constants.CalibrationResultQualified || after.ResultRegisteredAt != nil {
		t.Errorf("record partially updated: result=%q registered_at=%v", after.Result, after.ResultRegisteredAt)
	}
	if after.Status != constants.CalibrationStatusNormal {
		t.Errorf("status = %q, want normal (unchanged)", after.Status)
	}
}
