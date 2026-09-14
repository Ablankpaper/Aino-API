package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration239PhoneAuthIdentity(t *testing.T) {
	content, err := FS.ReadFile("239_phone_auth_identity.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ALTER TABLE auth_identities DROP CONSTRAINT IF EXISTS auth_identities_provider_type_check")
	require.Contains(t, sql, "CHECK (provider_type IN ('email', 'github', 'google', 'linuxdo', 'oidc', 'wechat', 'dingtalk', 'phone'))")
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS auth_identities_phone_per_user")
	require.Contains(t, sql, "CHECK (signup_source IN ('email', 'linuxdo', 'wechat', 'oidc', 'github', 'google', 'dingtalk', 'phone'))")
	require.Contains(t, sql, "user_provider_default_grants")
}
