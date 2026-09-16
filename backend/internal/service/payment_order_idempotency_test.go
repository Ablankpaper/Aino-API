package service

import (
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPaymentIdempotencyFingerprintKeepsPurchaseIntent(t *testing.T) {
	a := CreateOrderRequest{UserID: 17, ClientOrderID: uuid.NewString(), Amount: 20, PaymentType: "alipay", OrderType: "balance"}
	require.NoError(t, normalizePaymentIntent(&a))
	b := a
	b.ExpectedQuote = &PaymentQuote{RequestedAmount: "20.00", PayAmount: "20", CreditAmount: "20.00000000", FeeAmount: "0", PaymentCurrency: "CNY", CreditCurrency: "USD"}
	b.ClientIP = "192.0.2.1"
	require.NoError(t, normalizePaymentIntent(&b))
	require.Equal(t, a.requestHash, b.requestHash)
	for _, change := range []func(*CreateOrderRequest){func(r *CreateOrderRequest) { r.Amount = 21 }, func(r *CreateOrderRequest) { r.PaymentType = "wxpay" }, func(r *CreateOrderRequest) { r.UserID = 18 }} {
		b = a
		change(&b)
		require.NoError(t, normalizePaymentIntent(&b))
		require.NotEqual(t, a.requestHash, b.requestHash)
	}
}

func TestPaymentIdempotencyCheckoutUsesPersistedExactHTTPSOrigin(t *testing.T) {
	sel := &payment.InstanceSelection{ProviderKey: payment.TypeEasyPay, Config: map[string]string{"apiBase": "https://checkout.example.test/mapi.php"}}
	o := &dbent.PaymentOrder{Status: OrderStatusPending, ExpiresAt: time.Now().Add(time.Hour), ProviderSnapshot: map[string]any{"checkout_origins": paymentCheckoutOrigins(sel)}}
	qr := "weixin://valid-qr-data"
	o.QrCode = &qr
	for _, raw := range []string{"https://checkout.example.test/pay?token=secret", "http://checkout.example.test/pay", "https://checkout.example.test.evil.test/pay", "https://user@checkout.example.test/pay", "javascript:alert(1)", "//checkout.example.test/pay", "https://different.test/pay"} {
		o.PayURL = &raw
		got := PaymentOrderCheckout(o)
		require.NotNil(t, got)
		require.Equal(t, qr, got.QRCode)
		if raw == "https://checkout.example.test/pay?token=secret" {
			require.Equal(t, raw, got.PayURL)
		} else {
			require.Empty(t, got.PayURL)
		}
	}
	// Editing today's provider cannot expand an old order's browser destination policy.
	sel.Config["apiBase"] = "https://different.test"
	raw := "https://different.test/pay"
	o.PayURL = &raw
	require.Empty(t, PaymentOrderCheckout(o).PayURL)
	for _, status := range []string{OrderStatusPaid, OrderStatusRecharging, OrderStatusCompleted, OrderStatusRefunded, OrderStatusCancelled} {
		o.Status = status
		require.Nil(t, PaymentOrderCheckout(o))
	}
	o.Status = OrderStatusPending
	o.ExpiresAt = time.Now().Add(-time.Second)
	require.Nil(t, PaymentOrderCheckout(o))
}

func TestPaymentIdempotencyExpectedQuoteIsStrictAndComparisonOnly(t *testing.T) {
	q := PaymentQuote{RequestedAmount: "20", PayAmount: "20", CreditAmount: "2.8", FeeAmount: "0", PaymentCurrency: "CNY", CreditCurrency: "USD"}
	o := &dbent.PaymentOrder{Amount: 2.8, PayAmount: 20, ProviderSnapshot: map[string]any{"requested_amount": "20", "currency": "CNY"}}
	require.NoError(t, validateExpectedPaymentQuote(&q))
	require.True(t, paymentQuoteMatchesOrder(&q, o))
	for _, raw := range []string{"", "NaN", "1e2", "-1", "+20", "20.000000001"} {
		bad := q
		bad.PayAmount = raw
		require.Error(t, validateExpectedPaymentQuote(&bad))
	}
	q.PayAmount = "21"
	require.False(t, paymentQuoteMatchesOrder(&q, o))
	require.Equal(t, 20.0, o.PayAmount)
	q.FeeAmount = "-0.00"
	require.Error(t, validateExpectedPaymentQuote(&q))
	q.FeeAmount = "-0.01"
	require.Error(t, validateExpectedPaymentQuote(&q))
	for _, raw := range []string{"", "invalid", "-20", "20e2"} {
		o.ProviderSnapshot["requested_amount"] = raw
		fields := PaymentOrderDecimalDetails(o)
		require.Empty(t, fields.RequestedAmountDecimal)
		require.Empty(t, fields.FeeAmountDecimal)
		require.False(t, paymentQuoteMatchesOrder(&q, o))
	}
}
