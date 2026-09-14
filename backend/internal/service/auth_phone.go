package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// PhoneVerifyInput represents phone verification input for login/registration
type PhoneVerifyInput struct {
	Phone             string
	ChallengeID       string
	Code              string
	RegisterIfNew     bool
	AgreementRevision string
	InvitationCode    string
	PromoCode         string
}

// LoginOrRegisterPhone handles phone-based authentication
// Returns (user, isNewUser, error)
func (s *AuthService) LoginOrRegisterPhone(
	ctx context.Context,
	proof *PhoneCodeProof,
	input PhoneVerifyInput,
) (*User, bool, error) {
	if proof == nil {
		return nil, false, ErrPhoneAuthProof
	}
	if !proof.verified || strings.TrimSpace(proof.ChallengeID) == "" {
		return nil, false, ErrPhoneAuthProof
	}
	if strings.TrimSpace(input.ChallengeID) == "" || input.ChallengeID != proof.ChallengeID {
		return nil, false, ErrPhoneAuthProof
	}

	// Normalize both values and require the proof to match the submitted phone.
	normalizedPhone, err := NormalizeCNPhone(proof.Phone)
	if err != nil {
		return nil, false, ErrPhoneInvalid
	}
	requestedPhone, err := NormalizeCNPhone(input.Phone)
	if err != nil || requestedPhone != normalizedPhone {
		return nil, false, ErrPhoneAuthProof
	}

	// Verify proof purpose
	if proof.Purpose != "login" || proof.UserID != 0 {
		return nil, false, ErrPhoneAuthProof
	}

	// Check if phone identity exists
	user, err := s.getUserByPhoneIdentity(ctx, normalizedPhone)

	if err != nil && !errors.Is(err, ErrUserNotFound) {
		logger.LegacyPrintf("service.auth", "[Auth] Database error checking phone identity: %v", err)
		return nil, false, ErrServiceUnavailable
	}

	// Existing user login
	if user != nil {
		if !user.IsActive() {
			return nil, false, ErrUserNotActive
		}
		s.postAuthUserBootstrap(ctx, user, "phone", true)
		return user, false, nil
	}

	// New user registration
	if !input.RegisterIfNew {
		return nil, false, ErrUserNotFound
	}

	// Check if registration is enabled
	if s.settingService == nil || !s.settingService.IsRegistrationEnabled(ctx) {
		return nil, false, ErrRegDisabled
	}

	// Validate agreement if enabled
	if err := s.validateLoginAgreement(ctx, input.AgreementRevision); err != nil {
		return nil, false, err
	}

	// Check invitation code if required
	var invitationRedeemCode *RedeemCode
	if s.settingService != nil && s.settingService.IsInvitationCodeEnabled(ctx) {
		if strings.TrimSpace(input.InvitationCode) == "" {
			return nil, false, ErrInvitationCodeRequired
		}
		if s.redeemRepo == nil {
			return nil, false, ErrServiceUnavailable
		}
		redeemCode, err := s.redeemRepo.GetByCode(ctx, input.InvitationCode)
		if err != nil {
			logger.LegacyPrintf("service.auth", "[Auth] Invalid invitation code: %s, error: %v", input.InvitationCode, err)
			return nil, false, ErrInvitationCodeInvalid
		}
		if redeemCode.Type != RedeemTypeInvitation || !redeemCode.CanUse() {
			logger.LegacyPrintf("service.auth", "[Auth] Invitation code invalid: type=%s, status=%s", redeemCode.Type, redeemCode.Status)
			return nil, false, ErrInvitationCodeInvalid
		}
		invitationRedeemCode = redeemCode
	}

	// Create new user with phone identity
	newUser, created, err := s.createPhoneUser(ctx, normalizedPhone, invitationRedeemCode)
	if err != nil {
		return nil, false, err
	}
	if !created {
		if newUser == nil || !newUser.IsActive() {
			return nil, false, ErrUserNotActive
		}
		s.postAuthUserBootstrap(ctx, newUser, "phone", true)
		return newUser, false, nil
	}

	s.postAuthUserBootstrap(ctx, newUser, "phone", true)
	grantPlan := s.resolveSignupGrantPlan(ctx, "phone")
	s.assignSubscriptions(ctx, newUser.ID, grantPlan.Subscriptions, "auto assigned by signup defaults")
	_ = s.snapshotPlatformQuotaDefaults(ctx, newUser.ID, &grantPlan)
	newUser = s.applyOAuthSignupPromoCode(ctx, newUser, input.PromoCode)

	return newUser, true, nil
}

// getUserByPhoneIdentity finds a user by their phone identity
func (s *AuthService) getUserByPhoneIdentity(ctx context.Context, normalizedPhone string) (*User, error) {
	if s.entClient == nil {
		return nil, ErrServiceUnavailable
	}

	identity, err := s.entClient.AuthIdentity.Query().
		Where(
			authidentity.ProviderTypeEQ("phone"),
			authidentity.ProviderKeyEQ("default"),
			authidentity.ProviderSubjectEQ(normalizedPhone),
		).
		Only(ctx)

	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}

	return s.userRepo.GetByID(ctx, identity.UserID)
}

