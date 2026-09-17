//go:build integration && nativeconsumer

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAinoNativeFaultsHoldRealRotationAndRevocation(t *testing.T) {
	r := newAinoPlatformFixture(t)
	wireAinoNativeAccountRoutes(r)
	r.wireModel(t, 0)
	shutdown := make(chan struct{})
	var stop sync.Once
	f := newAinoNativeFaults(t, r, "contract-nonce", shutdown)
	r.server = httptest.NewServer(f.wrap(r.router))
	t.Cleanup(func() { stop.Do(func() { close(shutdown) }); r.server.Close() })
	control := func(body string) ainoNativeFaultSnapshot {
		t.Helper()
		status, data := r.http(t, "POST", "/fixture/control/faults", body, "", map[string]string{"X-Fixture-Nonce": "contract-nonce"})
		require.Equal(t, 200, status, string(data))
		var state ainoNativeFaultSnapshot
		require.NoError(t, json.Unmarshal(data, &state))
		return state
	}
	phone := fmt.Sprintf("139%08d", time.Now().UnixNano()%100000000)
	status, data := r.http(t, "POST", "/api/v1/auth/phone/send-code", fmt.Sprintf(`{"phone":%q}`, phone), "", nil)
	require.Equal(t, 200, status)
	status, data = r.http(t, "POST", "/api/v1/auth/phone/verify", fmt.Sprintf(`{"phone":%q,"challenge_id":%q,"code":%q,"register_if_new":true}`, phone, challengeID(t, data), r.sender.latestCode()), "", nil)
	require.Equal(t, 200, status)
	login := fixtureDecode[fixtureLogin](t, data)
	_, err := r.userRepo.SetBalance(r.ctx, login.User.ID, 10)
	require.NoError(t, err)
	require.NoError(t, r.userRepo.AddGroupToAllowedGroups(r.ctx, login.User.ID, r.modelGroupID))
	status, data = r.http(t, "POST", "/api/v1/desktop/credentials", fmt.Sprintf(`{"device_id":%q,"connection_grant_id":%q,"model_id":"fixture-tool-model"}`, uuid.NewString(), uuid.NewString()), login.AccessToken, nil)
	require.Equal(t, 200, status, string(data))
	lease := fixtureDecode[struct {
		APIKey string `json:"api_key"`
	}](t, data)
	probe := func(want int) {
		t.Helper()
		status, data := r.http(t, "POST", "/fixture/control/probe-retired-lease", "", "", map[string]string{"X-Fixture-Nonce": "contract-nonce"})
		require.Equal(t, 200, status, string(data))
		var result struct {
			HTTPStatus int  `json:"http_status"`
			Revoked    bool `json:"revoked"`
		}
		require.NoError(t, json.Unmarshal(data, &result))
		require.Equal(t, want, result.HTTPStatus)
		require.Equal(t, want == 401, result.Revoked)
		require.NotContains(t, string(data), lease.APIKey)
	}
	probe(200)
	control(`{"profile_401_for_current_access":true,"hold_refresh_response":true}`)
	status, _ = r.http(t, "GET", "/api/v1/auth/me", "", login.AccessToken, nil)
	require.Equal(t, 401, status)
	type result struct {
		code int
		data []byte
		err  error
	}
	async := func(path, body, token string, ctx context.Context) <-chan result {
		ch := make(chan result, 1)
		go func() {
			req, err := http.NewRequestWithContext(ctx, "POST", r.server.URL+path, strings.NewReader(body))
			if err != nil {
				ch <- result{err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			res, err := r.server.Client().Do(req)
			if err != nil {
				ch <- result{err: err}
				return
			}
			defer func() { _ = res.Body.Close() }()
			data, err := io.ReadAll(res.Body)
			ch <- result{res.StatusCode, data, err}
		}()
		return ch
	}
	refreshed := async("/api/v1/auth/refresh", fmt.Sprintf(`{"refresh_token":%q}`, login.RefreshToken), "", context.Background())
	require.Eventually(t, func() bool { s := f.snapshot(); return s.RefreshCompleted == 1 && s.RefreshWaiting == 1 }, 5*time.Second, 10*time.Millisecond)
	control(`{"hold_refresh_response":false}`)
	var rotated fixtureLogin
	select {
	case result := <-refreshed:
		require.NoError(t, result.err)
		require.Equal(t, 200, result.code)
		rotated = fixtureDecode[fixtureLogin](t, result.data)
	case <-time.After(5 * time.Second):
		t.Fatal("control release deadlocked")
	}
	status, _ = r.http(t, "GET", "/api/v1/auth/me", "", rotated.AccessToken, nil)
	require.Equal(t, 200, status, "fault only rejects access captured at arm time")
	control(`{"hold_logout_response":true,"hold_inference_before_auth":true}`)
	heldInference := async("/v1/chat/completions", `{"model":"fixture-tool-model","messages":[]}`, lease.APIKey, context.Background())
	require.Eventually(t, func() bool { return f.snapshot().InferenceWaiting == 1 }, 5*time.Second, 10*time.Millisecond)
	loggedOut := async("/api/v1/auth/logout", fmt.Sprintf(`{"refresh_token":%q}`, rotated.RefreshToken), "", context.Background())
	require.Eventually(t, func() bool { s := f.snapshot(); return s.LogoutCompleted == 1 && s.LogoutWaiting == 1 }, 5*time.Second, 10*time.Millisecond)
	probe(401)
	status, data = r.http(t, "POST", "/fixture/control/revoke-session", "", "", map[string]string{"X-Fixture-Nonce": "contract-nonce"})
	require.Equal(t, 200, status, string(data))
	control(`{"hold_inference_before_auth":false}`)
	select {
	case result := <-heldInference:
		require.NoError(t, result.err)
		require.Equal(t, http.StatusUnauthorized, result.code)
	case <-time.After(5 * time.Second):
		t.Fatal("inference release deadlocked")
	}
	control(`{"hold_logout_response":false}`)
	select {
	case result := <-loggedOut:
		require.NoError(t, result.err)
		require.Equal(t, 200, result.code)
	case <-time.After(5 * time.Second):
		t.Fatal("logout release deadlocked")
	}
	require.Zero(t, r.modelCalls.Load(), "revoked held inference and read-only probes must never reach provider")
	require.EqualValues(t, 1, f.snapshot().InferenceRequests)
	require.EqualValues(t, 1, f.snapshot().SessionRevocations)
	probe(401)
	require.Equal(t, "DESKTOP_CREDENTIAL_REVOKED", f.snapshot().RetiredLeaseErrorCode)
	status, _ = r.http(t, "GET", "/v1/usage", "", "invalid-fixture-key", nil)
	require.Equal(t, http.StatusUnauthorized, status)
	require.EqualValues(t, 1, f.snapshot().CredentialSuccesses)
}

func TestAinoNativeFaultsControlsStayReachableAndGatesEndWithLifecycle(t *testing.T) {
	r := newAinoPlatformFixture(t)
	wireAinoNativeAccountRoutes(r)
	shutdown := make(chan struct{})
	var stop sync.Once
	f := newAinoNativeFaults(t, r, "lifecycle-nonce", shutdown)
	r.router.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(204) })
	r.server = httptest.NewServer(f.wrap(r.router))
	t.Cleanup(func() { stop.Do(func() { close(shutdown) }); r.server.Close() })
	status, _ := r.http(t, "POST", "/fixture/control/faults", `{"offline":true}`, "", nil)
	require.Equal(t, 403, status)
	require.False(t, f.snapshot().Offline)
	control := func(body string) {
		t.Helper()
		status, data := r.http(t, "POST", "/fixture/control/faults", body, "", map[string]string{"X-Fixture-Nonce": "lifecycle-nonce"})
		require.Equal(t, 200, status, string(data))
	}
	control(`{"offline":true}`)
	req, err := http.NewRequest("GET", r.server.URL+"/api/v1/auth/me", nil)
	require.NoError(t, err)
	res, err := r.server.Client().Do(req)
	if res != nil {
		_ = res.Body.Close()
	}
	require.Error(t, err, "offline must drop socket, not return an HTTP error")
	require.Positive(t, f.snapshot().NetworkDrops)
	status, _ = r.http(t, "GET", "/fixture/control/faults", "", "", map[string]string{"X-Fixture-Nonce": "lifecycle-nonce"})
	require.Equal(t, 200, status)
	control(`{"offline":false,"hold_inference_before_auth":true}`)
	for _, end := range []string{"cancel", "shutdown"} {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, "POST", r.server.URL+"/v1/chat/completions", strings.NewReader(`{}`))
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() {
			res, err := r.server.Client().Do(req)
			if res != nil {
				_ = res.Body.Close()
			}
			done <- err
		}()
		require.Eventually(t, func() bool { return f.snapshot().InferenceWaiting == 1 }, 5*time.Second, 10*time.Millisecond)
		if end == "cancel" {
			cancel()
		} else {
			stop.Do(func() { close(shutdown) })
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("gate lifecycle failed to release request")
		}
		cancel()
		require.Eventually(t, func() bool { return f.snapshot().InferenceWaiting == 0 }, 5*time.Second, 10*time.Millisecond)
	}
	require.EqualValues(t, 1, f.snapshot().GateCancellations)
	require.EqualValues(t, 1, f.snapshot().GateShutdowns)
	require.Zero(t, f.snapshot().InferenceResponses, "lifecycle release must not dispatch held inference")
	require.Zero(t, f.snapshot().GateTimeouts)
	// An emergency timeout is an explicit fixture failure, never a successful
	// gate release that could make a client assertion pass accidentally.
	f.shutdown = make(chan struct{})
	f.timeout = 10 * time.Millisecond
	failures := make(chan string, 1)
	f.fail = func(message string) { failures <- message }
	status, _ = r.http(t, "POST", "/v1/chat/completions", `{}`, "", nil)
	require.Equal(t, http.StatusGatewayTimeout, status)
	require.EqualValues(t, 1, f.snapshot().GateTimeouts)
	require.Zero(t, f.snapshot().InferenceResponses)
	select {
	case failure := <-failures:
		require.Contains(t, failure, "timed out")
	default:
		t.Fatal("timeout did not report fixture failure")
	}
}

