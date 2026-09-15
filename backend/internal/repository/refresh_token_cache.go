package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	refreshTokenKeyPrefix   = "refresh_token:"
	userRefreshTokensPrefix = "user_refresh_tokens:"
	tokenFamilyPrefix       = "token_family:"
)

// refreshTokenKey generates the Redis key for a refresh token.
func refreshTokenKey(tokenHash string) string {
	return refreshTokenKeyPrefix + tokenHash
}

// userRefreshTokensKey generates the Redis key for user's token set.
func userRefreshTokensKey(userID int64) string {
	return fmt.Sprintf("%s%d", userRefreshTokensPrefix, userID)
}

// tokenFamilyKey generates the Redis key for token family set.
func tokenFamilyKey(familyID string) string {
	return tokenFamilyPrefix + familyID
}

type refreshTokenCache struct {
	rdb *redis.Client
}

// NewRefreshTokenCache creates a new RefreshTokenCache implementation.
func NewRefreshTokenCache(rdb *redis.Client) service.RefreshTokenCache {
	return &refreshTokenCache{rdb: rdb}
}

func (c *refreshTokenCache) StoreRefreshToken(ctx context.Context, tokenHash string, data *service.RefreshTokenData, ttl time.Duration) error {
	val, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal refresh token data: %w", err)
	}
	return c.storeIndexedRefreshToken(ctx, tokenHash, data, string(val), ttl)
}

func (c *refreshTokenCache) RotateRefreshToken(ctx context.Context, oldHash, newHash string, data *service.RefreshTokenData, ttl time.Duration) error {
	val, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return c.replaceIndexedRefreshToken(ctx, oldHash, newHash, data, string(val), ttl)
}

func (c *refreshTokenCache) GetRefreshToken(ctx context.Context, tokenHash string) (*service.RefreshTokenData, error) {
	key := refreshTokenKey(tokenHash)
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, service.ErrRefreshTokenNotFound
		}
		return nil, err
	}
	var data service.RefreshTokenData
	if err := json.Unmarshal([]byte(val), &data); err != nil {
		return nil, fmt.Errorf("unmarshal refresh token data: %w", err)
	}
	return &data, nil
}

func (c *refreshTokenCache) DeleteRefreshToken(ctx context.Context, tokenHash string) error {
	key := refreshTokenKey(tokenHash)
	return c.rdb.Del(ctx, key).Err()
}

func (c *refreshTokenCache) DeleteUserRefreshTokens(ctx context.Context, userID int64) error {
	return revokeUserRefreshFamilies.Run(ctx, c.rdb, []string{userRefreshTokensKey(userID), userTokenFamiliesKey(userID)}, refreshTokenKeyPrefix, tokenFamilyPrefix, revokedFamilyMember).Err()
}

func (c *refreshTokenCache) DeleteTokenFamily(ctx context.Context, familyID string) error {
	return revokeRefreshFamily.Run(ctx, c.rdb, []string{tokenFamilyKey(familyID)}, refreshTokenKeyPrefix, revokedFamilyMember).Err()
}

func (c *refreshTokenCache) AddToUserTokenSet(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error {
	key := userRefreshTokensKey(userID)
	return addRefreshIndex.Run(ctx, c.rdb, []string{key}, tokenHash, ttl.Milliseconds(), revokedFamilyMember).Err()
}

func (c *refreshTokenCache) AddToFamilyTokenSet(ctx context.Context, familyID string, tokenHash string, ttl time.Duration) error {
	key := tokenFamilyKey(familyID)
	return addRefreshIndex.Run(ctx, c.rdb, []string{key}, tokenHash, ttl.Milliseconds(), revokedFamilyMember).Err()
}

func (c *refreshTokenCache) GetUserTokenHashes(ctx context.Context, userID int64) ([]string, error) {
	key := userRefreshTokensKey(userID)
	return c.rdb.SMembers(ctx, key).Result()
}

func (c *refreshTokenCache) GetFamilyTokenHashes(ctx context.Context, familyID string) ([]string, error) {
	key := tokenFamilyKey(familyID)
	hashes, err := c.rdb.SMembers(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		if hash != revokedFamilyMember {
			result = append(result, hash)
		}
	}
	return result, nil
}

func (c *refreshTokenCache) IsTokenInFamily(ctx context.Context, familyID string, tokenHash string) (bool, error) {
	key := tokenFamilyKey(familyID)
	return c.rdb.SIsMember(ctx, key, tokenHash).Result()
}
