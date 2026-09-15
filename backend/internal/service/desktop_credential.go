package service

import (
	"context"
	"errors"
	"strconv"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

var (
	ErrDesktopCredentialNotFound = infraerrors.NotFound("DESKTOP_CREDENTIAL_NOT_FOUND", "desktop credential not found")
	ErrDesktopCredentialRevoked  = infraerrors.Unauthorized("DESKTOP_CREDENTIAL_REVOKED", "desktop credential or parent session has been revoked")
	ErrDesktopCredentialExpired  = infraerrors.Unauthorized("DESKTOP_CREDENTIAL_EXPIRED", "desktop credential has expired")
)

type DesktopCredential struct {
	ID                                           int64
	UserID                                       int64
	DeviceID, ConnectionGrantID, SessionFamilyID string
	TokenVersion                                 int64
	GroupID                                      int64
	ModelID                                      string
	APIKeyID                                     int64
	ExpiresAt                                    time.Time
	RevokedAt                                    *time.Time
	RevokeReason                                 string
	CreatedAt, UpdatedAt                         time.Time
}

type DesktopCredentialRevoker interface {
	RevokeFamily(context.Context, string) error
	RevokeUser(context.Context, int64, string) error
}

type DesktopCredentialRepository interface {
	DesktopCredentialRevoker
	IssueOrReuse(context.Context, *DesktopCredential, *APIKey, time.Duration) (*DesktopCredential, *APIKey, error)
	GetByAPIKeyID(context.Context, int64) (*DesktopCredential, error)
	ListByUser(context.Context, int64) ([]DesktopCredential, error)
}

// Parent activity is checked against live refresh tokens, never merely set membership.
func activeDesktopParent(ctx context.Context, cache RefreshTokenCache, user *User, family string) (time.Time, error) {
	reader, ok := cache.(interface {
		ActiveFamilyDeadline(context.Context, int64, string) (time.Time, error)
	})
	if !ok {
		return time.Time{}, ErrServiceUnavailable
	}
	deadline, err := reader.ActiveFamilyDeadline(ctx, user.ID, family)
	if err != nil {
		return time.Time{}, ErrServiceUnavailable
	}
	if !user.IsActive() || !deadline.After(time.Now()) {
		return time.Time{}, ErrDesktopCredentialRevoked
	}
	return deadline, nil
}

type DesktopCredentialService struct {
	repo   DesktopCredentialRepository
	keys   *APIKeyService
	models *DesktopModelService
	auth   *AuthService
	cache  RefreshTokenCache
}

func NewDesktopCredentialService(repo DesktopCredentialRepository, keys *APIKeyService, models *DesktopModelService, auth *AuthService, cache RefreshTokenCache) *DesktopCredentialService {
	return &DesktopCredentialService{repo: repo, keys: keys, models: models, auth: auth, cache: cache}
}

func (s *DesktopCredentialService) IssueOrReuseLease(ctx context.Context, req DesktopCredentialRequest) (*DesktopCredentialResponse, error) {
	if req.UserID <= 0 || len(req.SessionFamilyID) == 0 || len(req.SessionFamilyID) > 128 || req.ModelID == "" {
		return nil, infraerrors.BadRequest("INVALID_REQUEST", "authenticated session and model are required")
	}
	for _, id := range []string{req.DeviceID, req.ConnectionGrantID} {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed == uuid.Nil || parsed.String() != id {
			return nil, infraerrors.BadRequest("INVALID_REQUEST", "device and connection grant IDs must be canonical UUIDs")
		}
	}
	user, err := s.keys.userRepo.GetByID(ctx, req.UserID)
	if err != nil {
		return nil, err
	}
	deadline, err := activeDesktopParent(ctx, s.cache, user, req.SessionFamilyID)
	if err != nil {
		return nil, err
	}
	resolved, err := s.models.ResolveForUser(ctx, req.UserID, req.ModelID)
	if err != nil {
		return nil, err
	}
	settings, err := s.models.settings.GetDesktopSettings(ctx)
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(settings.CredentialTTLSeconds) * time.Second
	expires := time.Now().Add(ttl)
	if deadline.Before(expires) {
		expires = deadline
	}
	rawKey, err := s.keys.GenerateKey()
	if err != nil {
		return nil, err
	}
	groupID := resolved.GroupID
	candidate := &DesktopCredential{UserID: user.ID, DeviceID: req.DeviceID, ConnectionGrantID: req.ConnectionGrantID, SessionFamilyID: req.SessionFamilyID, TokenVersion: resolvedTokenVersion(user), GroupID: groupID, ModelID: req.ModelID, ExpiresAt: expires}
	key := &APIKey{UserID: user.ID, Name: "Aino desktop", Key: rawKey, GroupID: &groupID, Status: StatusActive, ExpiresAt: &expires, DesktopManaged: true}
	lease, key, err := s.repo.IssueOrReuse(ctx, candidate, key, ttl)
	if err != nil {
		return nil, err
	}
	// Close the issuance/revocation race before handing the secret to the caller.
	fresh, err := s.keys.userRepo.GetByID(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	if resolvedTokenVersion(fresh) != lease.TokenVersion {
		_ = s.repo.RevokeUser(ctx, user.ID, "identity_changed")
		return nil, ErrDesktopCredentialRevoked
	}
	if _, err = activeDesktopParent(ctx, s.cache, fresh, req.SessionFamilyID); err != nil {
		_ = s.repo.RevokeFamily(ctx, req.SessionFamilyID)
		return nil, err
	}
	s.keys.InvalidateAuthCacheByKey(ctx, key.Key)
	return &DesktopCredentialResponse{CredentialID: strconv.FormatInt(lease.ID, 10), APIKey: key.Key, BaseURL: "https://api.agentera.com.cn/v1", ExpiresAt: lease.ExpiresAt, Model: resolved.Model}, nil
}

type DesktopCredentialRequest struct {
	UserID                                                int64
	DeviceID, ConnectionGrantID, SessionFamilyID, ModelID string
}

type DesktopCredentialResponse struct {
	CredentialID string        `json:"credential_id"`
	APIKey       string        `json:"api_key"`
	BaseURL      string        `json:"base_url"`
	ExpiresAt    time.Time     `json:"expires_at"`
	Model        PlatformModel `json:"model"`
}

type DesktopDevice struct {
	DeviceID   string    `json:"device_id"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Revoked    bool      `json:"revoked"`
}

func (s *DesktopCredentialService) ListDevices(ctx context.Context, userID int64) ([]DesktopDevice, error) {
	leases, err := s.repo.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	devices := []DesktopDevice{}
	byID := map[string]int{}
	for _, lease := range leases {
		index, ok := byID[lease.DeviceID]
		if !ok {
			index = len(devices)
			byID[lease.DeviceID] = index
			devices = append(devices, DesktopDevice{DeviceID: lease.DeviceID, Revoked: true})
		}
		device := &devices[index]
		if lease.UpdatedAt.After(device.LastUsedAt) {
			device.LastUsedAt = lease.UpdatedAt
		}
		if lease.ExpiresAt.After(device.ExpiresAt) {
			device.ExpiresAt = lease.ExpiresAt
		}
		if lease.RevokedAt == nil && lease.ExpiresAt.After(time.Now()) {
			device.Revoked = false
		}
	}
	return devices, nil
}

func (s *DesktopCredentialService) RevokeDevice(ctx context.Context, userID int64, deviceID string) error {
	if _, err := uuid.Parse(deviceID); err != nil {
		return infraerrors.BadRequest("INVALID_DEVICE", "invalid device ID")
	}
	leases, err := s.repo.ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	families := map[string]bool{}
	for _, lease := range leases {
		if lease.DeviceID == deviceID {
			families[lease.SessionFamilyID] = true
		}
	}
	if len(families) == 0 {
		return ErrDesktopCredentialNotFound
	}
	var errs []error
	for family := range families {
		if err := s.auth.RevokeSessionFamily(ctx, family); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
