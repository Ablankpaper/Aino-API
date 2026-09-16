//go:build integration

package repository_test

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ainoPlatformFixture composes production handlers/services; only network
// providers are fixtures. Its HTTP client emulates protocol turns, not an Agent.
type ainoPlatformFixture struct {
	*desktopModelRig
	server                   *httptest.Server
	runID                    string
	modelCalls, paymentCalls atomic.Int64
}

func newAinoPlatformFixture(t *testing.T) *ainoPlatformFixture {
	t.Helper()
	r := &ainoPlatformFixture{desktopModelRig: newDesktopModelRig(t), runID: uuid.NewString()}
	refresh := repository.NewRefreshTokenCache(r.redis)
	leases := repository.NewDesktopCredentialRepository(r.client)
	r.keys.SetDesktopCredentialDependencies(leases, refresh, r.settings)
	r.auth.SetDesktopCredentialRevoker(leases)
	r.desktopHandler.SetCredentialService(service.NewDesktopCredentialService(leases, r.keys, r.models, r.auth, refresh))
	r.desktopHandler.SetCredentialSecurity(r.users, nil)
	authHandler := handler.NewAuthHandler(&config.Config{}, r.auth, r.users, r.settings, nil, nil, nil, nil)
	r.router.POST("/api/v1/auth/refresh", authHandler.RefreshToken)
	r.router.POST("/api/v1/auth/logout", authHandler.Logout)
	return r
}

func (r *ainoPlatformFixture) wireModel(t *testing.T, userID int64) {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/chat/completions" || req.Header.Get("Authorization") != "Bearer fixture-upstream-key" {
			t.Errorf("provider received unexpected route %s or upstream authentication", req.URL.Path)
			http.Error(w, "fixture protocol mismatch", 400)
			return
		}
		for name := range req.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-aino-") {
				t.Errorf("private attribution forwarded: %s", name)
			}
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", 400)
			return
		}
		if body.Model != "fixture-tool-model" || len(body.Messages) == 0 || len(body.Tools) != 1 {
			t.Error("missing model/messages/tools")
			http.Error(w, "invalid fixture request", 400)
			return
		}
		call := r.modelCalls.Add(1)
		last := body.Messages[len(body.Messages)-1]
		message := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "fixture-tool-call", "type": "function", "function": map[string]any{"name": "fixture_read", "arguments": `{"path":"fixture.txt"}`}}}}
		finish := "tool_calls"
		if last.Role == "tool" {
			if last.ToolCallID != "fixture-tool-call" || last.Content != "fixture-file-content" {
				t.Error("tool result lost in proxy")
				http.Error(w, "invalid tool result", 400)
				return
			}
			message = map[string]any{"role": "assistant", "content": "Verified fixture-file-content"}
			finish = "stop"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", fmt.Sprintf("fixture-%s-%d", r.runID, call))
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("fixture-%d", call), "object": "chat.completion", "created": time.Now().Unix(), "model": body.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
	}))
	t.Cleanup(provider.Close)
	g := r.group(t, false)
	input, output := 2e-6, 6e-6
	g.ModelPricing = []service.ChannelModelPricing{{Models: []string{"fixture-*"}, InputPrice: &input, OutputPrice: &output}}
	g.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"fixture-tool-model"}}
	require.NoError(t, r.groups.Update(r.ctx, g))
	require.NoError(t, r.userRepo.AddGroupToAllowedGroups(r.ctx, userID, g.ID))
	r.catalog(t, []service.DesktopModelEntry{desktopEntry("fixture-tool-model", g.ID)}, "fixture-tool-model")
	db := repository.GetIntegrationDB()
	accounts := repository.NewAccountRepository(r.client, db, nil)
	account := &service.Account{Name: "fixture-" + r.runID, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 5, Credentials: map[string]any{"api_key": "fixture-upstream-key", "base_url": provider.URL}, Extra: map[string]any{"openai_responses_mode": "force_chat_completions"}}
	require.NoError(t, accounts.Create(r.ctx, account))
	require.NoError(t, accounts.BindGroups(r.ctx, account.ID, []int64{g.ID}))
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.AllowPrivateHosts = true
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	concurrency := service.NewConcurrencyService(repository.NewConcurrencyCache(r.redis, 1, 60))
	cache := service.NewBillingCacheService(nil, r.userRepo, r.subs, nil, nil, r.rates, cfg, nil)
	t.Cleanup(cache.Stop)
	logs := repository.NewUsageLogRepository(r.client, db)
	gateway := service.NewOpenAIGatewayService(accounts, logs, repository.NewUsageBillingRepository(r.client, db), r.userRepo, r.subs, r.rates, repository.NewGatewayCache(r.redis), cfg, nil, concurrency, r.billing, nil, cache, repository.NewHTTPUpstream(cfg), &service.DeferredService{}, nil, nil, r.pricing, nil, nil, r.settings, nil)
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 16})
	t.Cleanup(pool.Stop)
	h := handler.NewOpenAIGatewayHandler(gateway, concurrency, cache, r.keys, pool, nil, nil, nil, cfg)
	r.router.POST("/v1/chat/completions", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(r.keys, nil, cfg)), middleware.GroupModelAllowlist(), h.ChatCompletions)
	usage := handler.NewUsageHandler(service.NewUsageService(logs, r.userRepo, r.client, nil), r.keys, nil, r.settings)
	r.router.GET("/api/v1/usage", gin.HandlerFunc(middleware.NewJWTAuthMiddleware(r.auth, r.users, r.settings, nil)), usage.List)
}

