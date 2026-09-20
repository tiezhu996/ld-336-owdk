package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/model"
	"github.com/medasset/medasset/internal/util"
	"gorm.io/gorm"
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

// FindByIDTx 在事务中按 ID 查询（调用方须先开启事务，跨 MySQL/SQLite 兼容）。
func (r *CalibrationRepository) FindByIDTx(tx *gorm.DB, id uint) (*model.CalibrationRecord, error) {
	var c model.CalibrationRecord
	err := tx.First(&c, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &c, err
}

// SyncDerivedStatuses 按统一时点把“逾期/即将到期/合格”落库（幂等）。
// 结果为不合格（unqualified）的终态记录永不被覆盖；返回本次各状态刷新行数。
// 列表筛选、到期预警、总览统计在读取前必须先调用本方法，保证同一 now 下口径一致且刷新后可回读。
func (r *CalibrationRepository) SyncDerivedStatuses(now time.Time) (expiredN, dueN, normalN int64, err error) {
	expiredBefore, dueBefore := util.CalibrationDeadlineBounds(now)
	// 三组条件互斥：unqualified 始终排除，按过期 → 即将到期 → 合格依次落库。
	res := r.db.Model(&model.CalibrationRecord{}).
		Where("result <> ? AND next_calibration_date IS NOT NULL AND next_calibration_date < ?",
			constants.CalibrationResultUnqualified, expiredBefore).
		Update("status", constants.CalibrationStatusExpired)
	if res.Error != nil {
		return 0, 0, 0, fmt.Errorf("sync expired calibration status: %w", res.Error)
	}
	expiredN = res.RowsAffected

	res = r.db.Model(&model.CalibrationRecord{}).
		Where("result <> ? AND next_calibration_date IS NOT NULL AND next_calibration_date >= ? AND next_calibration_date < ?",
			constants.CalibrationResultUnqualified, expiredBefore, dueBefore).
		Update("status", constants.CalibrationStatusDue)
	if res.Error != nil {
		return 0, 0, 0, fmt.Errorf("sync due calibration status: %w", res.Error)
	}
	dueN = res.RowsAffected

	res = r.db.Model(&model.CalibrationRecord{}).
		Where("result <> ? AND (next_calibration_date IS NULL OR next_calibration_date >= ?)",
			constants.CalibrationResultUnqualified, dueBefore).
		Update("status", constants.CalibrationStatusNormal)
	if res.Error != nil {
		return 0, 0, 0, fmt.Errorf("sync normal calibration status: %w", res.Error)
	}
	normalN = res.RowsAffected
	return expiredN, dueN, normalN, nil
}

// RegisterResultTx 在事务内原子登记计量结果。
// 仅当当日尚未登记结果（result_registered_at 为空或不在今日）时更新成功；
// 并发或重复登记时 RowsAffected=0，由调用方转 409，保证只能落一个终态。
func (r *CalibrationRepository) RegisterResultTx(tx *gorm.DB, id uint, fields map[string]interface{}) (int64, error) {
	q := tx.Model(&model.CalibrationRecord{}).Where("id = ?", id).
		Where("result_registered_at IS NULL OR DATE(result_registered_at) <> DATE(?)", fields["result_registered_at"])
	res := q.Updates(fields)
	if res.Error != nil {
		return 0, fmt.Errorf("register calibration result: %w", res.Error)
	}
	return res.RowsAffected, nil
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

// ListDue 查询计量到期预警清单（即将到期 + 已过期）。
// 调用方须先执行 SyncDerivedStatuses，按同一时点刷新状态；不合格记录为终态，不进入预警。
func (r *CalibrationRepository) ListDue(now time.Time) ([]model.CalibrationRecord, error) {
	var list []model.CalibrationRecord
	_, dueBefore := util.CalibrationDeadlineBounds(now)
	err := r.db.
		Where("status IN ? AND next_calibration_date IS NOT NULL AND next_calibration_date < ?",
			[]string{constants.CalibrationStatusDue, constants.CalibrationStatusExpired}, dueBefore).
		Order("next_calibration_date ASC").Find(&list).Error
	return list, err
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
