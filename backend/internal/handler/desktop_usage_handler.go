package handler

import (
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func parseDesktopUsageFilters(c *gin.Context) (*usagestats.UsageLogFilters, bool) {
	f := &usagestats.UsageLogFilters{SessionID: c.Query("session_id"), DesktopTurnID: c.Query("desktop_turn_id"), DesktopCallID: c.Query("desktop_call_id"), DesktopPurpose: c.Query("desktop_purpose")}
	validSession := utf8.ValidString(f.SessionID) && utf8.RuneCountInString(f.SessionID) <= 255
	for _, r := range f.SessionID {
		if r < 32 || r == 127 {
			validSession = false
		}
	}
	if !validSession || (f.DesktopTurnID != "" && !service.ValidDesktopUsageID(f.DesktopTurnID)) || (f.DesktopCallID != "" && !service.ValidDesktopUsageID(f.DesktopCallID)) || (f.DesktopPurpose != "" && !service.ValidDesktopUsagePurpose(f.DesktopPurpose)) {
		response.BadRequest(c, "Invalid usage correlation filter")
		return nil, false
	}
	return f, true
}
