package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func validSMSSettingsRequest() map[string]any {
	return map[string]any{
		"enabled": true, "provider": "aliyun", "sign_name": "fixture-sign", "template_code": "SMS_FIXTURE",
		"region_id": "cn-hangzhou", "request_timeout_seconds": 5,
		"template_params": map[string]string{"code": "code", "minutes": "ttl_minutes"}, "template_verified": true,
		"code_length": 6, "ttl_seconds": 300, "cooldown_seconds": 60, "max_attempts": 5,
		"phone_hour_limit": 5, "phone_day_limit": 10, "ip_hour_limit": 30, "global_day_limit": 1000,
	}
}

func TestUpdateSettingsWithUnchangedSMSPolicySkipsStepUpAndReturnsCurrentReadiness(t *testing.T) {
	cfg := validDeploymentSMSConfig()
	cfg.Enabled = true
	repo := &settingHandlerRepoStub{values: map[string]string{}}
	h := NewSettingHandler(service.NewSettingService(repo, &config.Config{SMS: cfg}), nil, nil, nil, nil, nil, nil)

	rec := doUpdateSettings(t, h, map[string]any{
		"sms": validSMSSettingsRequest(), "registration_enabled": true,
	}, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Data struct {
			SMS *service.SMSSettings `json:"sms"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(t, body.Data.SMS)
	require.True(t, body.Data.SMS.Ready)
	require.Equal(t, "cn-hangzhou", body.Data.SMS.RegionID)
	require.Equal(t, 5, body.Data.SMS.RequestTimeoutSeconds)
}

func validDeploymentSMSConfig() config.SMSConfig {
	return config.SMSConfig{
		Provider: "aliyun", AccessKeyID: "fixture-key", AccessKeySecret: "fixture-secret",
		HMACSecret: "fixture-hmac-secret-at-least-32-bytes", RegionID: "cn-hangzhou", RequestTimeoutSeconds: 5,
		SignName: "fixture-sign", TemplateCode: "SMS_FIXTURE", TemplateParams: map[string]string{"code": "code", "minutes": "ttl_minutes"}, TemplateVerified: true,
		CodeLength: 6, TTLSeconds: 300, CooldownSeconds: 60, MaxAttempts: 5, PhoneHourLimit: 5, PhoneDayLimit: 10, IPHourLimit: 30, GlobalDayLimit: 1000,
	}
}

func TestUpdateSMSSettingsRequiresStepUp(t *testing.T) {
	repo := &settingHandlerRepoStub{values: map[string]string{}}
	h := NewSettingHandler(service.NewSettingService(repo, &config.Config{SMS: validDeploymentSMSConfig()}), nil, nil, nil, nil, nil, nil)

	rec := doUpdateSettings(t, h, map[string]any{"sms": validSMSSettingsRequest()}, nil)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NotContains(t, repo.values, "sms.enabled")
}

func TestDiffSettingsAuditsSMSPolicyAsOneSafeSection(t *testing.T) {
	before := &service.SystemSettings{SMS: &service.SMSEditableSettings{Enabled: false}}
	after := &service.SystemSettings{SMS: &service.SMSEditableSettings{Enabled: true}}

	changed := diffSettings(before, after, nil, nil, UpdateSettingsRequest{SMS: after.SMS})

	require.Contains(t, changed, "sms")
}

func TestDiffSettingsDoesNotAuditUnchangedSMSPolicyAlongsideOtherEdit(t *testing.T) {
	sms := &service.SMSEditableSettings{Enabled: true, Provider: "aliyun"}
	before := &service.SystemSettings{SMS: sms, RegistrationEnabled: false}
	after := &service.SystemSettings{SMS: sms, RegistrationEnabled: true}

	changed := diffSettings(before, after, nil, nil, UpdateSettingsRequest{SMS: sms})

	require.NotContains(t, changed, "sms")
	require.Contains(t, changed, "registration_enabled")
}

func TestSendTestSMSRequiresExplicitReceiverBeforeStepUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/settings/send-test-sms", bytes.NewBufferString(`{"phone":""}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h := NewSettingHandler(nil, nil, nil, nil, nil, nil, nil)
	h.SetSMSService(&service.SMSService{})

	h.SendTestSMS(c)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSendTestSMSRequiresStepUpBeforeUsingSender(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/settings/send-test-sms", bytes.NewBufferString(`{"phone":"13900000000"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h := NewSettingHandler(nil, nil, nil, nil, nil, nil, nil)
	h.SetSMSService(&service.SMSService{})

	h.SendTestSMS(c)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
