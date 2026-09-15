//go:build integration

package repository_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/ent/desktopmodelcredential"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type desktopCredentialRig struct {
	*desktopModelRig
	credentials            *service.DesktopCredentialService
	userID                 int64
	token, refresh, family string
	device, grant          string
}

func newDesktopCredentialRig(t *testing.T) *desktopCredentialRig {
	t.Helper()
	r := newDesktopModelRig(t)
	cache := repository.NewRefreshTokenCache(r.redis)
	repo := repository.NewDesktopCredentialRepository(r.client)
	r.keys.SetDesktopCredentialDependencies(repo, cache, r.settings)
	r.auth.SetDesktopCredentialRevoker(repo)
	r.users.SetDesktopCredentialRevoker(repo)
	credentials := service.NewDesktopCredentialService(repo, r.keys, r.models, r.auth, cache)
	r.desktopHandler.SetCredentialService(credentials)
	r.desktopHandler.SetCredentialSecurity(r.users, nil)
	r.router.GET("/v1/models", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(r.keys, nil, &config.Config{})), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.router.GET("/v1/usage", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(r.keys, nil, &config.Config{})), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.router.POST("/v1/chat/completions", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(r.keys, nil, &config.Config{})), middleware.GroupModelAllowlist(), func(c *gin.Context) {
		key, _ := middleware.GetAPIKeyFromContext(c)
		c.JSON(200, gin.H{"group_id": key.GroupID})
	})
	r.router.POST("/v1/images/generations", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(r.keys, nil, &config.Config{})), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.router.GET("/v1beta/models", middleware.APIKeyAuthWithSubscriptionGoogle(r.keys, nil, &config.Config{}), func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	authHandler := handler.NewAuthHandler(&config.Config{}, r.auth, r.users, r.settings, nil, nil, nil, nil)
	r.router.POST("/logout", authHandler.Logout)
	u := r.user(t, service.StatusActive, false)
	pair, err := r.auth.GenerateTokenPair(r.ctx, u, "")
	require.NoError(t, err)
	claims, err := r.auth.ValidateToken(pair.AccessToken)
	require.NoError(t, err)
	g := r.group(t, false)
	input, output := 2e-6, 6e-6
	g.ModelPricing = []service.ChannelModelPricing{{Models: []string{"fixture-*"}, InputPrice: &input, OutputPrice: &output}}
	require.NoError(t, r.groups.Update(r.ctx, g))
	require.NoError(t, r.userRepo.AddGroupToAllowedGroups(r.ctx, u.ID, g.ID))
	r.catalog(t, []service.DesktopModelEntry{desktopEntry("fixture-a", g.ID), desktopEntry("fixture-b", g.ID)}, "fixture-a")
	return &desktopCredentialRig{desktopModelRig: r, credentials: credentials, userID: u.ID, token: pair.AccessToken, refresh: pair.RefreshToken, family: claims.SessionID, device: uuid.NewString(), grant: uuid.NewString()}
}

func (r *desktopCredentialRig) issue(t *testing.T, model string) service.DesktopCredentialResponse {
	t.Helper()
	body, err := json.Marshal(map[string]string{"device_id": r.device, "connection_grant_id": r.grant, "model_id": model})
	require.NoError(t, err)
	w := r.request(http.MethodPost, "/api/v1/desktop/credentials", string(body), r.token)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var result struct {
		Data service.DesktopCredentialResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.NotEmpty(t, result.Data.CredentialID)
	require.Equal(t, "https://api.agentera.com.cn/v1", result.Data.BaseURL)
	return result.Data
}

func TestDesktopLeaseConcurrentReuseAndCurrentModel(t *testing.T) {
	r := newDesktopCredentialRig(t)
	require.NoError(t, r.settingRepo.Set(r.ctx, "desktop.credential_ttl_seconds", "900"))
	results := make(chan *service.DesktopCredentialResponse, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"device_id": r.device, "connection_grant_id": r.grant, "model_id": "fixture-a"})
			response := r.request("POST", "/api/v1/desktop/credentials", string(body), r.token)
			var envelope struct {
				Data service.DesktopCredentialResponse `json:"data"`
			}
			err := json.Unmarshal(response.Body.Bytes(), &envelope)
			if response.Code != http.StatusOK {
				err = fmt.Errorf("credential request returned %d", response.Code)
			}
			results <- &envelope.Data
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	first := ""
	for res := range results {
		require.NotNil(t, res)
		if first == "" {
			first = res.APIKey
		}
		require.Equal(t, first, res.APIKey)
	}
	res := r.issue(t, "fixture-b")
	require.Equal(t, first, res.APIKey)
	require.Equal(t, "fixture-b", res.Model.ID)
	require.WithinDuration(t, time.Now().Add(900*time.Second), res.ExpiresAt, 5*time.Second)
	require.Equal(t, 200, r.request("GET", "/v1/models", "", res.APIKey).Code)
}

