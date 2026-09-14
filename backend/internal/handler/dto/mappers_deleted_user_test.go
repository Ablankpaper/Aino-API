package dto

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUserFromServiceShallow_MapsDeletedAt(t *testing.T) {
	ts := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)

	deleted := UserFromServiceShallow(&service.User{ID: 1, Email: "d@test.com", DeletedAt: &ts})
	require.NotNil(t, deleted.DeletedAt)
	require.Equal(t, ts, *deleted.DeletedAt)

	active := UserFromServiceShallow(&service.User{ID: 2, Email: "a@test.com"})
	require.Nil(t, active.DeletedAt, "active user must have nil DeletedAt")
}

func TestUserFromServiceShallow_HidesPhonePlaceholderEmail(t *testing.T) {
	out := UserFromServiceShallow(&service.User{
		ID:    18,
		Email: "550e8400-e29b-41d4-a716-446655440000@phone.aino.invalid",
	})

	require.NotNil(t, out)
	require.Empty(t, out.Email, "phone placeholder email must not be exposed to users")
}
