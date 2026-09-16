//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSMSRolloutAllowlistSurvivesAdminOverlayWithoutPublicExposure(t *testing.T) {
	ctx := context.Background()
	cfg := smsSettingsFixture()
	cfg.SMS.Enabled = true
	cfg.SMS.RolloutPhoneAllowlist = []string{"+8613900000000"}
	repo := &settingRepoStub{values: map[string]string{"sms.code_length": "8"}}
	settings := NewSettingService(repo, cfg)

	runtime, err := settings.smsRuntimeConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, 8, runtime.CodeLength)
	require.Equal(t, []string{"+8613900000000"}, runtime.RolloutPhoneAllowlist)

	admin, err := settings.GetSMSSettings(ctx)
	require.NoError(t, err)
	adminJSON, err := json.Marshal(admin)
	require.NoError(t, err)
	require.NotContains(t, string(adminJSON), "rollout_phone_allowlist")
	require.NotContains(t, string(adminJSON), "+8613900000000")

	public, err := settings.GetPublicSettings(ctx)
	require.NoError(t, err)
	publicJSON, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(publicJSON), "rollout_phone_allowlist")
	require.NotContains(t, string(publicJSON), "+8613900000000")
}
