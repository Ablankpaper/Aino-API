package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrSMSDisabled            = infraerrors.ServiceUnavailable("SMS_DISABLED", "phone verification is not enabled")
	ErrSMSNotConfigured       = infraerrors.ServiceUnavailable("SMS_NOT_CONFIGURED", "phone verification is not configured")
	ErrSMSUnavailable         = infraerrors.ServiceUnavailable("SMS_UNAVAILABLE", "phone verification is temporarily unavailable; please try again")
	ErrSMSDeliveryFailed      = infraerrors.ServiceUnavailable("SMS_DELIVERY_FAILED", "verification message could not be submitted")
	ErrSMSDeliveryUnknown     = infraerrors.ServiceUnavailable("SMS_DELIVERY_UNKNOWN", "verification message status is unknown; please wait before retrying")
	ErrSMSRateLimited         = infraerrors.TooManyRequests("SMS_RATE_LIMITED", "too many verification requests; please try again later")
	ErrPhoneRolloutRestricted = infraerrors.Forbidden("PHONE_ROLLOUT_RESTRICTED", "phone verification is not available")
	ErrPhoneCodeInvalid       = infraerrors.BadRequest("PHONE_CODE_INVALID", "invalid verification code")
	ErrPhoneCodeExpired       = infraerrors.BadRequest("PHONE_CODE_EXPIRED", "verification code has expired")
	ErrPhoneCodeExhausted     = infraerrors.BadRequest("PHONE_CODE_EXHAUSTED", "too many invalid verification attempts")
	ErrPhoneCodeUsed          = infraerrors.BadRequest("PHONE_CODE_USED", "verification code has already been used")
	ErrPhoneAuthProof         = infraerrors.BadRequest("PHONE_AUTH_PROOF_INVALID", "invalid phone verification proof")
)

var (
	templateVariableNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)
	smsRetryAfterPattern        = regexp.MustCompile(`(?i)retry after ([1-9][0-9]*) seconds`)
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

type SMSDeliveryOptions struct {
	Provider       string
	RegionID       string
	RequestTimeout time.Duration
}

type ConfigurableSMSSender interface {
	SendWithOptions(ctx context.Context, message SMSMessage, options SMSDeliveryOptions) (SMSSendResult, error)
}

// PhoneCodeInput represents the input for requesting a phone verification code
type PhoneCodeInput struct {
	Phone           string
	Purpose         string // login | bind_phone | admin_test
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
	ChallengeID     string
	// verified is intentionally private: callers must obtain a proof from
	// ConsumeCode instead of constructing one from request data.
	verified bool
}

