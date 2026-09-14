package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// SMSMessage represents an SMS to be sent
type SMSMessage struct {
	Phone        string
	SignName     string
	TemplateCode string
	Params       map[string]string
}

// SMSSendResult represents the result of sending an SMS
type SMSSendResult struct {
	BizID     string
	RequestID string
	Code      string
}

// SMSSender interface for sending SMS messages
type SMSSender interface {
	Send(ctx context.Context, message SMSMessage) (SMSSendResult, error)
}

// PhoneCodeInput represents the input for requesting a phone verification code
type PhoneCodeInput struct {
	Phone           string
	Purpose         string // login | bind_phone
	UserID          int64  // 0 for login, actual user ID for binding
	SessionFamilyID string
	ClientIP        string
}

// PhoneCodeChallenge represents a verification code challenge
type PhoneCodeChallenge struct {
	ID         string
	ExpiresIn  int
	RetryAfter int
	Delivery   string
}

// PhoneCodeProof represents proof of successful phone verification
type PhoneCodeProof struct {
	Phone           string
	Purpose         string
	UserID          int64
	SessionFamilyID string
}

// SMSConfig holds SMS service configuration
type SMSConfig struct {
	Enabled         bool
	Provider        string
	SignName        string
	TemplateCode    string
	TemplateParams  map[string]string
	CodeLength      int
	TTLSeconds      int
	CooldownSeconds int
	MaxAttempts     int
	PhoneHourLimit  int
	PhoneDayLimit   int
	IPHourLimit     int
	GlobalDayLimit  int
}

// SMSCache defines the interface for SMS challenge persistence
type SMSCache interface {
	// Rate limiting
	CheckAndReserveCooldown(ctx context.Context, phone string, cooldownSec int) error
	CheckAndReservePhoneLimit(ctx context.Context, phone string, hourLimit, dayLimit int) error
	CheckAndReserveIPLimit(ctx context.Context, ip string, hourLimit int) error
	CheckAndReserveGlobalLimit(ctx context.Context, dayLimit int) error

	// Challenge lifecycle
	CreateChallenge(ctx context.Context, challenge *StoredChallenge) error
	GetChallenge(ctx context.Context, challengeID string) (*StoredChallenge, error)
	ConsumeChallenge(ctx context.Context, challengeID string, code string) (*StoredChallenge, error)
	IncrementAttempts(ctx context.Context, challengeID string) error
}

// StoredChallenge represents a challenge stored in cache (exported for repository)
type StoredChallenge struct {
	ID              string
	Phone           string
	Purpose         string
	UserID          int64
	SessionFamilyID string
	CodeHMAC        string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	Attempts        int
	MaxAttempts     int
	Consumed        bool
	ConsumedAt      *time.Time
}

// SMSService handles phone verification code generation and validation
type SMSService struct {
	sender SMSSender
	cache  SMSCache
	config SMSConfig
	clock  func() time.Time
	rand   io.Reader
}

// NewSMSService creates a new SMS service
func NewSMSService(sender SMSSender, cache SMSCache, config SMSConfig, now time.Time, randReader io.Reader) *SMSService {
	clock := func() time.Time { return now }
	if randReader == nil {
		randReader = rand.Reader
	}
	return &SMSService{
		sender: sender,
		cache:  cache,
		config: config,
		clock:  clock,
		rand:   randReader,
	}
}

