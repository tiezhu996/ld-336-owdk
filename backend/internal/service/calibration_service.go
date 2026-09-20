package service

import (
	"fmt"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/dto"
	"github.com/medasset/medasset/internal/model"
	"github.com/medasset/medasset/internal/repository"
	"github.com/medasset/medasset/internal/util"
	"gorm.io/gorm"
)

// CalibrationService 计量与质控服务。
type CalibrationService struct {
	repo   *repository.CalibrationRepository
	device *repository.DeviceRepository
	audit  *AuditService
	log    *slog.Logger
}

func NewCalibrationService(repo *repository.CalibrationRepository, device *repository.DeviceRepository, audit *AuditService, log *slog.Logger) *CalibrationService {
	return &CalibrationService{repo: repo, device: device, audit: audit, log: log}
}

// startOfDay 返回 t 当日零点的本地时间（计量到期按自然日粒度计算）。
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// calibrationDueBounds 由统一时点推导"已过期/即将到期"边界：
// 下次计量日期 < expiredBefore 为已过期；< dueBefore 为即将到期（含当日及之后 30 天）。
// 列表筛选、到期预警与统计总览共用该边界，保证同一时点口径一致。
func calibrationDueBounds(now time.Time) (expiredBefore, dueBefore time.Time) {
	today := startOfDay(now)
	return today, today.AddDate(0, 0, constants.CalibrationDueWindowDays+1)
}

// deriveCalibrationStatus 计量状态机（与 CalibrationRepository.RefreshStatuses 的 SQL 规则一一对应）：
// 结果不合格始终"不合格"；否则按下次计量日期与统一时点比较得出已过期/即将到期/合格。
func deriveCalibrationStatus(result string, next *time.Time, now time.Time) string {
	if result == constants.CalibrationResultUnqualified {
		return constants.CalibrationStatusUnqualified
	}
	if next == nil {
		return constants.CalibrationStatusNormal
	}
	expiredBefore, dueBefore := calibrationDueBounds(now)
	switch {
	case next.Before(expiredBefore):
		return constants.CalibrationStatusExpired
	case next.Before(dueBefore):
		return constants.CalibrationStatusDue
	default:
		return constants.CalibrationStatusNormal
	}
}

// refreshStatuses 以请求内统一时点重算并持久化全部计量状态，刷新后列表/预警/总览可回读同一结果。
func (s *CalibrationService) refreshStatuses(now time.Time) error {
	changed, err := s.repo.RefreshStatuses(calibrationDueBounds(now))
	if err != nil {
		return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	if changed > 0 {
		s.log.Info(fmt.Sprintf(constants.LogCalibrationStatusRefreshed, util.FormatDateTime(now), changed))
	}
	return nil
}

// Create 建立计量台账。
func (s *CalibrationService) Create(req *dto.CreateCalibrationReq, operator string) (*model.CalibrationRecord, error) {
	taken, err := s.repo.IsInstrumentNoTaken(req.InstrumentNo)
	if err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	if taken {
		return nil, util.NewAppError(http.StatusConflict, constants.MsgDuplicateInstrumentNo, nil)
	}
	d, err := s.device.FindByID(req.DeviceID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(req.DeviceID), nil)
	}
	if err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	now := time.Now()
	last := req.LastCalibrationDate
	if last == nil {
		last = &now
	}
	next := last.AddDate(0, req.CalibrationCycleMonths, 0)
	c := &model.CalibrationRecord{
		InstrumentNo:            req.InstrumentNo,
		DeviceID:                d.ID,
		DeviceName:              d.Name,
		CalibrationCycleMonths:  req.CalibrationCycleMonths,
		LastCalibrationDate:     last,
		NextCalibrationDate:     &next,
		Status:                  deriveCalibrationStatus(constants.CalibrationResultQualified, &next, now),
		Result:                  constants.CalibrationResultQualified,
		CertificateNo:           req.CertificateNo,
		CalibrationOrg:          req.CalibrationOrg,
		Remark:                  req.Remark,
		CreatedBy:               operator,
	}
	if err := s.repo.Create(c); err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, "建立计量台账失败: instrument_no="+req.InstrumentNo, err)
	}
	s.log.Info(fmt.Sprintf(constants.LogCalibrationCreated, c.InstrumentNo, c.DeviceID, c.CalibrationCycleMonths, c.Status))
	s.audit.Record(0, operator, "CREATE", "calibration", util.Uint64String(c.ID), "建立计量台账: "+c.InstrumentNo, operator, "")
	return c, nil
}

