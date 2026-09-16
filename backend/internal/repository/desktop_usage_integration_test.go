//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestDesktopUsageHeadersArePrivateAndRequestScoped(t *testing.T) {
	r := newDesktopCredentialRig(t)
	lease := r.issue(t, "fixture-a")
	key, err := r.keys.GetByKey(r.ctx, lease.APIKey)
	require.NoError(t, err)
	ordinary, err := r.keys.Create(r.ctx, r.userID, service.CreateAPIKeyRequest{Name: "usage-ordinary", GroupID: key.GroupID})
	require.NoError(t, err)
	r.router.POST("/v1/messages", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(r.keys, nil, &config.Config{})), func(c *gin.Context) {
		for name := range c.Request.Header {
			require.False(t, strings.HasPrefix(strings.ToLower(name), "x-aino-"), "private header forwarded: %s", name)
		}
		c.JSON(200, gin.H{"session_id": service.ExtractClientSessionID(c)})
	})
	session, turn, call := uuid.NewString(), uuid.NewString(), uuid.NewString()
	headers := map[string]string{"X-Aino-Session-Id": session, "X-Aino-Turn-Id": turn, "X-Aino-Call-Id": call, "X-Aino-Purpose": "chat", "X-Aino-User-Id": "999"}
	request := func(token string, values map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"fixture-a"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		for name, value := range values {
			req.Header.Set(name, value)
		}
		w := httptest.NewRecorder()
		r.router.ServeHTTP(w, req)
		return w
	}
	w := request(lease.APIKey, headers)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, session, body["session_id"])
	// The cached key must not retain metadata from a previous request.
	w = request(lease.APIKey, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"session_id":""}`, w.Body.String())
	headers["X-Aino-Purpose"] = "arbitrary-user-controlled-purpose"
	require.Equal(t, http.StatusBadRequest, request(lease.APIKey, headers).Code)
	headers["X-Aino-Purpose"] = "chat"
	headers["X-Aino-Call-Id"] = "not-a-uuid"
	require.Equal(t, http.StatusBadRequest, request(lease.APIKey, headers).Code)
	// Ordinary keys cannot opt into desktop attribution, even with valid headers.
	headers["X-Aino-Call-Id"] = call
	w = request(ordinary.Key, headers)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"session_id":""}`, w.Body.String())
}

func TestDesktopUsageDoesNotLeakAcrossUsersWithSameTurnID(t *testing.T) {
	r := newDesktopCredentialRig(t)
	lease := r.issue(t, "fixture-a")
	key, err := r.keys.GetByKey(r.ctx, lease.APIKey)
	require.NoError(t, err)
	other := r.user(t, service.StatusActive, false)
	otherKey, err := r.client.APIKey.Create().SetUserID(other.ID).SetKey("sk-" + uuid.NewString()).SetName("other").Save(r.ctx)
	require.NoError(t, err)
	account, err := r.client.Account.Create().SetName("desktop-usage-fixture").SetPlatform(service.PlatformOpenAI).SetType(service.AccountTypeAPIKey).SetCredentials(map[string]any{}).Save(r.ctx)
	require.NoError(t, err)
	db := repository.GetIntegrationDB()
	logs := repository.NewUsageLogRepository(r.client, db)
	billing := repository.NewUsageBillingRepository(r.client, db)
	h := handler.NewUsageHandler(service.NewUsageService(logs, r.userRepo, r.client, nil), r.keys, nil, r.settings)
	auth := middleware.NewJWTAuthMiddleware(r.auth, r.users, r.settings, nil)
	r.router.GET("/api/v1/usage", gin.HandlerFunc(auth), h.List)
	r.router.GET("/api/v1/usage/stats", gin.HandlerFunc(auth), h.Stats)
	session, turn, call, purpose := uuid.NewString(), uuid.NewString(), uuid.NewString(), "chat"
	var before string
	require.NoError(t, db.QueryRowContext(r.ctx, "SELECT balance::text FROM users WHERE id=$1", r.userID).Scan(&before))
	for i, row := range []struct {
		userID, keyID int64
		cost          float64
		turnID        string
	}{
		{r.userID, key.ID, 0.010000005, turn},
		{other.ID, otherKey.ID, 0.99, turn},
		{r.userID, key.ID, 0.02, uuid.NewString()},
	} {
		requestID := uuid.NewString()
		cmd := &service.UsageBillingCommand{RequestID: requestID, UserID: row.userID, APIKeyID: row.keyID, AccountID: account.ID, Model: "fixture-a", BalanceCost: row.cost}
		result, err := billing.Apply(r.ctx, cmd)
		require.NoError(t, err)
		require.True(t, result.Applied)
		result, err = billing.Apply(r.ctx, cmd)
		require.NoError(t, err)
		require.False(t, result.Applied, "the existing request/key dedup must still prevent double billing")
		log := &service.UsageLog{UserID: row.userID, APIKeyID: row.keyID, AccountID: account.ID, RequestID: requestID, Model: "fixture-a", ActualCost: cmd.BalanceCost, SessionID: &session, DesktopTurnID: &row.turnID, DesktopCallID: &call, DesktopPurpose: &purpose, SettlementStatus: "settled", CreatedAt: time.Now()}
		if i == 1 {
			err = logs.(interface {
				CreateBestEffort(context.Context, *service.UsageLog) error
			}).CreateBestEffort(r.ctx, log)
		} else {
			_, err = logs.Create(r.ctx, log)
		}
		require.NoError(t, err)
	}
	var after string
	require.NoError(t, db.QueryRowContext(r.ctx, "SELECT balance::text FROM users WHERE id=$1", r.userID).Scan(&after))
	beforeDecimal, err := decimal.NewFromString(before)
	require.NoError(t, err)
	afterDecimal, err := decimal.NewFromString(after)
	require.NoError(t, err)
	require.Equal(t, "0.03000001", beforeDecimal.Sub(afterDecimal).StringFixed(8))
	for _, query := range []string{"desktop_turn_id=" + turn, "desktop_turn_id=" + turn + "&desktop_call_id=" + call + "&session_id=" + session + "&desktop_purpose=chat"} {
		w := r.request("GET", "/api/v1/usage?"+query+"&user_id="+fmt.Sprint(other.ID), "", r.token)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var body struct {
			Data struct {
				Items []map[string]any `json:"items"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Len(t, body.Data.Items, 1)
		item := body.Data.Items[0]
		require.Equal(t, "0.01000001", item["actual_cost_decimal"])
		require.Equal(t, "settled", item["settlement_status"])
		require.Equal(t, turn, item["desktop_turn_id"])
		require.Equal(t, call, item["desktop_call_id"])
		require.Equal(t, "USD", item["currency"])
		w = r.request("GET", "/api/v1/usage/stats?"+query, "", r.token)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var stats struct {
			Data struct {
				TotalRequests   int     `json:"total_requests"`
				TotalActualCost float64 `json:"total_actual_cost"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &stats))
		require.Equal(t, 1, stats.Data.TotalRequests)
		require.Equal(t, 0.01000001, stats.Data.TotalActualCost)
	}
	require.Equal(t, http.StatusBadRequest, r.request("GET", "/api/v1/usage?desktop_turn_id=invalid", "", r.token).Code)
	require.Equal(t, http.StatusForbidden, r.request("GET", fmt.Sprintf("/api/v1/usage?api_key_id=%d", otherKey.ID), "", r.token).Code)
}

