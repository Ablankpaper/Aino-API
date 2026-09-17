//go:build integration && nativeconsumer

package repository_test

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	Profile401ForCurrentAccess     bool    `json:"profile_401_for_current_access"`
	Offline                        bool    `json:"offline"`
	HoldRefreshResponse            bool    `json:"hold_refresh_response"`
	HoldLogoutResponse             bool    `json:"hold_logout_response"`
	HoldInferenceBeforeAuth        bool    `json:"hold_inference_before_auth"`
	HoldCredentialResponse         bool    `json:"hold_credential_response"`
	HoldCredentialUserID           int64   `json:"hold_credential_user_id"`
	ProfileCalls                   int64   `json:"profile_calls"`
	Profile401s                    int64   `json:"profile_401s"`
	NetworkDrops                   int64   `json:"network_drops"`
	RefreshStarted                 int64   `json:"refresh_started"`
	RefreshCompleted               int64   `json:"refresh_completed"`
	RefreshWaiting                 int64   `json:"refresh_waiting"`
	RefreshResponses               int64   `json:"refresh_responses"`
	LogoutStarted                  int64   `json:"logout_started"`
	LogoutCompleted                int64   `json:"logout_completed"`
	LogoutWaiting                  int64   `json:"logout_waiting"`
	LogoutResponses                int64   `json:"logout_responses"`
	CredentialRequests             int64   `json:"credential_requests"`
	CredentialSuccesses            int64   `json:"credential_successes"`
	CredentialWaiting              int64   `json:"credential_waiting"`
	InferenceRequests              int64   `json:"inference_requests"`
	InferenceWaiting               int64   `json:"inference_waiting"`
	InferenceResponses             int64   `json:"inference_responses"`
	AuthenticatedInferenceAttempts int64   `json:"authenticated_inference_attempts"`
	InferenceAttemptUnixMillis     []int64 `json:"inference_attempt_unix_millis"`
	ProviderAttempts               int64   `json:"provider_attempts"`
	ProviderAttemptUnixMillis      []int64 `json:"provider_attempt_unix_millis"`
	InferenceHTTPStatus            int     `json:"inference_http_status"`
	InferenceFailuresRemaining     int     `json:"inference_http_failures_remaining"`
	InferenceRetryAfterSeconds     int     `json:"inference_retry_after_seconds"`
	InferenceFaultResponses        int64   `json:"inference_http_fault_responses"`
	Inference429s                  int64   `json:"inference_429s"`
	Inference503s                  int64   `json:"inference_503s"`
	GateTimeouts                   int64   `json:"gate_timeouts"`
	GateCancellations              int64   `json:"gate_cancellations"`
	GateShutdowns                  int64   `json:"gate_shutdowns"`
	SessionRevocations             int64   `json:"session_revocations"`
	RetiredLeaseAvailable          bool    `json:"retired_lease_available"`
	RetiredLeaseHTTPStatus         int     `json:"retired_lease_http_status"`
	RetiredLeaseErrorCode          string  `json:"retired_lease_error_code"`
}

type ainoNativeFaultPatch struct {
	Profile401ForCurrentAccess *bool  `json:"profile_401_for_current_access"`
	Offline                    *bool  `json:"offline"`
	HoldRefreshResponse        *bool  `json:"hold_refresh_response"`
	HoldLogoutResponse         *bool  `json:"hold_logout_response"`
	HoldInferenceBeforeAuth    *bool  `json:"hold_inference_before_auth"`
	HoldCredentialResponse     *bool  `json:"hold_credential_response"`
	HoldCredentialUserID       *int64 `json:"hold_credential_user_id"`
	InferenceHTTPStatus        *int   `json:"inference_http_status"`
	InferenceFailures          *int   `json:"inference_http_failures"`
	InferenceRetryAfterSeconds *int   `json:"inference_retry_after_seconds"`
}

type ainoNativeUserCapture struct {
	access, latestFamily, retiredLease      string
	sessions                                map[string]*ainoNativeSessionCapture
	credentialRequests, credentialSuccesses int64
	inferenceRequests, inferenceResponses   int64
}

type ainoNativeSessionCapture struct {
	lastLease, retiredLease string
}

type ainoNativeCredentialOwner struct {
	userID int64
	family string
}

func (o ainoNativeCredentialOwner) valid() bool {
	return o.userID > 0 && o.family != ""
}

type ainoNativeUserSnapshot struct {
	CredentialRequests  int64 `json:"credential_requests"`
	CredentialSuccesses int64 `json:"credential_successes"`
	InferenceRequests   int64 `json:"inference_requests"`
	InferenceResponses  int64 `json:"inference_responses"`
}