// List 分页查询计量记录（先按统一时点刷新状态，使状态筛选与展示可回读）。
func (s *CalibrationService) List(page, pageSize int, deviceID uint, status string) (*util.PageResult, error) {
	if err := s.refreshStatuses(time.Now()); err != nil {
		return nil, err
	}
	list, total, err := s.repo.List(page, pageSize, deviceID, status)
	if err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	return &util.PageResult{List: list, Total: total, Page: page, PageSize: pageSize}, nil
}

// DueList 计量到期预警清单（与列表筛选、统计总览按同一时点刷新后读取同一份持久化状态）。
func (s *CalibrationService) DueList() ([]model.CalibrationRecord, error) {
	if err := s.refreshStatuses(time.Now()); err != nil {
		return nil, err
	}
	list, err := s.repo.ListDue()
	if err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	return list, nil
}

// RecordResult 登记计量结果；不合格自动标记设备禁用。
// 结果登记与设备禁用在同一事务完成：行锁串行化并发登记，当日重复登记冲突拒绝，
// 任一步骤失败整体回滚，保证同一记录只落一个终态、不产生部分更新。
func (s *CalibrationService) RecordResult(id uint, req *dto.CalibrationResultReq, operator string) (*model.CalibrationRecord, error) {
	now := time.Now()
	today := startOfDay(now)
	var updated *model.CalibrationRecord
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		c, err := s.repo.FindByIDForUpdate(tx, id)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "计量记录不存在: id="+util.Uint64String(id), nil)
		}
		if err != nil {
			return err
		}
		// 并发/重复登记只落一个终态：当日已登记过的记录拒绝再次登记。
		if c.ResultRegisteredAt != nil && !c.ResultRegisteredAt.Before(today) {
			return util.NewAppError(http.StatusConflict, constants.MsgDuplicateCalibrationResult, nil)
		}
		next := req.NextCalibrationDate
		if next == nil {
			nextDate := now.AddDate(0, c.CalibrationCycleMonths, 0)
			next = &nextDate
		}
		c.NextCalibrationDate = next
		c.LastCalibrationDate = &now
		c.ResultRegisteredAt = &now
		c.CertificateNo = req.CertificateNo
		c.CalibrationOrg = req.CalibrationOrg
		c.Remark = req.Remark
		c.Result = req.Result
		c.Status = deriveCalibrationStatus(req.Result, next, now)
		if req.Result == constants.CalibrationResultUnqualified {
			// 不合格设备自动标记禁用（同事务，失败整体回滚）。
			if err := s.disableDeviceLocked(tx, c.DeviceID); err != nil {
				return err
			}
		}
		if err := s.repo.UpdateTx(tx, c); err != nil {
			return err
		}
		updated = c
		return nil
	})
	if err != nil {
		return nil, wrapSvcErr(err)
	}
	s.log.Info(fmt.Sprintf(constants.LogCalibrationResult, updated.InstrumentNo, updated.Result, updated.Status, updated.DeviceID))
	s.audit.Record(0, operator, "RESULT", "calibration", util.Uint64String(updated.ID), "登记计量结果: "+updated.InstrumentNo+"="+updated.Result, operator, "")
	return updated, nil
}

// disableDeviceLocked 在事务内锁定设备行并置为已禁用；已禁用/已报废的设备幂等跳过，
// 避免重复登记或设备已终结时出现部分更新/状态回退。
func (s *CalibrationService) disableDeviceLocked(tx *gorm.DB, deviceID uint) error {
	d, err := s.device.FindByIDForUpdate(tx, deviceID)
	if errors.Is(err, repository.ErrNotFound) {
		return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(deviceID), nil)
	}
	if err != nil {
		return err
	}
	if d.Status == constants.DeviceStatusDisabled || d.Status == constants.DeviceStatusScrapped {
		s.log.Warn(fmt.Sprintf(constants.LogCalibrationDeviceSkip, d.ID, d.Status))
		return nil
	}
	return s.device.UpdateStatusTx(tx, deviceID, constants.DeviceStatusDisabled)
}
