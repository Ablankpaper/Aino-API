package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/dysmsapi"
)

// AliyunSMSSender implements SMSSender using Aliyun Dysmsapi
type AliyunSMSSender struct {
	client *dysmsapi.Client
}

// NewAliyunSMSSender creates a new Aliyun SMS sender
func NewAliyunSMSSender(accessKeyID, accessKeySecret, regionID string) (*AliyunSMSSender, error) {
	client, err := dysmsapi.NewClientWithAccessKey(regionID, accessKeyID, accessKeySecret)
	if err != nil {
		return nil, fmt.Errorf("create Aliyun client: %w", err)
	}

	return &AliyunSMSSender{
		client: client,
	}, nil
}

// Send sends an SMS message via Aliyun
func (s *AliyunSMSSender) Send(ctx context.Context, message SMSMessage) (SMSSendResult, error) {
	request := dysmsapi.CreateSendSmsRequest()
	request.Scheme = "https"
	request.PhoneNumbers = message.Phone
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

	result := SMSSendResult{
		Code:      response.Code,
		RequestID: response.RequestId,
		BizID:     response.BizId,
	}

	if response.Code != "OK" {
		return result, fmt.Errorf("SMS send failed: code=%s, message=%s", response.Code, response.Message)
	}

	return result, nil
}