type ainoNativeFaults struct {
	mu            sync.Mutex
	state         ainoNativeFaultSnapshot
	rig           *ainoPlatformFixture
	nonce         string
	shutdown      <-chan struct{}
	timeout       time.Duration
	fail          func(string)
	gates         map[string]chan struct{}
	firstUserID   int64
	deniedAccess  string
	users         map[int64]*ainoNativeUserCapture
	leaseOwners   map[string]ainoNativeCredentialOwner
	refreshOwners map[string]ainoNativeCredentialOwner
}

func newAinoNativeFaults(t *testing.T, r *ainoPlatformFixture, nonce string, shutdown <-chan struct{}) *ainoNativeFaults {
	// This route exists only in the nativeconsumer test server. Its real API-key
	// middleware permits this read-only desktop scope without executing a model.
	r.router.GET("/v1/usage", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(r.keys, nil, &config.Config{})), func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return &ainoNativeFaults{rig: r, nonce: nonce, shutdown: shutdown, timeout: 30 * time.Second, fail: func(message string) { t.Error(message) }, gates: make(map[string]chan struct{}), users: make(map[int64]*ainoNativeUserCapture), leaseOwners: make(map[string]ainoNativeCredentialOwner), refreshOwners: make(map[string]ainoNativeCredentialOwner)}
}

func (f *ainoNativeFaults) snapshot() ainoNativeFaultSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.state
	state.InferenceAttemptUnixMillis = append([]int64(nil), f.state.InferenceAttemptUnixMillis...)
	state.ProviderAttemptUnixMillis = append([]int64(nil), f.state.ProviderAttemptUnixMillis...)
	return state
}

func (f *ainoNativeFaults) userSnapshot(userID int64) (ainoNativeUserSnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, ok := f.users[userID]
	if !ok {
		return ainoNativeUserSnapshot{}, false
	}
	return ainoNativeUserSnapshot{CredentialRequests: user.credentialRequests, CredentialSuccesses: user.credentialSuccesses, InferenceRequests: user.inferenceRequests, InferenceResponses: user.inferenceResponses}, true
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
		user := f.users[f.firstUserID]
		if *p.Profile401ForCurrentAccess && (user == nil || user.access == "") {
			return errors.New("no captured access token")
		}
		f.state.Profile401ForCurrentAccess = *p.Profile401ForCurrentAccess
		f.deniedAccess = ""
		if *p.Profile401ForCurrentAccess {
			f.deniedAccess = user.access
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
	if p.HoldCredentialResponse != nil || p.HoldCredentialUserID != nil {
		hold := f.state.HoldCredentialResponse
		if p.HoldCredentialResponse != nil {
			hold = *p.HoldCredentialResponse
		}
		userID := f.state.HoldCredentialUserID
		if p.HoldCredentialUserID != nil {
			userID = *p.HoldCredentialUserID
		}
		if hold {
			if userID == 0 {
				userID = f.firstUserID
			}
			if _, ok := f.users[userID]; !ok {
				return errors.New("unknown fixture user for credential gate")
			}
		} else {
			userID = 0
		}
		if f.state.HoldCredentialResponse && (userID != f.state.HoldCredentialUserID || !hold) {
			f.setGate(f.credentialGateName(f.state.HoldCredentialUserID), false)
		}
		f.state.HoldCredentialResponse = hold
		f.state.HoldCredentialUserID = userID
		if hold {
			f.setGate(f.credentialGateName(userID), true)
		}
	}
	if p.InferenceHTTPStatus != nil || p.InferenceFailures != nil || p.InferenceRetryAfterSeconds != nil {
		status, remaining, retryAfter := f.state.InferenceHTTPStatus, f.state.InferenceFailuresRemaining, f.state.InferenceRetryAfterSeconds
		if p.InferenceHTTPStatus != nil {
			status = *p.InferenceHTTPStatus
		}
		if p.InferenceFailures != nil {
			remaining = *p.InferenceFailures
		}
		if p.InferenceRetryAfterSeconds != nil {
			retryAfter = *p.InferenceRetryAfterSeconds
		}
		if status != 0 && status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
			return errors.New("inference_http_status must be 0, 429, or 503")
		}
		if remaining < 0 || remaining > 8 {
			return errors.New("inference_http_failures must be between 0 and 8")
		}
		if status == 0 || remaining == 0 {
			status = 0
			remaining, retryAfter = 0, 0
		}
		if status == http.StatusTooManyRequests {
			if retryAfter < 1 || retryAfter > 5 {
				return errors.New("429 faults require inference_retry_after_seconds between 1 and 5")
			}
		} else if retryAfter != 0 {
			return errors.New("inference_retry_after_seconds is only valid for 429 faults")
		}
		f.state.InferenceHTTPStatus = status
		f.state.InferenceFailuresRemaining = remaining
		f.state.InferenceRetryAfterSeconds = retryAfter
	}
	return nil
}

