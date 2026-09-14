//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func verifiedPhoneProof() *PhoneCodeProof {
	return &PhoneCodeProof{
		Phone:       "+8613900000000",
		Purpose:     "login",
		ChallengeID: "challenge-1",
		verified:    true,
	}
}

func TestIsPhoneBindingRecentAuthRejectsMissingAndStaleAuthentication(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	require.False(t, IsPhoneBindingRecentAuth(time.Time{}, now), "legacy tokens without auth_time must reauthenticate before binding")
	require.False(t, IsPhoneBindingRecentAuth(now.Add(-StepUpGrantTTL-time.Second), now), "refreshing an old session must not make it recent")
	require.False(t, IsPhoneBindingRecentAuth(now.Add(time.Second), now), "future auth_time is invalid")
	require.True(t, IsPhoneBindingRecentAuth(now.Add(-StepUpGrantTTL), now))
}

func TestLoginOrRegisterPhoneRejectsUnverifiedOrMismatchedProof(t *testing.T) {
	ctx := context.Background()
	svc := &AuthService{}

	tests := []struct {
		name  string
		proof *PhoneCodeProof
		input PhoneVerifyInput
	}{
		{
			name:  "unverified proof",
			proof: &PhoneCodeProof{Phone: "+8613900000000", Purpose: "login", ChallengeID: "challenge-1"},
			input: PhoneVerifyInput{Phone: "13900000000", ChallengeID: "challenge-1"},
		},
		{
			name:  "challenge mismatch",
			proof: verifiedPhoneProof(),
			input: PhoneVerifyInput{Phone: "13900000000", ChallengeID: "other"},
		},
		{
			name:  "phone mismatch",
			proof: verifiedPhoneProof(),
			input: PhoneVerifyInput{Phone: "13800000000", ChallengeID: "challenge-1"},
		},
		{
			name: "purpose mismatch",
			proof: &PhoneCodeProof{
				Phone:       "+8613900000000",
				Purpose:     "bind_phone",
				ChallengeID: "challenge-1",
				verified:    true,
			},
			input: PhoneVerifyInput{Phone: "13900000000", ChallengeID: "challenge-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, created, err := svc.LoginOrRegisterPhone(ctx, tt.proof, tt.input)
			require.ErrorIs(t, err, ErrPhoneAuthProof)
			require.Nil(t, user)
			require.False(t, created)
		})
	}
}

func TestBindPhoneIdentityRejectsUnverifiedProofBeforePersistence(t *testing.T) {
	svc := &AuthService{}
	user, err := svc.BindPhoneIdentity(context.Background(), 42, PhoneCodeProof{
		Phone:       "+8613900000000",
		Purpose:     "bind_phone",
		UserID:      42,
		ChallengeID: "challenge-1",
	})

	require.ErrorIs(t, err, ErrPhoneAuthProof)
	require.Nil(t, user)
}
