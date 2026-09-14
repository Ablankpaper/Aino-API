package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// PhoneSendCodeRequest represents phone verification code request
type PhoneSendCodeRequest struct {
	Phone                 string `json:"phone" binding:"required"`
	TurnstileToken        string `json:"turnstile_token"`
	TencentCaptchaTicket  string `json:"tencent_captcha_ticket"`
	TencentCaptchaRandstr string `json:"tencent_captcha_randstr"`
}

// PhoneSendCodeResponse represents phone code sending response
type PhoneSendCodeResponse struct {
	ChallengeID string `json:"challenge_id"`
	ExpiresIn   int    `json:"expires_in"`
	RetryAfter  int    `json:"retry_after,omitempty"`
	Delivery    string `json:"delivery"`
}

// PhoneVerifyRequest represents phone verification request
type PhoneVerifyRequest struct {
	Phone             string `json:"phone" binding:"required"`
	ChallengeID       string `json:"challenge_id" binding:"required"`
	Code              string `json:"code" binding:"required"`
	RegisterIfNew     bool   `json:"register_if_new"`
	AgreementRevision string `json:"agreement_revision"`
	InvitationCode    string `json:"invitation_code"`
	PromoCode         string `json:"promo_code"`
}

// PhoneSendCode sends verification code to phone number
// POST /api/v1/auth/phone/send-code
func (h *AuthHandler) PhoneSendCode(c *gin.Context) {
	var req PhoneSendCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if h == nil || h.authService == nil {
		response.InternalError(c, "Auth service not configured")
		return
	}

	// Verify captcha
	proof := captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr)
	if err := h.authService.VerifyCaptcha(c.Request.Context(), proof, ip.GetClientIP(c)); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// Check if SMS service is configured
	if h.authService.SMSService() == nil {
		response.InternalError(c, "SMS service not configured")
		return
	}

	// Request phone verification code
	input := service.PhoneCodeInput{
		Phone:    req.Phone,
		Purpose:  "login",
		ClientIP: ip.GetClientIP(c),
	}

	challenge, err := h.authService.SMSService().RequestCode(c.Request.Context(), input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, PhoneSendCodeResponse{
		ChallengeID: challenge.ID,
		ExpiresIn:   challenge.ExpiresIn,
		RetryAfter:  challenge.RetryAfter,
		Delivery:    challenge.Delivery,
	})
}

// PhoneVerify verifies phone code and performs login or registration
// POST /api/v1/auth/phone/verify
func (h *AuthHandler) PhoneVerify(c *gin.Context) {
	var req PhoneVerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	// Check if SMS service is configured
	if h == nil || h.authService == nil || h.authService.SMSService() == nil {
		response.InternalError(c, "SMS service not configured")
		return
	}

	// Consume phone code and get proof
	input := service.PhoneCodeInput{
		Phone:    req.Phone,
		Purpose:  "login",
		ClientIP: ip.GetClientIP(c),
	}

	proof, err := h.authService.SMSService().ConsumeCode(c.Request.Context(), input, req.ChallengeID, req.Code)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// Login or register with proof
	verifyInput := service.PhoneVerifyInput{
		Phone:             req.Phone,
		ChallengeID:       req.ChallengeID,
		Code:              req.Code,
		RegisterIfNew:     req.RegisterIfNew,
		AgreementRevision: req.AgreementRevision,
		InvitationCode:    req.InvitationCode,
		PromoCode:         req.PromoCode,
	}

	user, isNewUser, err := h.authService.LoginOrRegisterPhone(c.Request.Context(), proof, verifyInput)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	_ = isNewUser // May be used for analytics/logging

	if err := h.ensureBackendModeAllowsUser(c.Request.Context(), user); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// Check if TOTP 2FA is enabled for this user
	if h.totpService != nil && h.settingSvc.IsTotpEnabled(c.Request.Context()) && user.TotpEnabled {
		// Create a temporary login session for 2FA
		tempToken, err := h.totpService.CreateLoginSession(c.Request.Context(), user.ID, user.Email)
		if err != nil {
			response.InternalError(c, "Failed to create 2FA session")
			return
		}

		// Mask phone for display
		maskedPhone := service.MaskPhone(proof.Phone)
		response.Success(c, TotpLoginResponse{
			Requires2FA:     true,
			TempToken:       tempToken,
			UserPhoneMasked: maskedPhone,
		})
		return
	}

	h.respondWithTokenPair(c, user)
}
