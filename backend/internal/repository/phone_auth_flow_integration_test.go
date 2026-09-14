//go:build integration

package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type phoneAuthFlowSender struct{ code string }

func (s *phoneAuthFlowSender) Send(_ context.Context, message service.SMSMessage) (service.SMSSendResult, error) {
	s.code = message.Params["code"]
	return service.SMSSendResult{Code: "OK"}, nil
}

func TestPhoneBindingFlowKeepsExistingAccountAndBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	client := repository.GetIntegrationEntClient()
	userRepo := repository.NewUserRepository(client, repository.GetIntegrationDB())
	user := &service.User{
		Email:       "phone-flow-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test",
		Role:        service.RoleUser,
		Status:      service.StatusActive,
		Balance:     37.5,
		Concurrency: 3,
	}
	require.NoError(t, user.SetPassword("existing-account-password"))
	require.NoError(t, userRepo.Create(ctx, user))
	t.Cleanup(func() {
		_, _ = client.User.Delete().Where(dbuser.IDEQ(user.ID)).Exec(ctx)
	})

	sender := &phoneAuthFlowSender{}
	sms := service.NewSMSService(sender, repository.NewSMSCache(repository.GetIntegrationRedis(), "phone-flow:"), service.SMSConfig{
		Enabled: true, HMACSecret: "integration-secret-at-least-32-bytes", TemplateParams: map[string]string{"code": "code"},
		CodeLength: 6, TTLSeconds: 300, CooldownSeconds: 60, MaxAttempts: 5, PhoneHourLimit: 5, PhoneDayLimit: 10, IPHourLimit: 30, GlobalDayLimit: 1000,
	}, time.Now().UTC(), bytes.NewReader(bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8}, 8)))
	auth := service.NewAuthService(client, userRepo, nil, nil, &config.Config{JWT: config.JWTConfig{Secret: "integration-phone-binding-secret", ExpireHour: 1}}, nil, nil, nil, nil, nil, nil, nil, nil)
	auth.SetSMSService(sms)
	users := service.NewUserService(userRepo, nil, nil, nil)
	h := handler.NewUserHandler(users, auth, nil, nil, nil, nil)
	authHandler := handler.NewAuthHandler(&config.Config{JWT: config.JWTConfig{Secret: "integration-phone-binding-secret", ExpireHour: 1}}, auth, users, nil, nil, nil, nil, nil)

	router := gin.New()
	protected := router.Group("")
	protected.Use(gin.HandlerFunc(middleware.NewJWTAuthMiddleware(auth, users, nil, nil)))
	protected.POST("/send", h.SendPhoneBindingCode)
	protected.POST("/bind", h.BindPhone)
	router.POST("/login/send", authHandler.PhoneSendCode)
	router.POST("/login/verify", authHandler.PhoneVerify)
	router.POST("/email/login", authHandler.Login)
	bindingJWT, err := auth.GenerateToken(ctx, user)
	require.NoError(t, err)

	send := httptest.NewRecorder()
	sendReq := httptest.NewRequest(http.MethodPost, "/send", bytes.NewBufferString(`{"phone":"13900000000"}`))
	sendReq.Header.Set("Authorization", "Bearer "+bindingJWT)
	router.ServeHTTP(send, sendReq)
	require.Equal(t, http.StatusOK, send.Code, send.Body.String())
	var sendEnvelope struct {
		Data struct {
			ChallengeID string `json:"challenge_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(send.Body.Bytes(), &sendEnvelope))
	require.NotEmpty(t, sendEnvelope.Data.ChallengeID)

	bind := httptest.NewRecorder()
	bindReq := httptest.NewRequest(http.MethodPost, "/bind", bytes.NewBufferString(`{"phone":"13900000000","challenge_id":"`+sendEnvelope.Data.ChallengeID+`","code":"`+sender.code+`"}`))
	bindReq.Header.Set("Authorization", "Bearer "+bindingJWT)
	router.ServeHTTP(bind, bindReq)
	require.Equal(t, http.StatusOK, bind.Code, bind.Body.String())
	require.NotContains(t, bind.Body.String(), "PasswordHash")
	require.NotContains(t, bind.Body.String(), "phone.aino.invalid")
	require.Contains(t, bind.Body.String(), `"phone_bound":true`)

	stored, err := userRepo.GetByID(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, user.ID, stored.ID)
	require.Equal(t, 37.5, stored.Balance, "binding must not create or merge an account balance")
	identity, err := client.AuthIdentity.Query().Where(
		authidentity.ProviderTypeEQ("phone"),
		authidentity.ProviderKeyEQ("default"),
		authidentity.ProviderSubjectEQ("+8613900000000"),
	).Only(ctx)
	require.NoError(t, err)
	require.Equal(t, user.ID, identity.UserID)

	// A successful binding must add a login identity to this same account, not
	// create a second user or move its balance.
	require.NoError(t, repository.GetIntegrationRedis().Del(ctx, "phone-flow:sms:cooldown:phone:+8613900000000").Err())
	loginSend := httptest.NewRecorder()
	router.ServeHTTP(loginSend, httptest.NewRequest(http.MethodPost, "/login/send", bytes.NewBufferString(`{"phone":"13900000000"}`)))
	require.Equal(t, http.StatusOK, loginSend.Code, loginSend.Body.String())
	var loginSendEnvelope struct {
		Data struct {
			ChallengeID string `json:"challenge_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(loginSend.Body.Bytes(), &loginSendEnvelope))
	login := httptest.NewRecorder()
	router.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/login/verify", bytes.NewBufferString(`{"phone":"13900000000","challenge_id":"`+loginSendEnvelope.Data.ChallengeID+`","code":"`+sender.code+`"}`)))
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	require.Contains(t, login.Body.String(), `"access_token"`)
	var loginEnvelope struct {
		Data struct {
			User struct {
				ID int64 `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &loginEnvelope))
	require.Equal(t, user.ID, loginEnvelope.Data.User.ID)

	emailLogin := httptest.NewRecorder()
	router.ServeHTTP(emailLogin, httptest.NewRequest(http.MethodPost, "/email/login", bytes.NewBufferString(`{"email":"`+user.Email+`","password":"existing-account-password"}`)))
	require.Equal(t, http.StatusOK, emailLogin.Code, emailLogin.Body.String())
	stored, err = userRepo.GetByID(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, 37.5, stored.Balance)
}
