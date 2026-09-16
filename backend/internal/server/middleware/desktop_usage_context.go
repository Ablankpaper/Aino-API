package middleware

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func bindDesktopUsage(c *gin.Context, key *service.APIKey) (*service.APIKey, bool) {
	headers := c.Request.Header
	names := []string{"X-Aino-Session-Id", "X-Aino-Turn-Id", "X-Aino-Call-Id", "X-Aino-Purpose"}
	values := make([]string, len(names))
	present, duplicate := false, false
	for i, name := range names {
		entries := headers.Values(name)
		present = present || len(entries) > 0
		duplicate = duplicate || len(entries) > 1
		values[i] = headers.Get(name)
	}
	// Strip the entire private namespace, including unrecognized attribution tags.
	for name := range headers {
		if strings.HasPrefix(strings.ToLower(name), "x-aino-") {
			delete(headers, name)
		}
	}
	if !key.DesktopManaged || !present {
		return key, true
	}
	if duplicate || !service.ValidDesktopUsageID(values[0]) || !service.ValidDesktopUsageID(values[1]) || !service.ValidDesktopUsageID(values[2]) || !service.ValidDesktopUsagePurpose(values[3]) {
		AbortWithError(c, http.StatusBadRequest, "INVALID_DESKTOP_USAGE", "Invalid desktop request correlation")
		return nil, false
	}
	copy := *key
	copy.DesktopUsage = &service.DesktopUsage{SessionID: values[0], TurnID: values[1], CallID: values[2], Purpose: values[3]}
	c.Request = c.Request.WithContext(service.WithDesktopUsageContext(c.Request.Context(), copy.DesktopUsage))
	return &copy, true
}
