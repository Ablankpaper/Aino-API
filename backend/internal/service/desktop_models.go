package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
)

type DesktopBootstrap struct {
	APIVersion     int     `json:"api_version"`
	DefaultModelID *string `json:"default_model_id"`
	ModelsEnabled  bool    `json:"models_enabled"`
	WalletCurrency string  `json:"wallet_currency"`
}

type PlatformModelPricing struct {
	Currency          string                `json:"currency"`
	Unit              string                `json:"unit"`
	Input             *string               `json:"input"`
	Output            *string               `json:"output"`
	CacheRead         *string               `json:"cache_read"`
	CacheWrite        *string               `json:"cache_write"`
	EffectiveUserRate string                `json:"effective_user_rate"`
	DetailAvailable   bool                  `json:"detail_available"`
	Tiers             []PlatformPricingTier `json:"tiers"`
	TimePricing       *PlatformTimePricing  `json:"time_pricing"`
	GroupPeak         *PlatformPeakPricing  `json:"group_peak"`
}

type PlatformPricingTier struct {
	MinTokens    int     `json:"min_tokens"`
	MaxTokens    *int    `json:"max_tokens"`
	Label        string  `json:"label"`
	Input        *string `json:"input"`
	Output       *string `json:"output"`
	CacheRead    *string `json:"cache_read"`
	CacheWrite   *string `json:"cache_write"`
	CacheWrite1h *string `json:"cache_write_1h"`
}

type PlatformTimePricing struct {
	Timezone     string                `json:"timezone"`
	WeekdaysOnly bool                  `json:"weekdays_only"`
	Periods      []PlatformPricePeriod `json:"periods"`
}

type PlatformPricePeriod struct {
	StartTime  string `json:"start_time"`
	EndTime    string `json:"end_time"`
	Multiplier string `json:"multiplier"`
}

type PlatformPeakPricing struct {
	Start      string `json:"start"`
	End        string `json:"end"`
	Multiplier string `json:"multiplier"`
}

type PlatformModel struct {
	ID              string                    `json:"id"`
	Model           string                    `json:"model"`
	DisplayName     string                    `json:"display_name"`
	ProviderLabel   string                    `json:"provider_label"`
	APIMode         string                    `json:"api_mode"`
	State           string                    `json:"state"`
	ReasonCode      *string                   `json:"reason_code"`
	IsDefault       bool                      `json:"is_default"`
	ContextWindow   *int                      `json:"context_window"`
	MaxOutputTokens *int                      `json:"max_output_tokens"`
	Capabilities    PlatformModelCapabilities `json:"capabilities"`
	BillingSource   string                    `json:"billing_source"`
	Pricing         PlatformModelPricing      `json:"pricing"`
}

// ResolvedPlatformModel stays server-side. A catalog entry cannot grant a group.
type ResolvedPlatformModel struct {
	GroupID  int64
	Platform string
	Model    PlatformModel
}

type DesktopModelService struct {
	settings    *SettingService
	keys        *APIKeyService
	billing     *BillingService
	pricing     *ModelPricingResolver
	eligibility *BillingCacheService
	routes      *CompositeRouteResolver
}

func NewDesktopModelService(settings *SettingService, keys *APIKeyService, billing *BillingService, pricing *ModelPricingResolver, eligibility *BillingCacheService, routes *CompositeRouteResolver) *DesktopModelService {
	return &DesktopModelService{settings: settings, keys: keys, billing: billing, pricing: pricing, eligibility: eligibility, routes: routes}
}

func (s *DesktopModelService) Bootstrap(ctx context.Context, userID int64) (*DesktopBootstrap, error) {
	settings, err := s.settings.GetDesktopSettings(ctx)
	if err != nil {
		return nil, err
	}
	models, err := s.ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	result := &DesktopBootstrap{APIVersion: DesktopAPIVersion, ModelsEnabled: settings.Enabled, WalletCurrency: "USD"}
	for _, model := range models {
		if model.IsDefault {
			id := model.ID
			result.DefaultModelID = &id
			break
		}
	}
	return result, nil
}

