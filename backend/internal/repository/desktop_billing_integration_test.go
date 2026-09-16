//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func preservePaymentSettings(t *testing.T, r *phoneAuthFlowRig, values map[string]string) {
	t.Helper()
	for key := range values {
		previous, err := r.settingRepo.GetValue(r.ctx, key)
		t.Cleanup(func() {
			if err != nil {
				_ = r.settingRepo.Delete(r.ctx, key)
			} else {
				_ = r.settingRepo.Set(r.ctx, key, previous)
			}
		})
	}
	require.NoError(t, r.settingRepo.SetMultiple(r.ctx, values))
}

func TestDesktopWalletRealBalancesAndOwnerIsolation(t *testing.T) {
	r := newDesktopModelRig(t)
	u, other := r.user(t, service.StatusActive, false), r.user(t, service.StatusActive, false)
	preservePaymentSettings(t, r.phoneAuthFlowRig, map[string]string{service.SettingPaymentEnabled: "false"})
	cfg := service.NewPaymentConfigService(r.client, r.settingRepo, nil)
	subscriptions := service.NewSubscriptionService(r.groups, r.subs, nil, r.client, nil)
	t.Cleanup(subscriptions.Stop)
	r.desktopHandler.SetBillingService(service.NewDesktopBillingService(repository.NewDesktopWalletRepository(repository.GetIntegrationDB()), subscriptions, cfg))
	// NUMERIC is read as text: a float round trip would erase the final cents.
	_, err := repository.GetIntegrationDB().ExecContext(r.ctx, "UPDATE users SET balance = $1, frozen_balance = $2 WHERE id = $3", "123456789012.12345678", "2.12345678", u.ID)
	require.NoError(t, err)
	token, err := r.auth.GenerateToken(r.ctx, u)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, r.request(http.MethodGet, "/api/v1/desktop/billing-summary", "", "").Code)
	w := r.request(http.MethodGet, fmt.Sprintf("/api/v1/desktop/billing-summary?user_id=%d", other.ID), "", token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var payload struct {
		Data service.PlatformWalletSummary `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.Equal(t, "123456789012.12345678", payload.Data.Balance)
	require.Equal(t, payload.Data.Balance, payload.Data.AvailableBalance, "holds are already removed from users.balance")
	require.Equal(t, "2.12345678", payload.Data.FrozenBalance)
	require.Equal(t, "USD", payload.Data.Currency)
	require.False(t, payload.Data.PaymentEnabled)
	require.Empty(t, payload.Data.ActiveSubscriptions)

	_, err = r.userRepo.SetBalance(r.ctx, u.ID, 0)
	require.NoError(t, err)
	g := r.group(t, true)
	daily, weekly := 10.0, 20.0
	g.DailyLimitUSD, g.WeeklyLimitUSD = &daily, &weekly
	require.NoError(t, r.groups.Update(r.ctx, g))
	now := time.Now()
	sub := &service.UserSubscription{UserID: u.ID, GroupID: g.ID, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Status: service.SubscriptionStatusActive, DailyWindowStart: &now, WeeklyWindowStart: &now, DailyUsageUSD: 3, WeeklyUsageUSD: 18}
	require.NoError(t, r.subs.Create(r.ctx, sub))
	w = r.request(http.MethodGet, "/api/v1/desktop/billing-summary", "", token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.Equal(t, "0.00000000", payload.Data.AvailableBalance)
	require.Len(t, payload.Data.ActiveSubscriptions, 1)
	require.Equal(t, "2.00000000", *payload.Data.ActiveSubscriptions[0].Remaining)
	require.Equal(t, "USD", payload.Data.ActiveSubscriptions[0].Unit)
	_, err = r.client.User.UpdateOneID(u.ID).SetStatus("disabled").Save(r.ctx)
	require.NoError(t, err)
	require.NotEqual(t, http.StatusOK, r.request(http.MethodGet, "/api/v1/desktop/billing-summary", "", token).Code)
}

func TestQuoteMatchesTheActualOrderAmounts(t *testing.T) {
	r := newDesktopModelRig(t)
	u := r.user(t, service.StatusActive, false)
	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		providerCalls.Add(1)
		require.Equal(t, "/mapi.php", req.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1,"trade_no":"fixture-payment","qrcode":"fixture-qr"}`))
	}))
	t.Cleanup(provider.Close)
	preservePaymentSettings(t, r.phoneAuthFlowRig, map[string]string{
		service.SettingPaymentEnabled: "true", service.SettingMinRechargeAmount: "1", service.SettingMaxRechargeAmount: "1000",
		service.SettingBalancePayDisabled: "false", service.SettingMaxPendingOrders: "100", service.SettingDailyRechargeLimit: "0",
		service.SettingCancelRateLimitOn: "false", service.SettingBalanceRechargeMult: "0.14", service.SettingRechargeFeeRate: "2.50",
		"payment_visible_method_alipay_source": "easypay", "payment_visible_method_alipay_enabled": "true",
	})
	configJSON, err := json.Marshal(map[string]string{"pid": "fixture", "pkey": "fixture-secret", "apiBase": provider.URL, "notifyUrl": provider.URL + "/notify", "returnUrl": provider.URL + "/return"})
	require.NoError(t, err)
	inst, err := r.client.PaymentProviderInstance.Create().SetName("desktop-quote-fixture").SetProviderKey(payment.TypeEasyPay).SetConfig(string(configJSON)).SetSupportedTypes(payment.TypeAlipay).SetEnabled(true).Save(r.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.client.PaymentProviderInstance.DeleteOneID(inst.ID).Exec(r.ctx) })
	t.Cleanup(func() { _, _ = r.client.PaymentOrder.Delete().Where(paymentorder.UserID(u.ID)).Exec(r.ctx) })
	cfg := service.NewPaymentConfigService(r.client, r.settingRepo, nil)
	payments := service.NewPaymentService(r.client, payment.NewRegistry(), payment.NewDefaultLoadBalancer(r.client, nil), nil, nil, cfg, r.userRepo, r.groups, nil)
	ph := handler.NewPaymentHandler(payments, cfg)
	routes.RegisterPaymentRoutes(r.router.Group("/api/v1"), ph, nil, nil,
		middleware.NewJWTAuthMiddleware(r.auth, r.users, r.settings, nil), func(c *gin.Context) { c.AbortWithStatus(http.StatusForbidden) }, func(c *gin.Context) { c.Next() }, r.settings, middleware.NewPanelRateLimiter(r.redis, r.settings))
	token, err := r.auth.GenerateToken(r.ctx, u)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, r.request(http.MethodPost, "/api/v1/payment/quote", `{"amount":"20.00","payment_type":"alipay","order_type":"balance"}`, "").Code)
	for index, values := range []struct{ multiplier, fee string }{{"0.14", "2.50"}, {"1.25", "1.17"}} {
		require.NoError(t, r.settingRepo.SetMultiple(r.ctx, map[string]string{service.SettingBalanceRechargeMult: values.multiplier, service.SettingRechargeFeeRate: values.fee}))
		w := r.request(http.MethodPost, "/api/v1/payment/quote", `{"amount":"20.00","payment_type":"alipay","order_type":"balance","user_id":999}`, token)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var quote struct {
			Data service.PaymentQuote `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &quote))
		require.EqualValues(t, index, providerCalls.Load(), "quote must not invoke a payment provider")
		count, err := r.client.PaymentOrder.Query().Where(paymentorder.UserID(u.ID)).Count(r.ctx)
		require.NoError(t, err)
		require.Equal(t, index, count, "quote must not create an order")
		order, err := payments.CreateOrder(r.ctx, service.CreateOrderRequest{UserID: u.ID, Amount: 20, PaymentType: "alipay", OrderType: "balance", ClientIP: "127.0.0.1"})
		require.NoError(t, err)
		require.Equal(t, quote.Data.PayAmount, decimal.NewFromFloat(order.PayAmount).StringFixed(2))
		require.Equal(t, quote.Data.CreditAmount, decimal.NewFromFloat(order.Amount).StringFixed(8))
		stored, err := payments.GetOrder(r.ctx, order.OrderID, u.ID)
		require.NoError(t, err)
		require.Equal(t, service.PaymentOrderCurrency(stored), quote.Data.PaymentCurrency)
		require.Equal(t, quote.Data.PayAmount, payment.FormatAmountForCurrency(stored.PayAmount, quote.Data.PaymentCurrency))
		require.Equal(t, quote.Data.CreditAmount, decimal.NewFromFloat(stored.Amount).StringFixed(8))
		require.Equal(t, "USD", quote.Data.CreditCurrency)
		pay, err := decimal.NewFromString(quote.Data.PayAmount)
		require.NoError(t, err)
		require.Equal(t, pay.Sub(decimal.NewFromInt(20)).StringFixed(2), quote.Data.FeeAmount)
	}
	for _, raw := range []string{"-1", "0", "NaN", "Infinity", "1e100000", "1.000000001", "0.99", "1000.01", "999999999999.99999999"} {
		w := r.request(http.MethodPost, "/api/v1/payment/quote", fmt.Sprintf(`{"amount":%q,"payment_type":"alipay","order_type":"balance"}`, raw), token)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
	for _, body := range []string{`{"amount":20,"payment_type":"alipay","order_type":"balance"}`, `{"amount":"20","payment_type":"alipay","order_type":"subscription"}`} {
		require.Equal(t, http.StatusBadRequest, r.request(http.MethodPost, "/api/v1/payment/quote", body, token).Code)
	}
	w := r.request(http.MethodPost, "/api/v1/payment/quote", `{"amount":"20","payment_type":"wxpay","order_type":"balance"}`, token)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.NoError(t, r.settingRepo.Set(r.ctx, service.SettingBalancePayDisabled, "true"))
	require.Equal(t, http.StatusForbidden, r.request(http.MethodPost, "/api/v1/payment/quote", `{"amount":"20","payment_type":"alipay","order_type":"balance"}`, token).Code)
	require.NoError(t, r.settingRepo.Set(r.ctx, service.SettingBalancePayDisabled, "false"))
	stripe, err := r.client.PaymentProviderInstance.Create().SetName("quote-currency-fixture").SetProviderKey(payment.TypeStripe).SetConfig(`{"currency":"KWD"}`).SetSupportedTypes(payment.TypeStripe).SetEnabled(true).Save(r.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.client.PaymentProviderInstance.DeleteOneID(stripe.ID).Exec(r.ctx) })
	require.NoError(t, r.settingRepo.Set(r.ctx, service.SettingRechargeFeeRate, "0"))
	w = r.request(http.MethodPost, "/api/v1/payment/quote", `{"amount":"20.001","payment_type":"stripe","order_type":"balance"}`, token)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "cannot quote precision the existing order storage would truncate: %s", w.Body.String())
	secondStripe, err := r.client.PaymentProviderInstance.Create().SetName("quote-conflicting-currency").SetProviderKey(payment.TypeStripe).SetConfig(`{"currency":"USD"}`).SetSupportedTypes(payment.TypeStripe).SetEnabled(true).Save(r.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.client.PaymentProviderInstance.DeleteOneID(secondStripe.ID).Exec(r.ctx) })
	w = r.request(http.MethodPost, "/api/v1/payment/quote", `{"amount":"20","payment_type":"stripe","order_type":"balance"}`, token)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "PAYMENT_METHOD_CURRENCY_CONFLICT")
	require.NoError(t, r.settingRepo.Set(r.ctx, service.SettingPaymentEnabled, "false"))
	require.Equal(t, http.StatusForbidden, r.request(http.MethodPost, "/api/v1/payment/quote", `{"amount":"20","payment_type":"alipay","order_type":"balance"}`, token).Code)
	require.EqualValues(t, 2, providerCalls.Load())
}