func (r *ainoPlatformFixture) wirePayment(t *testing.T) {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/mapi.php" {
			t.Error("unexpected payment request")
			http.Error(w, "unexpected route", 400)
			return
		}
		r.paymentCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":1,"trade_no":"fixture-payment","qrcode":"weixin://fixture"}`)
	}))
	t.Cleanup(provider.Close)
	preservePaymentSettings(t, r.phoneAuthFlowRig, map[string]string{
		service.SettingPaymentEnabled: "true", service.SettingMinRechargeAmount: "1", service.SettingMaxRechargeAmount: "1000", service.SettingBalancePayDisabled: "false", service.SettingMaxPendingOrders: "100", service.SettingDailyRechargeLimit: "0", service.SettingCancelRateLimitOn: "false", service.SettingBalanceRechargeMult: "0.14", service.SettingRechargeFeeRate: "2.50", "payment_visible_method_alipay_source": "easypay", "payment_visible_method_alipay_enabled": "true",
	})
	blob, err := json.Marshal(map[string]string{"pid": "fixture", "pkey": "fixture-secret", "apiBase": provider.URL, "notifyUrl": provider.URL + "/notify", "returnUrl": provider.URL + "/return"})
	require.NoError(t, err)
	instance, err := r.client.PaymentProviderInstance.Create().SetName("fixture-" + r.runID).SetProviderKey(payment.TypeEasyPay).SetConfig(string(blob)).SetSupportedTypes("alipay").SetEnabled(true).Save(r.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.client.PaymentProviderInstance.DeleteOneID(instance.ID).Exec(r.ctx) })
	cfg := service.NewPaymentConfigService(r.client, r.settingRepo, nil)
	redeem := service.NewRedeemService(repository.NewRedeemCodeRepository(r.client), r.userRepo, nil, nil, nil, r.client, nil, nil)
	registry := payment.NewRegistry()
	payments := service.NewPaymentService(r.client, registry, payment.NewDefaultLoadBalancer(r.client, nil), redeem, nil, cfg, r.userRepo, r.groups, nil)
	jwt := middleware.NewJWTAuthMiddleware(r.auth, r.users, r.settings, nil)
	routes.RegisterPaymentRoutes(r.router.Group("/api/v1"), handler.NewPaymentHandler(payments, cfg), nil, nil, jwt, func(c *gin.Context) { c.AbortWithStatus(403) }, func(c *gin.Context) { c.Next() }, r.settings, middleware.NewPanelRateLimiter(r.redis, r.settings))
	r.router.POST("/fixture/notify", handler.NewPaymentWebhookHandler(payments, registry).EasyPayNotify)
	subs := service.NewSubscriptionService(r.groups, r.subs, nil, r.client, nil)
	t.Cleanup(subs.Stop)
	r.desktopHandler.SetBillingService(service.NewDesktopBillingService(repository.NewDesktopWalletRepository(repository.GetIntegrationDB()), subs, cfg))
}

func (r *ainoPlatformFixture) start(t *testing.T) {
	t.Helper()
	r.server = httptest.NewServer(r.router)
	t.Cleanup(r.server.Close)
}

func (r *ainoPlatformFixture) http(t *testing.T, method, path, body, token string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, r.server.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, data
}

func fixtureSignature(p url.Values) string {
	keys := make([]string, 0, len(p))
	for key := range p {
		if key != "sign" && key != "sign_type" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+p.Get(key))
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(parts, "&")+"fixture-secret")))
}
