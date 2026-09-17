//go:build integration && nativeconsumer

package repository_test

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

type ainoNativeFaultSnapshot struct {
	Profile401ForCurrentAccess bool   `json:"profile_401_for_current_access"`
	Offline                    bool   `json:"offline"`
	HoldRefreshResponse        bool   `json:"hold_refresh_response"`
	HoldLogoutResponse         bool   `json:"hold_logout_response"`
	HoldInferenceBeforeAuth    bool   `json:"hold_inference_before_auth"`
	ProfileCalls               int64  `json:"profile_calls"`
	Profile401s                int64  `json:"profile_401s"`
	NetworkDrops               int64  `json:"network_drops"`
	RefreshStarted             int64  `json:"refresh_started"`
	RefreshCompleted           int64  `json:"refresh_completed"`
	RefreshWaiting             int64  `json:"refresh_waiting"`
	RefreshResponses           int64  `json:"refresh_responses"`
	LogoutStarted              int64  `json:"logout_started"`
	LogoutCompleted            int64  `json:"logout_completed"`
	LogoutWaiting              int64  `json:"logout_waiting"`
	LogoutResponses            int64  `json:"logout_responses"`
	CredentialRequests         int64  `json:"credential_requests"`
	CredentialSuccesses        int64  `json:"credential_successes"`
	InferenceRequests          int64  `json:"inference_requests"`
	InferenceWaiting           int64  `json:"inference_waiting"`
	InferenceResponses         int64  `json:"inference_responses"`
	GateTimeouts               int64  `json:"gate_timeouts"`
	GateCancellations          int64  `json:"gate_cancellations"`
	GateShutdowns              int64  `json:"gate_shutdowns"`
	SessionRevocations         int64  `json:"session_revocations"`
	RetiredLeaseAvailable      bool   `json:"retired_lease_available"`
	RetiredLeaseHTTPStatus     int    `json:"retired_lease_http_status"`
	RetiredLeaseErrorCode      string `json:"retired_lease_error_code"`
}

type ainoNativeFaultPatch struct {
	Profile401ForCurrentAccess *bool `json:"profile_401_for_current_access"`
	Offline                    *bool `json:"offline"`
	HoldRefreshResponse        *bool `json:"hold_refresh_response"`
	HoldLogoutResponse         *bool `json:"hold_logout_response"`
	HoldInferenceBeforeAuth    *bool `json:"hold_inference_before_auth"`
}

type ainoNativeFaults struct {
	mu                                                    sync.Mutex
	state                                                 ainoNativeFaultSnapshot
	rig                                                   *ainoPlatformFixture
	nonce                                                 string
	shutdown                                              <-chan struct{}
	timeout                                               time.Duration
	fail                                                  func(string)
	gates                                                 map[string]chan struct{}
	userID                                                int64
	access, deniedAccess, family, lastLease, retiredLease string
}

func newAinoNativeFaults(t *testing.T, r *ainoPlatformFixture, nonce string, shutdown <-chan struct{}) *ainoNativeFaults {
	// This route exists only in the nativeconsumer test server. Its real API-key
	// middleware permits this read-only desktop scope without executing a model.
	r.router.GET("/v1/usage", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(r.keys, nil, &config.Config{})), func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return &ainoNativeFaults{rig: r, nonce: nonce, shutdown: shutdown, timeout: 30 * time.Second, fail: func(message string) { t.Error(message) }, gates: make(map[string]chan struct{})}
}

func (f *ainoNativeFaults) snapshot() ainoNativeFaultSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *ainoNativeFaults) count(fn func(*ainoNativeFaultSnapshot)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.state)
}

func (f *ainoNativeFaults) setGate(name string, hold bool) {
	gate := f.gates[name]
	if hold && gate == nil {
		f.gates[name] = make(chan struct{})
	}
	if !hold && gate != nil {
		close(gate)
		delete(f.gates, name)
	}
}

func (f *ainoNativeFaults) waitGate(ctx context.Context, name string, waiting *int64) bool {
	f.mu.Lock()
	gate := f.gates[name]
	if gate == nil {
		f.mu.Unlock()
		return true
	}
	*waiting++
	f.mu.Unlock()
	defer func() { f.mu.Lock(); *waiting--; f.mu.Unlock() }()
	timer := time.NewTimer(f.timeout)
	defer timer.Stop()
	select {
	case <-gate:
		return true
	case <-ctx.Done():
		f.count(func(s *ainoNativeFaultSnapshot) { s.GateCancellations++ })
	case <-f.shutdown:
		f.count(func(s *ainoNativeFaultSnapshot) { s.GateShutdowns++ })
	case <-timer.C:
		f.count(func(s *ainoNativeFaultSnapshot) { s.GateTimeouts++ })
		f.fail("native fixture response gate timed out: " + name)
	}
	return false
}

func (f *ainoNativeFaults) patch(p ainoNativeFaultPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.Profile401ForCurrentAccess != nil {
		if *p.Profile401ForCurrentAccess && f.access == "" {
			return errors.New("no captured access token")
		}
		f.state.Profile401ForCurrentAccess = *p.Profile401ForCurrentAccess
		f.deniedAccess = ""
		if *p.Profile401ForCurrentAccess {
			f.deniedAccess = f.access
		}
	}
	if p.Offline != nil {
		f.state.Offline = *p.Offline
	}
	for _, gate := range []struct {
		name  string
		value *bool
		flag  *bool
	}{
		{"refresh", p.HoldRefreshResponse, &f.state.HoldRefreshResponse},
		{"logout", p.HoldLogoutResponse, &f.state.HoldLogoutResponse},
		{"inference", p.HoldInferenceBeforeAuth, &f.state.HoldInferenceBeforeAuth},
	} {
		if gate.value != nil {
			*gate.flag = *gate.value
			f.setGate(gate.name, *gate.value)
		}
	}
	return nil
}