// RequestCode requests a verification code to be sent
func (s *SMSService) RequestCode(ctx context.Context, input PhoneCodeInput) (*PhoneCodeChallenge, error) {
	if !s.config.Enabled {
		return nil, fmt.Errorf("SMS service is not enabled")
	}

	// Check rate limits
	if err := s.cache.CheckAndReserveCooldown(ctx, input.Phone, s.config.CooldownSeconds); err != nil {
		return nil, fmt.Errorf("cooldown: %w", err)
	}

	if err := s.cache.CheckAndReservePhoneLimit(ctx, input.Phone, s.config.PhoneHourLimit, s.config.PhoneDayLimit); err != nil {
		return nil, fmt.Errorf("phone limit: %w", err)
	}

	if err := s.cache.CheckAndReserveIPLimit(ctx, input.ClientIP, s.config.IPHourLimit); err != nil {
		return nil, fmt.Errorf("IP limit: %w", err)
	}

	if err := s.cache.CheckAndReserveGlobalLimit(ctx, s.config.GlobalDayLimit); err != nil {
		return nil, fmt.Errorf("global limit: %w", err)
	}

	// Generate verification code
	code, err := s.generateCode()
	if err != nil {
		return nil, fmt.Errorf("generate code: %w", err)
	}

	// Create challenge
	now := s.clock()
	challengeID := s.generateChallengeID()
	codeHMAC := s.computeCodeHMAC(challengeID, input.Phone, input.Purpose, code)

	challenge := &StoredChallenge{
		ID:              challengeID,
		Phone:           input.Phone,
		Purpose:         input.Purpose,
		UserID:          input.UserID,
		SessionFamilyID: input.SessionFamilyID,
		CodeHMAC:        codeHMAC,
		CreatedAt:       now,
		ExpiresAt:       now.Add(time.Duration(s.config.TTLSeconds) * time.Second),
		Attempts:        0,
		MaxAttempts:     s.config.MaxAttempts,
		Consumed:        false,
	}

	if err := s.cache.CreateChallenge(ctx, challenge); err != nil {
		return nil, fmt.Errorf("store challenge: %w", err)
	}

	// Send SMS
	message := SMSMessage{
		Phone:        input.Phone,
		SignName:     s.config.SignName,
		TemplateCode: s.config.TemplateCode,
		Params: map[string]string{
			"code": code,
			"ttl":  fmt.Sprintf("%d", s.config.TTLSeconds/60),
		},
	}

	result, err := s.sender.Send(ctx, message)
	if err != nil {
		return nil, fmt.Errorf("send SMS: %w", err)
	}

	_ = result // TODO: log result for audit

	return &PhoneCodeChallenge{
		ID:         challengeID,
		ExpiresIn:  s.config.TTLSeconds,
		RetryAfter: s.config.CooldownSeconds,
		Delivery:   "submitted",
	}, nil
}

// ConsumeCode validates and consumes a verification code
func (s *SMSService) ConsumeCode(ctx context.Context, input PhoneCodeInput, challengeID, code string) (*PhoneCodeProof, error) {
	// Get challenge
	challenge, err := s.cache.GetChallenge(ctx, challengeID)
	if err != nil {
		return nil, fmt.Errorf("challenge not found: %w", err)
	}

	// Check if consumed
	if challenge.Consumed {
		return nil, fmt.Errorf("code already used")
	}

	// Check expiration
	now := s.clock()
	if now.After(challenge.ExpiresAt) {
		return nil, fmt.Errorf("code expired")
	}

	// Check attempts
	if challenge.Attempts >= challenge.MaxAttempts {
		return nil, fmt.Errorf("too many attempts")
	}

	// Check phone and purpose match
	if challenge.Phone != input.Phone || challenge.Purpose != input.Purpose {
		return nil, fmt.Errorf("invalid challenge")
	}

	// For bind_phone purpose, verify user ID matches
	if input.Purpose == "bind_phone" && challenge.UserID != input.UserID {
		return nil, fmt.Errorf("user mismatch")
	}

	// Verify code with constant-time comparison
	expectedHMAC := s.computeCodeHMAC(challengeID, input.Phone, input.Purpose, code)
	if !hmac.Equal([]byte(expectedHMAC), []byte(challenge.CodeHMAC)) {
		// Increment attempts on failure
		_ = s.cache.IncrementAttempts(ctx, challengeID)
		return nil, fmt.Errorf("invalid code")
	}

	// Consume challenge atomically
	consumed, err := s.cache.ConsumeChallenge(ctx, challengeID, code)
	if err != nil {
		return nil, fmt.Errorf("consume challenge: %w", err)
	}

	if !consumed.Consumed {
		return nil, fmt.Errorf("challenge consumption failed")
	}

	// Return proof
	return &PhoneCodeProof{
		Phone:           challenge.Phone,
		Purpose:         challenge.Purpose,
		UserID:          challenge.UserID,
		SessionFamilyID: challenge.SessionFamilyID,
	}, nil
}

// generateCode generates a random verification code
func (s *SMSService) generateCode() (string, error) {
	// Generate random bytes
	bytes := make([]byte, s.config.CodeLength)
	if _, err := io.ReadFull(s.rand, bytes); err != nil {
		return "", err
	}

	// Convert to digits
	code := ""
	for i := 0; i < s.config.CodeLength; i++ {
		digit := int(bytes[i]) % 10
		code += fmt.Sprintf("%d", digit)
	}

	return code, nil
}

// generateChallengeID generates a random challenge ID
func (s *SMSService) generateChallengeID() string {
	bytes := make([]byte, 16)
	_, _ = io.ReadFull(s.rand, bytes)
	return hex.EncodeToString(bytes)
}

// computeCodeHMAC computes HMAC of challenge data and code
func (s *SMSService) computeCodeHMAC(challengeID, phone, purpose, code string) string {
	// TODO: use server secret from config
	secret := []byte("temporary-secret-key")
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(challengeID))
	h.Write([]byte(phone))
	h.Write([]byte(purpose))
	h.Write([]byte(code))
	return hex.EncodeToString(h.Sum(nil))
}
