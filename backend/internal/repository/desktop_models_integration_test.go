//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type desktopModelRig struct {
	keys           *service.APIKeyService
	desktopHandler *handler.DesktopHandler
	*phoneAuthFlowRig
	models  *service.DesktopModelService
	groups  service.GroupRepository
	subs    service.UserSubscriptionRepository
	rates   service.UserGroupRateRepository
	billing *service.BillingService
	pricing *service.ModelPricingResolver
}

func newDesktopModelRig(t *testing.T) *desktopModelRig {
	t.Helper()
	r := newPhoneAuthFlowRig(t)
	db := repository.GetIntegrationDB()
	groups := repository.NewGroupRepository(r.client, db)
	subs := repository.NewUserSubscriptionRepository(r.client)
	rates := repository.NewUserGroupRateRepository(db)
	cfg := &config.Config{APIKeyAuth: config.APIKeyAuthCacheConfig{L2TTLSeconds: 60}}
	keys := service.NewAPIKeyService(repository.NewAPIKeyRepository(r.client, db), r.userRepo, groups, subs, rates, repository.NewAPIKeyCache(r.redis), cfg)
	billing := service.NewBillingService(cfg, nil)
	channels := service.NewChannelService(repository.NewChannelRepository(db), groups, nil, nil, nil)
	pricing := service.NewModelPricingResolver(channels, billing)
	eligibility := service.NewBillingCacheService(nil, r.userRepo, subs, nil, nil, rates, cfg, nil)
	t.Cleanup(eligibility.Stop)
	r.settings.SetDefaultSubscriptionGroupReader(groups)
	models := service.NewDesktopModelService(r.settings, keys, billing, pricing, eligibility, service.NewCompositeRouteResolver(repository.NewCompositeModelRouteRepository(r.client)))
	desktopHandler := handler.NewDesktopHandler(models)
	routes.RegisterDesktopRoutes(r.router.Group("/api/v1"), &handler.Handlers{Desktop: desktopHandler}, middleware.NewJWTAuthMiddleware(r.auth, r.users, r.settings, nil), r.settings, middleware.NewPanelRateLimiter(r.redis, r.settings))
	for _, key := range []string{"desktop.enabled", "desktop.models", "desktop.default_model_id", "desktop.credential_ttl_seconds"} {
		before, err := r.settingRepo.GetValue(r.ctx, key)
		t.Cleanup(func() {
			if err != nil {
				_ = r.settingRepo.Delete(r.ctx, key)
			} else {
				_ = r.settingRepo.Set(r.ctx, key, before)
			}
		})
	}
	return &desktopModelRig{phoneAuthFlowRig: r, models: models, groups: groups, subs: subs, rates: rates, billing: billing, pricing: pricing, keys: keys, desktopHandler: desktopHandler}
}

func (r *desktopModelRig) group(t *testing.T, subscription bool) *service.Group {
	t.Helper()
	g := &service.Group{Name: fmt.Sprintf("desktop-fixture-%d", time.Now().UnixNano()), Platform: service.PlatformOpenAI, Status: service.StatusActive, IsExclusive: true, RateMultiplier: 2, SubscriptionType: service.SubscriptionTypeStandard, LongContextPricingEnabled: true}
	if subscription {
		g.SubscriptionType = service.SubscriptionTypeSubscription
	}
	require.NoError(t, r.groups.Create(r.ctx, g))
	t.Cleanup(func() { _ = r.groups.Delete(r.ctx, g.ID) })
	return g
}

func (r *desktopModelRig) catalog(t *testing.T, entries []service.DesktopModelEntry, defaultID string) {
	t.Helper()
	blob, err := json.Marshal(entries)
	require.NoError(t, err)
	require.NoError(t, r.settingRepo.SetMultiple(r.ctx, map[string]string{"desktop.enabled": "true", "desktop.models": string(blob), "desktop.default_model_id": defaultID, "desktop.credential_ttl_seconds": "3600"}))
}

func desktopEntry(id string, groupID int64) service.DesktopModelEntry {
	return service.DesktopModelEntry{ID: id, GroupID: groupID, Model: id, DisplayName: id, ProviderLabel: "Fixture", Platform: service.PlatformOpenAI, APIMode: "chat_completions", AgentVerified: true, Capabilities: service.PlatformModelCapabilities{Tools: true}}
}

