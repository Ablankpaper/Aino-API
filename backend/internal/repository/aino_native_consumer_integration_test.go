//go:build integration && nativeconsumer

package repository_test

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Explicit opt-in consumer mode; the ordinary integration suite never waits for
// an external process. TestMain owns disposable PostgreSQL/Redis containers.
func TestAinoNativeConsumer(t *testing.T) {
	dir := os.Getenv("AINO_NATIVE_FIXTURE_DIR")
	if dir == "" {
		t.Fatal("requires an isolated native consumer driver")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	temp, err := filepath.EvalSymlinks(os.TempDir())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(resolved, temp+string(os.PathSeparator)), "fixture directory must be owned temporary storage")
	info, err := os.Stat(resolved)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	r := newAinoPlatformFixture(t)
	nonceBytes := make([]byte, 32)
	_, err = rand.Read(nonceBytes)
	require.NoError(t, err)
	nonce := hex.EncodeToString(nonceBytes)
	phone := fmt.Sprintf("+86139%08d", time.Now().UnixNano()%100000000)
	fixturePath := filepath.Join(dir, "fixture-read.txt")
	content := "fixture-file-content-" + r.runID
	require.NoError(t, os.WriteFile(fixturePath, []byte(content+"\n"), 0600))
	protocol := &ainoNativeProtocol{rig: r, path: fixturePath, content: content, shutdown: make(chan struct{}), releaseTerminal: make(chan struct{})}
	r.modelProvider = protocol
	r.wireModel(t, 0)
	r.wirePayment(t)
	wireAinoNativeAccountRoutes(r)
	faults := newAinoNativeFaults(t, r, nonce, protocol.shutdown)
	// Real public settings projection with valid synthetic SMS deployment config;
	// SMS dispatch itself is the existing captured sender, never Aliyun transport.
	sms := config.SMSConfig{Enabled: true, Provider: "aliyun", AccessKeyID: "fixture-sms-id", AccessKeySecret: "fixture-sms-secret", HMACSecret: strings.Repeat("fixture-hmac", 4), RegionID: "cn-hangzhou", SignName: "fixture-sign", TemplateCode: "SMS_FIXTURE", TemplateParams: map[string]string{"code": "code", "minutes": "ttl_minutes"}, TemplateVerified: true, CodeLength: 6, TTLSeconds: 300, CooldownSeconds: 60, MaxAttempts: 5, PhoneHourLimit: 50, PhoneDayLimit: 50, IPHourLimit: 50, GlobalDayLimit: 1000, RequestTimeoutSeconds: 5}
	require.NoError(t, sms.Validate("release"))
	public := handler.NewSettingHandler(service.NewSettingService(r.settingRepo, &config.Config{SMS: sms}), "fixture")
	r.router.GET("/api/v1/settings/public", public.GetPublicSettings)
	var lock sync.Mutex
	var userID int64
	secrets := []string{"fixture-upstream-key"}
	completed := make(chan struct{})
	var finish sync.Once
	control := func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("X-Fixture-Nonce")), []byte(nonce)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		lock.Lock()
		defer lock.Unlock()
		switch req.URL.Path {
		case "/fixture/control/code":
			_ = json.NewEncoder(w).Encode(map[string]string{"code": r.sender.latestCode()})
		case "/fixture/control/state":
			var balance, cost string
			var calls, turns, orders int
			db := repository.GetIntegrationDB()
			if userID > 0 {
				if err := db.QueryRowContext(r.ctx, "SELECT balance::text FROM users WHERE id=$1", userID).Scan(&balance); err != nil {
					http.Error(w, "balance query failed", 500)
					return
				}
				if err := db.QueryRowContext(r.ctx, "SELECT count(*),count(distinct desktop_turn_id),coalesce(sum(actual_cost),0)::text FROM usage_logs WHERE user_id=$1 AND settlement_status='settled'", userID).Scan(&calls, &turns, &cost); err != nil {
					http.Error(w, "usage query failed", 500)
					return
				}
				if err := db.QueryRowContext(r.ctx, "SELECT count(*) FROM payment_orders WHERE user_id=$1", userID).Scan(&orders); err != nil {
					http.Error(w, "order query failed", 500)
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"run_id": r.runID, "user_id": userID, "balance": balance, "usage_cost": cost, "usage_calls": calls, "usage_turns": turns, "orders": orders, "model_calls": r.modelCalls.Load(), "payment_calls": r.paymentCalls.Load(), "tool_results": protocol.toolResults.Load(), "stream_started": protocol.streamStarted.Load(), "stream_cancelled": protocol.streamCancelled.Load(), "downstream_disconnects": protocol.downstreamDisconnects.Load(), "stream_drained": protocol.streamDrained.Load(), "stream_timeouts": protocol.streamTimeouts.Load(), "stream_shutdowns": protocol.streamShutdowns.Load()})
		case "/fixture/control/pay":
			if req.Method != "POST" || userID == 0 {
				http.Error(w, "invalid control request", 400)
				return
			}
			var outTradeNo, amount string
			err := repository.GetIntegrationDB().QueryRowContext(r.ctx, "SELECT out_trade_no,pay_amount::text FROM payment_orders WHERE user_id=$1 ORDER BY id DESC LIMIT 1", userID).Scan(&outTradeNo, &amount)
			if err != nil {
				http.Error(w, "no fixture order", http.StatusConflict)
				return
			}
			params := url.Values{"pid": {"fixture"}, "out_trade_no": {outTradeNo}, "trade_no": {"fixture-payment"}, "money": {amount}, "trade_status": {"TRADE_SUCCESS"}, "sign_type": {"MD5"}}
			params.Set("sign", fixtureSignature(params))
			// Real signature verification and fulfillment, twice for idempotency.
			for range 2 {
				rec := httptest.NewRecorder()
				request := httptest.NewRequest("POST", "/fixture/notify", strings.NewReader(params.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.router.ServeHTTP(rec, request)
				if rec.Code != 200 || rec.Body.String() != "success" {
					http.Error(w, "callback failed", 500)
					return
				}
			}
			_, _ = io.WriteString(w, `{"completed":true}`)
		case "/fixture/control/audit":
			data, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 32<<20))
			if err != nil {
				http.Error(w, "audit payload rejected", 400)
				return
			}
			leaked := false
			for _, secret := range secrets {
				if secret != "" && strings.Contains(string(data), secret) {
					leaked = true
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"leaked": leaked, "checked_credentials": len(secrets)})
		case "/fixture/control/complete":
			if req.Method != "POST" {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			protocol.closeActiveStream()
			finish.Do(func() { close(completed) })
			_, _ = io.WriteString(w, `{"completed":true}`)
		default:
			http.NotFound(w, req)
		}
	}
	// Destination-only substitution after real issuance. No arbitrary URL/Host
	// reflection, token synthesis, claim mutation, or production native override.
	native := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/fixture/control/") {
			control(w, req)
			return
		}
		finishWatching := protocol.watchNativeDownstreamRequest(req)
		defer finishWatching()
		capture := req.URL.Path == "/api/v1/auth/phone/verify" || req.URL.Path == "/api/v1/auth/refresh" || req.URL.Path == "/api/v1/desktop/credentials"
		if !capture {
			r.router.ServeHTTP(w, req)
			return
		}
		rec := httptest.NewRecorder()
		r.router.ServeHTTP(rec, req)
		body := rec.Body.Bytes()
		if rec.Code == 200 {
			var envelope map[string]any
			if json.Unmarshal(body, &envelope) != nil {
				http.Error(w, "fixture response malformed", 500)
				return
			}
			data, ok := envelope["data"].(map[string]any)
			if !ok {
				http.Error(w, "fixture response malformed", 500)
				return
			}
			lock.Lock()
			for _, key := range []string{"access_token", "refresh_token", "api_key"} {
				if secret, ok := data[key].(string); ok {
					secrets = append(secrets, secret)
				}
			}
			if req.URL.Path == "/api/v1/auth/phone/verify" {
				user, ok := data["user"].(map[string]any)
				if !ok {
					lock.Unlock()
					http.Error(w, "missing real user", 500)
					return
				}
				idValue, ok := user["id"].(float64)
				if !ok || idValue <= 0 || idValue != float64(int64(idValue)) {
					lock.Unlock()
					http.Error(w, "invalid real user id", http.StatusInternalServerError)
					return
				}
				id := int64(idValue)
				if userID == 0 {
					userID = id
					_, err = r.userRepo.SetBalance(r.ctx, id, 10)
					if err == nil {
						err = r.userRepo.AddGroupToAllowedGroups(r.ctx, id, r.modelGroupID)
					}
					if err != nil {
						lock.Unlock()
						http.Error(w, "fixture grant failed", 500)
						return
					}
				}
			}
			if req.URL.Path == "/api/v1/desktop/credentials" {
				if data["base_url"] != "https://api.agentera.com.cn/v1" {
					lock.Unlock()
					http.Error(w, "unexpected lease origin", 500)
					return
				}
				data["base_url"] = r.server.URL + "/v1"
			}
			lock.Unlock()
			body, _ = json.Marshal(envelope)
		}
		for name, values := range rec.Header() {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	})
	r.server = httptest.NewServer(faults.wrap(native))
	registerNativeProtocolCleanup(t, protocol, r.server.Close)
	manifest := map[string]string{"origin": r.server.URL, "nonce": nonce, "phone": phone, "run_id": r.runID, "fixture_path": fixturePath, "fixture_content": content}
	blob, err := json.Marshal(manifest)
	require.NoError(t, err)
	manifestPath := filepath.Join(dir, "manifest.json")
	require.NoError(t, os.WriteFile(manifestPath, blob, 0600))
	t.Cleanup(func() { _ = os.Remove(manifestPath); _ = os.Remove(fixturePath) })
	t.Logf("native fixture ready run_id=%s; bounded 8 minute lifetime; manifest private", r.runID)
	select {
	case <-completed:
	case <-time.After(8 * time.Minute):
		t.Error("native consumer failed to complete within bounded lifetime")
	}
}
