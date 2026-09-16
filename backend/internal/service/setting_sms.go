package service

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// SMSEditableSettings contains policy only. Secrets are never read from settings.
type SMSEditableSettings struct {
	Enabled               bool              `json:"enabled"`
	Provider              string            `json:"provider"`
	RegionID              string            `json:"region_id"`
	RequestTimeoutSeconds int               `json:"request_timeout_seconds"`
	SignName              string            `json:"sign_name"`
	TemplateCode          string            `json:"template_code"`
	TemplateParams        map[string]string `json:"template_params"`
	TemplateVerified      bool              `json:"template_verified"`
	CodeLength            int               `json:"code_length"`
	TTLSeconds            int               `json:"ttl_seconds"`
	CooldownSeconds       int               `json:"cooldown_seconds"`
	MaxAttempts           int               `json:"max_attempts"`
	PhoneHourLimit        int               `json:"phone_hour_limit"`
	PhoneDayLimit         int               `json:"phone_day_limit"`
	IPHourLimit           int               `json:"ip_hour_limit"`
	GlobalDayLimit        int               `json:"global_day_limit"`
}

type SMSSettings struct {
	SMSEditableSettings
	CredentialsConfigured bool   `json:"credentials_configured"`
	HMACConfigured        bool   `json:"hmac_configured"`
	Ready                 bool   `json:"ready"`
	ReasonCode            string `json:"reason_code,omitempty"`
}

var smsSettingKeys = func() []string {
	t := reflect.TypeOf(SMSEditableSettings{})
	keys := make([]string, t.NumField())
	for i := range keys {
		keys[i] = "sms." + t.Field(i).Tag.Get("json")
	}
	return keys
}()

func (s *SettingService) deploymentSMS() config.SMSConfig {
	c := config.SMSConfig{}
	if s != nil && s.cfg != nil {
		c = s.cfg.SMS
	}
	if c.Provider == "" {
		c.Provider = "aliyun"
	}
	if c.RegionID == "" {
		c.RegionID = "cn-hangzhou"
	}
	if c.RequestTimeoutSeconds == 0 {
		c.RequestTimeoutSeconds = 5
	}
	if c.CodeLength == 0 {
		c.CodeLength = 6
	}
	if c.TTLSeconds == 0 {
		c.TTLSeconds = 300
	}
	if c.CooldownSeconds == 0 {
		c.CooldownSeconds = 60
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 5
	}
	if c.PhoneHourLimit == 0 {
		c.PhoneHourLimit = 5
	}
	if c.PhoneDayLimit == 0 {
		c.PhoneDayLimit = 10
	}
	if c.IPHourLimit == 0 {
		c.IPHourLimit = 30
	}
	if c.GlobalDayLimit == 0 {
		c.GlobalDayLimit = 1000
	}
	if len(c.TemplateParams) == 0 {
		c.TemplateParams = map[string]string{"code": "code", "ttl": "ttl_minutes"}
	}
	return c
}

func smsEditable(c config.SMSConfig) SMSEditableSettings {
	return SMSEditableSettings{Enabled: c.Enabled, Provider: c.Provider, RegionID: c.RegionID, RequestTimeoutSeconds: c.RequestTimeoutSeconds,
		SignName: c.SignName, TemplateCode: c.TemplateCode,
		TemplateParams: c.TemplateParams, TemplateVerified: c.TemplateVerified, CodeLength: c.CodeLength, TTLSeconds: c.TTLSeconds,
		CooldownSeconds: c.CooldownSeconds, MaxAttempts: c.MaxAttempts, PhoneHourLimit: c.PhoneHourLimit, PhoneDayLimit: c.PhoneDayLimit,
		IPHourLimit: c.IPHourLimit, GlobalDayLimit: c.GlobalDayLimit}
}

func (e SMSEditableSettings) apply(c config.SMSConfig) config.SMSConfig {
	c.Enabled, c.Provider, c.RegionID, c.RequestTimeoutSeconds = e.Enabled, e.Provider, e.RegionID, e.RequestTimeoutSeconds
	c.SignName, c.TemplateCode = e.SignName, e.TemplateCode
	c.TemplateParams, c.TemplateVerified = e.TemplateParams, e.TemplateVerified
	c.CodeLength, c.TTLSeconds, c.CooldownSeconds, c.MaxAttempts = e.CodeLength, e.TTLSeconds, e.CooldownSeconds, e.MaxAttempts
	c.PhoneHourLimit, c.PhoneDayLimit, c.IPHourLimit, c.GlobalDayLimit = e.PhoneHourLimit, e.PhoneDayLimit, e.IPHourLimit, e.GlobalDayLimit
	return c
}

