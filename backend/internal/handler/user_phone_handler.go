package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// UserPhoneBindSendCodeRequest represents request to send phone binding code
type UserPhoneBindSendCodeRequest struct {
	Phone string `json:"phone" binding:"required"`
}

// UserPhoneBindSendCodeResponse represents phone binding code response
type UserPhoneBindSendCodeResponse struct {
	ChallengeID string `json:"challenge_id"`
	ExpiresIn   int    `json:"expires_in"`
	RetryAfter  int    `json:"retry_after,omitempty"`
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

	if !h.enforcePhoneBindingStepUp(c) {
		return
	}

	// Check if SMS service is configured
	if h.authService == nil || h.authService.SMSService() == nil {
		response.InternalError(c, "SMS service not configured")
		return
	}

	// Request phone binding code
	input := service.PhoneCodeInput{
		Phone:    req.Phone,
		Purpose:  "bind_phone",
		UserID:   subject.UserID,
		ClientIP: ip.GetClientIP(c),
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

	if !h.enforcePhoneBindingStepUp(c) {
		return
	}

	// Check if SMS service is configured
	if h.authService == nil || h.authService.SMSService() == nil {
		response.InternalError(c, "SMS service not configured")
		return
	}

	// Consume phone code
	input := service.PhoneCodeInput{
		Phone:    req.Phone,
		Purpose:  "bind_phone",
		UserID:   subject.UserID,
		ClientIP: ip.GetClientIP(c),
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

	// Return updated user profile
	response.Success(c, map[string]interface{}{
		"user": user,
	})
}
