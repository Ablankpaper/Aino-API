//go:build integration

package repository_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func challengeID(t *testing.T, wBody []byte) string {
	t.Helper()
	var body struct {
		Data struct {
			ChallengeID string `json:"challenge_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(wBody, &body))
	require.NotEmpty(t, body.Data.ChallengeID)
	return body.Data.ChallengeID
}

func TestPhoneFlowRejectsStaleAndMissingAuthentication(t *testing.T) {
	rig := newPhoneAuthFlowRig(t)
	u := rig.user(t, service.StatusActive, false)
	fresh, err := rig.auth.GenerateToken(rig.ctx, u)
	require.NoError(t, err)
	claims, err := rig.auth.ValidateToken(fresh)
	require.NoError(t, err)
	for _, authTime := range []int64{0, time.Now().Add(-service.StepUpGrantTTL - time.Second).Unix(), time.Now().Add(time.Minute).Unix()} {
		claims.AuthTime = authTime
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("integration-phone-binding-secret"))
		require.NoError(t, err)
		w := rig.request(http.MethodPost, "/send", `{"phone":"13900000000"}`, token)
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "RECENT_AUTH_REQUIRED")
	}
}

func TestPhoneFlowBindsExactSessionAndRejectsReplay(t *testing.T) {
	rig := newPhoneAuthFlowRig(t)
	u := rig.user(t, service.StatusActive, false)
	a, err := rig.auth.GenerateToken(rig.ctx, u)
	require.NoError(t, err)
	b, err := rig.auth.GenerateToken(rig.ctx, u)
	require.NoError(t, err)
	send := rig.request(http.MethodPost, "/send", `{"phone":"13900000001"}`, a)
	require.Equal(t, http.StatusOK, send.Code, send.Body.String())
	id := challengeID(t, send.Body.Bytes())
	wrong := rig.request(http.MethodPost, "/bind", `{"phone":"13900000001","challenge_id":"`+id+`","code":"`+rig.sender.latestCode()+`"}`, b)
	require.Equal(t, http.StatusBadRequest, wrong.Code, wrong.Body.String())
	ok := rig.request(http.MethodPost, "/bind", `{"phone":"13900000001","challenge_id":"`+id+`","code":"`+rig.sender.latestCode()+`"}`, a)
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	replay := rig.request(http.MethodPost, "/bind", `{"phone":"13900000001","challenge_id":"`+id+`","code":"`+rig.sender.latestCode()+`"}`, a)
	require.Equal(t, http.StatusBadRequest, replay.Code, replay.Body.String())
}

func TestPhoneFlowRegistrationClosedRejectsUnknownNumber(t *testing.T) {
	rig := newPhoneAuthFlowRig(t)
	require.NoError(t, rig.settingRepo.Set(rig.ctx, service.SettingKeyRegistrationEnabled, "false"))
	newNumber := rig.request(http.MethodPost, "/login/send", `{"phone":"13900000002"}`, "")
	require.Equal(t, http.StatusOK, newNumber.Code, newNumber.Body.String())
	id := challengeID(t, newNumber.Body.Bytes())
	rejected := rig.request(http.MethodPost, "/login/verify", `{"phone":"13900000002","challenge_id":"`+id+`","code":"`+rig.sender.latestCode()+`","register_if_new":true}`, "")
	require.Equal(t, http.StatusForbidden, rejected.Code, rejected.Body.String())
	count, err := rig.client.AuthIdentity.Query().Where(authidentity.ProviderTypeEQ("phone"), authidentity.ProviderSubjectEQ("+8613900000002")).Count(rig.ctx)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestPhoneFlowRegistrationClosedStillAllowsExistingPhone(t *testing.T) {
	rig := newPhoneAuthFlowRig(t)
	u := rig.user(t, service.StatusActive, false)
	_, err := rig.client.AuthIdentity.Create().SetUserID(u.ID).SetProviderType("phone").SetProviderKey("default").SetProviderSubject("+8613900000005").SetMetadata(map[string]any{}).Save(rig.ctx)
	require.NoError(t, err)
	require.NoError(t, rig.settingRepo.Set(rig.ctx, service.SettingKeyRegistrationEnabled, "false"))
	send := rig.request(http.MethodPost, "/login/send", `{"phone":"13900000005"}`, "")
	require.Equal(t, http.StatusOK, send.Code, send.Body.String())
	login := rig.request(http.MethodPost, "/login/verify", `{"phone":"13900000005","challenge_id":"`+challengeID(t, send.Body.Bytes())+`","code":"`+rig.sender.latestCode()+`"}`, "")
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
}

func TestPhoneFlowAgreementRequiredDoesNotCreateUser(t *testing.T) {
	rig := newPhoneAuthFlowRig(t)
	require.NoError(t, rig.settingRepo.SetMultiple(rig.ctx, map[string]string{
		service.SettingKeyLoginAgreementEnabled:   "true",
		service.SettingKeyLoginAgreementDocuments: `[{"id":"terms","title":"Terms","content":"terms"}]`,
	}))
	send := rig.request(http.MethodPost, "/login/send", `{"phone":"13900000006"}`, "")
	require.Equal(t, http.StatusOK, send.Code, send.Body.String())
	login := rig.request(http.MethodPost, "/login/verify", `{"phone":"13900000006","challenge_id":"`+challengeID(t, send.Body.Bytes())+`","code":"`+rig.sender.latestCode()+`","register_if_new":true}`, "")
	require.Equal(t, http.StatusBadRequest, login.Code, login.Body.String())
	require.Contains(t, login.Body.String(), "LOGIN_AGREEMENT_REQUIRED")
	count, err := rig.client.AuthIdentity.Query().Where(authidentity.ProviderTypeEQ("phone"), authidentity.ProviderSubjectEQ("+8613900000006")).Count(rig.ctx)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestPhoneFlowTotpDefersTokensAndRequiresStepUpForBinding(t *testing.T) {
	rig := newPhoneAuthFlowRig(t)
	u := rig.user(t, service.StatusActive, true)
	_, err := rig.client.AuthIdentity.Create().SetUserID(u.ID).SetProviderType("phone").SetProviderKey("default").SetProviderSubject("+8613900000003").SetMetadata(map[string]any{}).Save(rig.ctx)
	require.NoError(t, err)
	send := rig.request(http.MethodPost, "/login/send", `{"phone":"13900000003"}`, "")
	require.Equal(t, http.StatusOK, send.Code, send.Body.String())
	login := rig.request(http.MethodPost, "/login/verify", `{"phone":"13900000003","challenge_id":"`+challengeID(t, send.Body.Bytes())+`","code":"`+rig.sender.latestCode()+`"}`, "")
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	require.Contains(t, login.Body.String(), `"requires_2fa":true`)
	require.Contains(t, login.Body.String(), `"temp_token"`)
	require.NotContains(t, login.Body.String(), `"access_token"`)
	jwtToken, err := rig.auth.GenerateToken(rig.ctx, u)
	require.NoError(t, err)
	bind := rig.request(http.MethodPost, "/send", `{"phone":"13900000004"}`, jwtToken)
	require.Equal(t, http.StatusForbidden, bind.Code, bind.Body.String())
	require.Contains(t, bind.Body.String(), "STEP_UP_REQUIRED")
}