func TestDesktopCatalogRealPermissionsAndSubscription(t *testing.T) {
	r := newDesktopModelRig(t)
	u := r.user(t, service.StatusActive, false)
	allowed, forbidden, subscription := r.group(t, false), r.group(t, false), r.group(t, true)
	entry, hidden, subscribed := desktopEntry("fixture-a", allowed.ID), desktopEntry("fixture-b", forbidden.ID), desktopEntry("fixture-sub", subscription.ID)
	input, output := 2e-6, 6e-6
	for _, g := range []*service.Group{allowed, forbidden, subscription} {
		g.ModelPricing = []service.ChannelModelPricing{{Models: []string{"fixture-*"}, InputPrice: &input, OutputPrice: &output}}
		require.NoError(t, r.groups.Update(r.ctx, g))
	}
	allowed.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{entry.Model}}
	require.NoError(t, r.groups.Update(r.ctx, allowed))
	require.NoError(t, r.userRepo.AddGroupToAllowedGroups(r.ctx, u.ID, allowed.ID))
	sub := &service.UserSubscription{UserID: u.ID, GroupID: subscription.ID, StartsAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour), Status: service.SubscriptionStatusActive}
	require.NoError(t, r.subs.Create(r.ctx, sub))
	r.catalog(t, []service.DesktopModelEntry{entry, hidden, subscribed, desktopEntry("fixture-denied", allowed.ID)}, entry.ID)
	token, err := r.auth.GenerateToken(r.ctx, u)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, r.request(http.MethodGet, "/api/v1/desktop/models", "", "").Code)
	w := r.request(http.MethodGet, "/api/v1/desktop/models", "", token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var payload struct {
		Data []service.PlatformModel `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.NotEmpty(t, payload.Data)
	seen := map[string]bool{}
	for _, model := range payload.Data {
		seen[model.ID] = true
		resolved, err := r.models.ResolveForUser(r.ctx, u.ID, model.ID)
		require.NoError(t, err)
		require.Contains(t, []int64{allowed.ID, subscription.ID}, resolved.GroupID)
		if model.ID == subscribed.ID {
			require.Equal(t, "subscription", model.BillingSource)
		}
	}
	require.True(t, seen[entry.ID])
	require.True(t, seen[subscribed.ID])
	require.False(t, seen[hidden.ID])
	require.False(t, seen["fixture-denied"])
	require.NotContains(t, w.Body.String(), "group_id")
	require.NotContains(t, w.Body.String(), "api_key")
	_, err = r.models.ResolveForUser(r.ctx, u.ID, hidden.ID)
	require.Error(t, err)
	before, err := r.client.APIKey.Query().Count(r.ctx)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, r.request(http.MethodGet, "/api/v1/desktop/bootstrap", "", token).Code)
	after, err := r.client.APIKey.Query().Count(r.ctx)
	require.NoError(t, err)
	require.Equal(t, before, after, "bootstrap must never issue credentials")
	require.NoError(t, r.userRepo.RemoveGroupFromUserAllowedGroups(r.ctx, u.ID, allowed.ID))
	_, err = r.models.ResolveForUser(r.ctx, u.ID, entry.ID)
	require.Error(t, err, "revoking a group must remove it from the catalog")
}

func TestDesktopCatalogRealChannelPricingMatchesBilling(t *testing.T) {
	r := newDesktopModelRig(t)
	u := r.user(t, service.StatusActive, false)
	g := r.group(t, false)
	require.NoError(t, r.userRepo.AddGroupToAllowedGroups(r.ctx, u.ID, g.ID))
	rate := 0.75
	require.NoError(t, r.rates.SyncUserGroupRates(r.ctx, u.ID, map[int64]*float64{g.ID: &rate}))
	entry := desktopEntry("fixture-tiered", g.ID)
	input, output, higher, boundary := 2e-6, 6e-6, 4e-6, 100000
	channel := &service.Channel{Name: fmt.Sprintf("desktop-channel-%d", time.Now().UnixNano()), Status: service.StatusActive, BillingModelSource: service.BillingModelSourceRequested, GroupIDs: []int64{g.ID}, ModelPricing: []service.ChannelModelPricing{{Platform: service.PlatformOpenAI, Models: []string{entry.Model}, BillingMode: service.BillingModeToken, InputPrice: &input, OutputPrice: &output, Intervals: []service.PricingInterval{{MinTokens: 0, MaxTokens: &boundary, InputPrice: &input, OutputPrice: &output}, {MinTokens: boundary, InputPrice: &higher, OutputPrice: &output}}, TimePricing: &service.ChannelTimePricing{Timezone: "UTC", Periods: []service.ChannelTimePricingPeriod{{StartTime: "18:00", EndTime: "22:00", Multiplier: 1.5}}}}}}
	channels := repository.NewChannelRepository(repository.GetIntegrationDB())
	require.NoError(t, channels.Create(r.ctx, channel))
	t.Cleanup(func() { _ = channels.Delete(r.ctx, channel.ID) })
	r.catalog(t, []service.DesktopModelEntry{entry}, entry.ID)
	models, err := r.models.ListForUser(r.ctx, u.ID)
	require.NoError(t, err)
	require.NotEmpty(t, models)
	model := models[0]
	require.Equal(t, "available", model.State)
	require.True(t, model.IsDefault)
	require.Equal(t, "0.75", model.Pricing.EffectiveUserRate)
	require.NotNil(t, model.Pricing.TimePricing)
	require.Greater(t, len(model.Pricing.Tiers), 1)
	for _, tier := range model.Pricing.Tiers {
		tokens := tier.MinTokens + 1000
		at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
		cost, err := r.billing.CalculateTokenCostForRequest(service.TokenCostRequest{Ctx: r.ctx, Model: entry.Model, Group: g, Tokens: service.UsageTokens{InputTokens: tokens}, RateMultiplier: rate, PricingAt: at, Resolver: r.pricing})
		require.NoError(t, err)
		var perMillion float64
		_, err = fmt.Sscan(*tier.Input, &perMillion)
		require.NoError(t, err)
		require.InDelta(t, perMillion/1e6*float64(tokens)*rate, cost.ActualCost, 1e-8)
	}
	_, err = r.userRepo.SetBalance(r.ctx, u.ID, 0)
	require.NoError(t, err)
	models, err = r.models.ListForUser(r.ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, "insufficient_balance", models[0].State)
	require.False(t, models[0].IsDefault)
}
