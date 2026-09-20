package service

import (
	"errors"
	"fmt"
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
	// 新建台账初始视为合格，到期状态按统一时点推导（30 天内即将到期、逾期已过期）。
	c := &model.CalibrationRecord{
		InstrumentNo:           req.InstrumentNo,
		DeviceID:               d.ID,
		DeviceName:             d.Name,
		CalibrationCycleMonths: req.CalibrationCycleMonths,
		LastCalibrationDate:    last,
		NextCalibrationDate:    &next,
		Status:                 util.DeriveCalibrationStatus(constants.CalibrationResultQualified, &next, now),
		Result:                 constants.CalibrationResultQualified,
		CertificateNo:          req.CertificateNo,
		CalibrationOrg:         req.CalibrationOrg,
		Remark:                 req.Remark,
		CreatedBy:              operator,
	}
	if err := s.repo.Create(c); err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, "建立计量台账失败: instrument_no="+req.InstrumentNo, err)
	}
	s.log.Info(fmt.Sprintf(constants.LogCalibrationCreated, c.InstrumentNo, c.DeviceID, c.CalibrationCycleMonths, c.Status))
	s.audit.Record(0, operator, "CREATE", "calibration", util.Uint64String(c.ID), "建立计量台账: "+c.InstrumentNo, operator, "")
	return c, nil
}

// syncStatuses 按同一时点刷新计量状态（列表筛选、预警、总览共用），落库后刷新可回读。
func (s *CalibrationService) syncStatuses(now time.Time) error {
	expiredN, dueN, normalN, err := s.repo.SyncDerivedStatuses(now)
	if err != nil {
		return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	if expiredN+dueN+normalN > 0 {
		s.log.Info(fmt.Sprintf(constants.LogCalibrationStatusSynced,
			expiredN, dueN, normalN, util.FormatDateTime(now)))
	}
	return nil
}

// validCalibrationStatus 校验列表筛选状态值（状态机收口于 service）。
func validCalibrationStatus(status string) bool {
	switch status {
	case "", constants.CalibrationStatusNormal, constants.CalibrationStatusDue,
		constants.CalibrationStatusExpired, constants.CalibrationStatusUnqualified:
		return true
	}
	return false
}

// List 分页查询计量记录；先按统一时点刷新状态，再按刷新后的状态筛选。
func (s *CalibrationService) List(page, pageSize int, deviceID uint, status string) (*util.PageResult, error) {
	if !validCalibrationStatus(status) {
		return nil, util.NewAppError(http.StatusBadRequest, constants.MsgInvalidCalibrationStatus+": status="+status, nil)
	}
	now := time.Now()
	if err := s.syncStatuses(now); err != nil {
		return nil, err
	}
	list, total, err := s.repo.List(page, pageSize, deviceID, status)
	if err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	return &util.PageResult{List: list, Total: total, Page: page, PageSize: pageSize}, nil
}

// DueList 计量到期预警清单（被列表页与统计接口复用）；与列表筛选按同一时点计算。
func (s *CalibrationService) DueList() ([]model.CalibrationRecord, error) {
	now := time.Now()
	if err := s.syncStatuses(now); err != nil {
		return nil, err
	}
	list, err := s.repo.ListDue(now)
	if err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	return list, nil
}

// RecordResult 登记计量结果；不合格自动标记设备禁用。
// 计量结果更新与设备禁用在同一事务内完成；并发或当日重复登记只能落一个终态，任一失败全部回滚。
func (s *CalibrationService) RecordResult(id uint, req *dto.CalibrationResultReq, operator string) (*model.CalibrationRecord, error) {
	var updated *model.CalibrationRecord
	var conflictInstrumentNo string
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		// 事务内先读记录（不存在直接 404），再用条件 UPDATE 抢占今日唯一登记权。
		c, err := s.repo.FindByIDTx(tx, id)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "计量记录不存在: id="+util.Uint64String(id), nil)
		}
		if err != nil {
			return err
		}
		conflictInstrumentNo = c.InstrumentNo
		if c.ResultRegisteredAt != nil && util.SameCalendarDay(*c.ResultRegisteredAt, time.Now()) {
			return util.NewAppError(http.StatusConflict, constants.MsgDuplicateCalibrationResult+": id="+util.Uint64String(id), nil)
		}

		now := time.Now()
		next := req.NextCalibrationDate
		if next == nil {
			nextDate := now.AddDate(0, c.CalibrationCycleMonths, 0)
			next = &nextDate
		}
		status := util.DeriveCalibrationStatus(req.Result, next, now)
		fields := map[string]interface{}{
			"last_calibration_date":  &now,
			"next_calibration_date":  next,
			"result":                 req.Result,
			"status":                 status,
			"certificate_no":         req.CertificateNo,
			"calibration_org":        req.CalibrationOrg,
			"remark":                 req.Remark,
			"result_registered_at":   &now,
		}
		// 原子条件更新：并发重复登记时只有一个事务 RowsAffected=1，其余转 409 回滚。
		affected, err := s.repo.RegisterResultTx(tx, id, fields)
		if err != nil {
			return err
		}
		if affected == 0 {
			return util.NewAppError(http.StatusConflict, constants.MsgDuplicateCalibrationResult+": id="+util.Uint64String(id), nil)
		}

		if req.Result == constants.CalibrationResultUnqualified {
			// 不合格设备在同一事务内标记禁用；失败则结果登记一并回滚，杜绝部分更新。
			changed, err := s.device.DisableForCalibrationTx(tx, c.DeviceID)
			if err != nil {
				if errors.Is(err, repository.ErrDeviceScrapped) {
					return util.NewAppError(http.StatusConflict, constants.MsgDeviceInScrapped+": device_id="+util.Uint64String(c.DeviceID), err)
				}
				return err
			}
			if changed {
				s.log.Info(fmt.Sprintf(constants.LogDeviceStatusChanged, c.DeviceID, c.Status, constants.DeviceStatusDisabled))
			}
		}

		updated, err = s.repo.FindByIDTx(tx, id)
		return err
	})
	if err != nil {
		var appErr *util.AppError
		if errors.As(err, &appErr) && appErr.Code == http.StatusConflict {
			s.log.Warn(fmt.Sprintf(constants.LogCalibrationResultDuplicate,
				id, conflictInstrumentNo, operator, util.FormatDateTime(time.Now())))
		}
		return nil, wrapSvcErr(err)
	}
	s.log.Info(fmt.Sprintf(constants.LogCalibrationResult, updated.InstrumentNo, updated.Result, updated.Status, updated.DeviceID))
	s.audit.Record(0, operator, "RESULT", "calibration", util.Uint64String(updated.ID),
		"登记计量结果: "+updated.InstrumentNo+"="+updated.Result, operator, "")
	return updated, nil
}
