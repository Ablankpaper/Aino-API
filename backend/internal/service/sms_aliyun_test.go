package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	dysmsapi "github.com/alibabacloud-go/dysmsapi-20170525/v4/client"
	"github.com/alibabacloud-go/tea/tea"
	"github.com/stretchr/testify/require"
)

func newAliyunSMSBoundarySender(t *testing.T, handler http.Handler, timeout time.Duration) (*AliyunSMSSender, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := dysmsapi.NewClient(&openapi.Config{
		AccessKeyId:     tea.String("fixture-access-key"),
		AccessKeySecret: tea.String("fixture-access-secret"),
		Endpoint:        tea.String(strings.TrimPrefix(server.URL, "http://")),
		Protocol:        tea.String("HTTP"),
		RegionId:        tea.String("cn-hangzhou"),
	})
	require.NoError(t, err)
	return newAliyunSMSSenderWithClient(client, timeout), server
}

func TestAliyunSMSSenderUsesDomesticNumberAndExactTemplateParams(t *testing.T) {
	requests := make(chan url.Values, 1)
	sender, server := newAliyunSMSBoundarySender(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Code":"OK","RequestId":"request-id","BizId":"biz-id"}`))
	}), time.Second)
	defer server.Close()

	result, err := sender.Send(context.Background(), SMSMessage{
		Phone:        "+8613900000000",
		SignName:     "测试签名",
		TemplateCode: "SMS_123456",
		Params:       map[string]string{"code": "123456", "minutes": "5"},
	})

	require.NoError(t, err)
	query := <-requests
	require.Equal(t, "13900000000", query.Get("PhoneNumbers"))
	require.Equal(t, "测试签名", query.Get("SignName"))
	require.Equal(t, "SMS_123456", query.Get("TemplateCode"))
	var params map[string]string
	require.NoError(t, json.Unmarshal([]byte(query.Get("TemplateParam")), &params))
	require.Equal(t, map[string]string{"code": "123456", "minutes": "5"}, params)
	require.Equal(t, SMSSendResult{Code: "OK", RequestID: "request-id", BizID: "biz-id"}, result)
}

func TestAliyunSMSSenderDoesNotRetryProviderFailure(t *testing.T) {
	var requests atomic.Int32
	sender, server := newAliyunSMSBoundarySender(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "fixture failure", http.StatusInternalServerError)
	}), time.Second)
	defer server.Close()

	_, err := sender.Send(context.Background(), SMSMessage{
		Phone: "+8613900000000", SignName: "测试签名", TemplateCode: "SMS_123456", Params: map[string]string{"code": "123456"},
	})

	require.Error(t, err)
	require.EqualValues(t, 1, requests.Load())
}

func TestAliyunSMSSenderAppliesTotalRequestTimeout(t *testing.T) {
	started := make(chan struct{}, 1)
	var requests atomic.Int32
	sender, server := newAliyunSMSBoundarySender(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		started <- struct{}{}
		<-r.Context().Done()
	}), 40*time.Millisecond)
	defer server.Close()

	begin := time.Now()
	_, err := sender.Send(context.Background(), SMSMessage{
		Phone: "+8613900000000", SignName: "测试签名", TemplateCode: "SMS_123456", Params: map[string]string{"code": "123456"},
	})
	elapsed := time.Since(begin)

	require.Error(t, err)
	require.Less(t, elapsed, 500*time.Millisecond)
	require.EqualValues(t, 1, requests.Load())
	select {
	case <-started:
	default:
		t.Fatal("provider request never reached the local HTTP boundary")
	}
}

func TestAliyunSMSSenderBuildsClientFromCurrentDeliveryOptions(t *testing.T) {
	sender, server := newAliyunSMSBoundarySender(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Code":"OK","RequestId":"request-id","BizId":"biz-id"}`))
	}), time.Second)
	defer server.Close()

	var gotRegion string
	var gotTimeout time.Duration
	boundaryClient := sender.client
	sender.clientFactory = func(regionID string, timeout time.Duration) (*dysmsapi.Client, error) {
		gotRegion, gotTimeout = regionID, timeout
		return boundaryClient, nil
	}

	_, err := sender.SendWithOptions(context.Background(), SMSMessage{
		Phone: "+8613900000000", SignName: "test-sign", TemplateCode: "SMS_123456", Params: map[string]string{"code": "123456"},
	}, SMSDeliveryOptions{RegionID: "cn-shanghai", RequestTimeout: 9 * time.Second})

	require.NoError(t, err)
	require.Equal(t, "cn-shanghai", gotRegion)
	require.Equal(t, 9*time.Second, gotTimeout)
}
