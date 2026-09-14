//go:build integration

package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func (r *phoneAuthFlowRig) verifiedProof(t *testing.T, phone, purpose string, userID int64) *service.PhoneCodeProof {
	t.Helper()
	input := service.PhoneCodeInput{Phone: phone, Purpose: purpose, UserID: userID, ClientIP: "127.0.0.1"}
	if userID != 0 {
		input.SessionFamilyID = fmt.Sprintf("fixture-session-%d", userID)
	}
	challenge, err := r.auth.SMSService().RequestCode(r.ctx, input)
	require.NoError(t, err)
	proof, err := r.auth.SMSService().ConsumeCode(r.ctx, input, challenge.ID, r.sender.latestCode())
	require.NoError(t, err)
	// Represents the next request after cooldown; only this fixture's exact key
	// is removed. Ownership/consume logic and all database operations stay real.
	require.NoError(t, r.redis.Del(r.ctx, r.smsPrefix+"sms:cooldown:phone:"+phone).Err())
	return proof
}

func (r *phoneAuthFlowRig) cleanPhoneOwner(t *testing.T, phone string) {
	t.Helper()
	t.Cleanup(func() {
		identity, err := r.client.AuthIdentity.Query().Where(authidentity.ProviderTypeEQ("phone"), authidentity.ProviderSubjectEQ(phone)).Only(r.ctx)
		if err == nil {
			_, _ = r.client.User.Delete().Where(dbuser.IDEQ(identity.UserID)).Exec(r.ctx)
		}
	})
}

func TestPhoneFlowInvitationAndDisabledPolicy(t *testing.T) {
	r := newPhoneAuthFlowRig(t)
	before, err := r.client.User.Query().Count(r.ctx)
	require.NoError(t, err)
	require.NoError(t, r.settingRepo.Set(r.ctx, service.SettingKeyInvitationCodeEnabled, "true"))
	for i, invitation := range []string{"", "fixture-not-an-invitation"} {
		phone := fmt.Sprintf("1390000011%d", i)
		send := r.request(http.MethodPost, "/login/send", fmt.Sprintf(`{"phone":%q}`, phone), "")
		require.Equal(t, http.StatusOK, send.Code, send.Body.String())
		verify := r.request(http.MethodPost, "/login/verify", fmt.Sprintf(`{"phone":%q,"challenge_id":%q,"code":%q,"register_if_new":true,"invitation_code":%q}`, phone, challengeID(t, send.Body.Bytes()), r.sender.latestCode(), invitation), "")
		require.Equal(t, http.StatusBadRequest, verify.Code, verify.Body.String())
		reason := "INVITATION_CODE_REQUIRED"
		if invitation != "" {
			reason = "INVITATION_CODE_INVALID"
		}
		require.Contains(t, verify.Body.String(), reason)
	}
	after, err := r.client.User.Query().Count(r.ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	owner := r.user(t, service.StatusDisabled, false)
	_, err = r.client.AuthIdentity.Create().SetUserID(owner.ID).SetProviderType("phone").SetProviderKey("default").SetProviderSubject("+8613900000112").SetMetadata(map[string]any{}).Save(r.ctx)
	require.NoError(t, err)
	send := r.request(http.MethodPost, "/login/send", `{"phone":"13900000112"}`, "")
	require.Equal(t, http.StatusOK, send.Code, send.Body.String())
	verify := r.request(http.MethodPost, "/login/verify", fmt.Sprintf(`{"phone":"13900000112","challenge_id":%q,"code":%q}`, challengeID(t, send.Body.Bytes()), r.sender.latestCode()), "")
	require.Equal(t, http.StatusForbidden, verify.Code, verify.Body.String())
	require.Contains(t, verify.Body.String(), "USER_NOT_ACTIVE")
	require.NotContains(t, verify.Body.String(), "access_token")
}

func TestPhoneFlowConcurrentSignupAndBindingKeepOneOwner(t *testing.T) {
	r := newPhoneAuthFlowRig(t)
	phone := "+8613900000120"
	r.cleanPhoneOwner(t, phone)
	member := r.user(t, service.StatusActive, false)
	loginProof := r.verifiedProof(t, phone, "login", 0)
	bindProof := r.verifiedProof(t, phone, "bind_phone", member.ID)
	before, err := r.client.User.Query().Count(r.ctx)
	require.NoError(t, err)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var loggedIn *service.User
	var loginErr, bindErr error
	var created bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		loggedIn, created, loginErr = r.auth.LoginOrRegisterPhone(r.ctx, loginProof, service.PhoneVerifyInput{Phone: phone, ChallengeID: loginProof.ChallengeID, RegisterIfNew: true})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, bindErr = r.auth.BindPhoneIdentity(r.ctx, member.ID, *bindProof)
	}()
	close(start)
	wg.Wait()
	require.NoError(t, loginErr)
	identity, err := r.client.AuthIdentity.Query().Where(authidentity.ProviderTypeEQ("phone"), authidentity.ProviderSubjectEQ(phone)).Only(r.ctx)
	require.NoError(t, err)
	require.Equal(t, identity.UserID, loggedIn.ID)
	if identity.UserID == member.ID {
		require.NoError(t, bindErr)
		require.False(t, created)
	} else {
		require.ErrorIs(t, bindErr, service.ErrPhoneAlreadyBound)
		require.True(t, created)
	}
	after, err := r.client.User.Query().Count(r.ctx)
	require.NoError(t, err)
	expected := before
	if created {
		expected++
	}
	require.Equal(t, expected, after, "losing signup transaction must not leave an orphan user")
	stored, err := r.userRepo.GetByID(r.ctx, member.ID)
	require.NoError(t, err)
	require.Equal(t, member.Balance, stored.Balance)
	var grants int
	require.NoError(t, repository.GetIntegrationDB().QueryRowContext(r.ctx, `SELECT count(*) FROM user_provider_default_grants WHERE user_id=$1 AND provider_type='phone'`, member.ID).Scan(&grants))
	require.Zero(t, grants)
}

