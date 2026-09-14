package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	smsChallengePrefix      = "sms:challenge:"
	smsPhoneCooldownPrefix  = "sms:cooldown:phone:"
	smsPhoneHourPrefix      = "sms:limit:phone:hour:"
	smsPhoneDayPrefix       = "sms:limit:phone:day:"
	smsIPHourPrefix         = "sms:limit:ip:hour:"
	smsGlobalDayKey         = "sms:limit:global:day"
)

// SMSCache implements service.SMSCache using Redis
type SMSCache struct {
	rdb    *redis.Client
	prefix string
}

// NewSMSCache creates a new SMS cache
func NewSMSCache(rdb *redis.Client, prefix string) service.SMSCache {
	return &SMSCache{
		rdb:    rdb,
		prefix: prefix,
	}
}

func (c *SMSCache) key(suffix string) string {
	return c.prefix + suffix
}

// CheckAndReserveCooldown checks and reserves cooldown slot for a phone number
func (c *SMSCache) CheckAndReserveCooldown(ctx context.Context, phone string, cooldownSec int) error {
	key := c.key(smsPhoneCooldownPrefix + phone)

	// Try to set with NX (only if not exists)
	ok, err := c.rdb.SetNX(ctx, key, "1", time.Duration(cooldownSec)*time.Second).Result()
	if err != nil {
		return fmt.Errorf("redis error: %w", err)
	}

	if !ok {
		ttl, _ := c.rdb.TTL(ctx, key).Result()
		return fmt.Errorf("phone cooldown active, retry after %d seconds", int(ttl.Seconds()))
	}

	return nil
}

// CheckAndReservePhoneLimit checks and reserves phone rate limit slots
func (c *SMSCache) CheckAndReservePhoneLimit(ctx context.Context, phone string, hourLimit, dayLimit int) error {
	hourKey := c.key(smsPhoneHourPrefix + phone)
	dayKey := c.key(smsPhoneDayPrefix + phone)

	// Check hour limit
	hourCount, err := c.rdb.Incr(ctx, hourKey).Result()
	if err != nil {
		return fmt.Errorf("redis error: %w", err)
	}

	if hourCount == 1 {
		c.rdb.Expire(ctx, hourKey, 1*time.Hour)
	}

	if hourCount > int64(hourLimit) {
		return fmt.Errorf("phone hourly limit exceeded")
	}

	// Check day limit
	dayCount, err := c.rdb.Incr(ctx, dayKey).Result()
	if err != nil {
		// Rollback hour count
		c.rdb.Decr(ctx, hourKey)
		return fmt.Errorf("redis error: %w", err)
	}

	if dayCount == 1 {
		c.rdb.Expire(ctx, dayKey, 24*time.Hour)
	}

	if dayCount > int64(dayLimit) {
		// Rollback both counts
		c.rdb.Decr(ctx, hourKey)
		c.rdb.Decr(ctx, dayKey)
		return fmt.Errorf("phone daily limit exceeded")
	}

	return nil
}

// CheckAndReserveIPLimit checks and reserves IP rate limit slots
func (c *SMSCache) CheckAndReserveIPLimit(ctx context.Context, ip string, hourLimit int) error {
	key := c.key(smsIPHourPrefix + ip)

	count, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("redis error: %w", err)
	}

	if count == 1 {
		c.rdb.Expire(ctx, key, 1*time.Hour)
	}

	if count > int64(hourLimit) {
		c.rdb.Decr(ctx, key)
		return fmt.Errorf("IP hourly limit exceeded")
	}

	return nil
}

// CheckAndReserveGlobalLimit checks and reserves global rate limit slots
func (c *SMSCache) CheckAndReserveGlobalLimit(ctx context.Context, dayLimit int) error {
	key := c.key(smsGlobalDayKey)

	count, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("redis error: %w", err)
	}

	if count == 1 {
		c.rdb.Expire(ctx, key, 24*time.Hour)
	}

	if count > int64(dayLimit) {
		c.rdb.Decr(ctx, key)
		return fmt.Errorf("global daily limit exceeded")
	}

	return nil
}

// CreateChallenge creates a new verification code challenge
func (c *SMSCache) CreateChallenge(ctx context.Context, challenge *service.StoredChallenge) error {
	key := c.key(smsChallengePrefix + challenge.ID)

	data, err := json.Marshal(challenge)
	if err != nil {
		return fmt.Errorf("marshal challenge: %w", err)
	}

	ttl := time.Until(challenge.ExpiresAt)
	if ttl < 0 {
		return fmt.Errorf("challenge already expired")
	}

	if err := c.rdb.Set(ctx, key, data, ttl).Err(); err != nil {
		return fmt.Errorf("redis error: %w", err)
	}

	return nil
}

// GetChallenge retrieves a challenge by ID
func (c *SMSCache) GetChallenge(ctx context.Context, challengeID string) (*service.StoredChallenge, error) {
	key := c.key(smsChallengePrefix + challengeID)

	data, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, fmt.Errorf("challenge not found")
		}
		return nil, fmt.Errorf("redis error: %w", err)
	}

	var challenge service.StoredChallenge
	if err := json.Unmarshal(data, &challenge); err != nil {
		return nil, fmt.Errorf("unmarshal challenge: %w", err)
	}

	return &challenge, nil
}

// ConsumeChallenge atomically consumes a challenge if the code matches
func (c *SMSCache) ConsumeChallenge(ctx context.Context, challengeID string, code string) (*service.StoredChallenge, error) {
	key := c.key(smsChallengePrefix + challengeID)

	// Use Lua script for atomic consume
	script := `
		local key = KEYS[1]
		local data = redis.call('GET', key)
		if not data then
			return nil
		end

		local challenge = cjson.decode(data)
		if challenge.Consumed then
			return nil
		end

		challenge.Consumed = true
		challenge.ConsumedAt = ARGV[1]

		local ttl = redis.call('TTL', key)
		redis.call('SETEX', key, ttl, cjson.encode(challenge))

		return data
	`

	now := time.Now().Format(time.RFC3339Nano)
	result, err := c.rdb.Eval(ctx, script, []string{key}, now).Result()
	if err != nil {
		return nil, fmt.Errorf("consume failed: %w", err)
	}

	if result == nil {
		return nil, fmt.Errorf("challenge not found or already consumed")
	}

	var challenge service.StoredChallenge
	if err := json.Unmarshal([]byte(result.(string)), &challenge); err != nil {
		return nil, fmt.Errorf("unmarshal challenge: %w", err)
	}

	challenge.Consumed = true
	consumedAt := time.Now()
	challenge.ConsumedAt = &consumedAt

	return &challenge, nil
}

// IncrementAttempts increments the attempt counter for a challenge
func (c *SMSCache) IncrementAttempts(ctx context.Context, challengeID string) error {
	key := c.key(smsChallengePrefix + challengeID)

	// Use Lua script for atomic increment
	script := `
		local key = KEYS[1]
		local data = redis.call('GET', key)
		if not data then
			return nil
		end

		local challenge = cjson.decode(data)
		challenge.Attempts = challenge.Attempts + 1

		local ttl = redis.call('TTL', key)
		redis.call('SETEX', key, ttl, cjson.encode(challenge))

		return challenge.Attempts
	`

	_, err := c.rdb.Eval(ctx, script, []string{key}).Result()
	if err != nil {
		return fmt.Errorf("increment attempts failed: %w", err)
	}

	return nil
}