func (f *ainoNativeFaults) control(w http.ResponseWriter, req *http.Request) bool {
	path := req.URL.Path
	if path != "/fixture/control/faults" && path != "/fixture/control/revoke-session" && path != "/fixture/control/probe-retired-lease" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(req.Header.Get("X-Fixture-Nonce")), []byte(f.nonce)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	if path == "/fixture/control/faults" {
		if req.Method != "GET" && req.Method != "POST" {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return true
		}
		if req.Method == "POST" {
			var patch ainoNativeFaultPatch
			decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&patch); err != nil {
				http.Error(w, "invalid fault patch", 400)
				return true
			}
			if err := f.patch(patch); err != nil {
				http.Error(w, "no captured access token", http.StatusConflict)
				return true
			}
		}
		_ = json.NewEncoder(w).Encode(f.snapshot())
		return true
	}
	if req.Method != "POST" {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return true
	}
	if path == "/fixture/control/probe-retired-lease" {
		f.probeLease(w)
		return true
	}
	f.mu.Lock()
	family, userID := f.family, f.userID
	f.retireLease()
	f.mu.Unlock()
	if family == "" || userID == 0 {
		http.Error(w, "no fixture session", http.StatusConflict)
		return true
	}
	if err := f.rig.auth.RevokeSessionFamily(f.rig.ctx, family); err != nil {
		http.Error(w, "session revocation failed", 500)
		return true
	}
	f.count(func(s *ainoNativeFaultSnapshot) { s.SessionRevocations++ })
	_ = json.NewEncoder(w).Encode(map[string]bool{"revoked": true})
	return true
}

func (f *ainoNativeFaults) retireLease() {
	if f.lastLease != "" {
		f.retiredLease = f.lastLease
		f.state.RetiredLeaseAvailable = true
		f.state.RetiredLeaseHTTPStatus = 0
		f.state.RetiredLeaseErrorCode = ""
	}
}

func (f *ainoNativeFaults) probeLease(w http.ResponseWriter) {
	f.mu.Lock()
	if f.retiredLease == "" {
		f.retireLease()
	}
	lease := f.retiredLease
	f.mu.Unlock()
	if lease == "" {
		http.Error(w, "no captured lease", http.StatusConflict)
		return
	}
	req := httptest.NewRequest("GET", "/v1/usage", nil).WithContext(f.rig.ctx)
	req.Header.Set("Authorization", "Bearer "+lease)
	rec := httptest.NewRecorder()
	f.rig.router.ServeHTTP(rec, req)
	var payload struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	f.count(func(s *ainoNativeFaultSnapshot) {
		s.RetiredLeaseHTTPStatus = rec.Code
		s.RetiredLeaseErrorCode = payload.Code
	})
	_ = json.NewEncoder(w).Encode(map[string]any{"http_status": rec.Code, "error_code": payload.Code, "revoked": rec.Code == http.StatusUnauthorized})
}

func (f *ainoNativeFaults) captureIssued(body []byte) error {
	var envelope struct {
		Data struct {
			AccessToken string `json:"access_token"`
			APIKey      string `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if access := envelope.Data.AccessToken; access != "" {
		claims, err := f.rig.auth.ValidateToken(access)
		if err != nil {
			return err
		}
		f.mu.Lock()
		if claims.UserID <= 0 || claims.SessionID == "" || (f.userID != 0 && f.userID != claims.UserID) {
			f.mu.Unlock()
			return errors.New("issued access escaped fixture account")
		}
		f.userID, f.access, f.family = claims.UserID, access, claims.SessionID
		f.mu.Unlock()
	}
	if key := envelope.Data.APIKey; key != "" {
		apiKey, err := f.rig.keys.GetByKey(f.rig.ctx, key)
		if err != nil {
			return err
		}
		lease, err := repository.NewDesktopCredentialRepository(f.rig.client).GetByAPIKeyID(f.rig.ctx, apiKey.ID)
		if err != nil {
			return err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if lease.UserID != f.userID || lease.SessionFamilyID != f.family {
			return errors.New("issued lease escaped fixture account")
		}
		f.lastLease = key
	}
	return nil
}

func wireAinoNativeAccountRoutes(r *ainoPlatformFixture) {
	aliases := map[string]string{"/login/send": "/api/v1/auth/phone/send-code", "/login/verify": "/api/v1/auth/phone/verify", "/profile": "/api/v1/user/profile"}
	for _, route := range r.router.Routes() {
		if target, ok := aliases[route.Path]; ok {
			handlers := []gin.HandlerFunc{route.HandlerFunc}
			if route.Path == "/profile" {
				handlers = append([]gin.HandlerFunc{gin.HandlerFunc(middleware.NewJWTAuthMiddleware(r.auth, r.users, r.settings, nil))}, handlers...)
			}
			r.router.Handle(route.Method, target, handlers...)
		}
	}
	authHandler := handler.NewAuthHandler(&config.Config{}, r.auth, r.users, r.settings, nil, nil, nil, nil)
	r.router.GET("/api/v1/auth/me", gin.HandlerFunc(middleware.NewJWTAuthMiddleware(r.auth, r.users, r.settings, nil)), authHandler.GetCurrentUser)
}
