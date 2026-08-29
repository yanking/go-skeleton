package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"gorm.io/gorm"
)

// Merchant 商户配置仓储。
type Merchant struct {
	db *gorm.DB
}

// NewMerchant 构造仓储；db 句柄由装配层（initial）从 pgsql 组件解嵌传入。
func NewMerchant(db *gorm.DB) *Merchant {
	return &Merchant{db: db}
}

// FindByAppID 按 app_id 查询商户配置；未命中返回 ErrRowNotFound。
func (r *Merchant) FindByAppID(ctx context.Context, appID string) (*model.Merchant, error) {
	var m model.Merchant
	if err := r.db.WithContext(ctx).Where("app_id = ?", appID).First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRowNotFound
		}
		return nil, fmt.Errorf("查询商户: %w", err)
	}
	return &m, nil
}

// FindByID 按主键查询商户配置；未命中返回 ErrRowNotFound。
func (r *Merchant) FindByID(ctx context.Context, id int64) (*model.Merchant, error) {
	var m model.Merchant
	if err := r.db.WithContext(ctx).First(&m, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRowNotFound
		}
		return nil, fmt.Errorf("查询商户: %w", err)
	}
	return &m, nil
}
