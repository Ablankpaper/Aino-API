//go:build integration && rollbackrehearsal

package rollbackrehearsal_test

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

const (
	retainedPaymentEmail           = "rollback-payment@example.test"
	retainedPaymentPassword        = "rollback-payment-password"
	retainedPaymentMerchantID      = "rollback-fixture-merchant"
	retainedPaymentMerchantKey     = "rollback-fixture-merchant-secret"
	retainedPaymentOutTradeNo      = "rollback-retained-order"
	retainedPaymentTradeNo         = "rollback-retained-provider-trade"
	retainedPaymentRechargeCode    = "rollback-retained-credit"
	retainedPaymentStartingBalance = "11.25000000"
	retainedPaymentCreditAmount    = "7.75000000"
	retainedPaymentPayAmount       = "7.75"
)

type retainedPaymentFixture struct {
	UserID             int64  `json:"user_id"`
	OrderID            int64  `json:"order_id"`
	ProviderInstanceID int64  `json:"provider_instance_id"`
	Email              string `json:"email"`
	Password           string `json:"-"`
	OutTradeNo         string `json:"out_trade_no"`
	TradeNo            string `json:"trade_no"`
	RechargeCode       string `json:"recharge_code"`
	StartingBalance    string `json:"starting_balance"`
	CreditAmount       string `json:"credit_amount"`
	PayAmount          string `json:"pay_amount"`
}

type retainedPaymentSnapshot struct {
	Balance             string `json:"balance"`
	OrderStatus         string `json:"order_status"`
	PaymentTradeNo      string `json:"payment_trade_no,omitempty"`
	OrderAmount         string `json:"order_amount"`
	OrderPayAmount      string `json:"order_pay_amount"`
	Paid                bool   `json:"paid"`
	Completed           bool   `json:"completed"`
	RedeemCodeID        int64  `json:"redeem_code_id,omitempty"`
	RedeemCodeCount     int    `json:"redeem_code_count"`
	RedeemCodeStatus    string `json:"redeem_code_status,omitempty"`
	RedeemCodeValue     string `json:"redeem_code_value,omitempty"`
	RedeemCodeUsedBy    int64  `json:"redeem_code_used_by,omitempty"`
	OrderPaidAuditCount int    `json:"order_paid_audit_count"`
	RechargeAuditCount  int    `json:"recharge_success_audit_count"`
}

type retainedPaymentProof struct {
	Fixture                  retainedPaymentFixture  `json:"fixture"`
	PaymentEnabled           string                  `json:"payment_enabled"`
	NewOrderCreateStatus     int                     `json:"new_order_create_status"`
	CallbackStatus           int                     `json:"callback_status"`
	CallbackBody             string                  `json:"callback_body"`
	DuplicateCallbackStatus  int                     `json:"duplicate_callback_status"`
	DuplicateCallbackBody    string                  `json:"duplicate_callback_body"`
	OldOrderQueryStatus      int                     `json:"old_order_query_status"`
	RestoredOrderQueryStatus int                     `json:"restored_order_query_status"`
	BeforeRollback           retainedPaymentSnapshot `json:"before_rollback"`
	AfterOldCallback         retainedPaymentSnapshot `json:"after_old_callback"`
	AfterDuplicate           retainedPaymentSnapshot `json:"after_duplicate"`
	AfterCurrentRestore      retainedPaymentSnapshot `json:"after_current_restore"`
}

