package dto

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDesktopUsageWithoutLedgerReceiptIsUnknown(t *testing.T) {
	row := UsageLogFromService(&service.UsageLog{ActualCost: 10})
	require.Equal(t, "unknown", row.SettlementStatus)
	require.Nil(t, row.ActualCostDecimal)
	require.Equal(t, 10.0, row.ActualCost, "legacy numeric field keeps its original meaning")
}
