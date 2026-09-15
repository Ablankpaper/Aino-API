package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type desktopHandlerSettingRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r desktopHandlerSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func TestDesktopCatalogBootstrapDefaultsDisabledWithoutCreatingCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := service.NewSettingService(desktopHandlerSettingRepo{values: map[string]string{}}, &config.Config{})
	catalog := service.NewDesktopModelService(settings, nil, nil, nil, nil, nil)
	handler := NewDesktopHandler(catalog)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/desktop/bootstrap", nil)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 17})

	handler.GetBootstrap(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var envelope struct {
		Data service.DesktopBootstrap `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, 1, envelope.Data.APIVersion)
	require.False(t, envelope.Data.ModelsEnabled)
	require.Nil(t, envelope.Data.DefaultModelID)
	require.Equal(t, "USD", envelope.Data.WalletCurrency)
	require.NotContains(t, recorder.Body.String(), "api_key")
	require.NotContains(t, recorder.Body.String(), "credential")
}

func TestDesktopCatalogHandlerRequiresAuthenticatedSubject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := service.NewSettingService(desktopHandlerSettingRepo{values: map[string]string{}}, &config.Config{})
	handler := NewDesktopHandler(service.NewDesktopModelService(settings, nil, nil, nil, nil, nil))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/desktop/models", nil)

	handler.ListModels(c)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}
