package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/basketikun/chatgpt2api/internal/repository"
)

type QuotaError struct {
	Message string
}

func (e *QuotaError) Error() string { return e.Message }

type QuotaReservation struct {
	KeyID  string
	Amount int
}

type QuotaService struct {
	repo *repository.Repository
	mu   sync.Mutex
	zone *time.Location
}

func NewQuotaService(repo *repository.Repository) *QuotaService {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		zone = time.UTC
	}
	return &QuotaService{repo: repo, zone: zone}
}

func (s *QuotaService) Reserve(ctx context.Context, identity Identity, amount int) (*QuotaReservation, error) {
	if amount < 1 {
		return nil, errors.New("图片数量必须是正整数")
	}
	if identity.Role == "admin" {
		return nil, nil
	}
	if identity.ID == "" {
		return nil, &QuotaError{Message: "用户密钥无效"}
	}

	day, week, _, _ := s.periods(time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.repo.MutateAPIKey(ctx, identity.ID, func(item *repository.APIKey) error {
		if !item.Enabled {
			return &QuotaError{Message: "用户密钥已禁用"}
		}
		resetQuotaWindows(item, day, week)

		if item.DailyQuota != nil {
			left := maxInt(0, *item.DailyQuota-item.DailyUsed)
			if left < amount {
				return &QuotaError{Message: fmt.Sprintf("每日额度不足，剩余 %d 张", left)}
			}
		}
		if item.WeeklyQuota != nil {
			left := maxInt(0, *item.WeeklyQuota-item.WeeklyUsed)
			if left < amount {
				return &QuotaError{Message: fmt.Sprintf("每周额度不足，剩余 %d 张", left)}
			}
		}
		if item.TotalQuota != nil && *item.TotalQuota < amount {
			return &QuotaError{Message: fmt.Sprintf("总额度不足，剩余 %d 张", maxInt(0, *item.TotalQuota))}
		}

		item.DailyUsed += amount
		item.WeeklyUsed += amount
		item.TotalUsed += amount
		item.LifetimeUsed += amount
		if item.TotalQuota != nil {
			left := maxInt(0, *item.TotalQuota-amount)
			item.TotalQuota = &left
		}
		return nil
	})
	if errors.Is(err, repository.ErrNotFound) {
		return nil, &QuotaError{Message: "用户密钥不存在"}
	}
	if err != nil {
		return nil, err
	}
	return &QuotaReservation{KeyID: identity.ID, Amount: amount}, nil
}

func (s *QuotaService) Settle(ctx context.Context, reservation *QuotaReservation, actual int) error {
	if reservation == nil || reservation.KeyID == "" || reservation.Amount < 1 {
		return nil
	}
	actual = maxInt(0, minInt(actual, reservation.Amount))
	delta := actual - reservation.Amount
	if delta == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.repo.MutateAPIKey(ctx, reservation.KeyID, func(item *repository.APIKey) error {
		item.DailyUsed = maxInt(0, item.DailyUsed+delta)
		item.WeeklyUsed = maxInt(0, item.WeeklyUsed+delta)
		item.TotalUsed = maxInt(0, item.TotalUsed+delta)
		item.LifetimeUsed = maxInt(0, item.LifetimeUsed+delta)
		if item.TotalQuota != nil && delta < 0 {
			left := *item.TotalQuota - delta
			item.TotalQuota = &left
		}
		return nil
	})
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	return err
}

func (s *QuotaService) Usage(ctx context.Context, identity Identity) (map[string]any, error) {
	if identity.Role == "admin" {
		return map[string]any{
			"lifetime_used": 0,
			"daily":         quotaPeriod(0, nil, nil, time.Time{}),
			"weekly":        quotaPeriod(0, nil, nil, time.Time{}),
			"total":         map[string]any{"used": 0, "limit": nil, "remaining": nil},
			"blocked":       false,
			"reason":        nil,
		}, nil
	}

	day, week, dailyReset, weeklyReset := s.periods(time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.repo.MutateAPIKey(ctx, identity.ID, func(item *repository.APIKey) error {
		resetQuotaWindows(item, day, week)
		return nil
	})
	if errors.Is(err, repository.ErrNotFound) {
		return nil, &QuotaError{Message: "用户密钥不存在"}
	}
	if err != nil {
		return nil, err
	}

	dailyRemaining := remaining(item.DailyQuota, item.DailyUsed)
	weeklyRemaining := remaining(item.WeeklyQuota, item.WeeklyUsed)
	var totalLimit, totalRemaining any
	if item.TotalQuota != nil {
		totalRemaining = *item.TotalQuota
		totalLimit = *item.TotalQuota + item.TotalUsed
	}
	reason := ""
	switch {
	case !item.Enabled:
		reason = "用户密钥已禁用"
	case dailyRemaining != nil && *dailyRemaining == 0:
		reason = "每日额度已用尽"
	case weeklyRemaining != nil && *weeklyRemaining == 0:
		reason = "每周额度已用尽"
	case item.TotalQuota != nil && *item.TotalQuota == 0:
		reason = "总额度已用尽"
	}

	return map[string]any{
		"lifetime_used": item.LifetimeUsed,
		"daily":         quotaPeriod(item.DailyUsed, item.DailyQuota, dailyRemaining, dailyReset),
		"weekly":        quotaPeriod(item.WeeklyUsed, item.WeeklyQuota, weeklyRemaining, weeklyReset),
		"total":         map[string]any{"used": item.TotalUsed, "limit": totalLimit, "remaining": totalRemaining},
		"blocked":       reason != "",
		"reason":        nullableString(reason),
	}, nil
}

func (s *QuotaService) RefreshKey(ctx context.Context, keyID string) (repository.APIKey, error) {
	day, week, _, _ := s.periods(time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repo.MutateAPIKey(ctx, keyID, func(item *repository.APIKey) error {
		resetQuotaWindows(item, day, week)
		return nil
	})
}

func (s *QuotaService) periods(now time.Time) (string, string, time.Time, time.Time) {
	local := now.In(s.zone)
	dayDate := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.zone)
	weekday := (int(dayDate.Weekday()) + 6) % 7
	weekDate := dayDate.AddDate(0, 0, -weekday)
	return dayDate.Format("2006-01-02"), weekDate.Format("2006-01-02"), dayDate.AddDate(0, 0, 1), weekDate.AddDate(0, 0, 7)
}

func resetQuotaWindows(item *repository.APIKey, day, week string) {
	if item.DailyWindowStart != day {
		item.DailyWindowStart, item.DailyUsed = day, 0
	}
	if item.WeeklyWindowStart != week {
		item.WeeklyWindowStart, item.WeeklyUsed = week, 0
	}
}

func remaining(limit *int, used int) *int {
	if limit == nil {
		return nil
	}
	value := maxInt(0, *limit-used)
	return &value
}

func quotaPeriod(used int, limit *int, left *int, reset time.Time) map[string]any {
	var limitValue, remainingValue, resetValue any
	if limit != nil {
		limitValue, remainingValue = *limit, *left
	}
	if !reset.IsZero() {
		resetValue = reset.Format(time.RFC3339)
	}
	return map[string]any{"used": used, "limit": limitValue, "remaining": remainingValue, "reset_at": resetValue}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
