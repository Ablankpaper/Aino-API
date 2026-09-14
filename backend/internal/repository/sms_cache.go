package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	smsChallengePrefix     = "sms:challenge:"
	smsPhoneCooldownPrefix = "sms:cooldown:phone:"
	smsPhoneHourPrefix     = "sms:limit:phone:hour:"
	smsPhoneDayPrefix      = "sms:limit:phone:day:"
	smsIPHourPrefix        = "sms:limit:ip:hour:"
	smsGlobalDayKey        = "sms:limit:global:day"
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
	if c == nil || c.rdb == nil || cooldownSec <= 0 {
		return fmt.Errorf("SMS cooldown is not configured")
	}
	key := c.key(smsPhoneCooldownPrefix + phone)

	// Try to set with NX (only if not exists)
	ok, err := c.rdb.SetNX(ctx, key, "1", time.Duration(cooldownSec)*time.Second).Result()
	if err != nil {
		return fmt.Errorf("redis error: %w", err)
	}

	if !ok {
		ttl, err := c.rdb.TTL(ctx, key).Result()
		if err != nil {
			return fmt.Errorf("redis error reading cooldown: %w", err)
		}
		return fmt.Errorf("phone cooldown active, retry after %d seconds", maxInt64(1, int64(math.Ceil(ttl.Seconds()))))
	}

	return nil
}

// CheckAndReservePhoneLimit checks and reserves phone rate limit slots
func (c *SMSCache) CheckAndReservePhoneLimit(ctx context.Context, phone string, hourLimit, dayLimit int) error {
	if c == nil || c.rdb == nil || hourLimit <= 0 || dayLimit <= 0 {
		return fmt.Errorf("SMS phone limits are not configured")
	}
	hourKey := c.key(smsPhoneHourPrefix + phone)
	dayKey := c.key(smsPhoneDayPrefix + phone)
	return c.reserveCounterPair(ctx, hourKey, dayKey, hourLimit, dayLimit, "phone")
}

// CheckAndReserveIPLimit checks and reserves IP rate limit slots
func (c *SMSCache) CheckAndReserveIPLimit(ctx context.Context, ip string, hourLimit int) error {
	if c == nil || c.rdb == nil || hourLimit <= 0 {
		return fmt.Errorf("SMS IP limit is not configured")
	}
	key := c.key(smsIPHourPrefix + ip)
	return c.reserveCounter(ctx, key, hourLimit, time.Hour, "IP hourly")
}

// CheckAndReserveGlobalLimit checks and reserves global rate limit slots
func (c *SMSCache) CheckAndReserveGlobalLimit(ctx context.Context, dayLimit int) error {
	if c == nil || c.rdb == nil || dayLimit <= 0 {
		return fmt.Errorf("SMS global limit is not configured")
	}
	key := c.key(smsGlobalDayKey)
	return c.reserveCounter(ctx, key, dayLimit, 24*time.Hour, "global daily")
}

// CreateChallenge creates a new verification code challenge
func (c *SMSCache) CreateChallenge(ctx context.Context, challenge *service.StoredChallenge) error {
	if c == nil || c.rdb == nil || challenge == nil || strings.TrimSpace(challenge.ID) == "" {
		return fmt.Errorf("invalid SMS challenge")
	}
	key := c.key(smsChallengePrefix + challenge.ID)

	data, err := json.Marshal(challenge)
	if err != nil {
		return fmt.Errorf("marshal challenge: %w", err)
	}

	ttl := time.Duration(challenge.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = time.Until(challenge.ExpiresAt)
	}
	if ttl <= 0 {
		return fmt.Errorf("challenge already expired")
	}

	if err := c.rdb.Set(ctx, key, data, ttl).Err(); err != nil {
		return fmt.Errorf("redis error: %w", err)
	}

	return nil
}

