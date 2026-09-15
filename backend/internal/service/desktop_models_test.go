package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type desktopSettingsRepo struct {
	SettingRepository
	values map[string]string
}

func (r *desktopSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	values := make(map[string]string)
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			values[key] = value
		}
	}
	return values, nil
}

func (r *desktopSettingsRepo) GetAll(context.Context) (map[string]string, error) {
	values := make(map[string]string, len(r.values))
	for key, value := range r.values {
		values[key] = value
	}
	return values, nil
}

func (r *desktopSettingsRepo) SetMultiple(_ context.Context, values map[string]string) error {
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func TestDesktopCatalogSettingsDefaultClosedAndValidateDefault(t *testing.T) {
	ctx := context.Background()
	repo := &desktopSettingsRepo{values: map[string]string{}}
	settings := NewSettingService(repo, &config.Config{})
	initial, err := settings.GetDesktopSettings(ctx)
	require.NoError(t, err)
	require.False(t, initial.Enabled)
	require.Empty(t, initial.Models)
	require.Nil(t, initial.DefaultModelID)
	entry := DesktopModelEntry{ID: "fixture-a", GroupID: 11, Model: "fixture-upstream", DisplayName: "Fixture A", ProviderLabel: "Fixture", Platform: PlatformOpenAI, APIMode: "chat_completions", AgentVerified: true, Capabilities: PlatformModelCapabilities{Tools: true}}
	defaultID := entry.ID
	edit := DesktopSettings{Enabled: true, Models: []DesktopModelEntry{entry}, DefaultModelID: &defaultID, CredentialTTLSeconds: 3600}
	updates, err := settings.buildDesktopSettingsUpdates(edit)
	require.NoError(t, err)
	repo.values = updates
	loaded, err := settings.GetDesktopSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, edit, *loaded)
	edit.Models[0].AgentVerified = false
	_, err = settings.buildDesktopSettingsUpdates(edit)
	require.Error(t, err)
	edit.Models[0].AgentVerified = true
	edit.Models = append(edit.Models, entry)
	_, err = settings.buildDesktopSettingsUpdates(edit)
	require.Error(t, err)
}

func TestDesktopCatalogSystemSettingsRoundTripAndPublicCapability(t *testing.T) {
	ctx := context.Background()
	repo := &desktopSettingsRepo{values: map[string]string{}}
	settings := NewSettingService(repo, &config.Config{})
	entry := DesktopModelEntry{ID: "fixture", GroupID: 11, Model: "fixture-upstream", DisplayName: "Fixture", ProviderLabel: "Fixture", Platform: PlatformOpenAI, APIMode: "responses", AgentVerified: true, Capabilities: PlatformModelCapabilities{Tools: true}}
	groups := &desktopGroupRepo{groups: []Group{{ID: 11, Status: StatusActive, Platform: PlatformOpenAI}}}
	settings.SetDefaultSubscriptionGroupReader(groups)
	defaultID := entry.ID
	want := DesktopSettings{Enabled: true, Models: []DesktopModelEntry{entry}, DefaultModelID: &defaultID, CredentialTTLSeconds: 900}

	err := settings.UpdateSettings(ctx, &SystemSettings{Desktop: &want})
	require.NoError(t, err)
	all, err := settings.GetAllSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, &want, all.Desktop)
	public, err := settings.GetPublicSettings(ctx)
	require.NoError(t, err)
	require.True(t, public.DesktopEnabled)
	blob, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(blob), "fixture-upstream", "public capability must not expose the private catalog")
	require.NoError(t, settings.UpdateSettings(ctx, &SystemSettings{SiteName: "renamed site"}))
	preserved, err := settings.GetDesktopSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, &want, preserved, "old clients omitting desktop settings must preserve them")
	groups.groups[0].ModelAllowlist = GroupModelAllowlist{Enabled: true, Models: []string{"different-model"}}
	require.Error(t, settings.UpdateSettings(ctx, &SystemSettings{Desktop: &want}), "a default outside the actual group allowlist must not be saved")
	groups.groups = nil
	require.Error(t, settings.UpdateSettings(ctx, &SystemSettings{Desktop: &want}), "deleted groups must not be accepted")
}

type desktopUserRepo struct {
	UserRepository
	user *User
}

func (r *desktopUserRepo) GetByID(context.Context, int64) (*User, error) { return r.user, nil }

type desktopGroupRepo struct {
	GroupRepository
	groups []Group
}

func (r *desktopGroupRepo) ListActive(context.Context) ([]Group, error) { return r.groups, nil }

func (r *desktopGroupRepo) GetByID(_ context.Context, id int64) (*Group, error) {
	for i := range r.groups {
		if r.groups[i].ID == id {
			return &r.groups[i], nil
		}
	}
	return nil, ErrGroupNotFound
}

type desktopSubscriptionRepo struct{ UserSubscriptionRepository }

func (r *desktopSubscriptionRepo) ListActiveByUserID(context.Context, int64) ([]UserSubscription, error) {
	return nil, nil
}

