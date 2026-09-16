//go:build integration

package repository_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAinoPlatformPrePhoneSchemaUpgradePreservesExistingAccount(t *testing.T) {
	ctx := context.Background()
	db := repository.NewAinoPrePlatformDatabase(t)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name='api_keys' AND column_name='desktop_managed'").Scan(&count))
	require.Zero(t, count, "the fixture must begin before desktop schema, not merely seed latest tables")
	u := &service.User{}
	require.NoError(t, u.SetPassword("fixture-legacy-password"))
	var userID, groupID, subscriptionID, orderID, keyID int64
	runID := uuid.NewString()
	email := "fixture-" + runID + "@example.test"
	require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO users(email,password_hash,balance,signup_source) VALUES($1,$2,37.5,'email') RETURNING id", email, u.PasswordHash).Scan(&userID))
	_, err := db.ExecContext(ctx, "INSERT INTO auth_identities(user_id,provider_type,provider_key,provider_subject,verified_at) VALUES($1,'email','email',$2,now()),($1,'oidc','fixture-oidc',$3,now())", userID, email, "fixture-"+runID)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO groups(name,platform,subscription_type) VALUES($1,'openai','subscription') RETURNING id", "fixture-"+runID).Scan(&groupID))
	require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO user_subscriptions(user_id,group_id,starts_at,expires_at,daily_usage_usd) VALUES($1,$2,now(),now()+interval '30 days',1.25) RETURNING id", userID, groupID).Scan(&subscriptionID))
	require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO payment_orders(user_id,amount,pay_amount,out_trade_no,status,expires_at) VALUES($1,20,20,$2,'COMPLETED',now()+interval '1 day') RETURNING id", userID, "fixture-"+runID).Scan(&orderID))
	key := "sk-fixture-" + runID
	require.NoError(t, db.QueryRowContext(ctx, "INSERT INTO api_keys(user_id,key,name,group_id) VALUES($1,$2,'fixture-ordinary',$3) RETURNING id", userID, key, groupID).Scan(&keyID))
	// Precondition: this schema actually rejects phone identities.
	_, err = db.ExecContext(ctx, "INSERT INTO auth_identities(user_id,provider_type,provider_key,provider_subject) VALUES($1,'phone','phone','+8613900000099')", userID)
	require.Error(t, err)
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	r := newPhoneAuthFlowRigWithStorage(t, client, db)
	login := r.request("POST", "/email/login", fmt.Sprintf(`{"email":%q,"password":"fixture-legacy-password"}`, email), "")
	require.Equal(t, 200, login.Code)
	auth := fixtureDecode[fixtureLogin](t, login.Body.Bytes())
	require.Equal(t, userID, auth.User.ID)
	phone := fmt.Sprintf("138%08d", time.Now().UnixNano()%100000000)
	send := r.request("POST", "/send", fmt.Sprintf(`{"phone":%q}`, phone), auth.AccessToken)
	require.Equal(t, 200, send.Code)
	bound := r.request("POST", "/bind", fmt.Sprintf(`{"phone":%q,"challenge_id":%q,"code":%q}`, phone, challengeID(t, send.Body.Bytes()), r.sender.latestCode()), auth.AccessToken)
	require.Equal(t, 200, bound.Code)
	require.NoError(t, r.redis.Del(ctx, r.smsPrefix+"sms:cooldown:phone:+86"+phone).Err())
	send = r.request("POST", "/login/send", fmt.Sprintf(`{"phone":%q}`, phone), "")
	require.Equal(t, 200, send.Code)
	phoneLogin := r.request("POST", "/login/verify", fmt.Sprintf(`{"phone":%q,"challenge_id":%q,"code":%q}`, phone, challengeID(t, send.Body.Bytes()), r.sender.latestCode()), "")
	require.Equal(t, 200, phoneLogin.Code)
	require.Equal(t, userID, fixtureDecode[fixtureLogin](t, phoneLogin.Body.Bytes()).User.ID)
	require.Equal(t, 200, r.request("POST", "/email/login", fmt.Sprintf(`{"email":%q,"password":"fixture-legacy-password"}`, email), "").Code)
	groups := repository.NewGroupRepository(client, db)
	subs := repository.NewUserSubscriptionRepository(client)
	keys := service.NewAPIKeyService(repository.NewAPIKeyRepository(client, db), r.userRepo, groups, subs, repository.NewUserGroupRateRepository(db), repository.NewAPIKeyCache(r.redis), &config.Config{})
	r.router.GET("/fixture/legacy-key", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(keys, nil, &config.Config{})), func(c *gin.Context) {
		k, _ := middleware.GetAPIKeyFromContext(c)
		c.JSON(200, gin.H{"data": gin.H{"user_id": k.UserID, "key_id": k.ID}})
	})
	response := r.request("GET", "/fixture/legacy-key", "", key)
	require.Equal(t, 200, response.Code)
	owner := fixtureDecode[struct {
		UserID int64 `json:"user_id"`
		KeyID  int64 `json:"key_id"`
	}](t, response.Body.Bytes())
	require.Equal(t, userID, owner.UserID)
	require.Equal(t, keyID, owner.KeyID)
	var balance, orderStatus, subscriptionUsage string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT balance::text FROM users WHERE id=$1", userID).Scan(&balance))
	require.Equal(t, "37.50000000", balance)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT status FROM payment_orders WHERE id=$1 AND user_id=$2", orderID, userID).Scan(&orderStatus))
	require.Equal(t, "COMPLETED", orderStatus)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT daily_usage_usd::text FROM user_subscriptions WHERE id=$1 AND user_id=$2", subscriptionID, userID).Scan(&subscriptionUsage))
	require.Equal(t, "1.2500000000", subscriptionUsage)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM auth_identities WHERE user_id=$1 AND provider_type IN ('email','oidc','phone')", userID).Scan(&count))
	require.Equal(t, 3, count)
	require.NoError(t, repository.ApplyMigrations(ctx, db), "restarting migration runner must retain the upgraded data")
	t.Logf("run_id=%s schema through 238 -> all migrations; same user/email/OIDC/ordinary key/order/subscription/balance; phone login and old email login verified; old server binary rollback NOT exercised", runID)
}
