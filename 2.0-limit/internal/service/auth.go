package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"crypto/subtle"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/google/uuid"
)

var ErrUnauthorized = errors.New("密钥无效或已失效，请重新登录")

type Identity struct {
	ID    string
	Name  string
	Role  string
	Plans  []string
	Routes []string
}

type AuthService struct {
	repo     *repository.Repository
	adminKey string
	mu       sync.RWMutex
	keys     map[string]cachedKey
}

type cachedKey struct {
	item      repository.APIKey
	expiresAt time.Time
}

func NewAuthService(repo *repository.Repository, adminKey string) *AuthService {
	return &AuthService{repo: repo, adminKey: adminKey, keys: map[string]cachedKey{}}
}

func (s *AuthService) apiKey(ctx context.Context, key string) (repository.APIKey, error) {
	s.mu.RLock()
	cached, ok := s.keys[key]
	s.mu.RUnlock()
	if ok && time.Now().Before(cached.expiresAt) {
		return cached.item, nil
	}
	item, err := s.repo.APIKeyByValue(ctx, key)
	if err != nil {
		return item, err
	}
	s.mu.Lock()
	s.keys[key] = cachedKey{item: item, expiresAt: time.Now().Add(30 * time.Second)}
	s.mu.Unlock()
	return item, nil
}

func (s *AuthService) clearKeys() {
	s.mu.Lock()
	clear(s.keys)
	s.mu.Unlock()
}

func (s *AuthService) Authenticate(ctx context.Context, authorization string, touch bool) (Identity, error) {
	scheme, key, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || key == "" {
		return Identity{}, ErrUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.adminKey)) == 1 {
		return Identity{ID: "admin", Name: "管理员", Role: "admin", Routes: []string{"web"}}, nil
	}
	item, err := s.apiKey(ctx, key)
	if errors.Is(err, repository.ErrNotFound) {
		return Identity{}, ErrUnauthorized
	}
	if err != nil {
		return Identity{}, err
	}
	if touch {
		if err := s.repo.TouchAPIKey(ctx, item.ID, time.Now().UTC()); err != nil {
			return Identity{}, err
		}
	}
	return Identity{ID: item.ID, Name: item.Name, Role: item.Role, Plans: strings.Split(item.Plan, ","), Routes: strings.Split(item.Route, ",")}, nil
}

func (s *AuthService) List(ctx context.Context) ([]repository.APIKey, error) {
	return s.repo.APIKeys(ctx, "user")
}

func (s *AuthService) Create(ctx context.Context, name string, key string, plan []string, route []string, dailyQuota, weeklyQuota, totalQuota *int) (repository.APIKey, error) {
	if key == "" {
		return repository.APIKey{}, errors.New("请输入新的专用密钥")
	}
	if key == s.adminKey {
		return repository.APIKey{}, errors.New("这个密钥和管理员密钥冲突了，请换一个新的密钥")
	}
	exists, err := s.repo.APIKeyExists(ctx, key, "")
	if err != nil {
		return repository.APIKey{}, err
	}
	if exists {
		return repository.APIKey{}, errors.New("这个专用密钥已经存在，请换一个新的密钥")
	}
	hash := apiKeyHash(key)
	if _, err := s.repo.APIKeyByHash(ctx, hash); err == nil {
		return repository.APIKey{}, errors.New("这个专用密钥已经存在，请换一个新的密钥")
	} else if !errors.Is(err, repository.ErrNotFound) {
		return repository.APIKey{}, err
	}
	name, err = s.availableName(ctx, name, "")
	if err != nil {
		return repository.APIKey{}, err
	}
	item := repository.APIKey{ID: strings.ReplaceAll(uuid.NewString(), "-", ""), Name: name, Role: "user", Key: key, Plan: strings.Join(plan, ","), Route: strings.Join(route, ","), Enabled: true, DailyQuota: dailyQuota, WeeklyQuota: weeklyQuota, TotalQuota: totalQuota, CreatedAt: time.Now().UTC()}
	return item, s.repo.SaveAPIKey(ctx, item)
}

func (s *AuthService) Update(ctx context.Context, id string, name *string, enabled *bool, key *string, plan []string, route []string, dailyQuota, weeklyQuota, totalQuota *int, quotaFields map[string]bool) (repository.APIKey, error) {
	item, err := s.repo.APIKeyByID(ctx, id)
	if err != nil {
		return repository.APIKey{}, err
	}
	if item.Role != "user" {
		return repository.APIKey{}, repository.ErrNotFound
	}
	if name != nil {
		item.Name, err = s.availableName(ctx, *name, id)
		if err != nil {
			return repository.APIKey{}, err
		}
	}
	if key != nil {
		if *key == "" || *key == s.adminKey {
			return repository.APIKey{}, errors.New("请输入与管理员密钥不同的新密钥")
		}
		exists, err := s.repo.APIKeyExists(ctx, *key, id)
		if err != nil {
			return repository.APIKey{}, err
		}
		if exists {
			return repository.APIKey{}, errors.New("这个专用密钥已经存在，请换一个新的密钥")
		}
		hash := apiKeyHash(*key)
		if duplicate, err := s.repo.APIKeyByHash(ctx, hash); err == nil && duplicate.ID != id {
			return repository.APIKey{}, errors.New("这个专用密钥已经存在，请换一个新的密钥")
		} else if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return repository.APIKey{}, err
		}
		item.Key = *key
		item.KeyHash = nil
	}
	if enabled != nil {
		item.Enabled = *enabled
	}
	if len(plan) > 0 {
		item.Plan = strings.Join(plan, ",")
	}
	if len(route) > 0 {
		item.Route = strings.Join(route, ",")
	}
	if quotaFields["daily_quota"] { item.DailyQuota = dailyQuota }
	if quotaFields["weekly_quota"] { item.WeeklyQuota = weeklyQuota }
	if quotaFields["total_quota"] { item.TotalQuota = totalQuota }
	err = s.repo.SaveAPIKey(ctx, item)
	s.clearKeys()
	return item, err
}

func apiKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type LegacyAPIKey struct {
	ID string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
	KeyHash string `json:"key_hash"`
	Enabled bool `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	DailyQuota *int `json:"daily_quota"`
	WeeklyQuota *int `json:"weekly_quota"`
	TotalQuota *int `json:"total_quota"`
	DailyUsed int `json:"daily_used"`
	WeeklyUsed int `json:"weekly_used"`
	TotalUsed int `json:"total_used"`
	LifetimeUsed int `json:"lifetime_used"`
	DailyWindowStart string `json:"daily_window_start"`
	WeeklyWindowStart string `json:"weekly_window_start"`
	UsageMigrationVersion int `json:"usage_migration_version"`
}

func (s *AuthService) ImportLegacy(ctx context.Context, items []LegacyAPIKey) (int, int, error) {
	imported, skipped := 0, 0
	for _, old := range items {
		keyHash := strings.ToLower(strings.TrimSpace(old.KeyHash))
		if old.Role != "user" || len(keyHash) != 64 {
			skipped++
			continue
		}
		if _, err := hex.DecodeString(keyHash); err != nil { skipped++; continue }
		if old.ID == "" { old.ID = strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }
		if len(old.ID) > 32 { old.ID = old.ID[:32] }
		if old.Name == "" { old.Name = "普通用户" }
		if old.CreatedAt.IsZero() { old.CreatedAt = time.Now().UTC() }
		if old.DailyUsed < 0 { old.DailyUsed = 0 }
		if old.WeeklyUsed < 0 { old.WeeklyUsed = 0 }
		if old.TotalUsed < 0 { old.TotalUsed = 0 }
		if old.LifetimeUsed < 0 { old.LifetimeUsed = 0 }
		if old.DailyQuota != nil && *old.DailyQuota < 0 || old.WeeklyQuota != nil && *old.WeeklyQuota < 0 || old.TotalQuota != nil && *old.TotalQuota < 0 { skipped++; continue }
		if _, err := s.repo.APIKeyByID(ctx, old.ID); err == nil { skipped++; continue } else if !errors.Is(err, repository.ErrNotFound) { return imported, skipped, err }
		if _, err := s.repo.APIKeyByHash(ctx, keyHash); err == nil { skipped++; continue } else if !errors.Is(err, repository.ErrNotFound) { return imported, skipped, err }
		if exists, err := s.repo.APIKeyNameExists(ctx, old.Name, "user", ""); err != nil { return imported, skipped, err } else if exists { skipped++; continue }
		placeholder := "legacy-" + old.ID
		item := repository.APIKey{ID: old.ID, Name: old.Name, Role: "user", Key: placeholder, KeyHash: &keyHash, Plan: "free,plus,team,pro", Route: "web,codex", Enabled: old.Enabled, DailyQuota: old.DailyQuota, WeeklyQuota: old.WeeklyQuota, TotalQuota: old.TotalQuota, DailyUsed: old.DailyUsed, WeeklyUsed: old.WeeklyUsed, TotalUsed: old.TotalUsed, LifetimeUsed: old.LifetimeUsed, DailyWindowStart: old.DailyWindowStart, WeeklyWindowStart: old.WeeklyWindowStart, UsageMigrationVersion: old.UsageMigrationVersion, CreatedAt: old.CreatedAt, LastUsedAt: old.LastUsedAt}
		if err := s.repo.SaveAPIKey(ctx, item); err != nil { return imported, skipped, err }
		imported++
	}
	s.clearKeys()
	return imported, skipped, nil
}

func (s *AuthService) Delete(ctx context.Context, id string) error {
	item, err := s.repo.APIKeyByID(ctx, id)
	if err != nil {
		return err
	}
	if item.Role != "user" {
		return repository.ErrNotFound
	}
	err = s.repo.DeleteAPIKey(ctx, id)
	s.clearKeys()
	return err
}

func (s *AuthService) availableName(ctx context.Context, name string, excludeID string) (string, error) {
	if name != "" {
		exists, err := s.repo.APIKeyNameExists(ctx, name, "user", excludeID)
		if err != nil {
			return "", err
		}
		if exists {
			return "", errors.New("这个名称已经在使用中了，换一个更容易区分的名称吧")
		}
		return name, nil
	}
	name = "普通用户"
	for suffix := 2; ; suffix++ {
		exists, err := s.repo.APIKeyNameExists(ctx, name, "user", excludeID)
		if err != nil || !exists {
			return name, err
		}
		name = "普通用户 " + strconv.Itoa(suffix)
	}
}
