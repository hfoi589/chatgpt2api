package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm/clause"
	"gorm.io/gorm"
)

type APIKey struct {
	ID         string    `gorm:"type:varchar(32);primaryKey"`
	Name       string    `gorm:"type:varchar(255);not null;uniqueIndex:api_key_role_name"`
	Role       string    `gorm:"type:varchar(16);not null;uniqueIndex:api_key_role_name"`
	Key        string    `gorm:"type:varchar(255);not null;uniqueIndex"`
	KeyHash    *string   `gorm:"type:varchar(64);uniqueIndex:api_key_key_hash"`
	Plan       string    `gorm:"type:varchar(32);not null;default:free"`
	Route      string    `gorm:"type:varchar(32);not null;default:web"`
	Enabled    bool      `gorm:"not null"`
	DailyQuota *int
	WeeklyQuota *int
	TotalQuota *int
	DailyUsed int `gorm:"not null;default:0"`
	WeeklyUsed int `gorm:"not null;default:0"`
	TotalUsed int `gorm:"not null;default:0"`
	LifetimeUsed int `gorm:"not null;default:0"`
	DailyWindowStart string `gorm:"size:10"`
	WeeklyWindowStart string `gorm:"size:10"`
	UsageMigrationVersion int `gorm:"not null;default:0"`
	CreatedAt  time.Time `gorm:"not null"`
	LastUsedAt *time.Time
}

func (APIKey) TableName() string { return "api_keys" }

func (r *Repository) APIKeyByValue(ctx context.Context, key string) (APIKey, error) {
	var item APIKey
	if err := r.db.WithContext(ctx).Where("((key_hash IS NULL AND key = ?) OR key_hash = ?) AND enabled = ?", key, hashAPIKey(key), true).First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, err
	}
	return item, nil
}

func hashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (r *Repository) TouchAPIKey(ctx context.Context, id string, usedAt time.Time) error {
	return r.db.WithContext(ctx).Model(&APIKey{}).Where("id = ?", id).Update("last_used_at", usedAt).Error
}

func (r *Repository) APIKeys(ctx context.Context, role string) ([]APIKey, error) {
	var items []APIKey
	err := r.db.WithContext(ctx).Where("role = ?", role).Order("created_at").Find(&items).Error
	return items, err
}

func (r *Repository) APIKeyByID(ctx context.Context, id string) (APIKey, error) {
	var item APIKey
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, err
	}
	return item, nil
}

func (r *Repository) APIKeyExists(ctx context.Context, key string, excludeID string) (bool, error) {
	db := r.db.WithContext(ctx).Model(&APIKey{}).Where("key = ?", key)
	if excludeID != "" {
		db = db.Where("id <> ?", excludeID)
	}
	var count int64
	err := db.Count(&count).Error
	return count > 0, err
}

func (r *Repository) APIKeyNameExists(ctx context.Context, name string, role string, excludeID string) (bool, error) {
	db := r.db.WithContext(ctx).Model(&APIKey{}).Where("name = ? AND role = ?", name, role)
	if excludeID != "" {
		db = db.Where("id <> ?", excludeID)
	}
	var count int64
	err := db.Count(&count).Error
	return count > 0, err
}

func (r *Repository) SaveAPIKey(ctx context.Context, item APIKey) error {
	return r.db.WithContext(ctx).Save(&item).Error
}

func (r *Repository) MutateAPIKey(ctx context.Context, id string, mutate func(*APIKey) error) (APIKey, error) {
	var item APIKey
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&item).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := mutate(&item); err != nil {
			return err
		}
		return tx.Save(&item).Error
	})
	return item, err
}

func (r *Repository) APIKeyByHash(ctx context.Context, keyHash string) (APIKey, error) {
	var item APIKey
	if err := r.db.WithContext(ctx).Where("key_hash = ?", keyHash).First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, err
	}
	return item, nil
}

func (r *Repository) DeleteAPIKey(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Delete(&APIKey{}, "id = ?", id).Error
}
