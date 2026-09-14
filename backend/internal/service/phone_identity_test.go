package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeCNPhoneEquivalentForms(t *testing.T) {
	a, err := NormalizeCNPhone("13900000000")
	require.NoError(t, err)
	b, err := NormalizeCNPhone("+86 139 0000 0000")
	require.NoError(t, err)
	require.Equal(t, a, b)
	
	c, err := NormalizeCNPhone("0086-139-0000-0000")
	require.NoError(t, err)
	require.Equal(t, a, c)
	
	// Should reject multiple numbers
	_, err = NormalizeCNPhone("13900000000,13800000000")
	require.Error(t, err)
	
	// Should reject invalid format
	_, err = NormalizeCNPhone("12345")
	require.Error(t, err)
	
	// Should reject non-CN prefix
	_, err = NormalizeCNPhone("+1 415 555 0000")
	require.Error(t, err)
}

func TestPhonePlaceholderIsNotAContact(t *testing.T) {
	address, err := NewPhonePlaceholderEmail()
	require.NoError(t, err)
	require.True(t, IsPhonePlaceholderEmail(address))
	require.Contains(t, address, "@phone.aino.invalid")
	require.NotContains(t, address, "+86")
	require.NotContains(t, address, "139")
	
	// Real emails should not match
	require.False(t, IsPhonePlaceholderEmail("user@example.com"))
	require.False(t, IsPhonePlaceholderEmail("test@phone.aino.invalid.fake.com"))
}

func TestMaskPhoneE164(t *testing.T) {
	masked := MaskPhone("+8613900000000")
	require.Contains(t, masked, "139")
	require.Contains(t, masked, "****")
	require.NotContains(t, masked, "0000000")
	
	// Should handle already normalized
	masked2 := MaskPhone("+8613912345678")
	require.Contains(t, masked2, "139")
	require.Contains(t, masked2, "****")
	require.Contains(t, masked2, "5678")
}