func (f *ainoNativeFaults) credentialGateName(userID int64) string {
	return "credential:" + strconv.FormatInt(userID, 10)
}

func (f *ainoNativeFaults) providerAttempt(w http.ResponseWriter) bool {
	f.mu.Lock()
	f.state.ProviderAttempts++
	f.state.ProviderAttemptUnixMillis = append(f.state.ProviderAttemptUnixMillis, time.Now().UnixMilli())
	if len(f.state.ProviderAttemptUnixMillis) > 16 {
		f.state.ProviderAttemptUnixMillis = append([]int64(nil), f.state.ProviderAttemptUnixMillis[len(f.state.ProviderAttemptUnixMillis)-16:]...)
	}
	status := f.state.InferenceHTTPStatus
	if f.state.InferenceFailuresRemaining == 0 {
		status = 0
	}
	retryAfter := f.state.InferenceRetryAfterSeconds
	if status != 0 {
		f.state.InferenceFailuresRemaining--
		f.state.InferenceFaultResponses++
		if status == http.StatusTooManyRequests {
			f.state.Inference429s++
		} else {
			f.state.Inference503s++
		}
	}
	f.mu.Unlock()
	if status == 0 {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(status)
	errType, code, message := "server_error", "fixture_upstream_503", "Fixture upstream service unavailable"
	if status == http.StatusTooManyRequests {
		errType, code, message = "rate_limit_error", "rate_limit_exceeded", "Fixture upstream rate limit"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": errType, "code": code, "message": message}})
	return true
}

func (f *ainoNativeFaults) observeAuthenticatedInference(c *gin.Context) {
	f.count(func(s *ainoNativeFaultSnapshot) {
		s.AuthenticatedInferenceAttempts++
		s.InferenceAttemptUnixMillis = append(s.InferenceAttemptUnixMillis, time.Now().UnixMilli())
		if len(s.InferenceAttemptUnixMillis) > 16 {
			s.InferenceAttemptUnixMillis = append([]int64(nil), s.InferenceAttemptUnixMillis[len(s.InferenceAttemptUnixMillis)-16:]...)
		}
	})
	c.Next()
}

func (f *ainoNativeFaults) control(w http.ResponseWriter, req *http.Request) bool {
	path := req.URL.Path
	if path != "/fixture/control/faults" && path != "/fixture/control/balance" && path != "/fixture/control/revoke-session" && path != "/fixture/control/probe-retired-lease" {
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
				status := http.StatusBadRequest
				if err.Error() == "no captured access token" {
					status = http.StatusConflict
				}
				http.Error(w, err.Error(), status)
				return true
			}
		}
		_ = json.NewEncoder(w).Encode(f.snapshot())
		return true
	}
	if path == "/fixture/control/balance" {
		if req.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return true
		}
		var input struct {
			UserID int64  `json:"user_id"`
			State  string `json:"state"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || (input.State != "exhausted" && input.State != "restored") {
			http.Error(w, "invalid balance request", http.StatusBadRequest)
			return true
		}
		userID, ok := f.resolveUserID(input.UserID)
		if !ok {
			http.Error(w, "unknown fixture user", http.StatusNotFound)
			return true
		}
		balance := 0.0
		if input.State == "restored" {
			balance = 10
		}
		change, err := f.rig.userRepo.SetBalance(f.rig.ctx, userID, balance)
		if err != nil {
			http.Error(w, "balance update failed", http.StatusInternalServerError)
			return true
		}
		f.rig.keys.InvalidateAuthCacheByUserID(f.rig.ctx, userID)
		if f.rig.gatewayBillingCache != nil {
			if err := f.rig.gatewayBillingCache.InvalidateUserBalance(f.rig.ctx, userID); err != nil {
				http.Error(w, "balance cache invalidation failed", http.StatusInternalServerError)
				return true
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"user_id": userID, "state": input.State, "old_balance": change.Old, "balance": change.New})
		return true
	}
	if req.Method != "POST" {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return true
	}
	if path == "/fixture/control/probe-retired-lease" {
		userID, ok := f.userIDFromQuery(req)
		if !ok {
			http.Error(w, "unknown fixture user", http.StatusNotFound)
			return true
		}
		f.probeLease(w, userID)
		return true
	}
	userID, ok := f.userIDFromQuery(req)
	if !ok {
		http.Error(w, "unknown fixture user", http.StatusNotFound)
		return true
	}
	f.mu.Lock()
	user := f.users[userID]
	owner := ainoNativeCredentialOwner{userID: userID, family: user.latestFamily}
	f.retireLease(owner)
	f.mu.Unlock()
	if !owner.valid() {
		http.Error(w, "no fixture session", http.StatusConflict)
		return true
	}
	if err := f.rig.auth.RevokeSessionFamily(f.rig.ctx, owner.family); err != nil {
		http.Error(w, "session revocation failed", 500)
		return true
	}
	f.count(func(s *ainoNativeFaultSnapshot) { s.SessionRevocations++ })
	_ = json.NewEncoder(w).Encode(map[string]bool{"revoked": true})
	return true
}

func (f *ainoNativeFaults) resolveUserID(userID int64) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if userID == 0 {
		userID = f.firstUserID
	}
	_, ok := f.users[userID]
	return userID, ok
}

func (f *ainoNativeFaults) userIDFromQuery(req *http.Request) (int64, bool) {
	raw := req.URL.Query().Get("user_id")
	if raw == "" {
		return f.resolveUserID(0)
	}
	userID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || userID <= 0 {
		return 0, false
	}
	return f.resolveUserID(userID)
}

func (f *ainoNativeFaults) retireLease(owner ainoNativeCredentialOwner) {
	user := f.users[owner.userID]
	if user == nil {
		return
	}
	session := user.sessions[owner.family]
	if session != nil && session.lastLease != "" {
		session.retiredLease = session.lastLease
		user.retiredLease = session.retiredLease
		f.state.RetiredLeaseAvailable = true
		f.state.RetiredLeaseHTTPStatus = 0
		f.state.RetiredLeaseErrorCode = ""
	}
}

func (f *ainoNativeFaults) probeLease(w http.ResponseWriter, userID int64) {
	f.mu.Lock()
	user := f.users[userID]
	if user.retiredLease == "" {
		f.retireLease(ainoNativeCredentialOwner{userID: userID, family: user.latestFamily})
	}
	lease := user.retiredLease
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

func (f *ainoNativeFaults) captureIssued(body []byte, requestOwner ainoNativeCredentialOwner) error {
	var envelope struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			APIKey       string `json:"api_key"`
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
		if claims.UserID <= 0 || claims.SessionID == "" {
			return errors.New("issued access escaped fixture account")
		}
		issuedOwner := ainoNativeCredentialOwner{userID: claims.UserID, family: claims.SessionID}
		if requestOwner.valid() && issuedOwner != requestOwner {
			return errors.New("issued access escaped requesting fixture session")
		}
		f.mu.Lock()
		user, _ := f.ensureSession(issuedOwner)
		user.access, user.latestFamily = access, issuedOwner.family
		if envelope.Data.RefreshToken != "" {
			f.refreshOwners[envelope.Data.RefreshToken] = issuedOwner
		}
		f.mu.Unlock()
	}
	if key := envelope.Data.APIKey; key != "" {
		if !requestOwner.valid() {
			return errors.New("issued lease has no requesting fixture session")
		}
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
		if lease.UserID != requestOwner.userID || lease.SessionFamilyID != requestOwner.family {
			return errors.New("issued lease escaped fixture account")
		}
		_, session := f.ensureSession(requestOwner)
		session.lastLease = key
		f.leaseOwners[key] = requestOwner
	}
	return nil
}

// ensureSession requires f.mu and retains every real session family observed for
// a user so a later login cannot invalidate fixture bookkeeping for an older one.
func (f *ainoNativeFaults) ensureSession(owner ainoNativeCredentialOwner) (*ainoNativeUserCapture, *ainoNativeSessionCapture) {
	user := f.users[owner.userID]
	if user == nil {
		user = &ainoNativeUserCapture{sessions: make(map[string]*ainoNativeSessionCapture)}
		f.users[owner.userID] = user
		if f.firstUserID == 0 {
			f.firstUserID = owner.userID
		}
	}
	if user.sessions == nil {
		user.sessions = make(map[string]*ainoNativeSessionCapture)
	}
	session := user.sessions[owner.family]
	if session == nil {
		session = &ainoNativeSessionCapture{}
		user.sessions[owner.family] = session
	}
	return user, session
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