// SMSConfig holds SMS service configuration
type SMSConfig struct {
	Enabled               bool
	RolloutPhoneAllowlist []string
	Provider              string
	RegionID              string
	SignName              string
	TemplateCode          string
	TemplateParams        map[string]string
	HMACSecret            string
	RequestTimeoutSeconds int
	CodeLength            int
	TTLSeconds            int
	CooldownSeconds       int
	MaxAttempts           int
	PhoneHourLimit        int
	PhoneDayLimit         int
	IPHourLimit           int
	GlobalDayLimit        int
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

// AtomicSMSChallengeCache is implemented by the production Redis cache.  It
// validates the challenge binding, attempt budget, expiry and HMAC in one Lua
// transaction and either increments attempts or claims the challenge exactly
// once.  SMSService retains the older SMSCache methods only for compatibility
// with small test/third-party adapters; production wiring always provides this
// interface.
type AtomicSMSChallengeCache interface {
	VerifyAndConsumeChallenge(ctx context.Context, challengeID, phone, purpose string, userID int64, sessionFamilyID, expectedHMAC string, now time.Time) (SMSChallengeConsumeResult, error)
}

// SMSChallengeReplacementCache atomically makes a delivered challenge the
// current challenge for its principal and invalidates its predecessor.
type SMSChallengeReplacementCache interface {
	ReplaceChallenge(ctx context.Context, challenge *StoredChallenge) error
}

type SMSChallengeConsumeResult struct {
	Status     string
	Challenge  *StoredChallenge
	RetryAfter int
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
	TTLSeconds      int `json:"ttl_seconds,omitempty"`
}

// SMSService handles phone verification code generation and validation
type SMSService struct {
	settings *SettingService
	sender   SMSSender
	cache    SMSCache
	config   SMSConfig
	clock    func() time.Time
	rand     io.Reader
}

// NewSMSService creates a new SMS service
func NewSMSService(sender SMSSender, cache SMSCache, config SMSConfig, now time.Time, randReader io.Reader) *SMSService {
	clock := func() time.Time { return time.Now().UTC() }
	if !now.IsZero() {
		fixedNow := now.UTC()
		clock = func() time.Time { return fixedNow }
	}
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
	configured, err := s.configuredForRequest(ctx)
	if err != nil {
		return nil, err
	}
	if configured != s {
		return configured.RequestCode(ctx, input)
	}
	if err := s.validateRequest(input); err != nil {
		return nil, err
	}
	if err := s.validateRuntime(); err != nil {
		return nil, err
	}
	normalizedPhone, err := NormalizeCNPhone(input.Phone)
	if err != nil {
		return nil, ErrPhoneInvalid
	}
	input.Phone = normalizedPhone
	if err := s.validateRolloutPhone(input.Phone); err != nil {
		return nil, err
	}

	// Check rate limits
	if err := s.cache.CheckAndReserveCooldown(ctx, input.Phone, s.config.CooldownSeconds); err != nil {
		return nil, normalizeSMSCacheError(err)
	}

	if err := s.cache.CheckAndReservePhoneLimit(ctx, input.Phone, s.config.PhoneHourLimit, s.config.PhoneDayLimit); err != nil {
		return nil, normalizeSMSCacheError(err)
	}

	if err := s.cache.CheckAndReserveIPLimit(ctx, input.ClientIP, s.config.IPHourLimit); err != nil {
		return nil, normalizeSMSCacheError(err)
	}

	if err := s.cache.CheckAndReserveGlobalLimit(ctx, s.config.GlobalDayLimit); err != nil {
		return nil, normalizeSMSCacheError(err)
	}

	// Generate verification code
	code, err := s.generateCode()
	if err != nil {
		return nil, fmt.Errorf("generate code: %w", err)
	}

	// Create challenge
	now := s.clock()
	challengeID, err := s.generateChallengeID()
	if err != nil {
		return nil, fmt.Errorf("generate challenge id: %w", err)
	}
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
		TTLSeconds:      s.config.TTLSeconds,
	}

	// Send SMS
	templateParams, err := s.buildTemplateParams(code)
	if err != nil {
		return nil, err
	}
	message := SMSMessage{
		Phone:        input.Phone,
		SignName:     s.config.SignName,
		TemplateCode: s.config.TemplateCode,
		Params:       templateParams,
	}

	sendCtx := ctx
	var cancel context.CancelFunc
	if s.config.RequestTimeoutSeconds > 0 {
		sendCtx, cancel = context.WithTimeout(ctx, time.Duration(s.config.RequestTimeoutSeconds)*time.Second)
		defer cancel()
	}
	var result SMSSendResult
	if sender, ok := s.sender.(ConfigurableSMSSender); ok {
		result, err = sender.SendWithOptions(sendCtx, message, SMSDeliveryOptions{
			Provider:       s.config.Provider,
			RegionID:       s.config.RegionID,
			RequestTimeout: time.Duration(s.config.RequestTimeoutSeconds) * time.Second,
		})
	} else {
		result, err = s.sender.Send(sendCtx, message)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(sendCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrSMSDeliveryUnknown.WithCause(err)
		}
		return nil, ErrSMSDeliveryFailed.WithCause(err)
	}
	if !strings.EqualFold(strings.TrimSpace(result.Code), "OK") {
		return nil, ErrSMSDeliveryFailed.WithMetadata(map[string]string{"provider_code": strings.TrimSpace(result.Code)})
	}

	// Persist only after the provider accepted the request.  A sender failure
	// therefore cannot leave a usable challenge in Redis.
	if replacementCache, ok := s.cache.(SMSChallengeReplacementCache); ok {
		err = replacementCache.ReplaceChallenge(ctx, challenge)
	} else {
		err = s.cache.CreateChallenge(ctx, challenge)
	}
	if err != nil {
		return nil, ErrSMSDeliveryUnknown.WithCause(fmt.Errorf("store challenge: %w", err))
	}

	return &PhoneCodeChallenge{
		ID:         challengeID,
		ExpiresIn:  s.config.TTLSeconds,
		RetryAfter: s.config.CooldownSeconds,
		Delivery:   "submitted",
	}, nil
}

