package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	dysmsapi "github.com/alibabacloud-go/dysmsapi-20170525/v4/client"
	util "github.com/alibabacloud-go/tea-utils/v2/service"
	"github.com/alibabacloud-go/tea/dara"
	"github.com/alibabacloud-go/tea/tea"
)

const defaultAliyunSMSRequestTimeout = 5 * time.Second

// AliyunSMSSender implements SMSSender using Aliyun Dysmsapi
type AliyunSMSSender struct {
	client  *dysmsapi.Client
	timeout time.Duration
}

type contextAliyunHTTPClient struct {
	ctx  context.Context
	base dara.HttpClient
}

func (c *contextAliyunHTTPClient) Call(request *http.Request, transport *http.Transport) (*http.Response, error) {
	request = request.WithContext(c.ctx)
	if c.base != nil {
		return c.base.Call(request, transport)
	}
	return (&http.Client{Transport: transport}).Do(request)
}

// NewAliyunSMSSender creates a new Aliyun SMS sender
func NewAliyunSMSSender(accessKeyID, accessKeySecret, regionID string) (*AliyunSMSSender, error) {
	return NewAliyunSMSSenderWithOptions(accessKeyID, accessKeySecret, regionID, defaultAliyunSMSRequestTimeout)
}

// NewAliyunSMSSenderWithOptions creates a sender with automatic retries
// disabled. A total request timeout prevents an ambiguous network failure
// from silently outliving the caller's verification request.
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
	timeoutMillis := int(timeout.Milliseconds())
	client, err := dysmsapi.NewClient(&openapi.Config{
		AccessKeyId:     tea.String(accessKeyID),
		AccessKeySecret: tea.String(accessKeySecret),
		RegionId:        tea.String(regionID),
		Protocol:        tea.String("HTTPS"),
		ConnectTimeout:  tea.Int(timeoutMillis),
		ReadTimeout:     tea.Int(timeoutMillis),
	})
	if err != nil {
		return nil, fmt.Errorf("create Aliyun client: %w", err)
	}
	return newAliyunSMSSenderWithClient(client, timeout), nil
}

func newAliyunSMSSenderWithClient(client *dysmsapi.Client, timeout time.Duration) *AliyunSMSSender {
	if timeout <= 0 {
		timeout = defaultAliyunSMSRequestTimeout
	}
	return &AliyunSMSSender{client: client, timeout: timeout}
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
	request := new(dysmsapi.SendSmsRequest).
		SetPhoneNumbers(strings.TrimPrefix(normalizedPhone, "+86")).
		SetSignName(message.SignName).
		SetTemplateCode(message.TemplateCode)

	// Serialize template parameters
	paramsJSON, err := json.Marshal(message.Params)
	if err != nil {
		return SMSSendResult{}, fmt.Errorf("marshal template params: %w", err)
	}
	request.SetTemplateParam(string(paramsJSON))

	requestCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	client := *s.client
	client.HttpClient = &contextAliyunHTTPClient{ctx: requestCtx, base: client.HttpClient}
	timeoutMillis := int(s.timeout.Milliseconds())
	runtime := new(util.RuntimeOptions).
		SetAutoretry(false).
		SetMaxAttempts(1).
		SetConnectTimeout(timeoutMillis).
		SetReadTimeout(timeoutMillis)
	response, err := client.SendSmsWithOptions(request, runtime)
	if err != nil {
		return SMSSendResult{}, fmt.Errorf("send SMS: %w", err)
	}
	if response == nil || response.Body == nil {
		return SMSSendResult{}, fmt.Errorf("send SMS: empty provider response")
	}

	result := SMSSendResult{
		Code:      tea.StringValue(response.Body.Code),
		RequestID: tea.StringValue(response.Body.RequestId),
		BizID:     tea.StringValue(response.Body.BizId),
	}

	if result.Code != "OK" {
		return result, fmt.Errorf("SMS provider rejected request: code=%s", result.Code)
	}

	return result, nil
}