func (s *DesktopModelService) ListForUser(ctx context.Context, userID int64) ([]PlatformModel, error) {
	entries, err := s.resolveCatalog(ctx, userID)
	if err != nil {
		return nil, err
	}
	models := make([]PlatformModel, 0, len(entries))
	for _, entry := range entries {
		models = append(models, entry.Model)
	}
	return models, nil
}

func (s *DesktopModelService) ResolveForUser(ctx context.Context, userID int64, modelID string) (*ResolvedPlatformModel, error) {
	entries, err := s.resolveCatalog(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Model.ID != modelID {
			continue
		}
		if entry.Model.State != "available" {
			return nil, infraerrors.Forbidden(*entry.Model.ReasonCode, "selected platform model is unavailable")
		}
		return &entry, nil
	}
	return nil, infraerrors.Forbidden("DESKTOP_MODEL_NOT_ALLOWED", "selected platform model is not available to this account")
}

func (s *DesktopModelService) resolveCatalog(ctx context.Context, userID int64) ([]ResolvedPlatformModel, error) {
	settings, err := s.settings.GetDesktopSettings(ctx)
	if err != nil {
		return nil, err
	}
	result := []ResolvedPlatformModel{}
	if !settings.Enabled {
		return result, nil
	}
	user, err := s.keys.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !user.IsActive() {
		return nil, ErrUserNotActive
	}
	groups, err := s.keys.GetAvailableGroups(ctx, userID)
	if err != nil {
		return nil, err
	}
	rates, err := s.keys.GetUserGroupRates(ctx, userID)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*Group, len(groups))
	for i := range groups {
		byID[groups[i].ID] = &groups[i]
	}
	sort.SliceStable(settings.Models, func(i, j int) bool { return settings.Models[i].SortOrder < settings.Models[j].SortOrder })
	for _, entry := range settings.Models {
		group := byID[entry.GroupID]
		if group == nil || !group.ModelAllowlist.Allows(entry.Model) {
			continue
		}
		model := PlatformModel{ID: entry.ID, Model: entry.Model, DisplayName: entry.DisplayName, ProviderLabel: entry.ProviderLabel, APIMode: entry.APIMode, State: "available", ContextWindow: entry.ContextWindow, MaxOutputTokens: entry.MaxOutputTokens, Capabilities: entry.Capabilities, BillingSource: "balance"}
		if group.IsSubscriptionType() {
			model.BillingSource = "subscription"
		}
		routeMatches := group.Platform == entry.Platform
		if group.Platform == PlatformComposite {
			endpoint := map[string]string{"chat_completions": CompositeRouteEndpointChatCompletions, "responses": CompositeRouteEndpointResponses, "anthropic_messages": CompositeRouteEndpointMessages}[entry.APIMode]
			decision, routeErr := s.routes.Resolve(ctx, group.ID, entry.Model, endpoint)
			routeMatches = routeErr == nil && decision.Matched && decision.TargetPlatform == entry.Platform
		}
		model.Pricing = s.modelPricing(ctx, entry, group, rates)
		switch {
		case !routeMatches:
			setPlatformModelState(&model, "unavailable", "DESKTOP_MODEL_PLATFORM_MISMATCH")
		case !entry.AgentVerified || !entry.Capabilities.Tools:
			setPlatformModelState(&model, "unavailable", "DESKTOP_MODEL_NOT_VERIFIED")
		case !model.Pricing.DetailAvailable:
			setPlatformModelState(&model, "unavailable", "DESKTOP_MODEL_PRICING_UNAVAILABLE")
		default:
			s.applyEligibility(ctx, user, group, entry.Platform, &model)
		}
		model.IsDefault = model.State == "available" && settings.DefaultModelID != nil && *settings.DefaultModelID == entry.ID
		result = append(result, ResolvedPlatformModel{GroupID: group.ID, Platform: entry.Platform, Model: model})
	}
	return result, nil
}

func setPlatformModelState(model *PlatformModel, state, reason string) {
	model.State, model.ReasonCode = state, &reason
}

