//go:build unit

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPhoneBindingChallengeResponseIncludesDeliveryContract(t *testing.T) {
	payload, err := json.Marshal(UserPhoneBindSendCodeResponse{
		ChallengeID: "challenge-1",
		ExpiresIn:   300,
		RetryAfter:  60,
		Delivery:    "sms",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"challenge_id":"challenge-1","expires_in":300,"retry_after":60,"delivery":"sms"}`, string(payload))
}

func TestPhoneBindingChallengeRequestAcceptsExistingCaptchaProofContract(t *testing.T) {
	var request UserPhoneBindSendCodeRequest
	err := json.Unmarshal([]byte(`{"phone":"13900000000","turnstile_token":"turnstile","tencent_captcha_ticket":"ticket","tencent_captcha_randstr":"rand"}`), &request)
	require.NoError(t, err)
	require.Equal(t, "turnstile", request.TurnstileToken)
	require.Equal(t, "ticket", request.TencentCaptchaTicket)
	require.Equal(t, "rand", request.TencentCaptchaRandstr)
}

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
