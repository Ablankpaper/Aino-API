//go:build unit

package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAuthPhoneSendCodeMissingServiceReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/auth/phone/send-code", bytes.NewBufferString(`{"phone":"13900000000"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	(&AuthHandler{}).PhoneSendCode(c)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestUserPhoneBindingDoesNotTrustArbitraryUserIDContextValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/user/account-bindings/phone/send-code", bytes.NewBufferString(`{"phone":"13900000000"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set("user_id", int64(99))

	(&UserHandler{}).SendPhoneBindingCode(c)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
