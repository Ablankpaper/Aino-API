package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *PaymentHandler) Quote(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	subject, ok := requireAuth(c)
	if !ok {
		return
	}
	var input service.PaymentQuoteInput
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Invalid quote request")
		return
	}
	quote, err := h.paymentService.QuoteBalance(c.Request.Context(), subject.UserID, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, quote)
}
