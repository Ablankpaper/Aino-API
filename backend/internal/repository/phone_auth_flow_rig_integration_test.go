//go:build integration

package repository_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type phoneAuthFlowRig struct {
	ctx         context.Context
	client      *dbent.Client
	userRepo    service.UserRepository
	settingRepo service.SettingRepository
	settings    *service.SettingService
	auth        *service.AuthService
	users       *service.UserService
	sender      *phoneAuthFlowSender
	router      *gin.Engine
	redis       *redis.Client
	smsPrefix   string
}

func newPhoneAuthFlowRig(t *testing.T) *phoneAuthFlowRig {
	return newPhoneAuthFlowRigWithStorage(t, repository.GetIntegrationEntClient(), repository.GetIntegrationDB())
}

func newPhoneAuthFlowRigWithStorage(t *testing.T, client *dbent.Client, db *sql.DB) *phoneAuthFlowRig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	userRepo := repository.NewUserRepository(client, db)
	settingRepo := repository.NewSettingRepository(client)
	settingValues := map[string]string{
		service.SettingKeyRegistrationEnabled:     "true",
		service.SettingKeyInvitationCodeEnabled:   "false",
		service.SettingKeyTotpEnabled:             "true",
		service.SettingKeyStepUpEnabled:           "false",
		service.SettingKeyLoginAgreementEnabled:   "false",
		service.SettingKeyLoginAgreementDocuments: `[{"id":"terms","title":"Terms","content":"terms"}]`,
		service.SettingKeyEmailVerifyEnabled:      "false",
		service.SettingKeyPasswordResetEnabled:    "false",
	}
	original := map[string]*string{}
	for key := range settingValues {
		value, err := settingRepo.GetValue(ctx, key)
		if err == nil {
			original[key] = &value
		} else {
			original[key] = nil
		}
	}
	require.NoError(t, settingRepo.SetMultiple(ctx, settingValues))
	t.Cleanup(func() {
		for key, value := range original {
			if value == nil {
				_ = settingRepo.Delete(ctx, key)
			} else {
				_ = settingRepo.Set(ctx, key, *value)
			}
		}
	})
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "integration-phone-binding-secret", ExpireHour: 1, RefreshTokenExpireDays: 1}}
	settings := service.NewSettingService(settingRepo, cfg)
	sender := &phoneAuthFlowSender{}
	redisOptions := *repository.GetIntegrationRedis().Options()
	rdb := redis.NewClient(&redisOptions)
	t.Cleanup(func() { _ = rdb.Close() })
	smsPrefix := fmt.Sprintf("phone-flow:%d:", time.Now().UnixNano())
	sms := service.NewSMSService(sender, repository.NewSMSCache(rdb, smsPrefix), service.SMSConfig{
		Enabled: true, HMACSecret: "integration-secret-at-least-32-bytes", TemplateParams: map[string]string{"code": "code"},
		CodeLength: 6, TTLSeconds: 300, CooldownSeconds: 60, MaxAttempts: 5, PhoneHourLimit: 50, PhoneDayLimit: 50, IPHourLimit: 50, GlobalDayLimit: 1000,
	}, time.Time{}, nil)
	emails := service.NewEmailService(settingRepo, repository.NewEmailCache(rdb))
	auth := service.NewAuthService(client, userRepo, repository.NewRedeemCodeRepository(client), repository.NewRefreshTokenCache(rdb), cfg, settings, emails, nil, nil, nil, nil, nil, nil)
	auth.SetSMSService(sms)
	users := service.NewUserService(userRepo, settingRepo, nil, nil)
	totp := service.NewTotpService(userRepo, nil, repository.NewTotpCache(rdb), settings, nil, nil)
	userHandler := handler.NewUserHandler(users, auth, nil, nil, nil, nil)
	userHandler.SetStepUpDependencies(totp, settings)
	authHandler := handler.NewAuthHandler(cfg, auth, users, settings, nil, nil, totp, nil)
	router := gin.New()
	protected := router.Group("")
	protected.Use(gin.HandlerFunc(middleware.NewJWTAuthMiddleware(auth, users, settings, nil)))
	protected.POST("/send", userHandler.SendPhoneBindingCode)
	protected.POST("/bind", userHandler.BindPhone)
	protected.GET("/profile", userHandler.GetProfile)
	protected.DELETE("/bindings/:provider", userHandler.UnbindIdentity)
	router.POST("/login/send", authHandler.PhoneSendCode)
	router.POST("/login/verify", authHandler.PhoneVerify)
	router.POST("/email/login", authHandler.Login)
	return &phoneAuthFlowRig{ctx: ctx, client: client, userRepo: userRepo, settingRepo: settingRepo, settings: settings, auth: auth, users: users, sender: sender, router: router, redis: rdb, smsPrefix: smsPrefix}
}

func (r *phoneAuthFlowRig) user(t *testing.T, status string, totp bool) *service.User {
	t.Helper()
	u := &service.User{Email: fmt.Sprintf("%d@phone-flow.test", time.Now().UnixNano()), Role: service.RoleUser, Status: status, Balance: 37.5}
	require.NoError(t, u.SetPassword("existing-account-password"))
	u.TotpEnabled = totp
	require.NoError(t, r.userRepo.Create(r.ctx, u))
	if totp {
		_, err := r.client.User.UpdateOneID(u.ID).SetTotpEnabled(true).Save(r.ctx)
		require.NoError(t, err)
		u.TotpEnabled = true
	}
	t.Cleanup(func() { _, _ = r.client.User.Delete().Where(dbuser.IDEQ(u.ID)).Exec(r.ctx) })
	return u
}

func (r *phoneAuthFlowRig) request(method, path, body, token string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.router.ServeHTTP(w, req)
	return w
}
