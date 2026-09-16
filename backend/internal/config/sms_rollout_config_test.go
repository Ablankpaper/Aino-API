package config

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestLoadSMSRolloutPhoneAllowlistBindsCanonicalPhones(t *testing.T) {
	resetViperWithJWTSecret(t)
	viper.Set("sms.rollout_phone_allowlist", []string{"+8613900000000", "+8613800000000"})

	cfg, err := Load()

	require.NoError(t, err)
	require.Equal(t, []string{"+8613900000000", "+8613800000000"}, cfg.SMS.RolloutPhoneAllowlist)
}

func TestLoadSMSRolloutPhoneAllowlistRejectsInvalidEntriesWhileDisabled(t *testing.T) {
	for _, entry := range []string{"", "13900000000", "+86 139 0000 0000", "+14155550000", "+8612900000000"} {
		t.Run(entry, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			viper.Set("sms.enabled", false)
			viper.Set("sms.rollout_phone_allowlist", []string{entry})

			_, err := Load()

			require.ErrorContains(t, err, "sms.rollout_phone_allowlist")
		})
	}
}