func TestDesktopCatalogNeverWidensUserEntitlements(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{}
	userRepo := &desktopUserRepo{user: &User{ID: 17, Status: StatusActive, Balance: 10, AllowedGroups: []int64{11}, RestrictPublicGroups: true}}
	inputPrice, outputPrice := 0.000002, 0.000006
	groups := &desktopGroupRepo{groups: []Group{
		{ID: 11, Status: StatusActive, Platform: PlatformOpenAI, RateMultiplier: 2, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"fixture-a"}}, ModelPricing: []ChannelModelPricing{{Models: []string{"fixture-a"}, InputPrice: &inputPrice, OutputPrice: &outputPrice}}},
		{ID: 22, Status: StatusActive, Platform: PlatformOpenAI, RateMultiplier: 1},
	}}
	entries := []DesktopModelEntry{
		{ID: "a", GroupID: 11, Model: "fixture-a", DisplayName: "Fixture A", ProviderLabel: "Fixture", Platform: PlatformOpenAI, APIMode: "chat_completions", AgentVerified: true, Capabilities: PlatformModelCapabilities{Tools: true}},
		{ID: "b", GroupID: 22, Model: "fixture-b", DisplayName: "Fixture B", ProviderLabel: "Fixture", Platform: PlatformOpenAI, APIMode: "chat_completions", AgentVerified: true, Capabilities: PlatformModelCapabilities{Tools: true}},
		{ID: "denied", GroupID: 11, Model: "fixture-denied", DisplayName: "Denied", ProviderLabel: "Fixture", Platform: PlatformOpenAI, APIMode: "chat_completions", AgentVerified: true, Capabilities: PlatformModelCapabilities{Tools: true}},
	}
	settings := NewSettingService(&desktopSettingsRepo{values: map[string]string{}}, cfg)
	updates, err := settings.buildDesktopSettingsUpdates(DesktopSettings{Enabled: true, Models: entries, CredentialTTLSeconds: 3600})
	require.NoError(t, err)
	settings.settingRepo = &desktopSettingsRepo{values: updates}
	keys := NewAPIKeyService(nil, userRepo, groups, &desktopSubscriptionRepo{}, nil, nil, cfg)
	billing := NewBillingService(cfg, nil)
	eligibility := NewBillingCacheService(nil, userRepo, &desktopSubscriptionRepo{}, nil, nil, nil, cfg, nil)
	t.Cleanup(eligibility.Stop)
	service := NewDesktopModelService(settings, keys, billing, NewModelPricingResolver(nil, billing), eligibility, nil)
	models, err := service.ListForUser(ctx, 17)
	require.NoError(t, err)
	require.NotEmpty(t, models)
	for _, model := range models {
		resolved, err := service.ResolveForUser(ctx, 17, model.ID)
		require.NoError(t, err)
		require.Contains(t, userRepo.user.AllowedGroups, resolved.GroupID)
		require.True(t, groups.groups[0].ModelAllowlist.Allows(resolved.Model.Model))
	}
	_, err = service.ResolveForUser(ctx, 17, "b")
	require.Error(t, err)
	_, err = service.ResolveForUser(ctx, 17, "denied")
	require.Error(t, err)
	require.Equal(t, "2", models[0].Pricing.EffectiveUserRate)
	require.Equal(t, "2", *models[0].Pricing.Input)
	blob, err := json.Marshal(models)
	require.NoError(t, err)
	require.NotContains(t, string(blob), "group_id")
	require.NotContains(t, string(blob), "api_key")
	userRepo.user.Balance = 0
	models, err = service.ListForUser(ctx, 17)
	require.NoError(t, err)
	require.Equal(t, "insufficient_balance", models[0].State)
	_, err = service.ResolveForUser(ctx, 17, "a")
	require.Error(t, err)
}

func TestDesktopCatalogCompositeUsesActualRoute(t *testing.T) {
	cfg := &config.Config{}
	userRepo := &desktopUserRepo{user: &User{ID: 17, Status: StatusActive, Balance: 10}}
	input, output := 2e-6, 6e-6
	groups := &desktopGroupRepo{groups: []Group{{ID: 11, Platform: PlatformComposite, Status: StatusActive, RateMultiplier: 1, ModelPricing: []ChannelModelPricing{{Models: []string{"alias"}, InputPrice: &input, OutputPrice: &output}}}}}
	entry := DesktopModelEntry{ID: "alias", Model: "alias", GroupID: 11, DisplayName: "Alias", ProviderLabel: "Fixture", Platform: PlatformOpenAI, APIMode: "chat_completions", AgentVerified: true, Capabilities: PlatformModelCapabilities{Tools: true}}
	settings := NewSettingService(&desktopSettingsRepo{values: map[string]string{}}, cfg)
	updates, err := settings.buildDesktopSettingsUpdates(DesktopSettings{Enabled: true, Models: []DesktopModelEntry{entry}, CredentialTTLSeconds: 3600})
	require.NoError(t, err)
	settings.settingRepo = &desktopSettingsRepo{values: updates}
	keys := NewAPIKeyService(nil, userRepo, groups, &desktopSubscriptionRepo{}, nil, nil, cfg)
	billing := NewBillingService(cfg, nil)
	eligibility := NewBillingCacheService(nil, userRepo, &desktopSubscriptionRepo{}, nil, nil, nil, cfg, nil)
	t.Cleanup(eligibility.Stop)
	catalog := NewDesktopModelService(settings, keys, billing, NewModelPricingResolver(nil, billing), eligibility, nil)
	models, err := catalog.ListForUser(context.Background(), 17)
	require.NoError(t, err)
	require.Equal(t, "unavailable", models[0].State, "unknown composite aliases cannot be advertised as callable")
	catalog.routes = NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{{GroupID: 11, PublicModel: "alias", MatchType: CompositeRouteMatchExact, Endpoint: CompositeRouteEndpointChatCompletions, TargetPlatform: PlatformOpenAI, UpstreamModel: "actual-model", Enabled: true}}})
	models, err = catalog.ListForUser(context.Background(), 17)
	require.NoError(t, err)
	require.Equal(t, "available", models[0].State)
}
