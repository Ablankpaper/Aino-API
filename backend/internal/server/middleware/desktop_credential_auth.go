package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func enforceDesktopCredential(c *gin.Context, keys *service.APIKeyService, key *service.APIKey) bool {
	if !key.DesktopManaged {
		return true
	}
	if err := keys.ValidateDesktopCredential(c.Request.Context(), key); err != nil {
		status, code := http.StatusUnauthorized, "DESKTOP_CREDENTIAL_REVOKED"
		if errors.Is(err, service.ErrServiceUnavailable) {
			status, code = http.StatusServiceUnavailable, "DESKTOP_AUTH_UNAVAILABLE"
		}
		if errors.Is(err, service.ErrDesktopCredentialExpired) {
			code = "DESKTOP_CREDENTIAL_EXPIRED"
		}
		AbortWithError(c, status, code, "Desktop authorization is unavailable; renew or sign in again")
		return false
	}
	allowed := map[string]bool{
		"POST /v1/chat/completions": true, "POST /v1/responses": true,
		"POST /v1/messages": true, "POST /v1/messages/count_tokens": true,
		"GET /v1/models": true, "GET /v1/models/:model": true, "GET /v1/usage": true, "GET /v1/sub2api/billing": true,
	}
	if !allowed[c.Request.Method+" "+c.FullPath()] || strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
		AbortWithError(c, http.StatusForbidden, "DESKTOP_CREDENTIAL_SCOPE", "This endpoint is outside desktop inference authorization")
		return false
	}
	c.Request = c.Request.WithContext(service.WithDesktopCredentialGroup(c.Request.Context(), *key.GroupID))
	return true
}
