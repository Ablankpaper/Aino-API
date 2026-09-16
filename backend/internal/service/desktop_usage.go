package service

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// DesktopUsage belongs to one HTTP dispatch, never to the cached API key.
type DesktopUsage struct {
	SessionID string
	TurnID    string
	CallID    string
	Purpose   string
}

func ValidDesktopUsageID(value string) bool {
	if len(value) != 36 {
		return false
	}
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && strings.EqualFold(id.String(), value)
}

func ValidDesktopUsagePurpose(value string) bool {
	switch value {
	case "chat", "title", "compression", "vision", "delegation", "other_auxiliary":
		return true
	default:
		return false
	}
}

type desktopUsageContextKey struct{}

func WithDesktopUsageContext(ctx context.Context, usage *DesktopUsage) context.Context {
	return context.WithValue(ctx, desktopUsageContextKey{}, usage)
}

func attachDesktopUsage(log *UsageLog, key *APIKey) {
	if key == nil || !key.DesktopManaged || key.DesktopUsage == nil {
		return
	}
	u := key.DesktopUsage
	log.SessionID, log.DesktopTurnID, log.DesktopCallID, log.DesktopPurpose = &u.SessionID, &u.TurnID, &u.CallID, &u.Purpose
	log.SettlementStatus = "unknown"
}

// Only the transactional billing path can attest that a charge was settled.
func settleDesktopUsage(log *UsageLog, cmd *UsageBillingCommand) {
	if log == nil || log.DesktopCallID == nil {
		return
	}
	log.ActualCost = QuantizeUsageBillingAmount(cmd.BalanceCost + cmd.SubscriptionCost)
	log.SettlementStatus = "settled"
	if log.ActualCost == 0 {
		log.SettlementStatus = "not_charged"
	}
}