func (s *DesktopModelService) applyEligibility(ctx context.Context, user *User, group *Group, platform string, model *PlatformModel) {
	if s.eligibility.cfg.RunMode == config.RunModeSimple {
		return
	}
	var err error
	if group.IsSubscriptionType() {
		err = s.eligibility.checkSubscriptionEligibility(ctx, user.ID, group, nil)
	} else {
		err = s.eligibility.checkBalanceEligibility(ctx, user.ID)
		if err == nil {
			err = s.eligibility.checkUserPlatformQuotaEligibility(ctx, user.ID, platform)
		}
	}
	if err == nil {
		return
	}
	if errors.Is(err, ErrInsufficientBalance) {
		setPlatformModelState(model, "insufficient_balance", "INSUFFICIENT_BALANCE")
		return
	}
	quotaErrors := []error{ErrDailyLimitExceeded, ErrWeeklyLimitExceeded, ErrMonthlyLimitExceeded, ErrUserPlatformDailyQuotaExhausted, ErrUserPlatformWeeklyQuotaExhausted, ErrUserPlatformMonthlyQuotaExhausted}
	for _, quotaErr := range quotaErrors {
		if errors.Is(err, quotaErr) {
			setPlatformModelState(model, "quota_exhausted", "DESKTOP_QUOTA_EXHAUSTED")
			return
		}
	}
	setPlatformModelState(model, "unavailable", "DESKTOP_BILLING_UNAVAILABLE")
}

func decimalPrice(value *float64) *string {
	if value == nil {
		return nil
	}
	text := decimal.NewFromFloat(*value).Mul(decimal.NewFromInt(1000000)).String()
	return &text
}

func (s *DesktopModelService) modelPricing(ctx context.Context, entry DesktopModelEntry, group *Group, rates map[int64]float64) PlatformModelPricing {
	rate := group.RateMultiplier
	if override, ok := rates[group.ID]; ok {
		rate = override
	}
	result := PlatformModelPricing{Currency: "USD", Unit: "per_million_tokens", EffectiveUserRate: decimal.NewFromFloat(rate * group.PeakMultiplierAt(time.Now())).String(), Tiers: []PlatformPricingTier{}}
	schedule, err := s.billing.ResolveContextPricingSchedule(ctx, s.pricing, ContextPricingScheduleInput{Model: entry.Model, Group: group, Platform: entry.Platform})
	if err != nil || schedule == nil || len(schedule.Tiers) == 0 {
		return result
	}
	for _, tier := range schedule.Tiers {
		result.Tiers = append(result.Tiers, PlatformPricingTier{MinTokens: tier.MinTokens, MaxTokens: tier.MaxTokens, Label: tier.Label, Input: decimalPrice(tier.Input), Output: decimalPrice(tier.Output), CacheRead: decimalPrice(tier.CacheRead), CacheWrite: decimalPrice(tier.CacheWrite), CacheWrite1h: decimalPrice(tier.CacheWrite1h)})
	}
	first := result.Tiers[0]
	result.Input, result.Output, result.CacheRead, result.CacheWrite = first.Input, first.Output, first.CacheRead, first.CacheWrite
	result.DetailAvailable = true
	if schedule.TimePricing != nil {
		result.TimePricing = &PlatformTimePricing{Timezone: schedule.TimePricing.Timezone, WeekdaysOnly: schedule.TimePricing.WeekdaysOnly, Periods: []PlatformPricePeriod{}}
		for _, period := range schedule.TimePricing.Periods {
			result.TimePricing.Periods = append(result.TimePricing.Periods, PlatformPricePeriod{StartTime: period.StartTime, EndTime: period.EndTime, Multiplier: decimal.NewFromFloat(period.Multiplier).String()})
		}
	}
	if group.PeakRateEnabled {
		result.GroupPeak = &PlatformPeakPricing{Start: group.PeakStart, End: group.PeakEnd, Multiplier: decimal.NewFromFloat(group.PeakRateMultiplier).String()}
	}
	return result
}