func TestPhoneFlowRejectsSecondPhoneAndDeletedOwnerReuse(t *testing.T) {
	r := newPhoneAuthFlowRig(t)
	owner := r.user(t, service.StatusActive, false)
	phone := "+8613900000130"
	first := r.verifiedProof(t, phone, "bind_phone", owner.ID)
	_, err := r.auth.BindPhoneIdentity(r.ctx, owner.ID, *first)
	require.NoError(t, err)
	second := r.verifiedProof(t, "+8613900000131", "bind_phone", owner.ID)
	_, err = r.auth.BindPhoneIdentity(r.ctx, owner.ID, *second)
	require.ErrorIs(t, err, service.ErrPhoneAlreadyBound)
	count, err := r.client.AuthIdentity.Query().Where(authidentity.UserIDEQ(owner.ID), authidentity.ProviderTypeEQ("phone")).Count(r.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	_, err = r.client.User.UpdateOneID(owner.ID).SetDeletedAt(time.Now()).Save(r.ctx)
	require.NoError(t, err)
	login := r.verifiedProof(t, phone, "login", 0)
	_, created, err := r.auth.LoginOrRegisterPhone(r.ctx, login, service.PhoneVerifyInput{Phone: phone, ChallengeID: login.ChallengeID, RegisterIfNew: true})
	require.Error(t, err)
	require.False(t, created)
	identity, err := r.client.AuthIdentity.Query().Where(authidentity.ProviderTypeEQ("phone"), authidentity.ProviderSubjectEQ(phone)).Only(r.ctx)
	require.NoError(t, err)
	require.Equal(t, owner.ID, identity.UserID)
}

func TestPhoneFlowConcurrentBindingsForOneUser(t *testing.T) {
	r := newPhoneAuthFlowRig(t)
	u := r.user(t, service.StatusActive, false)
	proofs := []*service.PhoneCodeProof{
		r.verifiedProof(t, "+8613900000132", "bind_phone", u.ID),
		r.verifiedProof(t, "+8613900000133", "bind_phone", u.ID),
	}
	start := make(chan struct{})
	results := make(chan error, len(proofs))
	for _, proof := range proofs {
		go func(proof *service.PhoneCodeProof) {
			<-start
			_, err := r.auth.BindPhoneIdentity(r.ctx, u.ID, *proof)
			results <- err
		}(proof)
	}
	close(start)
	succeeded := 0
	for range proofs {
		err := <-results
		if err == nil {
			succeeded++
		} else {
			require.ErrorIs(t, err, service.ErrPhoneAlreadyBound)
		}
	}
	require.Equal(t, 1, succeeded)
	count, err := r.client.AuthIdentity.Query().Where(authidentity.UserIDEQ(u.ID), authidentity.ProviderTypeEQ("phone")).Count(r.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestPhoneFlowSenderAndRedisFailuresFailClosed(t *testing.T) {
	for _, scenario := range []string{"provider_failure", "provider_timeout", "redis_before", "redis_after"} {
		t.Run(scenario, func(t *testing.T) {
			r := newPhoneAuthFlowRig(t)
			expected := "SMS_DELIVERY_FAILED"
			calls := 1
			switch scenario {
			case "provider_failure":
				r.sender.failure = errors.New("fixture rejected")
			case "provider_timeout":
				r.sender.failure = context.DeadlineExceeded
				expected = "SMS_DELIVERY_UNKNOWN"
			case "redis_before":
				require.NoError(t, r.redis.Close())
				expected = "SMS_UNAVAILABLE"
				calls = 0
			case "redis_after":
				r.sender.afterSend = func() { _ = r.redis.Close() }
				expected = "SMS_DELIVERY_UNKNOWN"
			}
			before, err := r.client.User.Query().Count(r.ctx)
			require.NoError(t, err)
			w := r.request(http.MethodPost, "/login/send", `{"phone":"13900000140"}`, "")
			require.GreaterOrEqual(t, w.Code, 500, w.Body.String())
			require.Contains(t, w.Body.String(), expected)
			require.NotContains(t, w.Body.String(), "challenge_id")
			require.Equal(t, calls, r.sender.calls)
			keys, err := repository.GetIntegrationRedis().Keys(r.ctx, r.smsPrefix+"sms:challenge:*").Result()
			require.NoError(t, err)
			require.Empty(t, keys)
			after, err := r.client.User.Query().Count(r.ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestPhoneFlowSignupPrivacyAndFinalIdentity(t *testing.T) {
	r := newPhoneAuthFlowRig(t)
	phone := "+8613900000150"
	r.cleanPhoneOwner(t, phone)
	proof := r.verifiedProof(t, phone, "login", 0)
	u, created, err := r.auth.LoginOrRegisterPhone(r.ctx, proof, service.PhoneVerifyInput{Phone: phone, ChallengeID: proof.ChallengeID, RegisterIfNew: true})
	require.NoError(t, err)
	require.True(t, created)
	require.True(t, service.IsPhonePlaceholderEmail(u.Email))
	// Give the fixture a known password so the rejection proves the reserved
	// identity guard, rather than failing merely because the password is random.
	require.NoError(t, u.SetPassword("existing-account-password"))
	require.NoError(t, r.client.User.UpdateOneID(u.ID).SetPasswordHash(u.PasswordHash).Exec(r.ctx))
	count, err := r.client.AuthIdentity.Query().Where(authidentity.UserIDEQ(u.ID), authidentity.ProviderTypeEQ("email")).Count(r.ctx)
	require.NoError(t, err)
	require.Zero(t, count)
	login := r.request(http.MethodPost, "/email/login", fmt.Sprintf(`{"email":%q,"password":"existing-account-password"}`, u.Email), "")
	require.Equal(t, http.StatusUnauthorized, login.Code, login.Body.String())
	token, err := r.auth.GenerateToken(r.ctx, u)
	require.NoError(t, err)
	profile := r.request(http.MethodGet, "/profile", "", token)
	require.Equal(t, http.StatusOK, profile.Code, profile.Body.String())
	require.NotContains(t, profile.Body.String(), "phone.aino.invalid")
	require.NotContains(t, profile.Body.String(), phone)
	require.NoError(t, r.settingRepo.SetMultiple(r.ctx, map[string]string{
		service.SettingKeyEmailVerifyEnabled: "true", service.SettingKeyPasswordResetEnabled: "true",
	}))
	require.NoError(t, r.auth.RequestPasswordReset(r.ctx, u.Email, "https://fixture.example.test"))
	reset, err := repository.NewEmailCache(r.redis).GetPasswordResetToken(r.ctx, u.Email)
	require.ErrorIs(t, err, redis.Nil)
	require.Nil(t, reset, "placeholder recovery must not mint a password-reset token")
	unbind := r.request(http.MethodDelete, "/bindings/phone", "", token)
	require.Equal(t, http.StatusConflict, unbind.Code, unbind.Body.String())
	require.Contains(t, unbind.Body.String(), "IDENTITY_UNBIND_LAST_METHOD")
	notifications := service.NewNotificationEmailService(r.settingRepo, nil)
	require.NoError(t, notifications.Send(r.ctx, service.NotificationEmailSendInput{Event: service.NotificationEmailEventBalanceRechargeSuccess, RecipientEmail: u.Email, UserID: u.ID}))
	// A real recipient reaches the missing mail transport, while a placeholder
	// is suppressed before template/delivery work.
	require.Error(t, notifications.Send(r.ctx, service.NotificationEmailSendInput{Event: service.NotificationEmailEventBalanceRechargeSuccess, RecipientEmail: "fixture@example.test", UserID: u.ID}))
}

func TestPhoneFlowUnbindRequiresRecentAuthentication(t *testing.T) {
	r := newPhoneAuthFlowRig(t)
	u := r.user(t, service.StatusActive, false)
	_, err := r.client.AuthIdentity.Create().SetUserID(u.ID).SetProviderType("phone").SetProviderKey("default").SetProviderSubject("+8613900000160").SetMetadata(map[string]any{}).Save(r.ctx)
	require.NoError(t, err)
	token, err := r.auth.GenerateToken(r.ctx, u)
	require.NoError(t, err)
	claims, err := r.auth.ValidateToken(token)
	require.NoError(t, err)
	claims.AuthTime = 0
	stale, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("integration-phone-binding-secret"))
	require.NoError(t, err)
	w := r.request(http.MethodDelete, "/bindings/phone", "", stale)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var envelope struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, "RECENT_AUTH_REQUIRED", envelope.Reason)
}
