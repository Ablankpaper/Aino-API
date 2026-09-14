package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newSMSCacheTestClient(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
		mr.Close()
	})
	return mr, client
}

func TestSMSCacheVerifyAndConsumeChallengeIsAtomicAndBound(t *testing.T) {
	_, client := newSMSCacheTestClient(t)
	cache := &SMSCache{rdb: client, prefix: "test:sms:"}
	now := time.Now().UTC()
	challenge := &service.StoredChallenge{
		ID:          "challenge-atomic",
		Phone:       "+8613900000000",
		Purpose:     "bind_phone",
		UserID:      42,
		CodeHMAC:    "expected-hmac",
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Minute),
		TTLSeconds:  60,
		MaxAttempts: 2,
	}
	require.NoError(t, cache.CreateChallenge(context.Background(), challenge))

	result, err := cache.VerifyAndConsumeChallenge(context.Background(), challenge.ID, challenge.Phone, challenge.Purpose, challenge.UserID, challenge.CodeHMAC, now)
	require.NoError(t, err)
	require.Equal(t, "consumed", result.Status)
	require.NotNil(t, result.Challenge)
	require.True(t, result.Challenge.Consumed)

	result, err = cache.VerifyAndConsumeChallenge(context.Background(), challenge.ID, challenge.Phone, challenge.Purpose, challenge.UserID, challenge.CodeHMAC, now)
	require.NoError(t, err)
	require.Equal(t, "already_consumed", result.Status)
}

func TestSMSCacheVerifyAndConsumeChallengeCountsInvalidAttemptsAtomically(t *testing.T) {
	_, client := newSMSCacheTestClient(t)
	cache := &SMSCache{rdb: client, prefix: "test:sms:"}
	now := time.Now().UTC()
	challenge := &service.StoredChallenge{
		ID:          "challenge-attempts",
		Phone:       "+8613900000000",
		Purpose:     "login",
		CodeHMAC:    "expected-hmac",
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Minute),
		TTLSeconds:  60,
		MaxAttempts: 2,
	}
	require.NoError(t, cache.CreateChallenge(context.Background(), challenge))

	result, err := cache.VerifyAndConsumeChallenge(context.Background(), challenge.ID, challenge.Phone, challenge.Purpose, 0, "wrong-hmac", now)
	require.NoError(t, err)
	require.Equal(t, "invalid_code", result.Status)
	require.Equal(t, 1, result.Challenge.Attempts)

	result, err = cache.VerifyAndConsumeChallenge(context.Background(), challenge.ID, challenge.Phone, challenge.Purpose, 0, "wrong-hmac", now)
	require.NoError(t, err)
	require.Equal(t, "exhausted", result.Status)
	require.Equal(t, 2, result.Challenge.Attempts)

	result, err = cache.VerifyAndConsumeChallenge(context.Background(), challenge.ID, challenge.Phone, challenge.Purpose, 0, "expected-hmac", now)
	require.NoError(t, err)
	require.Equal(t, "exhausted", result.Status)
}

func TestSMSCacheCreateChallengeUsesExplicitTTL(t *testing.T) {
	mr, client := newSMSCacheTestClient(t)
	cache := &SMSCache{rdb: client, prefix: "test:sms:"}
	now := time.Now().UTC().Add(-time.Hour)
	challenge := &service.StoredChallenge{
		ID:          "challenge-ttl",
		Phone:       "+8613900000000",
		Purpose:     "login",
		CodeHMAC:    "expected-hmac",
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Minute),
		TTLSeconds:  30,
		MaxAttempts: 3,
	}
	require.NoError(t, cache.CreateChallenge(context.Background(), challenge))
	require.Greater(t, mr.TTL("test:sms:"+smsChallengePrefix+challenge.ID), time.Duration(0))
}
