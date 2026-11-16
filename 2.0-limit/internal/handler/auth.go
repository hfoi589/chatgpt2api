package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

type AuthHandler struct {
	service *service.AuthService
	quotas  *service.QuotaService
	version string
}

func NewAuthHandler(service *service.AuthService, quotas *service.QuotaService, version string) *AuthHandler {
	return &AuthHandler{service: service, quotas: quotas, version: version}
}

func (h *AuthHandler) RegisterLogin(router *gin.Engine) { router.POST("/auth/login", h.login) }

func (h *AuthHandler) RegisterRoutes(router *gin.RouterGroup) {
	router.GET("/auth/users", h.list)
	router.POST("/auth/users", h.create)
	router.POST("/auth/users/:keyID", h.update)
	router.DELETE("/auth/users/:keyID", h.delete)
	router.POST("/auth/users-import", h.importLegacy)
}

func (h *AuthHandler) RegisterUserRoutes(router *gin.RouterGroup) {
	router.GET("/auth/me/usage", h.usage)
}

func (h *AuthHandler) login(c *gin.Context) {
	identity, err := h.service.Authenticate(c.Request.Context(), c.GetHeader("Authorization"), true)
	if err != nil {
		writeError(c, http.StatusUnauthorized, service.ErrUnauthorized)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "version": h.version, "role": identity.Role, "subject_id": identity.ID, "name": identity.Name})
}

func (h *AuthHandler) list(c *gin.Context) {
	items, err := h.service.List(c.Request.Context())
	if err != nil { writeResult(c, nil, err); return }
	for index := range items {
		items[index], err = h.quotas.RefreshKey(c.Request.Context(), items[index].ID)
		if err != nil { writeResult(c, nil, err); return }
	}
	writeResult(c, gin.H{"items": toAPIKeyResponses(items)}, err)
}

func (h *AuthHandler) create(c *gin.Context) {
	var body CreateAPIKeyRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, errors.New("请输入新的专用密钥"))
		return
	}
	if err := validateQuotas(body.DailyQuota, body.WeeklyQuota, body.TotalQuota); err != nil {
		writeError(c, http.StatusBadRequest, err)
		return
	}
	item, err := h.service.Create(c.Request.Context(), body.Name, body.Key, body.Plan, body.Route, body.DailyQuota, body.WeeklyQuota, body.TotalQuota)
	if err != nil {
		writeError(c, http.StatusBadRequest, err)
		return
	}
	items, err := h.service.List(c.Request.Context())
	writeResult(c, gin.H{"item": toAPIKeyResponse(item), "items": toAPIKeyResponses(items)}, err)
}

func (h *AuthHandler) update(c *gin.Context) {
	var body UpdateAPIKeyRequest
	if err := c.ShouldBindJSON(&body); err != nil || body.Name == nil && body.Enabled == nil && body.Key == nil && len(body.Plan) == 0 && len(body.Route) == 0 && !body.DailyQuota.Present && !body.WeeklyQuota.Present && !body.TotalQuota.Present {
		writeError(c, http.StatusBadRequest, errors.New("还没有检测到改动，请修改后再保存"))
		return
	}
	if err := validateQuotaValues(body.DailyQuota.Value, body.WeeklyQuota.Value, body.TotalQuota.Value); err != nil {
		writeError(c, http.StatusBadRequest, err)
		return
	}
	quotaFields := map[string]bool{"daily_quota": body.DailyQuota.Present, "weekly_quota": body.WeeklyQuota.Present, "total_quota": body.TotalQuota.Present}
	item, err := h.service.Update(c.Request.Context(), c.Param("keyID"), body.Name, body.Enabled, body.Key, body.Plan, body.Route, body.DailyQuota.Value, body.WeeklyQuota.Value, body.TotalQuota.Value, quotaFields)
	if errors.Is(err, repository.ErrNotFound) {
		writeError(c, http.StatusNotFound, errors.New("这条用户密钥不存在，可能已经被删除"))
		return
	}
	if err != nil {
		writeError(c, http.StatusBadRequest, err)
		return
	}
	items, err := h.service.List(c.Request.Context())
	writeResult(c, gin.H{"item": toAPIKeyResponse(item), "items": toAPIKeyResponses(items)}, err)
}

func (h *AuthHandler) usage(c *gin.Context) {
	usage, err := h.quotas.Usage(c.Request.Context(), identity(c))
	writeResult(c, usage, err)
}

