package handler

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// UserPhoneBindSendCodeRequest represents request to send phone binding code
type UserPhoneBindSendCodeRequest struct {
	Phone                 string `json:"phone" binding:"required"`
	TurnstileToken        string `json:"turnstile_token"`
	TencentCaptchaTicket  string `json:"tencent_captcha_ticket"`
	TencentCaptchaRandstr string `json:"tencent_captcha_randstr"`
}

// UserPhoneBindSendCodeResponse represents phone binding code response
type UserPhoneBindSendCodeResponse struct {
	ChallengeID string `json:"challenge_id"`
	ExpiresIn   int    `json:"expires_in"`
	RetryAfter  int    `json:"retry_after,omitempty"`
	Delivery    string `json:"delivery"`
}

// UserPhoneBindRequest represents phone binding verification request
type UserPhoneBindRequest struct {
	Phone       string `json:"phone" binding:"required"`
	ChallengeID string `json:"challenge_id" binding:"required"`
	Code        string `json:"code" binding:"required"`
}

// SendPhoneBindingCode sends verification code for phone binding
// POST /api/v1/user/account-bindings/phone/send-code
func (h *UserHandler) SendPhoneBindingCode(c *gin.Context) {
	var req UserPhoneBindSendCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "Authentication required")
		return
	}

	if !h.enforcePhoneBindingSecurity(c, subject) {
		return
	}

	if h.authService == nil {
		response.InternalError(c, "SMS service not configured")
		return
	}
	proof := captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr)
	if err := h.authService.VerifyCaptcha(c.Request.Context(), proof, ip.GetClientIP(c)); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if h.authService.SMSService() == nil {
		response.InternalError(c, "SMS service not configured")
		return
	}

	// Request phone binding code
	input := service.PhoneCodeInput{
		Phone:           req.Phone,
		Purpose:         "bind_phone",
		UserID:          subject.UserID,
		SessionFamilyID: c.GetString(middleware2.ContextKeySessionID),
		ClientIP:        ip.GetClientIP(c),
	}

	challenge, err := h.authService.SMSService().RequestCode(c.Request.Context(), input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, UserPhoneBindSendCodeResponse{
		ChallengeID: challenge.ID,
		ExpiresIn:   challenge.ExpiresIn,
		RetryAfter:  challenge.RetryAfter,
		Delivery:    challenge.Delivery,
	})
}

// BindPhone verifies code and binds phone to current user
// POST /api/v1/user/account-bindings/phone
func (h *UserHandler) BindPhone(c *gin.Context) {
	var req UserPhoneBindRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "Authentication required")
		return
	}

	if !h.enforcePhoneBindingSecurity(c, subject) {
		return
	}

	// Check if SMS service is configured
	if h.authService == nil || h.authService.SMSService() == nil {
		response.InternalError(c, "SMS service not configured")
		return
	}

	// Consume phone code
	input := service.PhoneCodeInput{
		Phone:           req.Phone,
		Purpose:         "bind_phone",
		UserID:          subject.UserID,
		SessionFamilyID: c.GetString(middleware2.ContextKeySessionID),
		ClientIP:        ip.GetClientIP(c),
	}

	proof, err := h.authService.SMSService().ConsumeCode(c.Request.Context(), input, req.ChallengeID, req.Code)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// Bind phone identity
	user, err := h.authService.BindPhoneIdentity(c.Request.Context(), subject.UserID, *proof)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	profileResp, err := h.buildUserProfileResponse(c.Request.Context(), subject.UserID, user)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// Preserve the established phone-binding envelope while returning the same
	// safe profile DTO used by other identity-binding endpoints.
	response.Success(c, map[string]any{"user": profileResp})
}

func (h *UserHandler) enforcePhoneBindingSecurity(c *gin.Context, subject middleware2.AuthSubject) bool {
	if h == nil || h.userService == nil {
		response.InternalError(c, "User service not configured")
		return false
	}
	user, err := h.userService.GetByID(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return false
	}
	if user.TotpEnabled {
		if h.totpService == nil {
			response.InternalError(c, "Step-up verification service not configured")
			return false
		}
		return middleware2.EnforceStepUpAlways(c, h.totpService, h.userService)
	}
	if !service.IsPhoneBindingRecentAuth(subject.AuthTime, time.Now().UTC()) {
		response.ErrorFrom(c, service.ErrRecentAuthenticationRequired)
		return false
	}
	return true
}
