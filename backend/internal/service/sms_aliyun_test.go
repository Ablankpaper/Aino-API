package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/dysmsapi"
	"github.com/stretchr/testify/require"
)

type aliyunSMSClientStub struct {
	request  *dysmsapi.SendSmsRequest
	response *dysmsapi.SendSmsResponse
	err      error
}

func (s *aliyunSMSClientStub) SendSms(request *dysmsapi.SendSmsRequest) (*dysmsapi.SendSmsResponse, error) {
	s.request = request
	return s.response, s.err
}

func TestAliyunSMSSenderUsesDomesticNumberAndExactTemplateParams(t *testing.T) {
	response := dysmsapi.CreateSendSmsResponse()
	response.Code = "OK"
	response.RequestId = "request-id"
	response.BizId = "biz-id"
	client := &aliyunSMSClientStub{response: response}
	sender := &AliyunSMSSender{client: client}

	result, err := sender.Send(context.Background(), SMSMessage{
		Phone:        "+8613900000000",
		SignName:     "测试签名",
		TemplateCode: "SMS_123456",
		Params:       map[string]string{"code": "123456", "minutes": "5"},
	})

	require.NoError(t, err)
	require.Equal(t, "13900000000", client.request.PhoneNumbers)
	require.Equal(t, "https", client.request.Scheme)
	var params map[string]string
	require.NoError(t, json.Unmarshal([]byte(client.request.TemplateParam), &params))
	require.Equal(t, map[string]string{"code": "123456", "minutes": "5"}, params)
	require.Equal(t, "OK", result.Code)
}

func TestAliyunSMSSenderRejectsProviderBusinessError(t *testing.T) {
	response := dysmsapi.CreateSendSmsResponse()
	response.Code = "isv.BUSINESS_LIMIT_CONTROL"
	client := &aliyunSMSClientStub{response: response}
	sender := &AliyunSMSSender{client: client}

	result, err := sender.Send(context.Background(), SMSMessage{
		Phone: "+8613900000000", SignName: "测试签名", TemplateCode: "SMS_123456", Params: map[string]string{"code": "123456"},
	})

	require.Error(t, err)
	require.Equal(t, "isv.BUSINESS_LIMIT_CONTROL", result.Code)
}