// ConsumeCode validates and consumes a verification code
func (s *SMSService) ConsumeCode(ctx context.Context, input PhoneCodeInput, challengeID, code string) (*PhoneCodeProof, error) {
	configured, err := s.configuredForRequest(ctx)
	if err != nil {
		return nil, err
	}
	if configured != s {
		return configured.ConsumeCode(ctx, input, challengeID, code)
	}
	if err := s.validateRequest(input); err != nil {
		return nil, err
	}
	if err := s.validateRuntime(); err != nil {
		return nil, err
	}
	normalizedPhone, err := NormalizeCNPhone(input.Phone)
	if err != nil {
		return nil, ErrPhoneInvalid
	}
	input.Phone = normalizedPhone
	if err := s.validateRolloutPhone(input.Phone); err != nil {
		return nil, err
	}
	challengeID = strings.TrimSpace(challengeID)
	code = strings.TrimSpace(code)
	if challengeID == "" || len(code) != s.config.CodeLength || !allASCIIDigits(code) {
		return nil, ErrPhoneCodeInvalid
	}
	expectedHMAC := s.computeCodeHMAC(challengeID, input.Phone, input.Purpose, code)
	if atomicCache, ok := s.cache.(AtomicSMSChallengeCache); ok {
		result, err := atomicCache.VerifyAndConsumeChallenge(ctx, challengeID, input.Phone, input.Purpose, input.UserID, input.SessionFamilyID, expectedHMAC, s.clock())
		if err != nil {
			return nil, ErrSMSNotConfigured.WithCause(err)
		}
		return s.proofFromAtomicResult(result, challengeID)
	}

	// Get challenge
	challenge, err := s.cache.GetChallenge(ctx, challengeID)
	if err != nil {
		return nil, ErrPhoneCodeInvalid
	}

	// Check if consumed
	if challenge.Consumed {
		return nil, ErrPhoneCodeUsed
	}

	// Check expiration
	now := s.clock()
	if !now.Before(challenge.ExpiresAt) {
		return nil, ErrPhoneCodeExpired
	}

	// Check attempts
	if challenge.Attempts >= challenge.MaxAttempts {
		return nil, ErrPhoneCodeExhausted
	}

	// Check phone and purpose match
	if challenge.Phone != input.Phone || challenge.Purpose != input.Purpose {
		return nil, ErrPhoneAuthProof
	}

	// For bind_phone purpose, verify user ID matches
	if input.Purpose == "bind_phone" && (challenge.UserID != input.UserID || challenge.SessionFamilyID != input.SessionFamilyID) {
		return nil, ErrPhoneAuthProof
	}

	// Verify code with constant-time comparison
	if !hmac.Equal([]byte(expectedHMAC), []byte(challenge.CodeHMAC)) {
		// Increment attempts on failure
		if err := s.cache.IncrementAttempts(ctx, challengeID); err != nil {
			return nil, ErrSMSNotConfigured.WithCause(err)
		}
		if challenge.Attempts+1 >= challenge.MaxAttempts {
			return nil, ErrPhoneCodeExhausted
		}
		return nil, ErrPhoneCodeInvalid
	}

	// Consume challenge atomically
	consumed, err := s.cache.ConsumeChallenge(ctx, challengeID, code)
	if err != nil {
		return nil, ErrSMSNotConfigured.WithCause(err)
	}

	if !consumed.Consumed {
		return nil, ErrPhoneCodeUsed
	}

	// Return proof
	return &PhoneCodeProof{
		Phone:           challenge.Phone,
		Purpose:         challenge.Purpose,
		UserID:          challenge.UserID,
		SessionFamilyID: challenge.SessionFamilyID,
		ChallengeID:     challengeID,
		verified:        true,
	}, nil
}

func (s *SMSService) validateRuntime() error {
	if s == nil || !s.config.Enabled {
		return ErrSMSDisabled
	}
	if s.sender == nil || s.cache == nil || strings.TrimSpace(s.config.HMACSecret) == "" {
		return ErrSMSNotConfigured
	}
	if s.config.CodeLength < 4 || s.config.CodeLength > 8 || s.config.TTLSeconds <= 0 || s.config.MaxAttempts <= 0 {
		return ErrSMSNotConfigured
	}
	return nil
}