// GetChallenge retrieves a challenge by ID
func (c *SMSCache) GetChallenge(ctx context.Context, challengeID string) (*service.StoredChallenge, error) {
	if c == nil || c.rdb == nil || strings.TrimSpace(challengeID) == "" {
		return nil, fmt.Errorf("challenge not found")
	}
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
	if c == nil || c.rdb == nil || strings.TrimSpace(challengeID) == "" {
		return nil, fmt.Errorf("challenge not found or already consumed")
	}
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

		local ttl = redis.call('PTTL', key)
		if ttl <= 0 then
			return nil
		end
		redis.call('PSETEX', key, ttl, cjson.encode(challenge))

		return data
	`

	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := c.rdb.Eval(ctx, script, []string{key}, now).Result()
	if err != nil {
		return nil, fmt.Errorf("consume failed: %w", err)
	}

	if result == nil {
		return nil, fmt.Errorf("challenge not found or already consumed")
	}

	resultString, ok := redisResultString(result)
	if !ok {
		return nil, fmt.Errorf("unexpected consume result type")
	}
	var challenge service.StoredChallenge
	if err := json.Unmarshal([]byte(resultString), &challenge); err != nil {
		return nil, fmt.Errorf("unmarshal challenge: %w", err)
	}

	challenge.Consumed = true
	consumedAt := time.Now().UTC()
	challenge.ConsumedAt = &consumedAt

	return &challenge, nil
}

// IncrementAttempts increments the attempt counter for a challenge
func (c *SMSCache) IncrementAttempts(ctx context.Context, challengeID string) error {
	if c == nil || c.rdb == nil || strings.TrimSpace(challengeID) == "" {
		return fmt.Errorf("challenge not found")
	}
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

		local ttl = redis.call('PTTL', key)
		if ttl <= 0 then
			return nil
		end
		redis.call('PSETEX', key, ttl, cjson.encode(challenge))

		return challenge.Attempts
	`

	_, err := c.rdb.Eval(ctx, script, []string{key}).Result()
	if err != nil {
		return fmt.Errorf("increment attempts failed: %w", err)
	}

	return nil
}

// VerifyAndConsumeChallenge performs all challenge checks and the state
// transition in one Redis script.  A wrong code increments Attempts without
// extending the TTL; a correct code atomically marks the challenge consumed.
func (c *SMSCache) VerifyAndConsumeChallenge(
	ctx context.Context,
	challengeID, phone, purpose string,
	userID int64,
	sessionFamilyID string,
	expectedHMAC string,
	now time.Time,
) (service.SMSChallengeConsumeResult, error) {
	if c == nil || c.rdb == nil || strings.TrimSpace(challengeID) == "" {
		return service.SMSChallengeConsumeResult{}, fmt.Errorf("challenge not found")
	}
	const script = `
		local key = KEYS[1]
		local data = redis.call('GET', key)
		if not data then return {'not_found'} end
		local ttl = redis.call('PTTL', key)
		if ttl <= 0 then return {'expired'} end
		local challenge = cjson.decode(data)
		if challenge.Consumed == true or challenge.Consumed == 1 then
			return {'already_consumed'}
		end
		local attempts = tonumber(challenge.Attempts or 0)
		local max_attempts = tonumber(challenge.MaxAttempts or 0)
		if max_attempts > 0 and attempts >= max_attempts then
			return {'exhausted', data, tostring(ttl)}
		end
		if tostring(challenge.Phone or '') ~= ARGV[1] or tostring(challenge.Purpose or '') ~= ARGV[2] or tostring(challenge.UserID or 0) ~= ARGV[3] or tostring(challenge.SessionFamilyID or '') ~= ARGV[4] then
			return {'mismatch'}
		end
		if tostring(challenge.CodeHMAC or '') ~= ARGV[5] then
			attempts = attempts + 1
			challenge.Attempts = attempts
			local status = 'invalid_code'
			if max_attempts > 0 and attempts >= max_attempts then status = 'exhausted' end
			local encoded = cjson.encode(challenge)
			redis.call('PSETEX', key, ttl, encoded)
			return {status, encoded, tostring(ttl)}
		end
		challenge.Consumed = true
		challenge.ConsumedAt = ARGV[6]
		local encoded = cjson.encode(challenge)
		redis.call('PSETEX', key, ttl, encoded)
		return {'consumed', encoded, tostring(ttl)}
	`
	key := c.key(smsChallengePrefix + challengeID)
	result, err := c.rdb.Eval(ctx, script, []string{key}, phone, purpose, strconv.FormatInt(userID, 10), sessionFamilyID, expectedHMAC, now.UTC().Format(time.RFC3339Nano)).Result()
	if err != nil {
		return service.SMSChallengeConsumeResult{}, fmt.Errorf("verify challenge: %w", err)
	}
	items, ok := result.([]interface{})
	if !ok || len(items) == 0 {
		return service.SMSChallengeConsumeResult{}, fmt.Errorf("unexpected verify result")
	}
	status, ok := redisResultString(items[0])
	if !ok {
		return service.SMSChallengeConsumeResult{}, fmt.Errorf("unexpected verify status")
	}
	out := service.SMSChallengeConsumeResult{Status: status}
	if len(items) > 1 {
		if encoded, ok := redisResultString(items[1]); ok && encoded != "" {
			var challenge service.StoredChallenge
			if err := json.Unmarshal([]byte(encoded), &challenge); err != nil {
				return service.SMSChallengeConsumeResult{}, fmt.Errorf("unmarshal verify result: %w", err)
			}
			out.Challenge = &challenge
		}
	}
	if len(items) > 2 {
		if ttl, err := strconv.ParseInt(stringValue(items[2]), 10, 64); err == nil && ttl > 0 {
			out.RetryAfter = int(math.Ceil(float64(ttl) / 1000))
		}
	}
	return out, nil
}