func TestDesktopLeaseLogoutRevokesCachedKeyAndOldJWT(t *testing.T) {
	r := newDesktopCredentialRig(t)
	lease := r.issue(t, "fixture-a")
	key, err := r.keys.GetByKey(r.ctx, lease.APIKey)
	require.NoError(t, err)
	require.True(t, key.DesktopManaged)
	ordinary, err := r.keys.Create(r.ctx, r.userID, service.CreateAPIKeyRequest{Name: "ordinary", GroupID: key.GroupID})
	require.NoError(t, err)
	require.Equal(t, 200, r.request("GET", "/v1/models", "", lease.APIKey).Code)
	body, _ := json.Marshal(map[string]string{"refresh_token": r.refresh})
	require.Equal(t, 200, r.request("POST", "/logout", string(body), "").Code)
	require.Equal(t, 401, r.request("GET", "/v1/models", "", lease.APIKey).Code)
	require.Equal(t, 401, r.request("GET", "/v1/usage", "", lease.APIKey).Code)
	require.Equal(t, 200, r.request("GET", "/v1/models", "", ordinary.Key).Code)
	updatedName := "ordinary updated"
	_, err = r.keys.Update(r.ctx, ordinary.ID, r.userID, service.UpdateAPIKeyRequest{Name: &updatedName})
	require.NoError(t, err)
	_, err = r.credentials.IssueOrReuseLease(r.ctx, service.DesktopCredentialRequest{UserID: r.userID, DeviceID: r.device, ConnectionGrantID: uuid.NewString(), SessionFamilyID: r.family, ModelID: "fixture-a"})
	require.Error(t, err)
}

func TestDesktopLeaseRenewalExpiryAndManagedEditing(t *testing.T) {
	r := newDesktopCredentialRig(t)
	first := r.issue(t, "fixture-a")
	key, err := r.keys.GetByKey(r.ctx, first.APIKey)
	require.NoError(t, err)
	oldExpiry := time.Now().Add(5 * time.Minute)
	_, err = r.client.DesktopModelCredential.Update().Where(desktopmodelcredential.APIKeyIDEQ(key.ID)).SetExpiresAt(oldExpiry).Save(r.ctx)
	require.NoError(t, err)
	_, err = r.client.APIKey.UpdateOneID(key.ID).SetExpiresAt(oldExpiry).Save(r.ctx)
	require.NoError(t, err)
	renewed := r.issue(t, "fixture-a")
	require.Equal(t, first.CredentialID, renewed.CredentialID)
	require.Equal(t, first.APIKey, renewed.APIKey)
	require.True(t, renewed.ExpiresAt.After(oldExpiry.Add(40*time.Minute)))
	_, err = r.keys.Update(r.ctx, key.ID, r.userID, service.UpdateAPIKeyRequest{ClearExpiration: true})
	require.Error(t, err)
	_, err = repository.GetIntegrationDB().ExecContext(r.ctx, "UPDATE desktop_model_credentials SET created_at=$1,expires_at=$2 WHERE api_key_id=$3", time.Now().Add(-time.Hour), time.Now().Add(-time.Minute), key.ID)
	require.NoError(t, err)
	require.Equal(t, 401, r.request("GET", "/v1/usage", "", first.APIKey).Code)
	replaced := r.issue(t, "fixture-a")
	require.NotEqual(t, first.APIKey, replaced.APIKey)
	require.Equal(t, 401, r.request("GET", "/v1/models", "", first.APIKey).Code)
}

