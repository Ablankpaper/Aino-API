//go:build integration

package service_test

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

type paymentIdempotencyRig struct {
	client          *dbent.Client
	payments        *service.PaymentService
	router          *gin.Engine
	userID          int64
	creates         atomic.Int64
	queries         atomic.Int64
	loseResponse    atomic.Bool
	merchantOrders  sync.Map
	db              *sql.DB
	cfg             *service.PaymentConfigService
	timeoutResponse atomic.Bool
	queryReply      func(http.ResponseWriter, *http.Request)
}

func newPaymentIdempotencyRig(t *testing.T) *paymentIdempotencyRig {
	t.Helper()
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:18.1-alpine3.23", tcpostgres.WithDatabase("payment_fixture"), tcpostgres.WithUsername("fixture"), tcpostgres.WithPassword("fixture"), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(ctx)) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	r := &paymentIdempotencyRig{client: client, db: db}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		merchant := req.Form.Get("out_trade_no")
		if req.URL.Path == "/api.php" {
			r.queries.Add(1)
			if _, ok := r.merchantOrders.Load(merchant); !ok {
				t.Errorf("query changed merchant order: %s", merchant)
			}
			if r.queryReply != nil {
				r.queryReply(w, req)
				return
			}
			_, _ = w.Write([]byte(`{"code":1,"status":0,"money":"20.00"}`))
			return
		}
		r.creates.Add(1)
		r.merchantOrders.Store(merchant, true)
		if r.timeoutResponse.Load() {
			<-req.Context().Done()
			return
		}
		if r.loseResponse.Load() {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte(`{"code":1,"trade_no":"fixture","qrcode":"weixin://fixture","payurl":"https://attacker.invalid/checkout"}`))
	}))
	t.Cleanup(server.Close)
	settings := repository.NewSettingRepository(client)
	require.NoError(t, settings.SetMultiple(ctx, map[string]string{
		service.SettingPaymentEnabled: "true", service.SettingMinRechargeAmount: "1", service.SettingMaxRechargeAmount: "1000",
		service.SettingBalancePayDisabled: "false", service.SettingMaxPendingOrders: "100", service.SettingDailyRechargeLimit: "0",
		service.SettingCancelRateLimitOn: "false", service.SettingBalanceRechargeMult: "1", service.SettingRechargeFeeRate: "0",
		"payment_visible_method_alipay_source": "easypay", "payment_visible_method_alipay_enabled": "true",
	}))
	config, err := json.Marshal(map[string]string{"pid": "fixture", "pkey": "fixture-secret", "apiBase": server.URL, "notifyUrl": server.URL + "/notify", "returnUrl": server.URL + "/return"})
	require.NoError(t, err)
	_, err = client.PaymentProviderInstance.Create().SetName("fixture").SetProviderKey(payment.TypeEasyPay).SetConfig(string(config)).SetSupportedTypes("alipay").SetEnabled(true).Save(ctx)
	require.NoError(t, err)
	user, err := client.User.Create().SetEmail("fixture@example.test").SetPasswordHash("fixture").SetStatus("active").Save(ctx)
	require.NoError(t, err)
	r.userID = user.ID
	r.cfg = service.NewPaymentConfigService(client, settings, nil)
	r.restart()
	return r
}

func (r *paymentIdempotencyRig) restart() {
	users := repository.NewUserRepository(r.client, r.db)
	redeem := service.NewRedeemService(repository.NewRedeemCodeRepository(r.client), users, nil, nil, nil, r.client, nil, nil)
	registry := payment.NewRegistry()
	r.payments = service.NewPaymentService(r.client, registry, payment.NewDefaultLoadBalancer(r.client, nil), redeem, nil, r.cfg, users, nil, nil)
	h := handler.NewPaymentHandler(r.payments, r.cfg)
	r.router = gin.New()
	r.router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: r.userID})
		c.Next()
	})
	r.router.POST("/orders", h.CreateOrder)
	r.router.GET("/orders/:id", h.GetOrder)
	r.router.POST("/notify", handler.NewPaymentWebhookHandler(r.payments, registry).EasyPayNotify)
	r.router.POST("/public/verify", h.VerifyOrderPublic)
}

