//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func smsSettingsFixture() *config.Config {
	return &config.Config{SMS: config.SMSConfig{
		Provider: "aliyun", AccessKeyID: "fixture-access-key", AccessKeySecret: "fixture-access-secret",
		HMACSecret: strings.Repeat("fixture-hmac", 4), RegionID: "cn-hangzhou", SignName: "fixture-sign", TemplateCode: "SMS_FIXTURE",
		TemplateParams: map[string]string{"code": "code", "minutes": "ttl_minutes"}, TemplateVerified: true,
		CodeLength: 6, TTLSeconds: 300, CooldownSeconds: 60, MaxAttempts: 5, PhoneHourLimit: 5, PhoneDayLimit: 10, IPHourLimit: 30, GlobalDayLimit: 1000, RequestTimeoutSeconds: 5,
	}}
}

func TestSMSSettingsUpdateControlsBothRuntimeAndPublicReadiness(t *testing.T) {
	ctx := context.Background()
	cfg := smsSettingsFixture()
	repo := &settingRepoStub{values: map[string]string{}}
	a := NewSettingService(repo, cfg)
	b := NewSettingService(repo, cfg)
	initial, err := a.GetSMSSettings(ctx)
	require.NoError(t, err)
	require.False(t, initial.Ready)
	edit := initial.SMSEditableSettings
	edit.Enabled = true
	edit.CodeLength = 8
	edit.RegionID = "cn-shanghai"
	edit.RequestTimeoutSeconds = 9
	updates, err := a.buildSMSSettingsUpdates(edit)
	require.NoError(t, err)
	for key, value := range updates {
		repo.values[key] = value
	}
	public, err := b.GetPublicSettings(ctx)
	require.NoError(t, err)
	require.True(t, public.PhoneLoginEnabled)
	require.Equal(t, edit.CodeLength, public.PhoneCodeLength)
	runtime, err := b.smsRuntimeConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, public.PhoneCodeLength, runtime.CodeLength)
	require.Equal(t, "cn-shanghai", runtime.RegionID)
	require.Equal(t, 9, runtime.RequestTimeoutSeconds)
	sender := &configurableSMSTestSender{smsTestSender: smsTestSender{result: SMSSendResult{Code: "OK"}}}
	sms := newSMSTestService(sender, &smsTestCache{})
	sms.settings = b
	sms.rand = strings.NewReader(strings.Repeat("fixture-entropy", 4))
	_, err = sms.RequestCode(ctx, PhoneCodeInput{Phone: "13900000000", Purpose: "login", ClientIP: "192.0.2.10"})
	require.NoError(t, err)
	require.Equal(t, "cn-shanghai", sender.options.RegionID)
	require.Equal(t, 9*time.Second, sender.options.RequestTimeout)
	admin, err := b.GetSMSSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, edit.RegionID, admin.RegionID)
	require.Equal(t, edit.RequestTimeoutSeconds, admin.RequestTimeoutSeconds)
	blob, err := json.Marshal(admin)
	require.NoError(t, err)
	for _, secret := range []string{cfg.SMS.AccessKeyID, cfg.SMS.AccessKeySecret, cfg.SMS.HMACSecret} {
		require.NotContains(t, string(blob), secret)
	}
	repo.values["sms.enabled"] = "false"
	_, err = b.smsRuntimeConfig(ctx)
	require.ErrorIs(t, err, ErrSMSDisabled)
	public, err = a.GetPublicSettings(ctx)
	require.NoError(t, err)
	require.False(t, public.PhoneLoginEnabled)
}

func TestSMSSettingsRejectUnsafeEnableAndNeverPersistSecrets(t *testing.T) {
	cfg := smsSettingsFixture()
	s := NewSettingService(&settingRepoStub{values: map[string]string{}}, cfg)
	initial, err := s.GetSMSSettings(context.Background())
	require.NoError(t, err)
	for _, scenario := range []string{"missing_key", "unverified", "mapping", "region", "timeout", "limits"} {
		t.Run(scenario, func(t *testing.T) {
			edit := initial.SMSEditableSettings
			edit.Enabled = true
			originalSecret := cfg.SMS.AccessKeySecret
			defer func() { cfg.SMS.AccessKeySecret = originalSecret }()
			switch scenario {
			case "missing_key":
				cfg.SMS.AccessKeySecret = ""
			case "unverified":
				edit.TemplateVerified = false
			case "mapping":
				edit.TemplateParams = map[string]string{"code": "1234"}
			case "region":
				edit.RegionID = ""
			case "timeout":
				edit.RequestTimeoutSeconds = 31
			case "limits":
				edit.PhoneHourLimit = 0
			}
			_, err := s.buildSMSSettingsUpdates(edit)
			require.Error(t, err)
		})
	}
	repo := &settingRepoStub{values: map[string]string{}, err: errors.New("fixture database unavailable")}
	s = NewSettingService(repo, cfg)
	_, err = s.smsRuntimeConfig(context.Background())
	require.Error(t, err)
}

func TestSMSSettingsConfiguredBooleansRejectWhitespaceSecrets(t *testing.T) {
	cfg := smsSettingsFixture()
	cfg.SMS.AccessKeyID = "   "
	cfg.SMS.HMACSecret = "\t\n"
	s := NewSettingService(&settingRepoStub{values: map[string]string{}}, cfg)

	settings, err := s.GetSMSSettings(context.Background())

	require.NoError(t, err)
	require.False(t, settings.CredentialsConfigured)
	require.False(t, settings.HMACConfigured)
	require.False(t, settings.Ready)
}