func seedRetainedPaymentFixture(t *testing.T, db *sql.DB, baseURL string) retainedPaymentFixture {
	t.Helper()
	assertLoopbackURL(t, baseURL)
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte(retainedPaymentPassword), bcrypt.MinCost)
	require.NoError(t, err)
	fixture := retainedPaymentFixture{
		Email:           retainedPaymentEmail,
		Password:        retainedPaymentPassword,
		OutTradeNo:      retainedPaymentOutTradeNo,
		TradeNo:         retainedPaymentTradeNo,
		RechargeCode:    retainedPaymentRechargeCode,
		StartingBalance: retainedPaymentStartingBalance,
		CreditAmount:    retainedPaymentCreditAmount,
		PayAmount:       retainedPaymentPayAmount,
	}
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,balance,signup_source) VALUES($1,$2,$3,'email') RETURNING id`, fixture.Email, string(hash), fixture.StartingBalance).Scan(&fixture.UserID))
	_, err = db.ExecContext(ctx, `INSERT INTO auth_identities(user_id,provider_type,provider_key,provider_subject,verified_at) VALUES($1,'email','email',$2,now())`, fixture.UserID, fixture.Email)
	require.NoError(t, err)
	providerConfig, err := json.Marshal(map[string]string{
		"pid":       retainedPaymentMerchantID,
		"pkey":      retainedPaymentMerchantKey,
		"apiBase":   "http://127.0.0.1:1",
		"notifyUrl": baseURL + "/api/v1/payment/webhook/easypay",
		"returnUrl": baseURL + "/fixture-return",
	})
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO payment_provider_instances(provider_key,name,config,supported_types,enabled) VALUES('easypay','rollback retained callback',$1,'alipay',true) RETURNING id`, string(providerConfig)).Scan(&fixture.ProviderInstanceID))
	providerInstanceID := strconv.FormatInt(fixture.ProviderInstanceID, 10)
	providerSnapshot, err := json.Marshal(map[string]any{
		"schema_version":       1,
		"provider_instance_id": providerInstanceID,
		"provider_key":         "easypay",
		"merchant_id":          retainedPaymentMerchantID,
		"currency":             "CNY",
	})
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO payment_orders(
		user_id,user_email,user_name,amount,pay_amount,fee_rate,recharge_code,
		out_trade_no,payment_type,payment_trade_no,order_type,provider_instance_id,
		provider_key,provider_snapshot,status,expires_at,client_ip,src_host
	) VALUES($1,$2,'Rollback Payment Fixture',$3,$4,0,$5,$6,'alipay','',
		'balance',$7,'easypay',$8::jsonb,'PENDING',now()+interval '1 day','127.0.0.1','rollback.local') RETURNING id`,
		fixture.UserID, fixture.Email, fixture.CreditAmount, fixture.PayAmount,
		fixture.RechargeCode, fixture.OutTradeNo, providerInstanceID, string(providerSnapshot)).Scan(&fixture.OrderID))
	return fixture
}

func newRetainedPaymentProof(fixture retainedPaymentFixture) retainedPaymentProof {
	return retainedPaymentProof{Fixture: fixture}
}

func exerciseRetainedPaymentCallback(t *testing.T, db *sql.DB, baseURL string, fixture retainedPaymentFixture, proof *retainedPaymentProof) {
	t.Helper()
	before := proof.BeforeRollback
	require.Equal(t, fixture.StartingBalance, before.Balance)
	require.Equal(t, "PENDING", before.OrderStatus)
	require.Empty(t, before.PaymentTradeNo)
	require.Equal(t, "7.75", before.OrderAmount)
	require.Equal(t, fixture.PayAmount, before.OrderPayAmount)
	require.False(t, before.Paid)
	require.False(t, before.Completed)
	require.Zero(t, before.RedeemCodeCount)
	require.Zero(t, before.OrderPaidAuditCount)
	require.Zero(t, before.RechargeAuditCount)

	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT value FROM settings WHERE key='payment_enabled'`).Scan(&proof.PaymentEnabled))
	require.Equal(t, "false", proof.PaymentEnabled)
	token := login(t, baseURL, fixture.Email, fixture.Password)
	newOrderStatus, newOrderBody := request(t, http.MethodPost, baseURL+"/api/v1/payment/orders", `{"amount":10,"payment_type":"alipay"}`, token)
	proof.NewOrderCreateStatus = newOrderStatus
	require.Equal(t, http.StatusForbidden, newOrderStatus, "new order response: %s", newOrderBody)

	form := url.Values{
		"pid":          {retainedPaymentMerchantID},
		"out_trade_no": {fixture.OutTradeNo},
		"trade_no":     {fixture.TradeNo},
		"money":        {fixture.PayAmount},
		"trade_status": {"TRADE_SUCCESS"},
		"sign_type":    {"MD5"},
	}
	form.Set("sign", retainedEasyPaySignature(form))
	proof.CallbackStatus, proof.CallbackBody = postRetainedEasyPayCallback(t, baseURL, form)
	require.Equal(t, http.StatusOK, proof.CallbackStatus)
	require.Equal(t, "success", proof.CallbackBody)

	proof.AfterOldCallback = retainedPaymentSnapshotState(t, db, fixture)
	after := proof.AfterOldCallback
	require.Equal(t, "19.00000000", after.Balance)
	require.Equal(t, "COMPLETED", after.OrderStatus)
	require.Equal(t, fixture.TradeNo, after.PaymentTradeNo)
	require.Equal(t, "7.75", after.OrderAmount)
	require.Equal(t, fixture.PayAmount, after.OrderPayAmount)
	require.True(t, after.Paid)
	require.True(t, after.Completed)
	require.Equal(t, 1, after.RedeemCodeCount)
	require.Positive(t, after.RedeemCodeID)
	require.Equal(t, "used", after.RedeemCodeStatus)
	require.Equal(t, fixture.CreditAmount, after.RedeemCodeValue)
	require.Equal(t, fixture.UserID, after.RedeemCodeUsedBy)
	require.Equal(t, 1, after.OrderPaidAuditCount)
	require.Equal(t, 1, after.RechargeAuditCount)
	proof.OldOrderQueryStatus = requireRetainedOrderStatus(t, baseURL, token, fixture.OrderID, "COMPLETED")

	proof.DuplicateCallbackStatus, proof.DuplicateCallbackBody = postRetainedEasyPayCallback(t, baseURL, form)
	require.Equal(t, http.StatusOK, proof.DuplicateCallbackStatus)
	require.Equal(t, "success", proof.DuplicateCallbackBody)
	proof.AfterDuplicate = retainedPaymentSnapshotState(t, db, fixture)
	require.Equal(t, proof.AfterOldCallback, proof.AfterDuplicate, "duplicate callback must not grant balance or create a second ledger/audit entry")
}

