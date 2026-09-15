package repository

import (
	"context"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/ent/desktopmodelcredential"
	"github.com/Wei-Shaw/sub2api/ent/predicate"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type desktopCredentialRepository struct{ client *dbent.Client }

func NewDesktopCredentialRepository(client *dbent.Client) service.DesktopCredentialRepository {
	return &desktopCredentialRepository{client: client}
}

// The user row serializes issuance across API instances, including the first
// lease where there is no credential row to lock. The key and lease commit together.
func (r *desktopCredentialRepository) IssueOrReuse(ctx context.Context, candidate *service.DesktopCredential, key *service.APIKey, ttl time.Duration) (*service.DesktopCredential, *service.APIKey, error) {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.User.Query().Where(user.IDEQ(candidate.UserID), user.DeletedAtIsNil()).ForUpdate().Only(ctx); err != nil {
		return nil, nil, err
	}
	// A new grant ID cannot resurrect a parent that lost its authority.
	revoked, err := tx.DesktopModelCredential.Query().Where(desktopmodelcredential.UserIDEQ(candidate.UserID), desktopmodelcredential.SessionFamilyIDEQ(candidate.SessionFamilyID), desktopmodelcredential.RevokedAtNotNil(), desktopmodelcredential.RevokeReasonNEQ("expired")).Exist(ctx)
	if err != nil {
		return nil, nil, err
	}
	if revoked {
		return nil, nil, service.ErrDesktopCredentialRevoked
	}
	scope := []predicate.DesktopModelCredential{
		desktopmodelcredential.UserIDEQ(candidate.UserID), desktopmodelcredential.DeviceIDEQ(candidate.DeviceID),
		desktopmodelcredential.ConnectionGrantIDEQ(candidate.ConnectionGrantID), desktopmodelcredential.SessionFamilyIDEQ(candidate.SessionFamilyID), desktopmodelcredential.GroupIDEQ(candidate.GroupID),
	}
	previous, err := tx.DesktopModelCredential.Query().Where(scope...).Order(dbent.Desc(desktopmodelcredential.FieldID)).First(ctx)
	if err != nil && !dbent.IsNotFound(err) {
		return nil, nil, err
	}
	keyRepo := NewAPIKeyRepository(tx.Client(), nil)
	if previous != nil {
		if previous.TokenVersion != candidate.TokenVersion || previous.RevokedAt != nil && previous.RevokeReason != "expired" {
			return nil, nil, service.ErrDesktopCredentialRevoked
		}
		existing, err := keyRepo.GetByID(ctx, previous.APIKeyID)
		if err != nil {
			return nil, nil, err
		}
		if !existing.DesktopManaged || !existing.IsActive() || existing.GroupID == nil || *existing.GroupID != candidate.GroupID || existing.UserID != candidate.UserID {
			return nil, nil, service.ErrDesktopCredentialRevoked
		}
		if previous.RevokedAt == nil && previous.ExpiresAt.After(time.Now()) && existing.ExpiresAt != nil && existing.ExpiresAt.After(time.Now()) {
			// Reuse initially; renew after one third of the configured lifetime.
			if time.Until(previous.ExpiresAt) <= ttl*2/3 || candidate.ExpiresAt.Before(previous.ExpiresAt) {
				previous, err = tx.DesktopModelCredential.UpdateOne(previous).SetExpiresAt(candidate.ExpiresAt).Save(ctx)
				if err != nil {
					return nil, nil, err
				}
				existing.ExpiresAt = &candidate.ExpiresAt
				if err = keyRepo.Update(ctx, existing, service.APIKeyUpdateFields{ExpiresAt: true}); err != nil {
					return nil, nil, err
				}
			}
			if err = tx.Commit(); err != nil {
				return nil, nil, err
			}
			return desktopCredentialFromEntity(previous), existing, nil
		}
		if previous.RevokedAt == nil {
			if _, err = tx.DesktopModelCredential.UpdateOne(previous).SetRevokedAt(time.Now()).SetRevokeReason("expired").Save(ctx); err != nil {
				return nil, nil, err
			}
			if _, err = tx.APIKey.UpdateOneID(previous.APIKeyID).SetStatus(service.StatusAPIKeyDisabled).Save(ctx); err != nil {
				return nil, nil, err
			}
		}
	}
	if err = keyRepo.Create(ctx, key); err != nil {
		return nil, nil, err
	}
	created, err := tx.DesktopModelCredential.Create().SetUserID(candidate.UserID).SetDeviceID(candidate.DeviceID).
		SetConnectionGrantID(candidate.ConnectionGrantID).SetSessionFamilyID(candidate.SessionFamilyID).
		SetTokenVersion(candidate.TokenVersion).SetGroupID(candidate.GroupID).SetModelID(candidate.ModelID).
		SetAPIKeyID(key.ID).SetExpiresAt(candidate.ExpiresAt).Save(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	return desktopCredentialFromEntity(created), key, nil
}

func (r *desktopCredentialRepository) GetByAPIKeyID(ctx context.Context, id int64) (*service.DesktopCredential, error) {
	entity, err := r.client.DesktopModelCredential.Query().Where(desktopmodelcredential.APIKeyIDEQ(id)).Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, service.ErrDesktopCredentialNotFound
	}
	if err != nil {
		return nil, err
	}
	return desktopCredentialFromEntity(entity), nil
}

func (r *desktopCredentialRepository) ListByUser(ctx context.Context, userID int64) ([]service.DesktopCredential, error) {
	entities, err := r.client.DesktopModelCredential.Query().Where(desktopmodelcredential.UserIDEQ(userID)).Order(dbent.Desc(desktopmodelcredential.FieldUpdatedAt)).All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]service.DesktopCredential, 0, len(entities))
	for _, entity := range entities {
		result = append(result, *desktopCredentialFromEntity(entity))
	}
	return result, nil
}

func (r *desktopCredentialRepository) RevokeFamily(ctx context.Context, family string) error {
	return r.revoke(ctx, desktopmodelcredential.SessionFamilyIDEQ(family), "parent_session_revoked")
}

func (r *desktopCredentialRepository) RevokeUser(ctx context.Context, userID int64, reason string) error {
	return r.revoke(ctx, desktopmodelcredential.UserIDEQ(userID), reason)
}

func (r *desktopCredentialRepository) revoke(ctx context.Context, scope predicate.DesktopModelCredential, reason string) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.DesktopModelCredential.Query().Where(scope, desktopmodelcredential.RevokedAtIsNil()).All(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return tx.Commit()
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.APIKeyID)
	}
	if _, err = tx.DesktopModelCredential.Update().Where(scope, desktopmodelcredential.RevokedAtIsNil()).SetRevokedAt(time.Now()).SetRevokeReason(reason).Save(ctx); err != nil {
		return err
	}
	// Existing DB triggers enqueue durable cross-instance auth invalidations.
	if _, err = tx.APIKey.Update().Where(apikey.IDIn(ids...), apikey.DesktopManagedEQ(true)).SetStatus(service.StatusAPIKeyDisabled).Save(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func desktopCredentialFromEntity(e *dbent.DesktopModelCredential) *service.DesktopCredential {
	return &service.DesktopCredential{ID: e.ID, UserID: e.UserID, DeviceID: e.DeviceID, ConnectionGrantID: e.ConnectionGrantID, SessionFamilyID: e.SessionFamilyID, TokenVersion: e.TokenVersion, GroupID: e.GroupID, ModelID: e.ModelID, APIKeyID: e.APIKeyID, ExpiresAt: e.ExpiresAt, RevokedAt: e.RevokedAt, RevokeReason: e.RevokeReason, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
}