func TestDesktopLeaseDeviceOwnershipAndParentRevocation(t *testing.T) {
	r := newDesktopCredentialRig(t)
	lease := r.issue(t, "fixture-a")
	other := r.user(t, service.StatusActive, false)
	pair, err := r.auth.GenerateTokenPair(r.ctx, other, "")
	require.NoError(t, err)
	path := "/api/v1/desktop/devices/" + r.device
	require.Equal(t, 404, r.request("DELETE", path, "", pair.AccessToken).Code)
	require.Equal(t, 200, r.request("GET", "/v1/models", "", lease.APIKey).Code)
	listing := r.request("GET", "/api/v1/desktop/devices", "", r.token)
	require.Equal(t, 200, listing.Code)
	require.NotContains(t, listing.Body.String(), lease.APIKey)
	claims, err := r.auth.ValidateToken(r.token)
	require.NoError(t, err)
	claims.AuthTime = time.Now().Add(-time.Hour).Unix()
	stale, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("integration-phone-binding-secret"))
	require.NoError(t, err)
	require.Equal(t, 403, r.request("DELETE", path, "", stale).Code)
	require.Equal(t, 200, r.request("GET", "/v1/models", "", lease.APIKey).Code)
	require.Equal(t, 200, r.request("DELETE", path, "", r.token).Code)
	require.Equal(t, 401, r.request("GET", "/v1/models", "", lease.APIKey).Code)
	_, err = r.auth.RefreshTokenPair(r.ctx, r.refresh)
	require.Error(t, err)
	request, _ := json.Marshal(map[string]string{"device_id": r.device, "connection_grant_id": uuid.NewString(), "model_id": "fixture-a"})
	require.Equal(t, 401, r.request("POST", "/api/v1/desktop/credentials", string(request), r.token).Code)
}

func TestDesktopLeaseIdentityChangesNeverReviveKeys(t *testing.T) {
	for _, change := range []string{"password", "disable"} {
		t.Run(change, func(t *testing.T) {
			r := newDesktopCredentialRig(t)
			lease := r.issue(t, "fixture-a")
			require.Equal(t, 200, r.request("GET", "/v1/models", "", lease.APIKey).Code)
			if change == "password" {
				require.NoError(t, r.users.ChangePassword(r.ctx, r.userID, service.ChangePasswordRequest{CurrentPassword: "existing-account-password", NewPassword: "another-secure-password"}))
			} else {
				u, err := r.userRepo.GetByID(r.ctx, r.userID)
				require.NoError(t, err)
				u.Status = "disabled"
				require.NoError(t, r.userRepo.Update(r.ctx, u, service.UserUpdateFields{Status: true}))
				require.Equal(t, 401, r.request("GET", "/v1/models", "", lease.APIKey).Code)
				u.Status = service.StatusActive
				require.NoError(t, r.userRepo.Update(r.ctx, u, service.UserUpdateFields{Status: true}))
			}
			require.Equal(t, 401, r.request("GET", "/v1/models", "", lease.APIKey).Code)
			request, _ := json.Marshal(map[string]string{"device_id": r.device, "connection_grant_id": uuid.NewString(), "model_id": "fixture-a"})
			require.Equal(t, 401, r.request("POST", "/api/v1/desktop/credentials", string(request), r.token).Code)
			freshUser, err := r.userRepo.GetByID(r.ctx, r.userID)
			require.NoError(t, err)
			freshPair, err := r.auth.GenerateTokenPair(r.ctx, freshUser, "")
			require.NoError(t, err)
			r.token = freshPair.AccessToken
			require.NotEqual(t, lease.APIKey, r.issue(t, "fixture-a").APIKey)
		})
	}
}

func TestDesktopLeaseCreationFailureLeavesNoOrphanKey(t *testing.T) {
	r := newDesktopCredentialRig(t)
	existing := r.issue(t, "fixture-a")
	key, err := r.keys.GetByKey(r.ctx, existing.APIKey)
	require.NoError(t, err)
	count, err := r.client.APIKey.Query().Where(apikey.UserIDEQ(r.userID)).Count(r.ctx)
	require.NoError(t, err)
	past := time.Now().Add(-time.Hour)
	repo := repository.NewDesktopCredentialRepository(r.client)
	_, _, err = repo.IssueOrReuse(r.ctx, &service.DesktopCredential{UserID: r.userID, DeviceID: uuid.NewString(), ConnectionGrantID: uuid.NewString(), SessionFamilyID: r.family, GroupID: *key.GroupID, ModelID: "fixture-a", ExpiresAt: past}, &service.APIKey{UserID: r.userID, Key: "sk-isolated-orphan-" + uuid.NewString(), Name: "isolated", GroupID: key.GroupID, Status: service.StatusActive, DesktopManaged: true, ExpiresAt: &past}, time.Hour)
	require.Error(t, err)
	after, err := r.client.APIKey.Query().Where(apikey.UserIDEQ(r.userID)).Count(r.ctx)
	require.NoError(t, err)
	require.Equal(t, count, after)
}

