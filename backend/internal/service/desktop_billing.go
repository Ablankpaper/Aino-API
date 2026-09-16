package service

import (
	"context"
	"strconv"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
)

// Wallet balances stay decimal all the way from NUMERIC storage to the DTO.
type DesktopWalletBalances struct {
	Balance       string
	FrozenBalance string
	Status        string
}

type DesktopWalletRepository interface {
	ReadWalletBalances(context.Context, int64) (*DesktopWalletBalances, error)
}

type PlatformWalletSummary struct {
	Currency            string                       `json:"currency"`
	Balance             string                       `json:"balance"`
	FrozenBalance       string                       `json:"frozen_balance"`
	AvailableBalance    string                       `json:"available_balance"`
	PaymentEnabled      bool                         `json:"payment_enabled"`
	ActiveSubscriptions []PlatformWalletSubscription `json:"active_subscriptions"`
	UpdatedAt           time.Time                    `json:"updated_at"`
}

type PlatformWalletSubscription struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
	Remaining *string   `json:"remaining"`
	Unit      string    `json:"unit"`
}

type DesktopBillingService struct {
	wallets       DesktopWalletRepository
	subscriptions *SubscriptionService
	payments      *PaymentConfigService
}

func NewDesktopBillingService(wallets DesktopWalletRepository, subscriptions *SubscriptionService, payments *PaymentConfigService) *DesktopBillingService {
	return &DesktopBillingService{wallets: wallets, subscriptions: subscriptions, payments: payments}
}

func (s *DesktopBillingService) Summary(ctx context.Context, userID int64) (*PlatformWalletSummary, error) {
	balances, err := s.wallets.ReadWalletBalances(ctx, userID)
	if err != nil {
		return nil, err
	}
	if balances.Status != StatusActive {
		return nil, infraerrors.Forbidden("USER_INACTIVE", "user account is disabled")
	}
	cfg, err := s.payments.GetPaymentConfig(ctx)
	if err != nil {
		return nil, err
	}
	subs, err := s.subscriptions.ListActiveUserSubscriptions(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	result := &PlatformWalletSummary{Currency: "USD", Balance: balances.Balance, FrozenBalance: balances.FrozenBalance,
		// Reservation already subtracts the hold from users.balance.
		AvailableBalance: balances.Balance, PaymentEnabled: cfg.Enabled && !cfg.BalanceDisabled,
		ActiveSubscriptions: make([]PlatformWalletSubscription, 0, len(subs)), UpdatedAt: now}
	for _, sub := range subs {
		if sub.Status != SubscriptionStatusActive || sub.StartsAt.After(now) || !sub.ExpiresAt.After(now) || sub.Group == nil || sub.Group.Status != StatusActive || !sub.Group.IsSubscriptionType() {
			continue
		}
		result.ActiveSubscriptions = append(result.ActiveSubscriptions, PlatformWalletSubscription{
			ID: strconv.FormatInt(sub.ID, 10), Name: sub.Group.Name, ExpiresAt: sub.ExpiresAt,
			Remaining: walletSubscriptionRemaining(sub), Unit: "USD",
		})
	}
	return result, nil
}

// The tightest active window limits spend; unlimited subscriptions have no cash value.
func walletSubscriptionRemaining(sub UserSubscription) *string {
	var remaining *decimal.Decimal
	for _, window := range []struct {
		limit *float64
		used  float64
	}{
		{sub.Group.DailyLimitUSD, sub.DailyUsageUSD},
		{sub.Group.WeeklyLimitUSD, sub.WeeklyUsageUSD},
		{sub.Group.MonthlyLimitUSD, sub.MonthlyUsageUSD},
	} {
		if window.limit == nil || *window.limit <= 0 {
			continue
		}
		value := decimal.Max(decimal.Zero, decimal.NewFromFloat(*window.limit).Sub(decimal.NewFromFloat(window.used)))
		if remaining == nil || value.LessThan(*remaining) {
			remaining = &value
		}
	}
	if remaining == nil {
		return nil
	}
	formatted := remaining.StringFixed(8)
	return &formatted
}
