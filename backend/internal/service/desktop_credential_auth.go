package service

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type desktopCredentialGroupKey struct{}

func WithDesktopCredentialGroup(ctx context.Context, groupID int64) context.Context {
	return context.WithValue(ctx, desktopCredentialGroupKey{}, groupID)
}

func checkDesktopCredentialGroup(ctx context.Context, groupID int64) error {
	if fixed, ok := ctx.Value(desktopCredentialGroupKey{}).(int64); ok && fixed != groupID {
		return fmt.Errorf("desktop authorization cannot change billing group")
	}
	return nil
}

func desktopCredentialHasFixedGroup(ctx context.Context) bool {
	_, ok := ctx.Value(desktopCredentialGroupKey{}).(int64)
	return ok
}

func (s *APIKeyService) SetDesktopCredentialDependencies(repo DesktopCredentialRepository, cache RefreshTokenCache, settings *SettingService) {
	s.desktopRepo, s.desktopRefreshCache, s.desktopSettings = repo, cache, settings
}

// Managed keys are revalidated even on cache hits and read-only usage endpoints.
// Ordinary keys retain their existing authentication and lifetime semantics.
func (s *APIKeyService) ValidateDesktopCredential(ctx context.Context, key *APIKey) error {
	if !key.DesktopManaged {
		return nil
	}
	if s.desktopRepo == nil || s.desktopSettings == nil {
		return ErrServiceUnavailable
	}
	lease, err := s.desktopRepo.GetByAPIKeyID(ctx, key.ID)
	if errors.Is(err, ErrDesktopCredentialNotFound) {
		return ErrDesktopCredentialRevoked
	}
	if err != nil {
		return ErrServiceUnavailable
	}
	if lease.RevokedAt != nil || lease.UserID != key.UserID || key.GroupID == nil || lease.GroupID != *key.GroupID {
		return ErrDesktopCredentialRevoked
	}
	if !lease.ExpiresAt.After(time.Now()) {
		return ErrDesktopCredentialExpired
	}
	user, err := s.userRepo.GetByID(ctx, key.UserID)
	if err != nil {
		return ErrServiceUnavailable
	}
	if !user.IsActive() || resolvedTokenVersion(user) != lease.TokenVersion {
		return ErrDesktopCredentialRevoked
	}
	if _, err = activeDesktopParent(ctx, s.desktopRefreshCache, user, lease.SessionFamilyID); err != nil {
		return err
	}
	fresh, err := s.apiKeyRepo.GetByID(ctx, key.ID)
	if errors.Is(err, ErrAPIKeyNotFound) {
		return ErrDesktopCredentialRevoked
	}
	if err != nil {
		return ErrServiceUnavailable
	}
	if !fresh.IsActive() || !fresh.DesktopManaged || fresh.GroupID == nil || *fresh.GroupID != lease.GroupID {
		return ErrDesktopCredentialRevoked
	}
	if fresh.IsExpired() || fresh.ExpiresAt == nil {
		return ErrDesktopCredentialExpired
	}
	group, err := s.groupRepo.GetByID(ctx, lease.GroupID)
	if errors.Is(err, ErrGroupNotFound) {
		return ErrDesktopCredentialRevoked
	}
	if err != nil {
		return ErrServiceUnavailable
	}
	if !group.IsActive() || !s.canUserBindGroup(ctx, user, group) {
		return ErrDesktopCredentialRevoked
	}
	settings, err := s.desktopSettings.GetDesktopSettings(ctx)
	if err != nil {
		return ErrServiceUnavailable
	}
	if !settings.Enabled {
		return ErrDesktopCredentialRevoked
	}
	models := []string{}
	for _, entry := range settings.Models {
		if entry.GroupID == group.ID && entry.AgentVerified && entry.Capabilities.Tools && group.ModelAllowlist.Allows(entry.Model) {
			models = append(models, entry.Model)
		}
	}
	if len(models) == 0 {
		return ErrDesktopCredentialRevoked
	}
	groupCopy := *group
	groupCopy.ModelAllowlist = GroupModelAllowlist{Enabled: true, Models: models}
	// A desktop grant never authorizes a different billing group via fallback.
	groupCopy.FallbackGroupID = nil
	groupCopy.FallbackGroupIDOnInvalidRequest = nil
	*key = *fresh
	key.User, key.Group = user, &groupCopy
	return nil
}

func (s *AuthService) SetDesktopCredentialRevoker(repo DesktopCredentialRevoker) {
	s.desktopRevoker = repo
}
func (s *UserService) SetDesktopCredentialRevoker(repo DesktopCredentialRevoker) {
	s.desktopRevoker = repo
}
