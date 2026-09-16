//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func fixtureDecode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var envelope struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(data, &envelope))
	return envelope.Data
}

type fixtureLogin struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	User         struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

func TestAinoPlatformHTTPAccountModelPaymentLifecycle(t *testing.T) {
	r := newAinoPlatformFixture(t)
	r.start(t)
	// Registration is exercised before model setup so no user/token is seeded.
	phone := fmt.Sprintf("139%08d", time.Now().UnixNano()%100000000)
	status, data := r.http(t, "POST", "/login/send", fmt.Sprintf(`{"phone":%q}`, phone), "", nil)
	require.Equal(t, 200, status)
	verifyBody := fmt.Sprintf(`{"phone":%q,"challenge_id":%q,"code":%q,"register_if_new":true}`, phone, challengeID(t, data), r.sender.latestCode())
	status, data = r.http(t, "POST", "/login/verify", verifyBody, "", nil)
	require.Equal(t, 200, status)
	login := fixtureDecode[fixtureLogin](t, data)
	require.Positive(t, login.User.ID)
	require.NotEmpty(t, login.AccessToken)
	require.NotEmpty(t, login.RefreshToken)
	status, _ = r.http(t, "POST", "/login/verify", verifyBody, "", nil)
	require.NotEqual(t, 200, status, "OTP replay must be rejected")
	r.server.Close()
	_, err := r.userRepo.SetBalance(r.ctx, login.User.ID, 10)
	require.NoError(t, err)
	r.wireModel(t, login.User.ID)
	r.wirePayment(t)
	r.start(t)
	status, data = r.http(t, "POST", "/api/v1/auth/refresh", fmt.Sprintf(`{"refresh_token":%q}`, login.RefreshToken), "", nil)
	require.Equal(t, 200, status)
	rotated := fixtureDecode[fixtureLogin](t, data)
	require.NotEmpty(t, rotated.AccessToken)
	login.AccessToken, login.RefreshToken = rotated.AccessToken, rotated.RefreshToken
	status, data = r.http(t, "GET", "/profile", "", login.AccessToken, nil)
	require.Equal(t, 200, status)
	profile := fixtureDecode[struct {
		ID int64 `json:"id"`
	}](t, data)
	require.Equal(t, login.User.ID, profile.ID)
	status, data = r.http(t, "GET", "/api/v1/desktop/models", "", login.AccessToken, nil)
	require.Equal(t, 200, status)
	models := fixtureDecode[[]service.PlatformModel](t, data)
	require.Len(t, models, 1)
	require.Equal(t, "fixture-tool-model", models[0].ID)
	status, data = r.http(t, "POST", "/api/v1/desktop/credentials", fmt.Sprintf(`{"device_id":%q,"connection_grant_id":%q,"model_id":"fixture-tool-model"}`, uuid.NewString(), uuid.NewString()), login.AccessToken, nil)
	require.Equal(t, 200, status)
	lease := fixtureDecode[service.DesktopCredentialResponse](t, data)
	require.NotEmpty(t, lease.APIKey)
	session, turn := uuid.NewString(), uuid.NewString()
	headers := map[string]string{"X-Aino-Session-Id": session, "X-Aino-Turn-Id": turn, "X-Aino-Call-Id": uuid.NewString(), "X-Aino-Purpose": "chat"}
	tools := `"tools":[{"type":"function","function":{"name":"fixture_read","description":"read fixture","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]`
	first := `{"model":"fixture-tool-model","messages":[{"role":"user","content":"Read fixture.txt"}],` + tools + `}`
	status, _ = r.http(t, "POST", "/v1/chat/completions", first, "fixture-invalid-key", headers)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = r.http(t, "POST", "/v1/chat/completions", strings.Replace(first, "fixture-tool-model", "fixture-forbidden-model", 1), lease.APIKey, headers)
	require.Equal(t, http.StatusNotFound, status)
	require.Zero(t, r.modelCalls.Load(), "authentication and model authorization must reject before provider execution")
	status, data = r.http(t, "POST", "/v1/chat/completions", first, lease.APIKey, headers)
	require.Equal(t, 200, status, string(data))
	var response struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
			Finish  string          `json:"finish_reason"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(data, &response))
	require.Len(t, response.Choices, 1)
	require.Equal(t, "tool_calls", response.Choices[0].Finish)
	// Deliberate API protocol emulation: native acceptance must replace this
	// tool-result message with a real Python Agent read-only tool invocation.
	second := fmt.Sprintf(`{"model":"fixture-tool-model","messages":[{"role":"user","content":"Read fixture.txt"},%s,{"role":"tool","tool_call_id":"fixture-tool-call","content":"fixture-file-content"}],%s}`, response.Choices[0].Message, tools)
	headers["X-Aino-Call-Id"] = uuid.NewString()
	status, data = r.http(t, "POST", "/v1/chat/completions", second, lease.APIKey, headers)
	require.Equal(t, 200, status, string(data))
	require.Contains(t, string(data), "Verified fixture-file-content")
	require.EqualValues(t, 2, r.modelCalls.Load())
	db := repository.GetIntegrationDB()
	var count int
	var balance, usageCost string
	require.Eventually(t, func() bool {
		err := db.QueryRowContext(r.ctx, "SELECT count(*),coalesce(sum(actual_cost),0)::text FROM usage_logs WHERE user_id=$1 AND desktop_turn_id=$2 AND settlement_status='settled'", login.User.ID, turn).Scan(&count, &usageCost)
		return err == nil && count == 2
	}, 10*time.Second, 20*time.Millisecond, "both actual provider responses must settle through the worker")
	require.NoError(t, db.QueryRowContext(r.ctx, "SELECT balance::text FROM users WHERE id=$1", login.User.ID).Scan(&balance))
	before := decimal.NewFromInt(10)
	remaining := decimal.RequireFromString(balance)
	charge := decimal.RequireFromString(usageCost)
	require.Equal(t, "0.00020000", charge.StringFixed(8), "2 calls × (10 input × 2e-6 + 5 output × 6e-6) × group rate 2")
	require.True(t, before.Sub(remaining).Equal(charge))
	status, data = r.http(t, "GET", "/api/v1/usage?desktop_turn_id="+turn, "", login.AccessToken, nil)
	require.Equal(t, 200, status)
	usage := fixtureDecode[struct {
		Items []struct {
			UserID int64  `json:"user_id"`
			Call   string `json:"desktop_call_id"`
			Cost   string `json:"actual_cost_decimal"`
		} `json:"items"`
	}](t, data)
	require.Len(t, usage.Items, 2)
	require.NotEqual(t, usage.Items[0].Call, usage.Items[1].Call)
	for _, item := range usage.Items {
		require.Equal(t, "0.00010000", item.Cost)
	}
	status, data = r.http(t, "POST", "/api/v1/payment/quote", `{"amount":"20.00","payment_type":"alipay","order_type":"balance"}`, login.AccessToken, nil)
	require.Equal(t, 200, status)
	quote := fixtureDecode[service.PaymentQuote](t, data)
	require.Zero(t, r.paymentCalls.Load())
	require.NoError(t, db.QueryRowContext(r.ctx, "SELECT count(*) FROM payment_orders WHERE user_id=$1", login.User.ID).Scan(&count))
	require.Zero(t, count)
	orderBody, err := json.Marshal(map[string]any{"amount_decimal": "20.00", "client_order_id": uuid.NewString(), "payment_source": "aino_desktop", "payment_type": "alipay", "order_type": "balance", "expected_quote": quote})
	require.NoError(t, err)
	status, data = r.http(t, "POST", "/api/v1/payment/orders", string(orderBody), login.AccessToken, nil)
	require.Equal(t, 200, status, string(data))
	order := fixtureDecode[service.CreateOrderResponse](t, data)
	require.False(t, order.ConfirmationRequired)
	status, data = r.http(t, "POST", "/api/v1/payment/orders", string(orderBody), login.AccessToken, nil)
	require.Equal(t, 200, status)
	require.Equal(t, order.OrderID, fixtureDecode[service.CreateOrderResponse](t, data).OrderID)
	require.EqualValues(t, 1, r.paymentCalls.Load())
	params := url.Values{"pid": {"fixture"}, "out_trade_no": {order.OutTradeNo}, "trade_no": {"fixture-payment"}, "money": {quote.PayAmount}, "trade_status": {"TRADE_SUCCESS"}, "sign_type": {"MD5"}, "sign": {"invalid"}}
	_, data = r.http(t, "POST", "/fixture/notify", params.Encode(), "", map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	require.NotEqual(t, "success", string(data))
	var afterBad string
	require.NoError(t, db.QueryRowContext(r.ctx, "SELECT balance::text FROM users WHERE id=$1", login.User.ID).Scan(&afterBad))
	require.Equal(t, balance, afterBad)
	params.Set("sign", fixtureSignature(params))
	type callbackResult struct {
		status int
		body   string
		err    error
	}
	results := make(chan callbackResult, 4)
	for range 4 {
		go func() {
			res, err := (&http.Client{Timeout: 15 * time.Second}).Post(r.server.URL+"/fixture/notify", "application/x-www-form-urlencoded", strings.NewReader(params.Encode()))
			if err != nil {
				results <- callbackResult{err: err}
				return
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			results <- callbackResult{res.StatusCode, string(body), err}
		}()
	}
	succeeded := 0
	for range 4 {
		result := <-results
		require.NoError(t, result.err)
		if result.body == "success" {
			succeeded++
		} else {
			require.Equal(t, "handle failed", result.body)
			require.Equal(t, http.StatusInternalServerError, result.status, "a concurrent fulfillment lease asks the provider to retry")
		}
	}
	require.Positive(t, succeeded)
	_, data = r.http(t, "POST", "/fixture/notify", params.Encode(), "", map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	require.Equal(t, "success", string(data), "duplicate delivery after completion must acknowledge without crediting again")
	status, data = r.http(t, "GET", fmt.Sprintf("/api/v1/payment/orders/%d", order.OrderID), "", login.AccessToken, nil)
	require.Equal(t, 200, status)
	stored := fixtureDecode[struct {
		Status string `json:"status"`
	}](t, data)
	require.Equal(t, service.OrderStatusCompleted, stored.Status)
	status, data = r.http(t, "GET", "/api/v1/desktop/billing-summary", "", login.AccessToken, nil)
	require.Equal(t, 200, status)
	wallet := fixtureDecode[service.PlatformWalletSummary](t, data)
	expected := remaining.Add(decimal.RequireFromString(quote.CreditAmount))
	require.Equal(t, expected.StringFixed(8), wallet.Balance)
	status, data = r.http(t, "GET", "/profile", "", login.AccessToken, nil)
	require.Equal(t, 200, status)
	web := fixtureDecode[struct {
		ID      int64   `json:"id"`
		Balance float64 `json:"balance"`
	}](t, data)
	require.Equal(t, login.User.ID, web.ID)
	require.True(t, expected.Equal(decimal.NewFromFloat(web.Balance)))
	status, _ = r.http(t, "POST", "/api/v1/auth/logout", fmt.Sprintf(`{"refresh_token":%q}`, login.RefreshToken), "", nil)
	require.Equal(t, 200, status)
	status, _ = r.http(t, "POST", "/v1/chat/completions", first, lease.APIKey, headers)
	require.Equal(t, 401, status)
	require.EqualValues(t, 2, r.modelCalls.Load())
	t.Logf("run_id=%s real HTTP calls=2 settled usage=2 debit=%s signed callbacks=5 payment creates=1 credit=%s final balance=%s; API protocol emulation only", r.runID, charge.StringFixed(8), quote.CreditAmount, wallet.Balance)
}
