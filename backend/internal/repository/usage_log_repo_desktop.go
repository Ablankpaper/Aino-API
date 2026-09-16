package repository

import (
	"database/sql"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

func nullableDesktopString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func optionalSettlementStatus(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func appendDesktopUsageFilters(conditions []string, args []any, filters usagestats.UsageLogFilters) ([]string, []any) {
	for _, field := range []struct{ column, value string }{
		{"session_id", filters.SessionID}, {"desktop_turn_id", filters.DesktopTurnID},
		{"desktop_call_id", filters.DesktopCallID}, {"desktop_purpose", filters.DesktopPurpose},
	} {
		if field.value != "" {
			conditions = append(conditions, fmt.Sprintf("%s = $%d", field.column, len(args)+1))
			args = append(args, field.value)
		}
	}
	return conditions, args
}
