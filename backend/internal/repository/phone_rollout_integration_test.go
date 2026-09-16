//go:build integration

package repository_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	dbuser "github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPhoneRolloutAllowlistHTTPFlowWithPostgresAndRedis(t *testing.T) {
	r := newPhoneAuthFlowRig(t)
	deployment := &config.Config{SMS: config.SMSConfig{
		Enabled: true, RolloutPhoneAllowlist: []string{"+8613700000000", "+8613600000000"},
		Provider: "aliyun", AccessKeyID: "fixture-rollout-key", AccessKeySecret: "fixture-rollout-secret",
		HMACSecret: "fixture-rollout-hmac-secret-at-least-32-bytes", RegionID: "cn-hangzhou",
		SignName: "fixture-sign", TemplateCode: "SMS_FIXTURE", TemplateParams: map[string]string{"code": "code", "minutes": "ttl_minutes"}, TemplateVerified: true,
		RequestTimeoutSeconds: 5, CodeLength: 6, TTLSeconds: 300, CooldownSeconds: 60, MaxAttempts: 5,
		PhoneHourLimit: 50, PhoneDayLimit: 50, IPHourLimit: 50, GlobalDayLimit: 1000,
	}}
	require.NoError(t, deployment.SMS.Validate("release"))
	settings := service.NewSettingService(r.settingRepo, deployment)
	sms := service.ProvideSMSService(deployment, repository.NewSMSCache(r.redis, r.smsPrefix), r.sender, settings)
	r.auth.SetSMSService(sms)

	denied := r.request(http.MethodPost, "/login/send", `{"phone":"13800000000"}`, "")
	require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	require.Zero(t, phoneRolloutSenderCalls(r.sender))
	keys, err := r.redis.Keys(r.ctx, r.smsPrefix+"sms:*").Result()
	require.NoError(t, err)
	require.Empty(t, keys)
	require.NotContains(t, denied.Body.String(), "13800000000")
	require.NotContains(t, denied.Body.String(), "+8613800000000")

	allowed := r.request(http.MethodPost, "/login/send", `{"phone":"+86 137 0000 0000"}`, "")
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body.String())
	verify := r.request(http.MethodPost, "/login/verify", fmt.Sprintf(
		`{"phone":"13700000000","challenge_id":%q,"code":%q,"register_if_new":true}`,
		challengeID(t, allowed.Body.Bytes()), r.sender.latestCode()), "")
	require.Equal(t, http.StatusOK, verify.Code, verify.Body.String())
	var verified struct {
		Data struct {
			User struct {
				ID int64 `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(verify.Body.Bytes(), &verified))
	require.NotZero(t, verified.Data.User.ID)
	t.Cleanup(func() {
		_, _ = r.client.User.Delete().Where(dbuser.IDEQ(verified.Data.User.ID)).Exec(r.ctx)
	})

	stale := r.request(http.MethodPost, "/login/send", `{"phone":"13600000000"}`, "")
	require.Equal(t, http.StatusOK, stale.Code, stale.Body.String())
	staleChallengeID := challengeID(t, stale.Body.Bytes())
	staleCode := r.sender.latestCode()
	deployment.SMS.RolloutPhoneAllowlist = []string{"+8613700000000"}
	staleVerify := r.request(http.MethodPost, "/login/verify", fmt.Sprintf(
		`{"phone":"13600000000","challenge_id":%q,"code":%q,"register_if_new":true}`,
		staleChallengeID, staleCode), "")
	require.Equal(t, http.StatusForbidden, staleVerify.Code, staleVerify.Body.String())
	require.NotContains(t, staleVerify.Body.String(), "13600000000")
	require.NotContains(t, staleVerify.Body.String(), "+8613600000000")
	exists, err := r.redis.Exists(r.ctx, r.smsPrefix+"sms:challenge:"+staleChallengeID).Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, exists, "rollout removal must reject without consuming the Redis challenge")
}

func phoneRolloutSenderCalls(sender *phoneAuthFlowSender) int {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	return sender.calls
}
