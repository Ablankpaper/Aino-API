package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func validSMSConfigForTest() SMSConfig {
	return SMSConfig{
		Enabled:               true,
		Provider:              "aliyun",
		AccessKeyID:           "test-access-key",
		AccessKeySecret:       "test-access-secret",
		HMACSecret:            strings.Repeat("s", 32),
		SignName:              "测试签名",
		TemplateCode:          "SMS_123456",
		TemplateParams:        map[string]string{"code": "code", "minutes": "ttl_minutes"},
		TemplateVerified:      true,
		RegionID:              "cn-hangzhou",
		RequestTimeoutSeconds: 5,
		CodeLength:            6,
		TTLSeconds:            300,
		CooldownSeconds:       60,
		MaxAttempts:           5,
		PhoneHourLimit:        5,
		PhoneDayLimit:         10,
		IPHourLimit:           30,
		GlobalDayLimit:        1000,
	}
}

func TestSMSConfigValidationRejectsEnabledConfigWithoutSecrets(t *testing.T) {
	cfg := validSMSConfigForTest()
	cfg.HMACSecret = ""
	require.Error(t, validateSMSConfig(cfg, "release"))

	cfg = validSMSConfigForTest()
	cfg.AccessKeySecret = ""
	require.Error(t, validateSMSConfig(cfg, "release"))
}

func TestSMSConfigValidationRejectsFixedCodeLikeTemplateMapping(t *testing.T) {
	cfg := validSMSConfigForTest()
	cfg.TemplateParams = map[string]string{"code": "1234"}
	require.Error(t, validateSMSConfig(cfg, "release"))
}

func TestSMSConfigValidationRequiresCodeAndTTLTemplateMappings(t *testing.T) {
	cfg := validSMSConfigForTest()
	cfg.TemplateParams = map[string]string{"code": "code"}
	require.Error(t, validateSMSConfig(cfg, "release"))
}

func TestSMSConfigValidationAllowsDisabledConfigWithoutCredentials(t *testing.T) {
	require.NoError(t, validateSMSConfig(SMSConfig{}, "release"))
}

func TestSMSConfigValidationAcceptsCompleteReleaseConfig(t *testing.T) {
	require.NoError(t, validateSMSConfig(validSMSConfigForTest(), "release"))
}