func TestDesktopLeaseRefreshRaceAndStaleLogout(t *testing.T) {
	r := newDesktopCredentialRig(t)
	lease := r.issue(t, "fixture-a")
	rotated, err := r.auth.RefreshTokenPair(r.ctx, r.refresh)
	require.NoError(t, err)
	require.Equal(t, 200, r.request("GET", "/v1/models", "", lease.APIKey).Code)
	// The old refresh token's hash still locates its family for a racing logout.
	body, _ := json.Marshal(map[string]string{"refresh_token": r.refresh})
	require.Equal(t, 200, r.request("POST", "/logout", string(body), "").Code)
	_, err = r.auth.RefreshTokenPair(r.ctx, rotated.RefreshToken)
	require.Error(t, err)
	require.Equal(t, 401, r.request("GET", "/v1/models", "", lease.APIKey).Code)
	cache := repository.NewRefreshTokenCache(r.redis)
	sum := sha256.Sum256([]byte(rotated.RefreshToken))
	oldHash := hex.EncodeToString(sum[:])
	late := &service.RefreshTokenData{UserID: r.userID, FamilyID: r.family, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	rotator := cache.(interface {
		RotateRefreshToken(context.Context, string, string, *service.RefreshTokenData, time.Duration) error
	})
	require.Error(t, rotator.RotateRefreshToken(r.ctx, oldHash, "late-rotation", late, time.Hour))
	require.Error(t, cache.StoreRefreshToken(r.ctx, "late-store", late, time.Hour))
}

func TestDesktopLeaseGroupCatalogAndEndpointBoundaries(t *testing.T) {
	r := newDesktopCredentialRig(t)
	lease := r.issue(t, "fixture-a")
	key, err := r.keys.GetByKey(r.ctx, lease.APIKey)
	require.NoError(t, err)
	other := r.group(t, false)
	price := 2e-6
	other.ModelPricing = []service.ChannelModelPricing{{Models: []string{"fixture-c"}, InputPrice: &price, OutputPrice: &price}}
	require.NoError(t, r.groups.Update(r.ctx, other))
	require.NoError(t, r.userRepo.AddGroupToAllowedGroups(r.ctx, r.userID, other.ID))
	r.catalog(t, []service.DesktopModelEntry{desktopEntry("fixture-a", *key.GroupID), desktopEntry("fixture-c", other.ID)}, "fixture-a")
	otherLease := r.issue(t, "fixture-c")
	require.NotEqual(t, lease.APIKey, otherLease.APIKey)
	require.Equal(t, 200, r.request("POST", "/v1/chat/completions", `{"model":"fixture-a","messages":[]}`, lease.APIKey).Code)
	require.Equal(t, 404, r.request("POST", "/v1/chat/completions", `{"model":"fixture-c","messages":[]}`, lease.APIKey).Code)
	require.Equal(t, 403, r.request("POST", "/v1/images/generations", `{}`, lease.APIKey).Code)
	require.Equal(t, 403, r.request("GET", "/v1beta/models", "", lease.APIKey).Code)
	require.Equal(t, 401, r.request("GET", "/api/v1/desktop/models", "", lease.APIKey).Code)
	group, err := r.groups.GetByID(r.ctx, *key.GroupID)
	require.NoError(t, err)
	group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"unrelated"}}
	require.NoError(t, r.groups.Update(r.ctx, group))
	require.Equal(t, 401, r.request("GET", "/v1/models", "", lease.APIKey).Code)
	_, err = r.credentials.IssueOrReuseLease(r.ctx, service.DesktopCredentialRequest{UserID: r.userID, DeviceID: r.device, ConnectionGrantID: r.grant, SessionFamilyID: r.family, ModelID: "fixture-a"})
	require.Error(t, err)
	require.NoError(t, r.settingRepo.Set(r.ctx, "desktop.enabled", "false"))
	require.Equal(t, 401, r.request("GET", "/v1/models", "", otherLease.APIKey).Code)
}

func TestDesktopLeaseRequiresLiveRefreshAndAuthAvailability(t *testing.T) {
	r := newDesktopCredentialRig(t)
	lease := r.issue(t, "fixture-a")
	cache := repository.NewRefreshTokenCache(r.redis)
	sum := sha256.Sum256([]byte(r.refresh))
	hash := hex.EncodeToString(sum[:])
	require.NoError(t, cache.DeleteRefreshToken(r.ctx, hash))
	hashes, err := cache.GetFamilyTokenHashes(r.ctx, r.family)
	require.NoError(t, err)
	require.Contains(t, hashes, hash)
	require.Equal(t, 401, r.request("GET", "/v1/models", "", lease.APIKey).Code)
	r.keys.SetDesktopCredentialDependencies(repository.NewDesktopCredentialRepository(r.client), nil, r.settings)
	require.Equal(t, 503, r.request("GET", "/v1/models", "", lease.APIKey).Code)
}
