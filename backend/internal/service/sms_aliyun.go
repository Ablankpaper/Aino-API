package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aliyun/alibaba-cloud-sdk-go/sdk"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/auth/credentials"
	"github.com/aliyun/alibaba-cloud-sdk-go/services/dysmsapi"
)

const defaultAliyunSMSRequestTimeout = 5 * time.Second

type aliyunSMSClient interface {
	SendSms(request *dysmsapi.SendSmsRequest) (*dysmsapi.SendSmsResponse, error)
}

// AliyunSMSSender implements SMSSender using Aliyun Dysmsapi
type AliyunSMSSender struct {
	client aliyunSMSClient
}

// NewAliyunSMSSender creates a new Aliyun SMS sender
func NewAliyunSMSSender(accessKeyID, accessKeySecret, regionID string) (*AliyunSMSSender, error) {
	return NewAliyunSMSSenderWithOptions(accessKeyID, accessKeySecret, regionID, defaultAliyunSMSRequestTimeout)
}

// NewAliyunSMSSenderWithOptions creates a sender with automatic retries
// disabled.  A timeout is enforced by the SDK HTTP client so an ambiguous
// network failure cannot silently submit a second SMS.
func NewAliyunSMSSenderWithOptions(accessKeyID, accessKeySecret, regionID string, timeout time.Duration) (*AliyunSMSSender, error) {
	accessKeyID = strings.TrimSpace(accessKeyID)
	accessKeySecret = strings.TrimSpace(accessKeySecret)
	regionID = strings.TrimSpace(regionID)
	if accessKeyID == "" || accessKeySecret == "" || regionID == "" {
		return nil, fmt.Errorf("Aliyun SMS credentials and region are required")
	}
	if timeout <= 0 {
		timeout = defaultAliyunSMSRequestTimeout
	}
	sdkConfig := sdk.NewConfig().
		WithAutoRetry(false).
		WithMaxRetryTime(0).
		WithScheme("HTTPS").
		WithTimeout(timeout)
	client, err := dysmsapi.NewClientWithOptions(
		regionID,
		sdkConfig,
		credentials.NewAccessKeyCredential(accessKeyID, accessKeySecret),
	)
	if err != nil {
		return nil, fmt.Errorf("create Aliyun client: %w", err)
	}

	return &AliyunSMSSender{
		client: client,
	}, nil
}

// Send sends an SMS message via Aliyun
func (s *AliyunSMSSender) Send(ctx context.Context, message SMSMessage) (SMSSendResult, error) {
	if s == nil || s.client == nil {
		return SMSSendResult{}, fmt.Errorf("Aliyun SMS client is not configured")
	}
	if err := ctx.Err(); err != nil {
		return SMSSendResult{}, err
	}
	normalizedPhone, err := NormalizeCNPhone(message.Phone)
	if err != nil {
		return SMSSendResult{}, ErrPhoneInvalid
	}
	if strings.TrimSpace(message.SignName) == "" || strings.TrimSpace(message.TemplateCode) == "" {
		return SMSSendResult{}, fmt.Errorf("Aliyun SMS sign name and template code are required")
	}
	request := dysmsapi.CreateSendSmsRequest()
	request.Scheme = "https"
	request.PhoneNumbers = strings.TrimPrefix(normalizedPhone, "+86")
	request.SignName = message.SignName
	request.TemplateCode = message.TemplateCode

	// Serialize template parameters
	paramsJSON, err := json.Marshal(message.Params)
	if err != nil {
		return SMSSendResult{}, fmt.Errorf("marshal template params: %w", err)
	}
	request.TemplateParam = string(paramsJSON)

	response, err := s.client.SendSms(request)
	if err != nil {
		return SMSSendResult{}, fmt.Errorf("send SMS: %w", err)
	}
	if response == nil {
		return SMSSendResult{}, fmt.Errorf("send SMS: empty provider response")
	}

	result := SMSSendResult{
		Code:      response.Code,
		RequestID: response.RequestId,
		BizID:     response.BizId,
	}

	if response.Code != "OK" {
		return result, fmt.Errorf("SMS provider rejected request: code=%s", response.Code)
	}

	return result, nil
}
