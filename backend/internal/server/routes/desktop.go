package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RegisterDesktopRoutes registers the read-only authenticated desktop catalog.
func RegisterDesktopRoutes(
	v1 *gin.RouterGroup,
	h *handler.Handlers,
	jwtAuth middleware.JWTAuthMiddleware,
	settingService *service.SettingService,
	panelRateLimiter *middleware.PanelRateLimiter,
) {
	desktop := v1.Group("/desktop")
	desktop.Use(gin.HandlerFunc(jwtAuth))
	desktop.Use(middleware.BackendModeUserGuard(settingService))
	desktop.Use(panelRateLimiter.Global())
	{
		desktop.GET("/bootstrap", h.Desktop.GetBootstrap)
		desktop.GET("/models", h.Desktop.ListModels)
		desktop.GET("/billing-summary", h.Desktop.BillingSummary)
		desktop.POST("/credentials", h.Desktop.ProvisionCredential)
		desktop.GET("/devices", h.Desktop.ListDevices)
		desktop.DELETE("/devices/:device_id", h.Desktop.RevokeDevice)
	}
}
