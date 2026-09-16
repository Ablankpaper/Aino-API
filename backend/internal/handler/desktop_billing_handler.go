package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *DesktopHandler) SetBillingService(billing *service.DesktopBillingService) {
	h.billing = billing
}

func (h *DesktopHandler) BillingSummary(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	subject, ok := requireAuth(c)
	if !ok {
		return
	}
	summary, err := h.billing.Summary(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, summary)
}
