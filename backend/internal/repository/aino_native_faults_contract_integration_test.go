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
