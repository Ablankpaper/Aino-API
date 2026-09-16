//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDesktopUsageRecordedFromBothBillingPaths(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, failed := range []bool{false, true} {
			t.Run(protocol+map[bool]string{false: "/settled", true: "/failed"}[failed], func(t *testing.T) {
				usageRepo := &openAIRecordUsageLogRepoStub{}
				billingRepo := &openAIRecordUsageBillingRepoStub{}
				if failed {
					billingRepo.err = errors.New("transaction unavailable")
				}
				correlation := &DesktopUsage{SessionID: uuid.NewString(), TurnID: uuid.NewString(), CallID: uuid.NewString(), Purpose: "compression"}
				key := &APIKey{ID: 1, DesktopManaged: true, DesktopUsage: correlation}
				user, account := &User{ID: 2}, &Account{ID: 3}
				var err error
				if protocol == "openai" {
					svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
					err = svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{APIKey: key, User: user, Account: account, Result: &OpenAIForwardResult{RequestID: uuid.NewString(), Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 10, OutputTokens: 5}}})
				} else {
					svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
					err = svc.RecordUsage(context.Background(), &RecordUsageInput{APIKey: key, User: user, Account: account, Result: &ForwardResult{RequestID: uuid.NewString(), Model: "claude-sonnet-4", Usage: ClaudeUsage{InputTokens: 10, OutputTokens: 5}}})
				}
				log := usageRepo.lastLog
				require.NotNil(t, log)
				require.Equal(t, &correlation.SessionID, log.SessionID)
				require.Equal(t, &correlation.TurnID, log.DesktopTurnID)
				require.Equal(t, &correlation.CallID, log.DesktopCallID)
				require.Equal(t, &correlation.Purpose, log.DesktopPurpose)
				if failed {
					require.Error(t, err)
					require.Equal(t, "unknown", log.SettlementStatus)
					require.Nil(t, log.ActualCostDecimal, "no proven charge must not become a free call")
				} else {
					require.NoError(t, err)
					require.Equal(t, "settled", log.SettlementStatus)
					require.Positive(t, log.ActualCost)
					require.Equal(t, billingRepo.lastCmd.BalanceCost, log.ActualCost, "display must match the quantized ledger command")
				}
			})
		}
	}
}