func (s *SettingService) smsSettingsFromValues(values map[string]string) (*SMSSettings, error) {
	c := s.deploymentSMS()
	e := smsEditable(c)
	fields := reflect.TypeOf(e)
	stored := map[string]json.RawMessage{}
	for i, key := range smsSettingKeys {
		if raw, ok := values[key]; ok {
			if fields.Field(i).Type.Kind() == reflect.String {
				stored[fields.Field(i).Tag.Get("json")], _ = json.Marshal(raw)
			} else {
				stored[fields.Field(i).Tag.Get("json")] = json.RawMessage(raw)
			}
		}
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, ErrSMSNotConfigured
	}
	if err := json.Unmarshal(encoded, &e); err != nil {
		return nil, ErrSMSNotConfigured
	}
	c = e.apply(c)
	result := &SMSSettings{
		SMSEditableSettings:   e,
		CredentialsConfigured: strings.TrimSpace(c.AccessKeyID) != "" && strings.TrimSpace(c.AccessKeySecret) != "",
		HMACConfigured:        len(strings.TrimSpace(c.HMACSecret)) >= 32,
	}
	if !e.Enabled {
		result.ReasonCode = "SMS_DISABLED"
	} else if c.Validate("release") != nil {
		result.ReasonCode = "SMS_NOT_CONFIGURED"
	} else {
		result.Ready = true
	}
	return result, nil
}

func (s *SettingService) GetSMSSettings(ctx context.Context) (*SMSSettings, error) {
	values, err := s.settingRepo.GetMultiple(ctx, smsSettingKeys)
	if err != nil {
		return nil, ErrSMSUnavailable.WithCause(err)
	}
	return s.smsSettingsFromValues(values)
}

func (s *SettingService) buildSMSSettingsUpdates(edit SMSEditableSettings) (map[string]string, error) {
	c := edit.apply(s.deploymentSMS())
	if err := c.ValidatePolicy(); err != nil {
		return nil, infraerrors.BadRequest("INVALID_SMS_SETTINGS", err.Error())
	}
	if err := c.Validate("release"); err != nil {
		return nil, infraerrors.BadRequest("INVALID_SMS_SETTINGS", err.Error())
	}
	encoded, err := json.Marshal(edit)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	updates := make(map[string]string, len(fields))
	for name, raw := range fields {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			updates["sms."+name] = str
		} else {
			updates["sms."+name] = string(raw)
		}
	}
	return updates, nil
}

func (s *SettingService) smsRuntimeConfig(ctx context.Context) (SMSConfig, error) {
	settings, err := s.GetSMSSettings(ctx)
	if err != nil {
		return SMSConfig{}, err
	}
	if !settings.Enabled {
		return SMSConfig{}, ErrSMSDisabled
	}
	if !settings.Ready {
		return SMSConfig{}, ErrSMSNotConfigured
	}
	c := settings.apply(s.deploymentSMS())
	return SMSConfig{Enabled: c.Enabled, Provider: c.Provider, RegionID: c.RegionID, SignName: c.SignName, TemplateCode: c.TemplateCode, TemplateParams: c.TemplateParams,
		HMACSecret: c.HMACSecret, RequestTimeoutSeconds: c.RequestTimeoutSeconds, CodeLength: c.CodeLength, TTLSeconds: c.TTLSeconds,
		CooldownSeconds: c.CooldownSeconds, MaxAttempts: c.MaxAttempts, PhoneHourLimit: c.PhoneHourLimit, PhoneDayLimit: c.PhoneDayLimit,
		IPHourLimit: c.IPHourLimit, GlobalDayLimit: c.GlobalDayLimit, RolloutPhoneAllowlist: append([]string(nil), c.RolloutPhoneAllowlist...)}, nil
}

func (s *SMSService) configuredForRequest(ctx context.Context) (*SMSService, error) {
	if s.settings == nil {
		return s, nil
	}
	cfg, err := s.settings.smsRuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	copy := *s
	copy.config, copy.settings = cfg, nil
	return &copy, nil
}