func (r *paymentIdempotencyRig) create(id, amount string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	body := fmt.Sprintf(`{"amount":%s,"client_order_id":%q,"payment_type":"alipay","order_type":"balance"}`, amount, id)
	req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.router.ServeHTTP(w, req)
	return w
}

func TestOrderRetryKeepsOneMerchantOrder(t *testing.T) {
	r := newPaymentIdempotencyRig(t)
	id := uuid.NewString()
	first, second := r.create(id, "20.00"), r.create(id, "20")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	var a, b struct {
		Data service.CreateOrderResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &a))
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &b))
	require.Equal(t, a.Data.OrderID, b.Data.OrderID)
	require.EqualValues(t, 1, r.creates.Load())
	require.Equal(t, http.StatusConflict, r.create(id, "30").Code)
	require.Empty(t, a.Data.PayURL, "desktop must not navigate an unrelated checkout origin")
	require.Equal(t, "weixin://fixture", a.Data.QRCode)
	_, err := r.payments.GetOrder(context.Background(), a.Data.OrderID, r.userID+1)
	require.Error(t, err)
	_, err = r.payments.CancelOrder(context.Background(), a.Data.OrderID, r.userID+1)
	require.Error(t, err)
	public := httptest.NewRecorder()
	r.router.ServeHTTP(public, httptest.NewRequest(http.MethodPost, "/public/verify", strings.NewReader(fmt.Sprintf(`{"out_trade_no":%q}`, a.Data.OutTradeNo))))
	require.Equal(t, http.StatusOK, public.Code, public.Body.String())
	require.NotContains(t, public.Body.String(), "weixin://")
	require.NotContains(t, public.Body.String(), "checkout")
	other, err := r.client.User.Create().SetEmail("other@example.test").SetPasswordHash("fixture").SetStatus("active").Save(context.Background())
	require.NoError(t, err)
	otherOrder, err := r.payments.CreateOrder(context.Background(), service.CreateOrderRequest{UserID: other.ID, ClientOrderID: id, Amount: 20, PaymentType: "alipay"})
	require.NoError(t, err)
	require.NotEqual(t, a.Data.OrderID, otherOrder.OrderID)
}