func (c *SMSCache) reserveCounter(ctx context.Context, key string, limit int, expiry time.Duration, label string) error {
	const script = `
		local count = redis.call('INCR', KEYS[1])
		if count == 1 then redis.call('EXPIRE', KEYS[1], ARGV[2]) end
		if count > tonumber(ARGV[1]) then
			redis.call('DECR', KEYS[1])
			return 0
		end
		return count
	`
	result, err := c.rdb.Eval(ctx, script, []string{key}, strconv.Itoa(limit), strconv.FormatInt(int64(expiry/time.Second), 10)).Result()
	if err != nil {
		return fmt.Errorf("redis error: %w", err)
	}
	if n, ok := result.(int64); ok && n == 0 {
		return fmt.Errorf("%s limit exceeded", label)
	}
	return nil
}

func (c *SMSCache) reserveCounterPair(ctx context.Context, hourKey, dayKey string, hourLimit, dayLimit int, label string) error {
	const script = `
		local hour = redis.call('INCR', KEYS[1])
		if hour == 1 then redis.call('EXPIRE', KEYS[1], 3600) end
		if hour > tonumber(ARGV[1]) then
			redis.call('DECR', KEYS[1])
			return -1
		end
		local day = redis.call('INCR', KEYS[2])
		if day == 1 then redis.call('EXPIRE', KEYS[2], 86400) end
		if day > tonumber(ARGV[2]) then
			redis.call('DECR', KEYS[1])
			redis.call('DECR', KEYS[2])
			return -2
		end
		return 1
	`
	result, err := c.rdb.Eval(ctx, script, []string{hourKey, dayKey}, strconv.Itoa(hourLimit), strconv.Itoa(dayLimit)).Result()
	if err != nil {
		return fmt.Errorf("redis error: %w", err)
	}
	n, ok := result.(int64)
	if !ok {
		return fmt.Errorf("unexpected rate limit result")
	}
	switch n {
	case -1:
		return fmt.Errorf("%s hourly limit exceeded", label)
	case -2:
		return fmt.Errorf("%s daily limit exceeded", label)
	default:
		return nil
	}
}

func redisResultString(value interface{}) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	default:
		return "", false
	}
}

func stringValue(value interface{}) string {
	if s, ok := redisResultString(value); ok {
		return s
	}
	return fmt.Sprint(value)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