func TestAinoNativeRecoveryFaultsUseRealAuthGatewayAndRepository(t *testing.T) {
	r := newAinoPlatformFixture(t)
	wireAinoNativeAccountRoutes(r)
	protocol := &ainoNativeProtocol{rig: r, path: "/tmp/native-recovery-contract", content: "recovery-contract", shutdown: make(chan struct{}), releaseTerminal: make(chan struct{})}
	f := newAinoNativeFaults(t, r, "recovery-nonce", protocol.shutdown)
	protocol.faults = f
	r.modelProvider = protocol
	r.nativeInferenceObserver = f.observeAuthenticatedInference
	r.wireModel(t, 0)
	r.server = httptest.NewServer(f.wrap(r.router))
	registerNativeProtocolCleanup(t, protocol, r.server.Close)

	phone := fmt.Sprintf("139%08d", time.Now().UnixNano()%100000000)
	status, data := r.http(t, http.MethodPost, "/api/v1/auth/phone/send-code", fmt.Sprintf(`{"phone":%q}`, phone), "", nil)
	require.Equal(t, http.StatusOK, status)
	status, data = r.http(t, http.MethodPost, "/api/v1/auth/phone/verify", fmt.Sprintf(`{"phone":%q,"challenge_id":%q,"code":%q,"register_if_new":true}`, phone, challengeID(t, data), r.sender.latestCode()), "", nil)
	require.Equal(t, http.StatusOK, status)
	login := fixtureDecode[fixtureLogin](t, data)
	_, err := r.userRepo.SetBalance(r.ctx, login.User.ID, 10)
	require.NoError(t, err)
	require.NoError(t, r.userRepo.AddGroupToAllowedGroups(r.ctx, login.User.ID, r.modelGroupID))
	status, data = r.http(t, http.MethodPost, "/api/v1/desktop/credentials", fmt.Sprintf(`{"device_id":%q,"connection_grant_id":%q,"model_id":"fixture-tool-model"}`, uuid.NewString(), uuid.NewString()), login.AccessToken, nil)
	require.Equal(t, http.StatusOK, status, string(data))
	lease := fixtureDecode[struct {
		APIKey string `json:"api_key"`
	}](t, data)
	headers := map[string]string{"X-Aino-Session-Id": uuid.NewString(), "X-Aino-Turn-Id": uuid.NewString(), "X-Aino-Call-Id": uuid.NewString(), "X-Aino-Purpose": "chat"}
	body := `{"model":"fixture-tool-model","stream":false,"messages":[{"role":"user","content":"Read fixture"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	control := func(path, payload string) ([]byte, http.Header) {
		t.Helper()
		req, requestErr := http.NewRequest(http.MethodPost, r.server.URL+path, strings.NewReader(payload))
		require.NoError(t, requestErr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Fixture-Nonce", "recovery-nonce")
		response, requestErr := r.server.Client().Do(req)
		require.NoError(t, requestErr)
		defer func() { _ = response.Body.Close() }()
		responseBody, requestErr := io.ReadAll(response.Body)
		require.NoError(t, requestErr)
		require.Equal(t, http.StatusOK, response.StatusCode, string(responseBody))
		return responseBody, response.Header.Clone()
	}
	infer := func() (int, []byte, http.Header) {
		t.Helper()
		req, requestErr := http.NewRequest(http.MethodPost, r.server.URL+"/v1/chat/completions", strings.NewReader(body))
		require.NoError(t, requestErr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+lease.APIKey)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		response, requestErr := r.server.Client().Do(req)
		require.NoError(t, requestErr)
		defer func() { _ = response.Body.Close() }()
		responseBody, requestErr := io.ReadAll(response.Body)
		require.NoError(t, requestErr)
		return response.StatusCode, responseBody, response.Header.Clone()
	}

	control("/fixture/control/balance", fmt.Sprintf(`{"user_id":%d,"state":"exhausted"}`, login.User.ID))
	status, data, _ = infer()
	require.Equal(t, http.StatusForbidden, status, string(data))
	require.Contains(t, string(data), "INSUFFICIENT_BALANCE")
	require.Zero(t, f.snapshot().AuthenticatedInferenceAttempts)
	require.Zero(t, f.snapshot().ProviderAttempts)
	control("/fixture/control/balance", fmt.Sprintf(`{"user_id":%d,"state":"restored"}`, login.User.ID))

	control("/fixture/control/faults", `{"inference_http_status":429,"inference_http_failures":1,"inference_retry_after_seconds":1}`)
	status, data, responseHeaders := infer()
	require.Equal(t, http.StatusTooManyRequests, status, string(data))
	require.Equal(t, "1", responseHeaders.Get("Retry-After"))
	require.JSONEq(t, `{"error":{"type":"rate_limit_error","message":"Upstream rate limit exceeded, please retry later"}}`, string(data))
	state := f.snapshot()
	require.EqualValues(t, 1, state.AuthenticatedInferenceAttempts)
	require.EqualValues(t, 1, state.ProviderAttempts)
	require.EqualValues(t, 1, state.InferenceFaultResponses)
	require.Zero(t, r.modelCalls.Load(), "fault response must not count as provider model success")
	time.Sleep(1100 * time.Millisecond)
	headers["X-Aino-Call-Id"] = uuid.NewString()
	status, data, _ = infer()
	require.Equal(t, http.StatusOK, status, string(data))
	require.EqualValues(t, 1, r.modelCalls.Load())
	require.Eventually(t, func() bool {
		var count int
		err := repository.GetIntegrationDB().QueryRowContext(r.ctx, "SELECT count(*) FROM usage_logs WHERE user_id=$1 AND settlement_status='settled'", login.User.ID).Scan(&count)
		return err == nil && count == 1
	}, 5*time.Second, 20*time.Millisecond)
	state = f.snapshot()
	require.EqualValues(t, 2, state.AuthenticatedInferenceAttempts)
	require.Len(t, state.InferenceAttemptUnixMillis, 2)
	require.GreaterOrEqual(t, state.InferenceAttemptUnixMillis[1]-state.InferenceAttemptUnixMillis[0], int64(1000))
}

func TestAinoNativeServerFaultAndSecondUserStayIsolated(t *testing.T) {
	r := newAinoPlatformFixture(t)
	wireAinoNativeAccountRoutes(r)
	protocol := &ainoNativeProtocol{rig: r, path: "/tmp/native-isolation-contract", content: "isolation-contract", shutdown: make(chan struct{}), releaseTerminal: make(chan struct{})}
	f := newAinoNativeFaults(t, r, "isolation-nonce", protocol.shutdown)
	protocol.faults = f
	r.modelProvider = protocol
	r.nativeInferenceObserver = f.observeAuthenticatedInference
	r.wireModel(t, 0)
	r.server = httptest.NewServer(f.wrap(r.router))
	registerNativeProtocolCleanup(t, protocol, r.server.Close)

	register := func(phone string) (fixtureLogin, string) {
		t.Helper()
		status, data := r.http(t, http.MethodPost, "/api/v1/auth/phone/send-code", fmt.Sprintf(`{"phone":%q}`, phone), "", nil)
		require.Equal(t, http.StatusOK, status)
		status, data = r.http(t, http.MethodPost, "/api/v1/auth/phone/verify", fmt.Sprintf(`{"phone":%q,"challenge_id":%q,"code":%q,"register_if_new":true}`, phone, challengeID(t, data), r.sender.latestCode()), "", nil)
		require.Equal(t, http.StatusOK, status)
		login := fixtureDecode[fixtureLogin](t, data)
		_, err := r.userRepo.SetBalance(r.ctx, login.User.ID, 10)
		require.NoError(t, err)
		require.NoError(t, r.userRepo.AddGroupToAllowedGroups(r.ctx, login.User.ID, r.modelGroupID))
		status, data = r.http(t, http.MethodPost, "/api/v1/desktop/credentials", fmt.Sprintf(`{"device_id":%q,"connection_grant_id":%q,"model_id":"fixture-tool-model"}`, uuid.NewString(), uuid.NewString()), login.AccessToken, nil)
		require.Equal(t, http.StatusOK, status, string(data))
		lease := fixtureDecode[struct {
			APIKey string `json:"api_key"`
		}](t, data)
		return login, lease.APIKey
	}
	phoneBase := time.Now().UnixNano() % 100000000
	firstPhone := fmt.Sprintf("137%08d", phoneBase)
	secondPhone := fmt.Sprintf("136%08d", (phoneBase+1)%100000000)
	first, firstLease := register(firstPhone)
	second, secondLease := register(secondPhone)
	require.NotEqual(t, first.User.ID, second.User.ID)

	control := func(path, payload string) {
		t.Helper()
		status, data := r.http(t, http.MethodPost, path, payload, "", map[string]string{"X-Fixture-Nonce": "isolation-nonce"})
		require.Equal(t, http.StatusOK, status, string(data))
	}
	body := `{"model":"fixture-tool-model","stream":false,"messages":[{"role":"user","content":"Read fixture"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	infer := func(lease string) (int, []byte, http.Header) {
		t.Helper()
		headers := map[string]string{"X-Aino-Session-Id": uuid.NewString(), "X-Aino-Turn-Id": uuid.NewString(), "X-Aino-Call-Id": uuid.NewString(), "X-Aino-Purpose": "chat"}
		req, err := http.NewRequest(http.MethodPost, r.server.URL+"/v1/chat/completions", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+lease)
		req.Header.Set("Content-Type", "application/json")
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		response, err := r.server.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		responseBody, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, responseBody, response.Header.Clone()
	}

	control("/fixture/control/balance", fmt.Sprintf(`{"user_id":%d,"state":"exhausted"}`, first.User.ID))
	status, data, _ := infer(firstLease)
	require.Equal(t, http.StatusForbidden, status, string(data))
	control("/fixture/control/faults", `{"inference_http_status":503,"inference_http_failures":1}`)
	status, data, responseHeaders := infer(secondLease)
	require.Equal(t, http.StatusBadGateway, status, string(data))
	require.Empty(t, responseHeaders.Get("Retry-After"))
	require.JSONEq(t, `{"error":{"type":"upstream_error","message":"Upstream service temporarily unavailable"}}`, string(data))
	state := f.snapshot()
	require.EqualValues(t, 1, state.AuthenticatedInferenceAttempts, "first user must reject in real auth before ingress")
	require.EqualValues(t, 1, state.ProviderAttempts)
	require.EqualValues(t, 1, state.Inference503s)
	require.Zero(t, r.modelCalls.Load())
	status, data, _ = infer(secondLease)
	require.Equal(t, http.StatusOK, status, string(data))
	require.EqualValues(t, 1, r.modelCalls.Load())
	firstState, ok := f.userSnapshot(first.User.ID)
	require.True(t, ok)
	secondState, ok := f.userSnapshot(second.User.ID)
	require.True(t, ok)
	require.EqualValues(t, 1, firstState.InferenceRequests)
	require.EqualValues(t, 2, secondState.InferenceRequests)
	require.EqualValues(t, 1, firstState.InferenceResponses)
	require.EqualValues(t, 2, secondState.InferenceResponses)

	// A second real login for B creates another family; issuing from B's older,
	// still-valid access must remain valid rather than following a latest-family pointer.
	require.NoError(t, r.redis.Del(r.ctx, r.smsPrefix+"sms:cooldown:phone:+86"+secondPhone).Err())
	secondRelogin, _ := register(secondPhone)
	require.Equal(t, second.User.ID, secondRelogin.User.ID)
	status, data = r.http(t, http.MethodPost, "/api/v1/desktop/credentials", fmt.Sprintf(`{"device_id":%q,"connection_grant_id":%q,"model_id":"fixture-tool-model"}`, uuid.NewString(), uuid.NewString()), second.AccessToken, nil)
	require.Equal(t, http.StatusOK, status, string(data))

	// Desktop logout has no bearer header. The privately captured refresh token
	// must retire B's matching family without touching A's still-live lease.
	status, data = r.http(t, http.MethodPost, "/api/v1/auth/logout", fmt.Sprintf(`{"refresh_token":%q}`, second.RefreshToken), "", nil)
	require.Equal(t, http.StatusOK, status, string(data))
	probe := func(userID int64) int {
		t.Helper()
		status, data := r.http(t, http.MethodPost, fmt.Sprintf("/fixture/control/probe-retired-lease?user_id=%d", userID), "", "", map[string]string{"X-Fixture-Nonce": "isolation-nonce"})
		require.Equal(t, http.StatusOK, status, string(data))
		var result struct {
			HTTPStatus int `json:"http_status"`
		}
		require.NoError(t, json.Unmarshal(data, &result))
		return result.HTTPStatus
	}
	require.Equal(t, http.StatusUnauthorized, probe(second.User.ID))
	require.Equal(t, http.StatusOK, probe(first.User.ID))
}
