package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CalibrationRepository 计量台账仓储。
type CalibrationRepository struct {
	db *gorm.DB
}

func NewCalibrationRepository(db *gorm.DB) *CalibrationRepository {
	return &CalibrationRepository{db: db}
}

// DB 返回底层数据库句柄（供 service 层开启事务）。
func (r *CalibrationRepository) DB() *gorm.DB { return r.db }

// Create 创建计量记录。
func (r *CalibrationRepository) Create(c *model.CalibrationRecord) error {
	return r.db.Create(c).Error
}

// FindByID 按 ID 查询。
func (r *CalibrationRepository) FindByID(id uint) (*model.CalibrationRecord, error) {
	var c model.CalibrationRecord
	err := r.db.First(&c, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &c, err
}

// FindByIDForUpdate 在事务中按 ID 加锁查询（SELECT ... FOR UPDATE），
// 用于登记计量结果时串行化并发写入，保证同一记录只落一个终态。
func (r *CalibrationRepository) FindByIDForUpdate(tx *gorm.DB, id uint) (*model.CalibrationRecord, error) {
	var c model.CalibrationRecord
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &c, err
}

// List 分页查询计量记录。
func (r *CalibrationRepository) List(page, pageSize int, deviceID uint, status string) ([]model.CalibrationRecord, int64, error) {
	var list []model.CalibrationRecord
	var total int64
	q := r.db.Model(&model.CalibrationRecord{})
	if deviceID > 0 {
		q = q.Where("device_id = ?", deviceID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error
	return list, total, err
}

// Update 更新计量记录。
func (r *CalibrationRepository) Update(c *model.CalibrationRecord) error {
	return r.db.Save(c).Error
}

// UpdateTx 在指定事务中更新更新计量记录。
func (r *CalibrationRepository) UpdateTx(tx *gorm.DB, c *model.CalibrationRecord) error {
	return tx.Save(c).Error
}

// ListDue 查询已刷新为"即将到期/已过期"的计量记录（计量到期预警）。
// 与列表筛选、统计总览读取同一份持久化状态，保证同一时点口径一致。
func (r *CalibrationRepository) ListDue() ([]model.CalibrationRecord, error) {
	var list []model.CalibrationRecord
	err := r.db.Where("status IN ?", []string{constants.CalibrationStatusDue, constants.CalibrationStatusExpired}).
		Order("next_calibration_date ASC").Find(&list).Error
	return list, err
}

// RefreshStatuses 以统一边界重算并持久化全部计量状态（单条原子 UPDATE，要么全量生效要么不生效）。
// 规则：结果不合格始终 unqualified；下次计量日期早于 expiredBefore 为 expired；
// 早于 dueBefore 为 due；其余为 normal。返回状态发生变化的行数。
func (r *CalibrationRepository) RefreshStatuses(expiredBefore, dueBefore time.Time) (int64, error) {
	res := r.db.Session(&gorm.Session{AllowGlobalUpdate: true}).
		Model(&model.CalibrationRecord{}).
		Update("status", gorm.Expr(`CASE
			WHEN result = ? THEN ?
			WHEN next_calibration_date IS NOT NULL AND next_calibration_date < ? THEN ?
			WHEN next_calibration_date IS NOT NULL AND next_calibration_date < ? THEN ?
			ELSE ? END`,
			constants.CalibrationResultUnqualified, constants.CalibrationStatusUnqualified,
			expiredBefore, constants.CalibrationStatusExpired,
			dueBefore, constants.CalibrationStatusDue,
			constants.CalibrationStatusNormal))
	return res.RowsAffected, res.Error
}

// CountByStatus 按状态统计。
func (r *CalibrationRepository) CountByStatus(status string) (int64, error) {
	var n int64
	err := r.db.Model(&model.CalibrationRecord{}).Where("status = ?", status).Count(&n).Error
	return n, err
}

// IsInstrumentNoTaken 判断InstrumentNo是否已存在。
func (r *CalibrationRepository) IsInstrumentNoTaken(v string) (bool, error) {
	var n int64
	if err := r.db.Model(&model.CalibrationRecord{}).Where("instrument_no = ?", v).Count(&n).Error; err != nil {
		return false, fmt.Errorf("check instrument_no: %w", err)
	}
	return n > 0, nil
}
