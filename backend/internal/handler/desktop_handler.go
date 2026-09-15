package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type DesktopHandler struct {
	models *service.DesktopModelService
}

func NewDesktopHandler(models *service.DesktopModelService) *DesktopHandler {
	return &DesktopHandler{models: models}
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
