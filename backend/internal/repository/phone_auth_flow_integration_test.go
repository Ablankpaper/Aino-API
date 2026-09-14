//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type phoneAuthFlowSender struct {
	mu        sync.Mutex
	code      string
	calls     int
	failure   error
	afterSend func()
}

func (s *phoneAuthFlowSender) Send(_ context.Context, message service.SMSMessage) (service.SMSSendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = message.Params["code"]
	s.calls++
	if s.afterSend != nil {
		s.afterSend()
	}
	return service.SMSSendResult{Code: "OK"}, s.failure
}

func (s *phoneAuthFlowSender) latestCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

func TestPhoneBindingFlowKeepsExistingAccountAndBalance(t *testing.T) {
	r := newPhoneAuthFlowRig(t)
	user := r.user(t, service.StatusActive, false)
	bindingJWT, err := r.auth.GenerateToken(r.ctx, user)
	require.NoError(t, err)
	send := r.request(http.MethodPost, "/send", `{"phone":"13900000000"}`, bindingJWT)
	require.Equal(t, http.StatusOK, send.Code, send.Body.String())
	bind := r.request(http.MethodPost, "/bind", fmt.Sprintf(`{"phone":"13900000000","challenge_id":%q,"code":%q}`, challengeID(t, send.Body.Bytes()), r.sender.latestCode()), bindingJWT)
	require.Equal(t, http.StatusOK, bind.Code, bind.Body.String())
	require.NotContains(t, bind.Body.String(), "PasswordHash")
	require.NotContains(t, bind.Body.String(), "phone.aino.invalid")
	require.Contains(t, bind.Body.String(), `"phone_bound":true`)
	require.NoError(t, r.redis.Del(r.ctx, r.smsPrefix+"sms:cooldown:phone:+8613900000000").Err())
	loginSend := r.request(http.MethodPost, "/login/send", `{"phone":"13900000000"}`, "")
	require.Equal(t, http.StatusOK, loginSend.Code, loginSend.Body.String())
	login := r.request(http.MethodPost, "/login/verify", fmt.Sprintf(`{"phone":"13900000000","challenge_id":%q,"code":%q}`, challengeID(t, loginSend.Body.Bytes()), r.sender.latestCode()), "")
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	var envelope struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			User         struct {
				ID int64 `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &envelope))
	require.Equal(t, user.ID, envelope.Data.User.ID)
	require.NotEmpty(t, envelope.Data.AccessToken)
	require.NotEmpty(t, envelope.Data.RefreshToken)
	emailLogin := r.request(http.MethodPost, "/email/login", fmt.Sprintf(`{"email":%q,"password":"existing-account-password"}`, user.Email), "")
	require.Equal(t, http.StatusOK, emailLogin.Code, emailLogin.Body.String())
	stored, err := r.userRepo.GetByID(r.ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, user.Balance, stored.Balance)
}
