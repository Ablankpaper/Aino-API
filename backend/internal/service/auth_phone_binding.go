package service

import (
	"context"
	"errors"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// BindPhoneIdentity verifies and binds a phone number to the current user.
// The phone number must be verified via SMS before binding. Phone binding does
// NOT grant first-bind bonuses (unlike email/OAuth).
func (s *AuthService) BindPhoneIdentity(
	ctx context.Context,
	userID int64,
	proof PhoneCodeProof,
) (*User, error) {
	if s == nil {
		return nil, ErrServiceUnavailable
	}
	if !proof.verified || strings.TrimSpace(proof.ChallengeID) == "" {
		return nil, ErrPhoneAuthProof
	}

	// Normalize phone number
	normalizedPhone, err := NormalizeCNPhone(proof.Phone)
	if err != nil {
		return nil, ErrPhoneInvalid
	}

	// Verify the proof is for binding the current user
	if proof.Purpose != "bind_phone" || proof.UserID != userID {
		return nil, ErrPhoneAuthProof
	}

	// Get current user
	currentUser, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	// Check if this phone is already bound to another user
	if err := s.ensurePhoneIdentityAvailableForUser(ctx, currentUser, normalizedPhone); err != nil {
		return nil, err
	}

	// Bind phone identity in transaction
	if s.entClient != nil {
		if err := s.updateBoundPhoneIdentityTx(ctx, currentUser, normalizedPhone); err != nil {
			return nil, err
		}
		s.revokePhoneIdentitySessions(ctx, userID)
		return currentUser, nil
	}

	// Fallback: non-transactional path (for tests without entClient)
	return nil, ErrServiceUnavailable
}

// ensurePhoneIdentityAvailableForUser checks if the phone number is available for binding.
// Returns ErrPhoneAlreadyBound if the phone is owned by another user.
func (s *AuthService) ensurePhoneIdentityAvailableForUser(
	ctx context.Context,
	currentUser *User,
	phone string,
) error {
	if currentUser == nil {
		return ErrUserNotFound
	}
	if s.entClient == nil {
		return ErrServiceUnavailable
	}

	// Check if phone is already bound to any user
	existingIdentity, err := s.entClient.AuthIdentity.Query().
		Where(
			authidentity.ProviderTypeEQ("phone"),
			authidentity.ProviderKeyEQ("default"),
			authidentity.ProviderSubjectEQ(phone),
		).
		Only(ctx)

	switch {
	case err == nil:
		// Phone exists - check if it belongs to current user
		if existingIdentity.UserID == currentUser.ID {
			// User's own phone - allow rebind/update
			return nil
		}
		return ErrPhoneAlreadyBound
	case dbent.IsNotFound(err):
		// Phone not bound yet - OK
		return nil
	default:
		return ErrServiceUnavailable
	}
}

func (s *AuthService) updateBoundPhoneIdentityTx(
	ctx context.Context,
	currentUser *User,
	phone string,
) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return s.updateBoundPhoneIdentityWithClient(ctx, tx.Client(), currentUser, phone)
	}

	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return ErrServiceUnavailable
	}
	defer func() { _ = tx.Rollback() }()

	txCtx := dbent.NewTxContext(ctx, tx)
	if err := s.updateBoundPhoneIdentityWithClient(txCtx, tx.Client(), currentUser, phone); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return ErrServiceUnavailable
	}
	return nil
}

func (s *AuthService) updateBoundPhoneIdentityWithClient(
	ctx context.Context,
	client *dbent.Client,
	currentUser *User,
	phone string,
) error {
	if client == nil || currentUser == nil || currentUser.ID <= 0 {
		return ErrServiceUnavailable
	}

	// Create or update phone auth identity
	if err := ensureBoundPhoneAuthIdentityWithClient(ctx, client, currentUser.ID, phone, "auth_service_phone_bind"); err != nil {
		if errors.Is(err, ErrPhoneAlreadyBound) {
			return ErrPhoneAlreadyBound
		}
		return ErrServiceUnavailable
	}

	// Note: Phone binding does NOT grant first-bind bonuses per requirements
	// "绑定不复制账户、不合并余额、不发重复赠金"

	return nil
}

func (s *AuthService) revokePhoneIdentitySessions(ctx context.Context, userID int64) {
	if err := s.RevokeAllUserSessions(ctx, userID); err != nil {
		logger.LegacyPrintf("service.auth", "[Auth] Failed to revoke refresh sessions after phone identity bind for user %d: %v", userID, err)
	}
}

// ensureBoundPhoneAuthIdentityWithClient creates or updates a phone auth identity.
// Uses ON CONFLICT DO NOTHING to handle concurrent binding attempts.
func ensureBoundPhoneAuthIdentityWithClient(
	ctx context.Context,
	client *dbent.Client,
	userID int64,
	phone string,
	source string,
) error {
	if client == nil || userID <= 0 || phone == "" {
		return nil
	}

	if strings.TrimSpace(source) == "" {
		source = "auth_service_phone_bind"
	}

	// Create phone identity (idempotent via ON CONFLICT)
	if err := client.AuthIdentity.Create().
		SetUserID(userID).
		SetProviderType("phone").
		SetProviderKey("default").
		SetProviderSubject(phone).
		SetVerifiedAt(time.Now().UTC()).
		SetMetadata(map[string]any{"source": strings.TrimSpace(source)}).
		OnConflictColumns(
			authidentity.FieldProviderType,
			authidentity.FieldProviderKey,
			authidentity.FieldProviderSubject,
		).
		DoNothing().
		Exec(ctx); err != nil {
		if !isSQLNoRowsError(err) {
			return err
		}
	}

	// Verify ownership after create (handle race conditions)
	identity, err := client.AuthIdentity.Query().
		Where(
			authidentity.ProviderTypeEQ("phone"),
			authidentity.ProviderKeyEQ("default"),
			authidentity.ProviderSubjectEQ(phone),
		).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil
		}
		return err
	}
	if identity.UserID != userID {
		return ErrPhoneAlreadyBound
	}
	return nil
}
