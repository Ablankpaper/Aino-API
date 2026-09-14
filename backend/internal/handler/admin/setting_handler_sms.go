package admin

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type sendTestSMSRequest struct {
	Phone string `json:"phone"`
}

type sendTestSMSResponse struct {
	ExpiresIn  int    `json:"expires_in"`
	RetryAfter int    `json:"retry_after,omitempty"`
	Delivery   string `json:"delivery"`
}

// SendTestSMS sends one deliberately isolated verification message. The
// resulting admin_test challenge has no endpoint that can exchange it for an
// authenticated or binding proof.
func (h *SettingHandler) SendTestSMS(c *gin.Context) {
	var req sendTestSMSRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Phone) == "" {
		response.BadRequest(c, "phone is required")
		return
	}
	if !middleware.EnforceStepUpAlways(c, h.totpService, h.userService) {
		return
	}
	if h.smsService == nil {
		response.ErrorFrom(c, service.ErrSMSNotConfigured)
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "Authentication required")
		return
	}
	challenge, err := h.smsService.RequestCode(c.Request.Context(), service.PhoneCodeInput{
		Phone:           req.Phone,
		Purpose:         "admin_test",
		UserID:          subject.UserID,
		SessionFamilyID: middleware.StepUpSessionKey(c, subject.UserID),
		ClientIP:        ip.GetClientIP(c),
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, sendTestSMSResponse{
		ExpiresIn:  challenge.ExpiresIn,
		RetryAfter: challenge.RetryAfter,
		Delivery:   challenge.Delivery,
	})
}
