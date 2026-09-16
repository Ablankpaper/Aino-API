package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"time"
)

type DesktopHandler struct {
	users       *service.UserService
	totp        *service.TotpService
	models      *service.DesktopModelService
	credentials *service.DesktopCredentialService
	billing     *service.DesktopBillingService
}

func NewDesktopHandler(models *service.DesktopModelService) *DesktopHandler {
	return &DesktopHandler{models: models}
}

func (h *DesktopHandler) SetCredentialService(credentials *service.DesktopCredentialService) {
	h.credentials = credentials
}

func ProvideDesktopHandler(models *service.DesktopModelService, credentials *service.DesktopCredentialService, users *service.UserService, totp *service.TotpService, billing *service.DesktopBillingService) *DesktopHandler {
	h := NewDesktopHandler(models)
	h.SetCredentialService(credentials)
	h.SetCredentialSecurity(users, totp)
	h.SetBillingService(billing)
	return h
}

func (h *DesktopHandler) SetCredentialSecurity(users *service.UserService, totp *service.TotpService) {
	h.users, h.totp = users, totp
}

// GetBootstrap returns desktop capability metadata without provisioning a key
// or touching any upstream model service.
func (h *DesktopHandler) GetBootstrap(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	bootstrap, err := h.models.Bootstrap(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, bootstrap)
}

// ListModels returns only catalog entries authorized for the current user.
func (h *DesktopHandler) ListModels(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	models, err := h.models.ListForUser(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, models)
}

// ProvisionCredential provisions a managed API key for the desktop client.
func (h *DesktopHandler) ProvisionCredential(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	var req struct {
		DeviceID          string `json:"device_id" binding:"required"`
		ConnectionGrantID string `json:"connection_grant_id" binding:"required"`
		ModelID           string `json:"model_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	// Extract session_family_id from JWT claims stored in context
	sessionID := c.GetString(middleware.ContextKeySessionID)
	if sessionID == "" {
		response.Unauthorized(c, "Session ID not found")
		return
	}

	credReq := service.DesktopCredentialRequest{
		UserID:            subject.UserID,
		DeviceID:          req.DeviceID,
		ConnectionGrantID: req.ConnectionGrantID,
		SessionFamilyID:   sessionID,
		ModelID:           req.ModelID,
	}

	cred, err := h.credentials.IssueOrReuseLease(c.Request.Context(), credReq)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, cred)
}

func (h *DesktopHandler) ListDevices(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	devices, err := h.credentials.ListDevices(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, devices)
}

func (h *DesktopHandler) RevokeDevice(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	if h.users == nil {
		response.ErrorFrom(c, service.ErrServiceUnavailable)
		return
	}
	user, err := h.users.GetByID(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if user.TotpEnabled {
		if h.totp == nil {
			response.ErrorFrom(c, service.ErrServiceUnavailable)
			return
		}
		if !middleware.EnforceStepUpAlways(c, h.totp, h.users) {
			return
		}
	} else if !service.IsPhoneBindingRecentAuth(subject.AuthTime, time.Now()) {
		response.ErrorFrom(c, service.ErrRecentAuthenticationRequired)
		return
	}
	if err := h.credentials.RevokeDevice(c.Request.Context(), subject.UserID, c.Param("device_id")); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"revoked": true})
}