// createPhoneUser creates a new user with phone identity
func (s *AuthService) createPhoneUser(
	ctx context.Context,
	normalizedPhone string,
	invitation *RedeemCode,
) (*User, bool, error) {
	// Generate placeholder email
	placeholderEmail, err := NewPhonePlaceholderEmail()
	if err != nil {
		logger.LegacyPrintf("service.auth", "[Auth] Failed to generate placeholder email: %v", err)
		return nil, false, ErrServiceUnavailable
	}

	// Generate random password hash
	randomPassword, err := randomHexString(32)
	if err != nil {
		logger.LegacyPrintf("service.auth", "[Auth] Failed to generate random password: %v", err)
		return nil, false, ErrServiceUnavailable
	}
	hashedPassword, err := s.HashPassword(randomPassword)
	if err != nil {
		return nil, false, ErrServiceUnavailable
	}

	grantPlan := s.resolveSignupGrantPlan(ctx, "phone")

	var defaultRPMLimit int
	if s.settingService != nil {
		defaultRPMLimit = s.settingService.GetDefaultUserRPMLimit(ctx)
	}

	user := &User{
		Email:        placeholderEmail,
		PasswordHash: hashedPassword,
		Role:         RoleUser,
		Balance:      grantPlan.Balance,
		Concurrency:  grantPlan.Concurrency,
		RPMLimit:     defaultRPMLimit,
		Status:       StatusActive,
		SignupSource: "phone",
	}

	// Create user and phone identity in transaction
	if s.entClient == nil {
		return nil, false, ErrServiceUnavailable
	}

	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		logger.LegacyPrintf("service.auth", "[Auth] Failed to start transaction: %v", err)
		return nil, false, ErrServiceUnavailable
	}
	defer func() { _ = tx.Rollback() }()

	txCtx := dbent.NewTxContext(ctx, tx)

	// Create user
	if err := s.createUserWithRegistrationEmailGuard(txCtx, user); err != nil {
		if errors.Is(err, ErrEmailExists) {
			// Race condition: another registration used same placeholder (extremely unlikely)
			// Retry with new placeholder
			logger.LegacyPrintf("service.auth", "[Auth] Placeholder email collision, very unlikely: %v", err)
			return nil, false, ErrServiceUnavailable
		}
		logger.LegacyPrintf("service.auth", "[Auth] Database error creating phone user: %v", err)
		return nil, false, ErrServiceUnavailable
	}

	// Claim invitation if provided
	if invitation != nil {
		if err := s.redeemRepo.Use(txCtx, invitation.ID, user.ID); err != nil {
			logger.LegacyPrintf("service.auth", "[Auth] Failed to claim invitation code: %v", err)
			return nil, false, ErrInvitationCodeInvalid
		}
	}

	// Create phone identity
	now := time.Now()
	_, err = tx.Client().AuthIdentity.Create().
		SetUserID(user.ID).
		SetProviderType("phone").
		SetProviderKey("default").
		SetProviderSubject(normalizedPhone).
		SetMetadata(map[string]interface{}{}).
		SetVerifiedAt(now).
		SetCreatedAt(now).
		SetUpdatedAt(now).
		Save(txCtx)

	if err != nil {
		// Check for uniqueness violation
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			// Race condition: phone was registered by another request
			// Query the final owner
			if err := tx.Rollback(); err != nil {
				logger.LegacyPrintf("service.auth", "[Auth] Rollback error: %v", err)
			}
			existingUser, queryErr := s.getUserByPhoneIdentity(ctx, normalizedPhone)
			if queryErr == nil && existingUser != nil {
				return existingUser, false, nil
			}
			return nil, false, ErrPhoneAlreadyBound
		}
		logger.LegacyPrintf("service.auth", "[Auth] Failed to create phone identity: %v", err)
		return nil, false, ErrServiceUnavailable
	}

	if err := tx.Commit(); err != nil {
		logger.LegacyPrintf("service.auth", "[Auth] Failed to commit transaction: %v", err)
		return nil, false, ErrServiceUnavailable
	}

	// Initialize affiliate profile
	if s.affiliateService != nil {
		if _, err := s.affiliateService.EnsureUserAffiliate(ctx, user.ID); err != nil {
			logger.LegacyPrintf("service.auth", "[Auth] Failed to initialize affiliate profile for user %d: %v", user.ID, err)
		}
	}

	return user, true, nil
}

// validateLoginAgreement checks if user has agreed to current agreement revision
func (s *AuthService) validateLoginAgreement(ctx context.Context, agreedRevision string) error {
	if s == nil || s.settingService == nil {
		return ErrServiceUnavailable
	}
	settings, err := s.settingService.GetPublicSettings(ctx)
	if err != nil {
		return ErrServiceUnavailable
	}
	if !settings.LoginAgreementEnabled {
		return nil
	}
	if strings.TrimSpace(agreedRevision) == "" {
		return ErrLoginAgreementRequired
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(agreedRevision)), []byte(settings.LoginAgreementRevision)) != 1 {
		return ErrLoginAgreementInvalid
	}
	return nil
}
