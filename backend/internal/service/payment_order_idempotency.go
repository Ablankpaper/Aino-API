package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const paymentCreationLease = 30 * time.Second
const PaymentSourceAinoDesktop = "aino_desktop"

var paymentQuoteDecimalPattern = regexp.MustCompile(`^[0-9]{1,18}(\.[0-9]{1,8})?$`)

type PaymentCheckout struct {
	QRCode    string    `json:"qr_code,omitempty"`
	PayURL    string    `json:"pay_url,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

type PaymentOrderDecimalFields struct {
	ClientOrderID          string `json:"client_order_id,omitempty"`
	RequestedAmountDecimal string `json:"requested_amount_decimal,omitempty"`
	PayAmountDecimal       string `json:"pay_amount_decimal"`
	CreditAmountDecimal    string `json:"credit_amount_decimal"`
	FeeAmountDecimal       string `json:"fee_amount_decimal,omitempty"`
	PaymentCurrency        string `json:"payment_currency"`
	CreditCurrency         string `json:"credit_currency"`
}

func PaymentOrderDecimalDetails(o *dbent.PaymentOrder) PaymentOrderDecimalFields {
	if o == nil {
		return PaymentOrderDecimalFields{}
	}
	currency := PaymentOrderCurrency(o)
	fields := PaymentOrderDecimalFields{PayAmountDecimal: payment.FormatAmountForCurrency(o.PayAmount, currency), CreditAmountDecimal: decimal.NewFromFloat(o.Amount).StringFixed(8), PaymentCurrency: currency, CreditCurrency: "USD"}
	if raw := psStringValue(o.ClientOrderID); len(raw) == 36 {
		if id, err := uuid.Parse(raw); err == nil && id != uuid.Nil {
			fields.ClientOrderID = id.String()
		}
	}
	requested, _ := o.ProviderSnapshot["requested_amount"].(string)
	if _, err := ParsePaymentAmountDecimal(requested); err == nil {
		d, _ := decimal.NewFromString(requested)
		fields.RequestedAmountDecimal = d.String()
		fields.FeeAmountDecimal = decimal.NewFromFloat(o.PayAmount).Sub(d).StringFixed(int32(payment.CurrencyMaxFractionDigits(currency)))
	}
	return fields
}

// A dispatch outcome survives provider error mapping and checkout persistence failures.
type paymentAttemptError struct {
	cause     error
	submitted bool
}

func (e *paymentAttemptError) Error() string { return "payment creation attempt failed" }
func (e *paymentAttemptError) Unwrap() error { return e.cause }

func normalizePaymentIntent(req *CreateOrderRequest) error {
	if req.ClientOrderID == "" {
		if NormalizePaymentSource(req.PaymentSource) == PaymentSourceAinoDesktop {
			return infraerrors.BadRequest("CLIENT_ORDER_ID_REQUIRED", "desktop checkout requires client_order_id")
		}
		return nil
	}
	if req.OrderType != payment.OrderTypeBalance || (req.PaymentType != payment.TypeAlipay && req.PaymentType != payment.TypeWxpay) {
		return infraerrors.BadRequest("UNSUPPORTED_RESUMABLE_CHECKOUT", "resumable checkout supports balance Alipay and WeChat orders")
	}
	if req.OpenID != "" || req.IsWeChatBrowser {
		return infraerrors.BadRequest("UNSUPPORTED_RESUMABLE_CHECKOUT", "resumable checkout requires scan or browser checkout")
	}
	id, err := uuid.Parse(req.ClientOrderID)
	if err != nil || len(req.ClientOrderID) != 36 || id == uuid.Nil {
		return infraerrors.BadRequest("INVALID_CLIENT_ORDER_ID", "client_order_id must be a UUID")
	}
	req.ClientOrderID = id.String()
	if math.IsNaN(req.Amount) || math.IsInf(req.Amount, 0) {
		return infraerrors.BadRequest("INVALID_AMOUNT", "amount must be finite")
	}
	if err := validateExpectedPaymentQuote(req.ExpectedQuote); err != nil {
		return err
	}
	returnURL, err := CanonicalizeReturnURL(req.ReturnURL, req.SrcHost, req.SrcURL)
	if err != nil {
		return err
	}
	req.requestHash, err = BuildIdempotencyFingerprint("POST", "/payment/orders", fmt.Sprintf("user:%d", req.UserID), struct {
		Amount, PaymentType, OrderType, ReturnURL, OpenID string
		PlanID                                            int64
	}{decimal.NewFromFloat(req.Amount).String(), req.PaymentType, req.OrderType, returnURL, req.OpenID, req.PlanID})
	return err
}

func validateExpectedPaymentQuote(q *PaymentQuote) error {
	if q == nil {
		return nil
	}
	for i, raw := range []string{q.RequestedAmount, q.PayAmount, q.CreditAmount, q.FeeAmount} {
		if !paymentQuoteDecimalPattern.MatchString(raw) {
			return infraerrors.BadRequest("INVALID_EXPECTED_QUOTE", "invalid quote decimal")
		}
		d, err := decimal.NewFromString(raw)
		if err != nil || d.IsNegative() || (i < 3 && !d.IsPositive()) {
			return infraerrors.BadRequest("INVALID_EXPECTED_QUOTE", "invalid quote amount")
		}
	}
	if len(q.PaymentCurrency) != 3 || q.PaymentCurrency != strings.ToUpper(q.PaymentCurrency) || q.CreditCurrency != "USD" {
		return infraerrors.BadRequest("INVALID_EXPECTED_QUOTE", "invalid quote currency")
	}
	for _, c := range q.PaymentCurrency {
		if c < 'A' || c > 'Z' {
			return infraerrors.BadRequest("INVALID_EXPECTED_QUOTE", "invalid quote currency")
		}
	}
	return nil
}

func paymentQuoteMatchesOrder(q *PaymentQuote, o *dbent.PaymentOrder) bool {
	if q == nil || o == nil {
		return false
	}
	fields := PaymentOrderDecimalDetails(o)
	if fields.RequestedAmountDecimal == "" {
		return false
	}
	actual := []string{fields.RequestedAmountDecimal, fields.PayAmountDecimal, fields.CreditAmountDecimal, fields.FeeAmountDecimal}
	for i, raw := range []string{q.RequestedAmount, q.PayAmount, q.CreditAmount, q.FeeAmount} {
		d, err := decimal.NewFromString(raw)
		value, _ := decimal.NewFromString(actual[i])
		if err != nil || !d.Equal(value) {
			return false
		}
	}
	return q.PaymentCurrency == PaymentOrderCurrency(o) && q.CreditCurrency == "USD"
}

func (s *PaymentService) findClientOrder(ctx context.Context, req CreateOrderRequest) (*dbent.PaymentOrder, error) {
	o, err := s.entClient.PaymentOrder.Query().Where(paymentorder.UserIDEQ(req.UserID), paymentorder.ClientOrderIDEQ(req.ClientOrderID)).Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup durable payment intent: %w", err)
	}
	if o.RequestHash != req.requestHash {
		return nil, ErrIdempotencyKeyConflict
	}
	return o, nil
}

func (s *PaymentService) resumeClientOrder(ctx context.Context, o *dbent.PaymentOrder, req CreateOrderRequest) (*CreateOrderResponse, error) {
	if o.Status == OrderStatusPending && o.ExpiresAt.After(time.Now()) {
		if o.CreationState == "ready" || o.CreationState == "creating" {
			return s.createPersistedClientOrder(ctx, o, req)
		}
		if o.CreationState == "submitted" {
			token := uuid.NewString()
			n, err := s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(o.ID), paymentorder.StatusEQ(OrderStatusPending), paymentorder.CreationStateEQ("submitted"), paymentorder.Or(paymentorder.CreationLeaseUntilIsNil(), paymentorder.CreationLeaseUntilLTE(time.Now()))).SetCreationLeaseToken(token).SetCreationLeaseUntil(time.Now().Add(paymentCreationLease)).Save(ctx)
			if err != nil {
				return nil, err
			}
			if n == 1 {
				// Unknown or missing query results never authorize another CreatePayment.
				s.reconcilePaid(ctx, o)
				_, err = s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(o.ID), paymentorder.StatusEQ(OrderStatusPending), paymentorder.CreationLeaseTokenEQ(token)).ClearCreationLeaseUntil().Save(ctx)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return s.clientOrderResponse(ctx, o, req)
}

func (s *PaymentService) createPersistedClientOrder(ctx context.Context, o *dbent.PaymentOrder, req CreateOrderRequest) (*CreateOrderResponse, error) {
	token := uuid.NewString()
	n, err := s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(o.ID), paymentorder.StatusEQ(OrderStatusPending), paymentorder.ExpiresAtGT(time.Now()), paymentorder.Or(paymentorder.CreationStateEQ("ready"), paymentorder.And(paymentorder.CreationStateEQ("creating"), paymentorder.CreationLeaseUntilLTE(time.Now())))).SetCreationState("creating").SetCreationLeaseToken(token).SetCreationLeaseUntil(time.Now().Add(paymentCreationLease)).Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return s.clientOrderResponse(ctx, o, req)
	}
	req.creationLeaseToken = token
	err = s.invokePersistedClientOrder(ctx, o, req)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var attempt *paymentAttemptError
		if errors.As(err, &attempt) && attempt.submitted {
			// The durable submitted marker is the recovery authority even if this write fails.
			_, _ = s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(o.ID), paymentorder.StatusEQ(OrderStatusPending), paymentorder.CreationLeaseTokenEQ(token)).ClearCreationLeaseUntil().Save(cleanupCtx)
			return s.clientOrderResponse(ctx, o, req)
		}
		_, releaseErr := s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(o.ID), paymentorder.StatusEQ(OrderStatusPending), paymentorder.CreationLeaseTokenEQ(token), paymentorder.CreationStateEQ("creating")).SetCreationState("ready").ClearCreationLeaseUntil().Save(cleanupCtx)
		if releaseErr != nil {
			return nil, releaseErr
		}
		return nil, err
	}
	return s.clientOrderResponse(ctx, o, req)
}

func (s *PaymentService) invokePersistedClientOrder(ctx context.Context, o *dbent.PaymentOrder, req CreateOrderRequest) error {
	inst, err := s.getOrderProviderInstance(ctx, o)
	if err != nil {
		return err
	}
	if inst == nil || !inst.Enabled {
		return infraerrors.ServiceUnavailable("PAYMENT_PROVIDER_UNAVAILABLE", "original payment provider is unavailable")
	}
	config, err := s.loadBalancer.GetInstanceConfig(ctx, int64(inst.ID))
	if err != nil {
		return err
	}
	if inst.PaymentMode != "" {
		config["paymentMode"] = inst.PaymentMode
	}
	sel := &payment.InstanceSelection{InstanceID: fmt.Sprint(inst.ID), ProviderKey: inst.ProviderKey, PaymentMode: inst.PaymentMode, Config: config}
	if !clientOrderProviderMatches(o, sel, req) {
		return infraerrors.Conflict("PAYMENT_PROVIDER_CHANGED", "original payment provider configuration changed")
	}
	cfg, err := s.configService.GetPaymentConfig(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return infraerrors.Forbidden("PAYMENT_DISABLED", "payment system is disabled")
	}
	if cfg.BalanceDisabled {
		return infraerrors.Forbidden("BALANCE_PAYMENT_DISABLED", "balance recharge has been disabled")
	}
	_, err = s.invokeProvider(ctx, o, req, cfg, req.Amount, payment.FormatAmountForCurrency(o.PayAmount, PaymentOrderCurrency(o)), o.PayAmount, nil, sel)
	return err
}

func (s *PaymentService) clientOrderResponse(ctx context.Context, o *dbent.PaymentOrder, req CreateOrderRequest) (*CreateOrderResponse, error) {
	var err error
	o, err = s.entClient.PaymentOrder.Get(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	confirmation := o.Status == OrderStatusPending && !paymentQuoteMatchesOrder(req.ExpectedQuote, o)
	if o.Status == OrderStatusPending && o.ConfirmationRequired != confirmation {
		// Fulfillment uses updated_at as its lease version. Metadata may only
		// touch a still-pending order, including when a callback races this read.
		_, err = s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(o.ID), paymentorder.StatusEQ(OrderStatusPending)).SetConfirmationRequired(confirmation).Save(ctx)
		if err != nil {
			return nil, err
		}
		o, err = s.entClient.PaymentOrder.Get(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		confirmation = o.Status == OrderStatusPending && o.ConfirmationRequired
	}
	checkout := PaymentOrderCheckout(o)
	resp := &CreateOrderResponse{OrderID: o.ID, Amount: o.Amount, PayAmount: o.PayAmount, FeeRate: o.FeeRate, Status: o.Status, PaymentType: o.PaymentType, OutTradeNo: o.OutTradeNo, ExpiresAt: o.ExpiresAt, Currency: PaymentOrderCurrency(o), ConfirmationRequired: confirmation, PaymentUnknown: o.Status == OrderStatusPending && o.CreationState != "complete", Checkout: checkout, ResultType: payment.CreatePaymentResultOrderCreated}
	resp.PaymentOrderDecimalFields = PaymentOrderDecimalDetails(o)
	if checkout != nil {
		resp.PayURL = checkout.PayURL
		resp.QRCode = checkout.QRCode
	}
	return resp, nil
}

func clientOrderProviderMatches(o *dbent.PaymentOrder, sel *payment.InstanceSelection, req CreateOrderRequest) bool {
	current := buildPaymentOrderProviderSnapshot(sel, req)
	for _, key := range []string{"provider_instance_id", "provider_key", "merchant_app_id", "merchant_id", "currency", "payment_mode", "creation_provider_origin"} {
		if psSnapshotStringValue(o.ProviderSnapshot[key]) != psSnapshotStringValue(current[key]) {
			return false
		}
	}
	return true
}

func paymentCheckoutOrigins(sel *payment.InstanceSelection) []string {
	if sel == nil {
		return nil
	}
	var origins []string
	switch sel.ProviderKey {
	case payment.TypeEasyPay:
		origins = []string{sel.Config["apiBase"]}
	case payment.TypeAlipay:
		origins = []string{"https://openapi.alipay.com", "https://openapi-sandbox.dl.alipaydev.com", "https://qr.alipay.com"}
	case payment.TypeWxpay:
		origins = []string{"https://wx.tenpay.com"}
	}
	var safe []string
	for _, raw := range origins {
		u, err := url.Parse(raw)
		if err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil {
			safe = append(safe, "https://"+strings.ToLower(u.Host))
		}
	}
	return safe
}

// Only authenticated owner DTOs may include this data; public lookup DTOs omit it.
func PaymentOrderCheckout(o *dbent.PaymentOrder) *PaymentCheckout {
	if o == nil || o.Status != OrderStatusPending || !o.ExpiresAt.After(time.Now()) {
		return nil
	}
	checkout := &PaymentCheckout{QRCode: psStringValue(o.QrCode), ExpiresAt: o.ExpiresAt}
	if raw := psStringValue(o.PayURL); raw != "" {
		u, err := url.Parse(raw)
		if err == nil && u.Scheme == "https" && u.User == nil && u.Host != "" {
			origin := "https://" + strings.ToLower(u.Host)
			var origins []string
			switch values := o.ProviderSnapshot["checkout_origins"].(type) {
			case []string:
				origins = values
			case []any:
				for _, v := range values {
					if str, ok := v.(string); ok {
						origins = append(origins, str)
					}
				}
			}
			for _, allowed := range origins {
				if origin == allowed {
					checkout.PayURL = raw
					break
				}
			}
		}
	}
	if checkout.QRCode == "" && checkout.PayURL == "" {
		return nil
	}
	return checkout
}
