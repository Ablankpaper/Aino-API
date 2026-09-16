package service

import (
	"context"
	"math"
	"regexp"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
)

type PaymentQuoteInput struct {
	Amount      string `json:"amount" binding:"required"`
	PaymentType string `json:"payment_type" binding:"required"`
	OrderType   string `json:"order_type" binding:"required"`
}

type PaymentQuote struct {
	RequestedAmount string `json:"requested_amount"`
	PayAmount       string `json:"pay_amount"`
	PaymentCurrency string `json:"payment_currency"`
	CreditAmount    string `json:"credit_amount"`
	CreditCurrency  string `json:"credit_currency"`
	FeeAmount       string `json:"fee_amount"`
}

var paymentDecimalPattern = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,8})?$`)

// The legacy order API uses float64. Reject values it cannot round-trip, rather
// than quote one decimal value and silently charge a different one.
func ParsePaymentAmountDecimal(raw string) (float64, error) {
	if !paymentDecimalPattern.MatchString(raw) {
		return 0, infraerrors.BadRequest("INVALID_AMOUNT", "amount must be a plain decimal with at most 12 integer and 8 fractional digits")
	}
	d, err := decimal.NewFromString(raw)
	if err != nil || !d.IsPositive() {
		return 0, infraerrors.BadRequest("INVALID_AMOUNT", "amount must be positive")
	}
	value := d.InexactFloat64()
	if !decimal.NewFromFloat(value).Equal(d) {
		return 0, infraerrors.BadRequest("INVALID_AMOUNT", "amount exceeds supported precision")
	}
	return value, nil
}

type paymentOrderAmounts struct {
	credit   float64
	limit    float64
	pay      float64
	payText  string
	currency string
}

func resolvePaymentOrderAmounts(req CreateOrderRequest, cfg *PaymentConfig, plan *dbent.SubscriptionPlan, currency string) (*paymentOrderAmounts, error) {
	if math.IsNaN(cfg.RechargeFeeRate) || math.IsInf(cfg.RechargeFeeRate, 0) || cfg.RechargeFeeRate < 0 || cfg.RechargeFeeRate > 100 {
		return nil, infraerrors.ServiceUnavailable("INVALID_RECHARGE_FEE_RATE", "invalid recharge fee configuration")
	}
	a := &paymentOrderAmounts{credit: req.Amount, limit: req.Amount, currency: currency}
	if plan != nil {
		a.credit, a.limit = plan.Price, plan.Price
	}
	if math.IsNaN(a.limit) || math.IsInf(a.limit, 0) || a.limit <= 0 || a.limit >= 1e18 {
		return nil, infraerrors.BadRequest("INVALID_AMOUNT", "amount exceeds payment storage range")
	}
	if plan == nil && req.OrderType == payment.OrderTypeBalance {
		a.credit = calculateCreditedBalance(req.Amount, cfg.BalanceRechargeMultiplier)
	}
	// Wallet amounts use NUMERIC(20,8); payment-order amounts use NUMERIC(20,2).
	if math.IsNaN(a.credit) || math.IsInf(a.credit, 0) || a.credit <= 0 || a.credit >= 1e12 {
		return nil, infraerrors.BadRequest("INVALID_AMOUNT", "credited amount exceeds wallet storage range")
	}
	var err error
	a.payText, a.pay, err = calculateCreateOrderPayAmountForOrderType(a.limit, cfg.RechargeFeeRate, currency, req.OrderType, cfg.SubscriptionUSDToCNYRate)
	if err != nil {
		return nil, err
	}
	if math.IsInf(a.pay, 0) || a.pay >= 1e18 {
		return nil, infraerrors.BadRequest("INVALID_AMOUNT", "payment amount exceeds storage range")
	}
	return a, nil
}

// QuoteBalance reads configuration only: no order, provider call, or capacity reservation.
func (s *PaymentService) QuoteBalance(ctx context.Context, userID int64, input PaymentQuoteInput) (*PaymentQuote, error) {
	if input.OrderType != payment.OrderTypeBalance {
		return nil, infraerrors.BadRequest("INVALID_ORDER_TYPE", "quotes currently support balance recharge only")
	}
	amount, err := ParsePaymentAmountDecimal(input.Amount)
	if err != nil {
		return nil, err
	}
	req := CreateOrderRequest{UserID: userID, Amount: amount, PaymentType: NormalizeVisibleMethod(strings.TrimSpace(input.PaymentType)), OrderType: payment.OrderTypeBalance}
	cfg, err := s.configService.GetPaymentConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, infraerrors.Forbidden("PAYMENT_DISABLED", "payment system is disabled")
	}
	if _, err := s.validateOrderInput(ctx, req, cfg); err != nil {
		return nil, err
	}
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !user.IsActive() {
		return nil, infraerrors.Forbidden("USER_INACTIVE", "user account is disabled")
	}
	currency, err := s.configService.ValidateMethodCurrencyConsistency(ctx, req.PaymentType)
	if err != nil {
		return nil, err
	}
	methods, err := s.configService.GetAvailableMethodLimits(ctx)
	if err != nil {
		return nil, err
	}
	method, ok := methods.Methods[req.PaymentType]
	if !ok {
		return nil, infraerrors.ServiceUnavailable("PAYMENT_GATEWAY_ERROR", "method_not_configured")
	}
	a, err := resolvePaymentOrderAmounts(req, cfg, nil, currency)
	if err != nil {
		return nil, err
	}
	// Legacy orders store pay_amount at two decimal places. Do not offer a
	// quote that would disagree with the persisted order after rounding.
	payDecimal, err := decimal.NewFromString(a.payText)
	if err != nil || !payDecimal.Equal(payDecimal.Round(2)) {
		return nil, infraerrors.ServiceUnavailable("PAYMENT_CURRENCY_UNSUPPORTED", "payment storage does not support this amount precision")
	}
	if (method.SingleMin > 0 && a.pay < method.SingleMin) || (method.SingleMax > 0 && a.pay > method.SingleMax) {
		return nil, infraerrors.BadRequest("INVALID_AMOUNT", "amount outside payment method limits")
	}
	fee := payDecimal.Sub(decimal.NewFromFloat(a.limit))
	return &PaymentQuote{RequestedAmount: decimal.NewFromFloat(amount).String(), PayAmount: a.payText,
		PaymentCurrency: a.currency, CreditAmount: decimal.NewFromFloat(a.credit).StringFixed(8), CreditCurrency: "USD",
		FeeAmount: fee.StringFixed(int32(payment.CurrencyMaxFractionDigits(currency)))}, nil
}