func verifyRetainedPaymentAfterRestore(t *testing.T, db *sql.DB, baseURL string, fixture retainedPaymentFixture, proof *retainedPaymentProof) {
	t.Helper()
	proof.AfterCurrentRestore = retainedPaymentSnapshotState(t, db, fixture)
	require.Equal(t, proof.AfterDuplicate, proof.AfterCurrentRestore)
	token := login(t, baseURL, fixture.Email, fixture.Password)
	proof.RestoredOrderQueryStatus = requireRetainedOrderStatus(t, baseURL, token, fixture.OrderID, "COMPLETED")
}

func retainedPaymentSnapshotState(t *testing.T, db *sql.DB, fixture retainedPaymentFixture) retainedPaymentSnapshot {
	t.Helper()
	ctx := context.Background()
	var state retainedPaymentSnapshot
	require.NoError(t, db.QueryRowContext(ctx, `SELECT balance::text FROM users WHERE id=$1`, fixture.UserID).Scan(&state.Balance))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status,payment_trade_no,amount::text,pay_amount::text,paid_at IS NOT NULL,completed_at IS NOT NULL FROM payment_orders WHERE id=$1 AND user_id=$2 AND out_trade_no=$3`, fixture.OrderID, fixture.UserID, fixture.OutTradeNo).Scan(
		&state.OrderStatus, &state.PaymentTradeNo, &state.OrderAmount, &state.OrderPayAmount, &state.Paid, &state.Completed,
	))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM redeem_codes WHERE code=$1`, fixture.RechargeCode).Scan(&state.RedeemCodeCount))
	if state.RedeemCodeCount > 0 {
		require.Equal(t, 1, state.RedeemCodeCount)
		require.NoError(t, db.QueryRowContext(ctx, `SELECT id,status,value::text,used_by FROM redeem_codes WHERE code=$1`, fixture.RechargeCode).Scan(&state.RedeemCodeID, &state.RedeemCodeStatus, &state.RedeemCodeValue, &state.RedeemCodeUsedBy))
	}
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM payment_audit_logs WHERE order_id=$1 AND action='ORDER_PAID'`, strconv.FormatInt(fixture.OrderID, 10)).Scan(&state.OrderPaidAuditCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM payment_audit_logs WHERE order_id=$1 AND action='RECHARGE_SUCCESS'`, strconv.FormatInt(fixture.OrderID, 10)).Scan(&state.RechargeAuditCount))
	return state
}

func requireRetainedOrderStatus(t *testing.T, baseURL, token string, orderID int64, expected string) int {
	t.Helper()
	status, raw := request(t, http.MethodGet, fmt.Sprintf("%s/api/v1/payment/orders/%d", baseURL, orderID), "", token)
	require.Equal(t, http.StatusOK, status, "retained order query response: %s", raw)
	envelope := decodeEnvelope(t, raw)
	var order struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(envelope.Data, &order))
	require.Equal(t, orderID, order.ID)
	require.Equal(t, expected, order.Status)
	return status
}

func postRetainedEasyPayCallback(t *testing.T, baseURL string, form url.Values) (int, string) {
	t.Helper()
	endpoint := baseURL + "/api/v1/payment/webhook/easypay"
	assertLoopbackURL(t, endpoint)
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "aino-rollback-retained-callback")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(body)
}

func retainedEasyPaySignature(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key == "sign" || key == "sign_type" || values.Get(key) == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values.Get(key))
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&") + retainedPaymentMerchantKey))
	return hex.EncodeToString(sum[:])
}