func (h *AuthHandler) importLegacy(c *gin.Context) {
	var raw json.RawMessage
	if err := c.ShouldBindJSON(&raw); err != nil {
		writeError(c, http.StatusBadRequest, errors.New("无效的 1.8 用户密钥备份文件"))
		return
	}
	var items []service.LegacyAPIKey
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			writeError(c, http.StatusBadRequest, errors.New("无效的 1.8 用户密钥备份文件"))
			return
		}
	} else {
		var snapshot struct { Items []service.LegacyAPIKey `json:"items"` }
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			writeError(c, http.StatusBadRequest, errors.New("无效的 1.8 用户密钥备份文件"))
			return
		}
		items = snapshot.Items
	}
	if len(items) == 0 {
		writeError(c, http.StatusBadRequest, errors.New("备份中没有可导入的用户密钥"))
		return
	}
	imported, skipped, err := h.service.ImportLegacy(c.Request.Context(), items)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err)
		return
	}
	current, err := h.service.List(c.Request.Context())
	writeResult(c, gin.H{"imported": imported, "skipped": skipped, "items": toAPIKeyResponses(current)}, err)
}

func (h *AuthHandler) delete(c *gin.Context) {
	err := h.service.Delete(c.Request.Context(), c.Param("keyID"))
	if errors.Is(err, repository.ErrNotFound) {
		writeError(c, http.StatusNotFound, errors.New("这条用户密钥不存在，可能已经被删除"))
		return
	}
	items, listErr := h.service.List(c.Request.Context())
	if err == nil {
		err = listErr
	}
	writeResult(c, gin.H{"items": toAPIKeyResponses(items)}, err)
}

func toAPIKeyResponse(item repository.APIKey) APIKeyResponse {
	key := item.Key
	if item.KeyHash != nil { key = "" }
	return APIKeyResponse{ID: item.ID, Name: item.Name, Role: item.Role, Key: key, Plan: strings.Split(item.Plan, ","), Route: strings.Split(item.Route, ","), Enabled: item.Enabled, CreatedAt: item.CreatedAt, LastUsedAt: item.LastUsedAt, DailyQuota: item.DailyQuota, WeeklyQuota: item.WeeklyQuota, TotalQuota: item.TotalQuota, DailyUsed: item.DailyUsed, WeeklyUsed: item.WeeklyUsed, TotalUsed: item.TotalUsed, LifetimeUsed: item.LifetimeUsed}
}

func toAPIKeyResponses(items []repository.APIKey) []APIKeyResponse {
	result := make([]APIKeyResponse, 0, len(items))
	for _, item := range items {
		result = append(result, toAPIKeyResponse(item))
	}
	return result
}

type APIKeyResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	Key        string     `json:"key"`
	Plan       []string   `json:"plan"`
	Route      []string   `json:"route"`
	Enabled    bool       `json:"enabled"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	DailyQuota *int       `json:"daily_quota"`
	WeeklyQuota *int      `json:"weekly_quota"`
	TotalQuota *int       `json:"total_quota"`
	DailyUsed int         `json:"daily_used"`
	WeeklyUsed int        `json:"weekly_used"`
	TotalUsed int         `json:"total_used"`
	LifetimeUsed int      `json:"lifetime_used"`
}
type CreateAPIKeyRequest struct {
	Name  string   `json:"name"`
	Key   string   `json:"key" binding:"required"`
	Plan  []string `json:"plan" binding:"required,min=1,dive,oneof=free plus team pro"`
	Route []string `json:"route" binding:"required,min=1,dive,oneof=web codex"`
	DailyQuota *int `json:"daily_quota"`
	WeeklyQuota *int `json:"weekly_quota"`
	TotalQuota *int `json:"total_quota"`
}
type UpdateAPIKeyRequest struct {
	Name    *string `json:"name"`
	Enabled *bool   `json:"enabled"`
	Key     *string `json:"key"`
	Plan    []string `json:"plan" binding:"omitempty,dive,oneof=free plus team pro"`
	Route   []string `json:"route" binding:"omitempty,dive,oneof=web codex"`
	DailyQuota NullableInt `json:"daily_quota"`
	WeeklyQuota NullableInt `json:"weekly_quota"`
	TotalQuota NullableInt `json:"total_quota"`
}

type NullableInt struct { Present bool; Value *int }
func (v *NullableInt) UnmarshalJSON(data []byte) error {
	v.Present = true
	if string(data) == "null" { v.Value = nil; return nil }
	var parsed int
	if err := json.Unmarshal(data, &parsed); err != nil { return err }
	v.Value = &parsed
	return nil
}
func validateQuotas(values ...*int) error { return validateQuotaValues(values...) }
func validateQuotaValues(values ...*int) error {
	for _, value := range values { if value != nil && *value < 0 { return fmt.Errorf("额度必须是非负整数，留空表示不限制") } }
	return nil
}