func TestPaymentUnknownAndConcurrentRetryDoNotResubmit(t *testing.T) {
	r := newPaymentIdempotencyRig(t)
	r.loseResponse.Store(true)
	id := uuid.NewString()
	first := r.create(id, "20")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	r.restart()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := r.create(id, "20.00")
			if w.Code != http.StatusOK {
				t.Errorf("retry: %d %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, r.creates.Load())
	require.Positive(t, r.queries.Load())
	orders, err := r.client.PaymentOrder.Query().Where(paymentorder.UserID(r.userID)).All(context.Background())
	require.NoError(t, err)
	require.Len(t, orders, 1)
	require.Equal(t, service.OrderStatusPending, orders[0].Status)
}

func TestPaymentIdempotencyConcurrentInitialRequests(t *testing.T) {
	r := newPaymentIdempotencyRig(t)
	id := uuid.NewString()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := r.create(id, "20")
			if w.Code != http.StatusOK {
				t.Errorf("create: %d %s", w.Code, w.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 1, r.creates.Load())
	count, err := r.client.PaymentOrder.Query().Where(paymentorder.UserID(r.userID)).Count(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestPaymentUnknownCheckoutSaveFailureAndTimeout(t *testing.T) {
	r := newPaymentIdempotencyRig(t)
	var fail atomic.Bool
	fail.Store(true)
	r.client.PaymentOrder.Use(func(next dbent.Mutator) dbent.Mutator {
		return dbent.MutateFunc(func(ctx context.Context, m dbent.Mutation) (dbent.Value, error) {
			pm := m.(*dbent.PaymentOrderMutation)
			if _, ok := pm.QrCode(); ok && fail.CompareAndSwap(true, false) {
				return nil, errors.New("fixture checkout persistence failure")
			}
			return next.Mutate(ctx, m)
		})
	})
	id := uuid.NewString()
	w := r.create(id, "20")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"payment_unknown":true`)
	r.restart()
	w = r.create(id, "20.00")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.EqualValues(t, 1, r.creates.Load())
	require.Positive(t, r.queries.Load())

	r.timeoutResponse.Store(true)
	timeoutID := uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := r.payments.CreateOrder(ctx, service.CreateOrderRequest{UserID: r.userID, ClientOrderID: timeoutID, Amount: 20, PaymentType: "alipay", ClientIP: "127.0.0.1"})
	require.Error(t, err)
	r.timeoutResponse.Store(false)
	r.restart()
	w = r.create(timeoutID, "20")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"payment_unknown":true`)
	require.EqualValues(t, 2, r.creates.Load(), "timeout recovery must query, not submit again")
	orders, err := r.client.PaymentOrder.Query().All(context.Background())
	require.NoError(t, err)
	for _, o := range orders {
		require.Equal(t, service.OrderStatusPending, o.Status)
	}
}

func TestPaymentIdempotencyDecimalQuoteAndLegacyCompatibility(t *testing.T) {
	r := newPaymentIdempotencyRig(t)
	id := uuid.NewString()
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.router.ServeHTTP(w, req)
		return w
	}
	for _, body := range []string{
		`{"amount":0,"amount_decimal":"20"}`, `{"amount":21,"amount_decimal":"20"}`, `{"amount":20.0000000000000001,"amount_decimal":"20"}`, `{"amount_decimal":"NaN"}`, `{"amount_decimal":"1e2"}`, `{"amount_decimal":"0"}`, `{"amount_decimal":20}`, `{"amount":"20"}`, `{"amount":20,"client_order_id":"not-a-uuid"}`, `{"amount":20,"payment_source":"aino_desktop"}`, fmt.Sprintf(`{"amount":20,"payment_source":"aino_desktop","client_order_id":%q}`, id),
	} {
		w := post(strings.TrimSuffix(body, "}") + `,"payment_type":"alipay"}`)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
	base := fmt.Sprintf(`"amount_decimal":"20.00","client_order_id":%q,"payment_type":"alipay"`, id)
	w := post(`{` + base + `}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"confirmation_required":true`)
	quote := `"expected_quote":{"requested_amount":"20","pay_amount":"20.00","credit_amount":"20.00000000","fee_amount":"0.00","payment_currency":"CNY","credit_currency":"USD"}`
	w = post(`{` + base + `,` + quote + `}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"confirmation_required":false`)
	require.EqualValues(t, 1, r.creates.Load())
	var result struct {
		Data service.CreateOrderResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	get := httptest.NewRecorder()
	r.router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/orders/%d", result.Data.OrderID), nil))
	require.Contains(t, get.Body.String(), `"checkout"`)
	require.Contains(t, get.Body.String(), `"confirmation_required":false`)
	for _, fragment := range []string{`"client_order_id":"` + id + `"`, `"requested_amount_decimal":"20"`, `"pay_amount_decimal":"20.00"`, `"credit_amount_decimal":"20.00000000"`, `"fee_amount_decimal":"0.00"`, `"payment_currency":"CNY"`, `"credit_currency":"USD"`} {
		require.Contains(t, get.Body.String(), fragment)
		require.Contains(t, w.Body.String(), fragment)
	}
	require.NotContains(t, get.Body.String(), "attacker.invalid")
	w = post(`{` + base + `,` + strings.Replace(quote, `"pay_amount":"20.00"`, `"pay_amount":"21.00"`, 1) + `}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"confirmation_required":true`)
	for range 2 {
		w = post(`{"amount":20,"payment_type":"alipay"}`)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "https://attacker.invalid/checkout", "legacy site provider behavior remains unchanged")
		require.NotContains(t, w.Body.String(), "requested_amount_decimal")
		require.NotContains(t, w.Body.String(), "fee_amount_decimal")
	}
	require.EqualValues(t, 3, r.creates.Load())
	for _, amount := range []string{"5.004", "5.005"} {
		quote, err := r.payments.QuoteBalance(context.Background(), r.userID, service.PaymentQuoteInput{Amount: amount, PaymentType: "alipay", OrderType: "balance"})
		require.Error(t, err, "the existing CNY provider boundary rejects sub-cent inputs")
		require.Nil(t, quote)
	}
	require.EqualValues(t, 3, r.creates.Load())
	require.NoError(t, repository.NewSettingRepository(r.client).Set(context.Background(), service.SettingRechargeFeeRate, "1.17"))
	for _, amount := range []string{"5.00", "5.01"} {
		quote, err := r.payments.QuoteBalance(context.Background(), r.userID, service.PaymentQuoteInput{Amount: amount, PaymentType: "alipay", OrderType: "balance"})
		require.NoError(t, err)
		body, err := json.Marshal(map[string]any{"amount_decimal": amount, "client_order_id": uuid.NewString(), "payment_source": "aino_desktop", "payment_type": "alipay", "order_type": "balance", "expected_quote": quote})
		require.NoError(t, err)
		w = post(string(body))
		require.Equal(t, http.StatusOK, w.Code, "quote %s fee %s: %s", amount, quote.FeeAmount, w.Body.String())
		var created struct {
			Data service.CreateOrderResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
		require.False(t, created.Data.ConfirmationRequired)
		require.Equal(t, quote.PayAmount, created.Data.PayAmountDecimal)
		require.Equal(t, quote.CreditAmount, created.Data.CreditAmountDecimal)
		require.Equal(t, quote.FeeAmount, created.Data.FeeAmountDecimal)
	}
}

func TestPaymentIdempotencyReclaimsOnlyUndispatchedCreation(t *testing.T) {
	r := newPaymentIdempotencyRig(t)
	var fail atomic.Bool
	fail.Store(true)
	r.client.PaymentOrder.Use(func(next dbent.Mutator) dbent.Mutator {
		return dbent.MutateFunc(func(ctx context.Context, m dbent.Mutation) (dbent.Value, error) {
			pm := m.(*dbent.PaymentOrderMutation)
			if state, ok := pm.CreationState(); ok && state == "submitted" && fail.CompareAndSwap(true, false) {
				return nil, errors.New("fixture durable dispatch marker failure")
			}
			return next.Mutate(ctx, m)
		})
	})
	id := uuid.NewString()
	w := r.create(id, "20")
	require.NotEqual(t, http.StatusOK, w.Code)
	require.Zero(t, r.creates.Load())
	o, err := r.client.PaymentOrder.Query().Only(context.Background())
	require.NoError(t, err)
	require.Equal(t, "ready", o.CreationState)
	_, err = r.client.PaymentOrder.UpdateOneID(o.ID).SetCreationState("creating").SetCreationLeaseUntil(time.Now().Add(-time.Minute)).Save(context.Background())
	require.NoError(t, err)
	require.NoError(t, repository.NewSettingRepository(r.client).SetMultiple(context.Background(), map[string]string{service.SettingRechargeFeeRate: "10", service.SettingBalanceRechargeMult: "2"}))
	r.restart()
	w = r.create(id, "20")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.EqualValues(t, 1, r.creates.Load())
	stored, err := r.client.PaymentOrder.Query().Only(context.Background())
	require.NoError(t, err)
	require.Equal(t, o.ID, stored.ID)
	require.Equal(t, o.OutTradeNo, stored.OutTradeNo)
	require.Equal(t, o.PayAmount, stored.PayAmount)
	require.Equal(t, o.Amount, stored.Amount)
	require.NoError(t, r.db.Close())
	w = r.create(id, "20")
	require.NotEqual(t, http.StatusOK, w.Code)
	require.EqualValues(t, 1, r.creates.Load(), "database failure must not degrade to creating a second merchant order")
}

func TestPaymentIdempotencyRecoveryPreservesFulfillmentLease(t *testing.T) {
	r := newPaymentIdempotencyRig(t)
	id := uuid.NewString()
	w := r.create(id, "20")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	o, err := r.client.PaymentOrder.Query().Only(context.Background())
	require.NoError(t, err)
	lease := time.Now().UTC().Truncate(time.Microsecond)
	_, err = r.client.PaymentOrder.UpdateOneID(o.ID).SetStatus(service.OrderStatusRecharging).SetUpdatedAt(lease).Save(context.Background())
	require.NoError(t, err)
	_, err = r.payments.CreateOrder(context.Background(), service.CreateOrderRequest{UserID: r.userID, ClientOrderID: id, Amount: 20, PaymentType: "alipay", ExpectedQuote: &service.PaymentQuote{RequestedAmount: "20", PayAmount: "20", CreditAmount: "20", FeeAmount: "0", PaymentCurrency: "CNY", CreditCurrency: "USD"}})
	require.NoError(t, err)
	stored, err := r.client.PaymentOrder.Get(context.Background(), o.ID)
	require.NoError(t, err)
	require.True(t, stored.UpdatedAt.Equal(lease), "checkout metadata must not invalidate the independent fulfillment lease")
	require.Equal(t, service.OrderStatusRecharging, stored.Status)
	var callback atomic.Bool
	r.client.PaymentOrder.Use(func(next dbent.Mutator) dbent.Mutator {
		return dbent.MutateFunc(func(ctx context.Context, m dbent.Mutation) (dbent.Value, error) {
			pm := m.(*dbent.PaymentOrderMutation)
			if _, ok := pm.QrCode(); ok && callback.CompareAndSwap(false, true) {
				id, _ := pm.ID()
				_, err := r.client.PaymentOrder.UpdateOneID(id).SetStatus(service.OrderStatusRecharging).SetUpdatedAt(lease).Save(ctx)
				if err != nil {
					return nil, err
				}
			}
			return next.Mutate(ctx, m)
		})
	})
	secondID := uuid.NewString()
	w = r.create(secondID, "20")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	second, err := r.client.PaymentOrder.Query().Where(paymentorder.ClientOrderIDEQ(secondID)).Only(context.Background())
	require.NoError(t, err)
	require.True(t, second.UpdatedAt.Equal(lease), "late provider checkout persistence/release must preserve a concurrent fulfillment lease")
	require.Equal(t, service.OrderStatusRecharging, second.Status)
}

func (r *paymentIdempotencyRig) notify(outTradeNo, money, pid, status string, valid bool) *httptest.ResponseRecorder {
	p := url.Values{"pid": {pid}, "out_trade_no": {outTradeNo}, "trade_no": {"fixture"}, "money": {money}, "trade_status": {status}}
	keys := make([]string, 0, len(p))
	for key := range p {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(p))
	for _, key := range keys {
		parts = append(parts, key+"="+p.Get(key))
	}
	sign := fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(parts, "&")+"fixture-secret")))
	if !valid {
		sign = "invalid"
	}
	p.Set("sign", sign)
	p.Set("sign_type", "MD5")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(p.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.router.ServeHTTP(w, req)
	return w
}

func TestPaymentIdempotencyQueryBackfillPreservesCallbackFulfillmentLease(t *testing.T) {
	r := newPaymentIdempotencyRig(t)
	r.loseResponse.Store(true)
	id := uuid.NewString()
	w := r.create(id, "20")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	o, err := r.client.PaymentOrder.Query().Only(ctx)
	require.NoError(t, err)
	require.Equal(t, "submitted", o.CreationState)
	require.Empty(t, o.PaymentTradeNo)

	queryStarted, allowQuery := make(chan struct{}), make(chan struct{})
	fulfillmentStarted, allowFulfillment := make(chan struct{}), make(chan struct{})
	var releaseQuery, releaseFulfillment sync.Once
	defer releaseQuery.Do(func() { close(allowQuery) })
	defer releaseFulfillment.Do(func() { close(allowFulfillment) })
	r.queryReply = func(w http.ResponseWriter, req *http.Request) {
		close(queryStarted)
		select {
		case <-allowQuery:
			_, _ = w.Write([]byte(`{"code":1,"status":1,"money":"20.00","trade_no":"fixture"}`))
		case <-ctx.Done():
		}
	}
	// Redeem creation occurs after the callback has acquired and reloaded the
	// real fulfillment lease, but before it can credit balance or complete.
	r.client.RedeemCode.Use(func(next dbent.Mutator) dbent.Mutator {
		return dbent.MutateFunc(func(callCtx context.Context, m dbent.Mutation) (dbent.Value, error) {
			if m.Op() == dbent.OpCreate {
				close(fulfillmentStarted)
				select {
				case <-allowFulfillment:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return next.Mutate(callCtx, m)
		})
	})
	recoveryDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { recoveryDone <- r.create(id, "20") }()
	select {
	case <-queryStarted:
	case <-ctx.Done():
		t.Fatal("recovery query did not start")
	}
	callbackDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { callbackDone <- r.notify(o.OutTradeNo, "20", "fixture", "TRADE_SUCCESS", true) }()
	select {
	case <-fulfillmentStarted:
	case <-ctx.Done():
		t.Fatal("callback did not acquire fulfillment")
	}
	claimed, err := r.client.PaymentOrder.Get(ctx, o.ID)
	require.NoError(t, err)
	require.Equal(t, service.OrderStatusRecharging, claimed.Status)
	require.Equal(t, "fixture", claimed.PaymentTradeNo)
	releaseQuery.Do(func() { close(allowQuery) })
	select {
	case result := <-recoveryDone:
		require.Equal(t, http.StatusOK, result.Code, result.Body.String())
	case <-ctx.Done():
		t.Fatal("recovery did not finish")
	}
	afterQuery, err := r.client.PaymentOrder.Get(ctx, o.ID)
	require.NoError(t, err)
	releaseFulfillment.Do(func() { close(allowFulfillment) })
	select {
	case result := <-callbackDone:
		require.Equal(t, http.StatusOK, result.Code, result.Body.String())
	case <-ctx.Done():
		t.Fatal("callback did not finish")
	}
	finished, err := r.client.PaymentOrder.Get(ctx, o.ID)
	require.NoError(t, err)
	u, err := r.client.User.Get(ctx, r.userID)
	require.NoError(t, err)
	require.Equal(t, 20.0, u.Balance)
	require.Equal(t, service.OrderStatusCompleted, finished.Status)
	require.True(t, claimed.UpdatedAt.Equal(afterQuery.UpdatedAt), "query backfill must preserve the callback's fulfillment lease version")
	r.notify(o.OutTradeNo, "20", "fixture", "TRADE_SUCCESS", true)
	u, err = r.client.User.Get(ctx, r.userID)
	require.NoError(t, err)
	require.Equal(t, 20.0, u.Balance)
	require.EqualValues(t, 1, r.creates.Load())
}

func TestPaymentIdempotencySignedCallbacksCreditOnlyOnce(t *testing.T) {
	r := newPaymentIdempotencyRig(t)
	w := r.create(uuid.NewString(), "20")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	o, err := r.client.PaymentOrder.Query().Only(context.Background())
	require.NoError(t, err)
	notify := func(money, pid, status string, valid bool) *httptest.ResponseRecorder {
		return r.notify(o.OutTradeNo, money, pid, status, valid)
	}
	notify("20", "fixture", "TRADE_SUCCESS", false)
	notify("30", "fixture", "TRADE_SUCCESS", true)
	notify("20", "other-merchant", "TRADE_SUCCESS", true)
	u, err := r.client.User.Get(context.Background(), r.userID)
	require.NoError(t, err)
	require.Zero(t, u.Balance)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); notify("20", "fixture", "TRADE_SUCCESS", true) }()
	}
	wg.Wait()
	notify("20", "fixture", "WAIT_BUYER_PAY", true)
	u, err = r.client.User.Get(context.Background(), r.userID)
	require.NoError(t, err)
	require.Equal(t, 20.0, u.Balance)
	o, err = r.client.PaymentOrder.Get(context.Background(), o.ID)
	require.NoError(t, err)
	require.Equal(t, service.OrderStatusCompleted, o.Status)
	for _, status := range []string{service.OrderStatusRefunded, service.OrderStatusCancelled} {
		_, err = r.client.PaymentOrder.UpdateOneID(o.ID).SetStatus(status).Save(context.Background())
		require.NoError(t, err)
		notify("20", "fixture", "TRADE_SUCCESS", true)
		u, err = r.client.User.Get(context.Background(), r.userID)
		require.NoError(t, err)
		require.Equal(t, 20.0, u.Balance)
	}
}
