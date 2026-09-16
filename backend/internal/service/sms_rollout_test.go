package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type rolloutSMSTestSender struct {
	calls   int
	message SMSMessage
}

func (s *rolloutSMSTestSender) Send(_ context.Context, message SMSMessage) (SMSSendResult, error) {
	s.calls++
	s.message = message
	return SMSSendResult{Code: "OK"}, nil
}

func TestSMSRolloutAllowlistMatchesNormalizedPhoneBeforeReservation(t *testing.T) {
	sender := &rolloutSMSTestSender{}
	cache := &smsTestCache{}
	svc := newSMSTestService(sender, cache)
	svc.config.RolloutPhoneAllowlist = []string{"+8613900000000"}

	challenge, err := svc.RequestCode(context.Background(), PhoneCodeInput{
		Phone: "+86 139 0000 0000", Purpose: "login", ClientIP: "192.0.2.10",
	})

	require.NoError(t, err)
	require.NotNil(t, challenge)
	require.Equal(t, 1, sender.calls)
	require.Equal(t, "+8613900000000", sender.message.Phone)
	require.True(t, cache.reserved)
}

func TestSMSRolloutAllowlistDeniesBeforeReservationOrSend(t *testing.T) {
	sender := &rolloutSMSTestSender{}
	cache := &smsTestCache{}
	svc := newSMSTestService(sender, cache)
	svc.config.RolloutPhoneAllowlist = []string{"+8613900000000"}

	challenge, err := svc.RequestCode(context.Background(), PhoneCodeInput{
		Phone: "13800000000", Purpose: "login", ClientIP: "192.0.2.10",
	})

	require.ErrorIs(t, err, ErrPhoneRolloutRestricted)
	require.Nil(t, challenge)
	require.Zero(t, sender.calls)
	require.False(t, cache.reserved)
}

func TestSMSRolloutAllowlistOmissionPreservesGlobalEnablement(t *testing.T) {
	sender := &rolloutSMSTestSender{}
	svc := newSMSTestService(sender, &smsTestCache{})

	challenge, err := svc.RequestCode(context.Background(), PhoneCodeInput{
		Phone: "13800000000", Purpose: "login", ClientIP: "192.0.2.10",
	})

	require.NoError(t, err)
	require.NotNil(t, challenge)
	require.Equal(t, 1, sender.calls)
}

func TestSMSRolloutAllowlistNeverOverridesGlobalDisable(t *testing.T) {
	sender := &rolloutSMSTestSender{}
	svc := newSMSTestService(sender, &smsTestCache{})
	svc.config.Enabled = false
	svc.config.RolloutPhoneAllowlist = []string{"+8613900000000"}

	challenge, err := svc.RequestCode(context.Background(), PhoneCodeInput{
		Phone: "13900000000", Purpose: "login", ClientIP: "192.0.2.10",
	})

	require.ErrorIs(t, err, ErrSMSDisabled)
	require.Nil(t, challenge)
	require.Zero(t, sender.calls)
}

func TestSMSRolloutAllowlistInvalidRuntimeEntryFailsClosed(t *testing.T) {
	sender := &rolloutSMSTestSender{}
	cache := &smsTestCache{}
	svc := newSMSTestService(sender, cache)
	svc.config.RolloutPhoneAllowlist = []string{"not-a-phone"}

	challenge, err := svc.RequestCode(context.Background(), PhoneCodeInput{
		Phone: "13900000000", Purpose: "login", ClientIP: "192.0.2.10",
	})

	require.ErrorIs(t, err, ErrSMSNotConfigured)
	require.Nil(t, challenge)
	require.Zero(t, sender.calls)
	require.False(t, cache.reserved)
}

func TestSMSRolloutAllowlistRechecksBeforeConsumingChallenge(t *testing.T) {
	sender := &rolloutSMSTestSender{}
	cache := &smsTestCache{}
	svc := newSMSTestService(sender, cache)
	svc.config.RolloutPhoneAllowlist = []string{"+8613900000000"}
	challenge, err := svc.RequestCode(context.Background(), PhoneCodeInput{
		Phone: "13900000000", Purpose: "login", ClientIP: "192.0.2.10",
	})
	require.NoError(t, err)

	svc.config.RolloutPhoneAllowlist = []string{"+8613800000000"}
	proof, err := svc.ConsumeCode(context.Background(), PhoneCodeInput{
		Phone: "13900000000", Purpose: "login", ClientIP: "192.0.2.10",
	}, challenge.ID, sender.message.Params["code"])

	require.ErrorIs(t, err, ErrPhoneRolloutRestricted)
	require.Nil(t, proof)
	require.False(t, cache.challenge.Consumed)
}
