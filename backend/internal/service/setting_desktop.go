package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const DesktopAPIVersion = 1

var desktopSettingKeys = []string{"desktop.enabled", "desktop.models", "desktop.default_model_id", "desktop.credential_ttl_seconds"}
var desktopCatalogID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

type PlatformModelCapabilities struct {
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

// DesktopModelEntry selects an existing group; it never defines independent prices.
type DesktopModelEntry struct {
	ID              string                    `json:"id"`
	GroupID         int64                     `json:"group_id"`
	Model           string                    `json:"model"`
	DisplayName     string                    `json:"display_name"`
	ProviderLabel   string                    `json:"provider_label"`
	Platform        string                    `json:"platform"`
	APIMode         string                    `json:"api_mode"`
	SortOrder       int                       `json:"sort_order"`
	AgentVerified   bool                      `json:"agent_verified"`
	ContextWindow   *int                      `json:"context_window"`
	MaxOutputTokens *int                      `json:"max_output_tokens"`
	Capabilities    PlatformModelCapabilities `json:"capabilities"`
}

type DesktopSettings struct {
	Enabled              bool                `json:"enabled"`
	Models               []DesktopModelEntry `json:"models"`
	DefaultModelID       *string             `json:"default_model_id"`
	CredentialTTLSeconds int                 `json:"credential_ttl_seconds"`
}

func (s *SettingService) GetDesktopSettings(ctx context.Context) (*DesktopSettings, error) {
	values, err := s.settingRepo.GetMultiple(ctx, desktopSettingKeys)
	if err != nil {
		return nil, err
	}
	settings, err := desktopSettingsFromValues(values)
	if err != nil {
		return nil, err
	}
	return &settings, nil
}

func desktopSettingsFromValues(values map[string]string) (DesktopSettings, error) {
	settings := DesktopSettings{Models: []DesktopModelEntry{}, CredentialTTLSeconds: 3600}
	var err error
	if raw, ok := values["desktop.enabled"]; ok {
		settings.Enabled, err = strconv.ParseBool(raw)
		if err != nil {
			return settings, invalidDesktopSettings("invalid enabled flag")
		}
	}
	if raw, ok := values["desktop.models"]; ok {
		if err := json.Unmarshal([]byte(raw), &settings.Models); err != nil {
			return settings, invalidDesktopSettings("invalid model catalog")
		}
	}
	if raw := values["desktop.default_model_id"]; raw != "" {
		settings.DefaultModelID = &raw
	}
	if raw, ok := values["desktop.credential_ttl_seconds"]; ok {
		settings.CredentialTTLSeconds, err = strconv.Atoi(raw)
		if err != nil {
			return settings, invalidDesktopSettings("invalid credential lifetime")
		}
	}
	if settings.Models == nil {
		settings.Models = []DesktopModelEntry{}
	}
	if err := validateDesktopSettings(settings); err != nil {
		return settings, err
	}
	return settings, nil
}

func invalidDesktopSettings(message string) error {
	return infraerrors.BadRequest("INVALID_DESKTOP_SETTINGS", message)
}

func validateDesktopSettings(settings DesktopSettings) error {
	if settings.CredentialTTLSeconds < 300 || settings.CredentialTTLSeconds > 3600 {
		return invalidDesktopSettings("credential lifetime must be between 300 and 3600 seconds")
	}
	if len(settings.Models) > 200 {
		return invalidDesktopSettings("model catalog exceeds 200 entries")
	}
	seen := make(map[string]bool)
	for _, entry := range settings.Models {
		if !desktopCatalogID.MatchString(entry.ID) || seen[entry.ID] || entry.GroupID <= 0 {
			return invalidDesktopSettings("model IDs must be unique and select an existing group")
		}
		seen[entry.ID] = true
		for _, value := range []string{entry.Model, entry.DisplayName, entry.ProviderLabel} {
			if strings.TrimSpace(value) == "" || len(value) > 256 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
				return invalidDesktopSettings("model names and labels must be nonempty text")
			}
		}
		if !map[string]bool{"chat_completions": true, "responses": true, "anthropic_messages": true}[entry.APIMode] {
			return invalidDesktopSettings("unsupported model protocol")
		}
		if !isConcreteRequestPlatform(entry.Platform) {
			return invalidDesktopSettings("select a concrete model platform")
		}
		if entry.ContextWindow != nil && *entry.ContextWindow <= 0 || entry.MaxOutputTokens != nil && *entry.MaxOutputTokens <= 0 {
			return invalidDesktopSettings("model limits must be positive or unknown")
		}
		if entry.ContextWindow != nil && entry.MaxOutputTokens != nil && *entry.MaxOutputTokens > *entry.ContextWindow {
			return invalidDesktopSettings("output limit exceeds context window")
		}
		if settings.DefaultModelID != nil && *settings.DefaultModelID == entry.ID && (!entry.AgentVerified || !entry.Capabilities.Tools) {
			return invalidDesktopSettings("default model must have verified Agent tool and streaming support")
		}
	}
	if settings.DefaultModelID != nil && !seen[*settings.DefaultModelID] {
		return invalidDesktopSettings("default model is not in the catalog")
	}
	return nil
}

// Use the same repository reader as subscription settings. Reading a catalog
// never grants access to a group or changes its allowlist.
func (s *SettingService) validateDesktopGroupReferences(ctx context.Context, settings DesktopSettings) error {
	for _, entry := range settings.Models {
		if s.defaultSubGroupReader == nil {
			return invalidDesktopSettings("group validation is unavailable")
		}
		group, err := s.defaultSubGroupReader.GetByID(ctx, entry.GroupID)
		if errors.Is(err, ErrGroupNotFound) {
			return invalidDesktopSettings("catalog group no longer exists")
		}
		if err != nil {
			return err
		}
		if group.Platform != PlatformComposite && group.Platform != entry.Platform {
			return invalidDesktopSettings("catalog platform does not match its group")
		}
		if !group.ModelAllowlist.Allows(entry.Model) {
			return invalidDesktopSettings("catalog model is not allowed by its group")
		}
		if settings.DefaultModelID != nil && *settings.DefaultModelID == entry.ID && !group.IsActive() {
			return invalidDesktopSettings("default model must belong to an active group")
		}
	}
	return nil
}

func (s *SettingService) buildDesktopSettingsUpdates(settings DesktopSettings) (map[string]string, error) {
	if settings.CredentialTTLSeconds == 0 {
		settings.CredentialTTLSeconds = 3600
	}
	if err := validateDesktopSettings(settings); err != nil {
		return nil, err
	}
	if settings.Models == nil {
		settings.Models = []DesktopModelEntry{}
	}
	models, err := json.Marshal(settings.Models)
	if err != nil {
		return nil, err
	}
	defaultID := ""
	if settings.DefaultModelID != nil {
		defaultID = *settings.DefaultModelID
	}
	return map[string]string{
		"desktop.enabled":                strconv.FormatBool(settings.Enabled),
		"desktop.models":                 string(models),
		"desktop.default_model_id":       defaultID,
		"desktop.credential_ttl_seconds": strconv.Itoa(settings.CredentialTTLSeconds),
	}, nil
}
