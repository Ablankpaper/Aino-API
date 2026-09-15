package dto

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestManagedKeySecretOnlyReturnedByProvisioning(t *testing.T) {
	key := &service.APIKey{ID: 42, Key: "sk-fixture-managed-secret", DesktopManaged: true}
	data, err := json.Marshal(APIKeyFromService(key))
	require.NoError(t, err)
	require.NotContains(t, string(data), key.Key)
	require.Contains(t, string(data), `"desktop_managed":true`)
	require.NotEmpty(t, key.Key)
	key.DesktopManaged = false
	require.Equal(t, key.Key, APIKeyFromService(key).Key)
}