func TestDesktopUsageAuthenticationToLedger(t *testing.T) {
	r := newDesktopCredentialRig(t)
	lease := r.issue(t, "fixture-a")
	db := repository.GetIntegrationDB()
	logs := repository.NewUsageLogRepository(r.client, db)
	account, err := r.client.Account.Create().SetName("usage-real-path").SetPlatform(service.PlatformOpenAI).SetType(service.AccountTypeAPIKey).SetCredentials(map[string]any{}).Save(r.ctx)
	require.NoError(t, err)
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	cache := service.NewBillingCacheService(nil, r.userRepo, r.subs, nil, nil, r.rates, cfg, nil)
	t.Cleanup(cache.Stop)
	usage := service.NewGatewayService(nil, r.groups, logs, repository.NewUsageBillingRepository(r.client, db),
		r.userRepo, r.subs, r.rates, nil, cfg, nil, nil, r.billing, nil, cache, nil, nil, &service.DeferredService{},
		nil, nil, nil, nil, nil, nil, nil, r.pricing, nil, nil, nil)
	requestID := uuid.NewString()
	r.router.POST("/v1/messages", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(r.keys, nil, cfg)), func(c *gin.Context) {
		key, ok := middleware.GetAPIKeyFromContext(c)
		require.True(t, ok)
		// Usage workers detach from the HTTP context in production. The authenticated
		// request-local key must carry its correlation without retaining Gin.
		err := usage.RecordUsage(context.Background(), &service.RecordUsageInput{
			APIKey: key, User: key.User, Account: &service.Account{ID: account.ID, Platform: account.Platform, Type: account.Type},
			Result: &service.ForwardResult{RequestID: requestID, Model: "fixture-a", Usage: service.ClaudeUsage{InputTokens: 10, OutputTokens: 5}},
		})
		require.NoError(t, err)
		c.Status(http.StatusOK)
	})
	session, turn, call := uuid.NewString(), uuid.NewString(), uuid.NewString()
	var before, after string
	require.NoError(t, db.QueryRowContext(r.ctx, "SELECT balance::text FROM users WHERE id=$1", r.userID).Scan(&before))
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"fixture-a"}`))
		for name, value := range map[string]string{"Authorization": "Bearer " + lease.APIKey, "X-Aino-Session-Id": session, "X-Aino-Turn-Id": turn, "X-Aino-Call-Id": call, "X-Aino-Purpose": "chat"} {
			req.Header.Set(name, value)
		}
		w := httptest.NewRecorder()
		r.router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	require.NoError(t, db.QueryRowContext(r.ctx, "SELECT balance::text FROM users WHERE id=$1", r.userID).Scan(&after))
	beforeDecimal, err := decimal.NewFromString(before)
	require.NoError(t, err)
	afterDecimal, err := decimal.NewFromString(after)
	require.NoError(t, err)
	require.Equal(t, "0.00010000", beforeDecimal.Sub(afterDecimal).StringFixed(8))
	var id int64
	require.NoError(t, db.QueryRowContext(r.ctx, "SELECT id FROM usage_logs WHERE request_id=$1", requestID).Scan(&id))
	row, err := logs.GetByID(r.ctx, id)
	require.NoError(t, err)
	require.Equal(t, r.userID, row.UserID)
	require.Equal(t, &session, row.SessionID)
	require.Equal(t, &turn, row.DesktopTurnID)
	require.Equal(t, &call, row.DesktopCallID)
	require.Equal(t, "settled", row.SettlementStatus)
	require.Equal(t, "0.00010000", *row.ActualCostDecimal)
}