func (s *SMSService) validateRolloutPhone(normalizedPhone string) error {
	if len(s.config.RolloutPhoneAllowlist) == 0 {
		return nil
	}
	allowed := false
	for _, configuredPhone := range s.config.RolloutPhoneAllowlist {
		canonicalPhone, err := NormalizeCNPhone(configuredPhone)
		if err != nil || canonicalPhone != configuredPhone {
			return ErrSMSNotConfigured
		}
		if canonicalPhone == normalizedPhone {
			allowed = true
		}
	}
	if !allowed {
		return ErrPhoneRolloutRestricted
	}
	return nil
}

func (s *SMSService) validateRequest(input PhoneCodeInput) error {
	if s == nil {
		return ErrSMSNotConfigured
	}
	switch strings.TrimSpace(input.Purpose) {
	case "login":
		if input.UserID != 0 {
			return ErrPhoneAuthProof
		}
	case "bind_phone", "admin_test":
		if input.UserID <= 0 || strings.TrimSpace(input.SessionFamilyID) == "" {
			return ErrPhoneAuthProof
		}
	default:
		return ErrPhoneAuthProof
	}
	return nil
}

func (s *SMSService) buildTemplateParams(code string) (map[string]string, error) {
	mappings := s.config.TemplateParams
	if len(mappings) == 0 {
		mappings = map[string]string{"code": "code"}
	}
	params := make(map[string]string, len(mappings))
	hasCode := false
	for name, semantic := range mappings {
		if !templateVariableNamePattern.MatchString(name) {
			return nil, ErrSMSNotConfigured.WithMetadata(map[string]string{"field": "template_params"})
		}
		switch strings.ToLower(strings.TrimSpace(semantic)) {
		case "code":
			params[name] = code
			hasCode = true
		case "ttl_minutes":
			params[name] = strconv.Itoa(maxInt(1, s.config.TTLSeconds/60))
		default:
			return nil, ErrSMSNotConfigured.WithMetadata(map[string]string{"field": "template_params"})
		}
	}
	if !hasCode {
		return nil, ErrSMSNotConfigured.WithMetadata(map[string]string{"field": "template_params"})
	}
	return params, nil
}

func allASCIIDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func normalizeSMSCacheError(err error) error {
	if err == nil {
		return nil
	}
	if infraerrors.IsTooManyRequests(err) {
		return err
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "limit") || strings.Contains(lower, "cooldown") {
		limited := ErrSMSRateLimited
		if match := smsRetryAfterPattern.FindStringSubmatch(err.Error()); len(match) == 2 {
			limited = limited.WithMetadata(map[string]string{"retry_after": match[1]})
		}
		return limited.WithCause(err)
	}
	return ErrSMSUnavailable.WithCause(err)
}

func (s *SMSService) proofFromAtomicResult(result SMSChallengeConsumeResult, challengeID string) (*PhoneCodeProof, error) {
	switch result.Status {
	case "consumed":
		if result.Challenge == nil {
			return nil, ErrSMSNotConfigured
		}
		return &PhoneCodeProof{
			Phone:           result.Challenge.Phone,
			Purpose:         result.Challenge.Purpose,
			UserID:          result.Challenge.UserID,
			SessionFamilyID: result.Challenge.SessionFamilyID,
			ChallengeID:     challengeID,
			verified:        true,
		}, nil
	case "expired":
		return nil, ErrPhoneCodeExpired
	case "exhausted":
		return nil, ErrPhoneCodeExhausted
	case "already_consumed":
		return nil, ErrPhoneCodeUsed
	case "mismatch":
		return nil, ErrPhoneAuthProof
	case "invalid_code":
		return nil, ErrPhoneCodeInvalid
	default:
		return nil, ErrPhoneCodeInvalid
	}
}

// generateCode generates a random verification code
func (s *SMSService) generateCode() (string, error) {
	var b strings.Builder
	for b.Len() < s.config.CodeLength {
		var one [1]byte
		if _, err := io.ReadFull(s.rand, one[:]); err != nil {
			return "", err
		}
		// Discard the six values above 249 to avoid modulo bias.
		if one[0] >= 250 {
			continue
		}
		_ = b.WriteByte('0' + one[0]%10)
	}
	return b.String(), nil
}

// generateChallengeID generates a random challenge ID
func (s *SMSService) generateChallengeID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := io.ReadFull(s.rand, bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// computeCodeHMAC computes HMAC of challenge data and code
func (s *SMSService) computeCodeHMAC(challengeID, phone, purpose, code string) string {
	h := hmac.New(sha256.New, []byte(s.config.HMACSecret))
	for _, value := range []string{"aino.sms.v1", challengeID, phone, purpose, code} {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
