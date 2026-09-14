//go:build integration

package repository

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestSMSCacheOneUseAcrossConcurrentVerification verifies that SMS codes
// can only be consumed once across concurrent attempts
func TestSMSCacheOneUseAcrossConcurrentVerification(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()

	// Create SMS cache with integration Redis
	cache := NewSMSCache(integrationRedis, "test:sms:")

	// Create a challenge
	now := time.Now()
	challenge := &service.StoredChallenge{
		ID:              "test-challenge-001",
		Phone:           "+8613900000000",
		Purpose:         "login",
		UserID:          0,
		SessionFamilyID: "",
		CodeHMAC:        "dummy-hmac-for-test",
		CreatedAt:       now,
		ExpiresAt:       now.Add(5 * time.Minute),
		Attempts:        0,
		MaxAttempts:     5,
		Consumed:        false,
	}

	err := cache.CreateChallenge(ctx, challenge)
	require.NoError(t, err)

	// Try to consume the same challenge concurrently
	var successCounter int32
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.ConsumeChallenge(ctx, challenge.ID, "123456")
			if err == nil {
				atomic.AddInt32(&successCounter, 1)
			}
		}()
	}

	wg.Wait()

	// Only one goroutine should succeed
	require.Equal(t, int32(1), atomic.LoadInt32(&successCounter), "exactly one concurrent attempt should succeed")

	// Verify challenge is marked as consumed
	retrieved, err := cache.GetChallenge(ctx, challenge.ID)
	require.NoError(t, err)
	require.True(t, retrieved.Consumed, "challenge should be marked as consumed")
}

// TestSMSCacheRateLimiting verifies phone cooldown and rate limiting
func TestSMSCacheRateLimiting(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	cache := NewSMSCache(integrationRedis, "test:sms:ratelimit:")

	phone := "+8613900000001"

	// First request should succeed
	err := cache.CheckAndReserveCooldown(ctx, phone, 2)
	require.NoError(t, err)

	// Immediate second request should fail (cooldown active)
	err = cache.CheckAndReserveCooldown(ctx, phone, 2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cooldown")

	// Wait for cooldown to expire
	time.Sleep(3 * time.Second)

	// Third request should succeed after cooldown
	err = cache.CheckAndReserveCooldown(ctx, phone, 2)
	require.NoError(t, err)
}

// TestSMSCachePhoneLimit verifies hourly and daily phone limits
func TestSMSCachePhoneLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	cache := NewSMSCache(integrationRedis, "test:sms:phonelimit:")

	phone := "+8613900000002"

	// Reserve up to hourly limit
	for i := 0; i < 3; i++ {
		err := cache.CheckAndReservePhoneLimit(ctx, phone, 3, 10)
		require.NoError(t, err, "request %d should succeed", i+1)
	}

	// Next request should fail (hourly limit exceeded)
	err := cache.CheckAndReservePhoneLimit(ctx, phone, 3, 10)
	require.Error(t, err)
	require.Contains(t, err.Error(), "hourly limit")
}

// TestSMSCacheIPLimit verifies IP-based rate limiting
func TestSMSCacheIPLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	cache := NewSMSCache(integrationRedis, "test:sms:iplimit:")

	ip := "192.0.2.1"

	// Reserve up to IP hourly limit
	for i := 0; i < 5; i++ {
		err := cache.CheckAndReserveIPLimit(ctx, ip, 5)
		require.NoError(t, err, "request %d should succeed", i+1)
	}

	// Next request should fail (IP hourly limit exceeded)
	err := cache.CheckAndReserveIPLimit(ctx, ip, 5)
	require.Error(t, err)
	require.Contains(t, err.Error(), "IP hourly limit")
}

// TestSMSCacheIncrementAttempts verifies failed attempt tracking
func TestSMSCacheIncrementAttempts(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	cache := NewSMSCache(integrationRedis, "test:sms:attempts:")

	now := time.Now()
	challenge := &service.StoredChallenge{
		ID:              "test-challenge-attempts",
		Phone:           "+8613900000003",
		Purpose:         "login",
		UserID:          0,
		SessionFamilyID: "",
		CodeHMAC:        "dummy-hmac",
		CreatedAt:       now,
		ExpiresAt:       now.Add(5 * time.Minute),
		Attempts:        0,
		MaxAttempts:     5,
		Consumed:        false,
	}

	err := cache.CreateChallenge(ctx, challenge)
	require.NoError(t, err)

	// Increment attempts
	for i := 0; i < 3; i++ {
		err := cache.IncrementAttempts(ctx, challenge.ID)
		require.NoError(t, err)
	}

	// Verify attempts count
	retrieved, err := cache.GetChallenge(ctx, challenge.ID)
	require.NoError(t, err)
	require.Equal(t, 3, retrieved.Attempts, "should have 3 failed attempts")
}
