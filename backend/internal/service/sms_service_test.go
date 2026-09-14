package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type smsTestSender struct {
	message SMSMessage
	result  SMSSendResult
	err     error
}

func (s *smsTestSender) Send(_ context.Context, message SMSMessage) (SMSSendResult, error) {
	s.message = message
	return s.result, s.err
}

type smsTestCache struct {
	challenge   *StoredChallenge
	reserved    bool
	cooldownErr error
}

func (c *smsTestCache) CheckAndReserveCooldown(context.Context, string, int) error {
	if c.cooldownErr != nil {
		return c.cooldownErr
	}
	if c.reserved {
		return errors.New("phone cooldown active")
	}
	c.reserved = true
	return nil
}
func (c *smsTestCache) CheckAndReservePhoneLimit(context.Context, string, int, int) error { return nil }
func (c *smsTestCache) CheckAndReserveIPLimit(context.Context, string, int) error         { return nil }
func (c *smsTestCache) CheckAndReserveGlobalLimit(context.Context, int) error             { return nil }
func (c *smsTestCache) CreateChallenge(_ context.Context, challenge *StoredChallenge) error {
	c.challenge = challenge
	return nil
}
func (c *smsTestCache) GetChallenge(context.Context, string) (*StoredChallenge, error) {
	if c.challenge == nil {
		return nil, errors.New("challenge not found")
	}
	copy := *c.challenge
	return &copy, nil
}
func (c *smsTestCache) ConsumeChallenge(context.Context, string, string) (*StoredChallenge, error) {
	if c.challenge == nil || c.challenge.Consumed {
		return nil, errors.New("challenge not found or already consumed")
	}
	c.challenge.Consumed = true
	return c.challenge, nil
}
func (c *smsTestCache) IncrementAttempts(context.Context, string) error {
	if c.challenge != nil {
		c.challenge.Attempts++
	}
	return nil
}

func newSMSTestService(sender SMSSender, cache SMSCache) *SMSService {
	return NewSMSService(sender, cache, SMSConfig{
		Enabled:         true,
		Provider:        "test",
		HMACSecret:      "test-secret-that-is-long-enough",
		TemplateParams:  map[string]string{"code": "code", "minutes": "ttl_minutes"},
		CodeLength:      6,
		TTLSeconds:      300,
		CooldownSeconds: 60,
		MaxAttempts:     5,
		PhoneHourLimit:  5,
		PhoneDayLimit:   10,
		IPHourLimit:     30,
		GlobalDayLimit:  1000,
	}, time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC), bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22}))
}

func TestSMSRequestCodeCanonicalizesPhoneAndUsesConfiguredTemplateVariables(t *testing.T) {
	sender := &smsTestSender{result: SMSSendResult{Code: "OK"}}
	cache := &smsTestCache{}
	svc := newSMSTestService(sender, cache)

	challenge, err := svc.RequestCode(context.Background(), PhoneCodeInput{
		Phone:    "+86 139 0000 0000",
		Purpose:  "login",
		ClientIP: "192.0.2.10",
	})

	require.NoError(t, err)
	require.NotNil(t, challenge)
	require.Equal(t, "+8613900000000", sender.message.Phone)
	require.Equal(t, "123456", sender.message.Params["code"])
	require.Equal(t, "5", sender.message.Params["minutes"])
	require.NotEmpty(t, cache.challenge.CodeHMAC)
}

func TestSMSRequestCodeRejectsMissingHMACSecret(t *testing.T) {
	sender := &smsTestSender{result: SMSSendResult{Code: "OK"}}
	svc := newSMSTestService(sender, &smsTestCache{})
	svc.config.HMACSecret = ""

	_, err := svc.RequestCode(context.Background(), PhoneCodeInput{Phone: "13900000000", Purpose: "login"})

	require.ErrorIs(t, err, ErrSMSNotConfigured)
}

func TestSMSRequestCodeRejectsNonOKProviderResult(t *testing.T) {
	sender := &smsTestSender{result: SMSSendResult{Code: "isv.BUSINESS_LIMIT_CONTROL"}}
	svc := newSMSTestService(sender, &smsTestCache{})

	_, err := svc.RequestCode(context.Background(), PhoneCodeInput{Phone: "13900000000", Purpose: "login"})

	require.ErrorIs(t, err, ErrSMSDeliveryFailed)
}

func TestSMSRequestCodePreservesCooldownForRetryAfter(t *testing.T) {
	sender := &smsTestSender{result: SMSSendResult{Code: "OK"}}
	cache := &smsTestCache{cooldownErr: errors.New("phone cooldown active, retry after 37 seconds")}
	svc := newSMSTestService(sender, cache)

	_, err := svc.RequestCode(context.Background(), PhoneCodeInput{Phone: "13900000000", Purpose: "login"})

	require.ErrorIs(t, err, ErrSMSRateLimited)
	require.Equal(t, "37", infraerrors.FromError(err).Metadata["retry_after"])
}

func TestSMSBindCodeCannotBeConsumedFromAnotherSession(t *testing.T) {
	sender := &smsTestSender{result: SMSSendResult{Code: "OK"}}
	cache := &smsTestCache{}
	svc := newSMSTestService(sender, cache)

	challenge, err := svc.RequestCode(context.Background(), PhoneCodeInput{
		Phone:           "13900000000",
		Purpose:         "bind_phone",
		UserID:          42,
		SessionFamilyID: "session-a",
		ClientIP:        "192.0.2.10",
	})
	require.NoError(t, err)

	proof, err := svc.ConsumeCode(context.Background(), PhoneCodeInput{
		Phone:           "13900000000",
		Purpose:         "bind_phone",
		UserID:          42,
		SessionFamilyID: "session-b",
		ClientIP:        "192.0.2.10",
	}, challenge.ID, "123456")

	require.ErrorIs(t, err, ErrPhoneAuthProof)
	require.Nil(t, proof)
	require.False(t, cache.challenge.Consumed, "a mismatched session must not consume the proof")
}

func TestNewSMSServiceUsesLiveClockWhenNowIsZero(t *testing.T) {
	svc := NewSMSService(nil, nil, SMSConfig{Enabled: true, HMACSecret: "secret"}, time.Time{}, io.Reader(strings.NewReader("x")))
	first := svc.clock()
	time.Sleep(2 * time.Millisecond)
	second := svc.clock()
	require.True(t, second.After(first))
}
