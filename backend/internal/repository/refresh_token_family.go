package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const revokedFamilyMember = "__revoked__"

// Legacy indexing calls must not shorten the lifetime of another token or a
// revocation tombstone, nor repopulate a revoked family after logout.
var addRefreshIndex = redis.NewScript(`
if redis.call('SISMEMBER',KEYS[1],ARGV[3])==1 then return 0 end
local previous=redis.call('PTTL',KEYS[1])
redis.call('SADD',KEYS[1],ARGV[1])
redis.call('PEXPIRE',KEYS[1],math.max(previous,tonumber(ARGV[2])))
return 1
`)

func userTokenFamiliesKey(userID int64) string { return fmt.Sprintf("user_token_families:%d", userID) }

// Indexing and storing are atomic, so logout cannot miss a token being rotated.
var storeRefreshFamily = redis.NewScript(`
if redis.call('SISMEMBER',KEYS[2],ARGV[4]) == 1 then return 0 end
if ARGV[6]~='' and redis.call('EXISTS',ARGV[6])==0 then return 0 end
local now = redis.call('TIME')
local ttl = tonumber(ARGV[5]) - (tonumber(now[1])*1000 + math.floor(tonumber(now[2])/1000))
if ttl <= 0 then return 0 end
redis.call('SET',KEYS[1],ARGV[1],'PX',ttl)
redis.call('SET',KEYS[5],ARGV[3],'PX',ttl)
if ARGV[6]~='' then redis.call('DEL',ARGV[6]) end
for i=2,4 do
 local previous=redis.call('PTTL',KEYS[i])
 redis.call('SADD',KEYS[i], i==4 and ARGV[3] or ARGV[2])
 redis.call('PEXPIRE',KEYS[i],math.max(ttl,previous))
end
return 1
`)

var revokeRefreshFamily = redis.NewScript(`
local ttl=math.max(redis.call('PTTL',KEYS[1]),1)
for _,hash in ipairs(redis.call('SMEMBERS',KEYS[1])) do
 if hash~=ARGV[2] then
  local key=ARGV[1]..hash
  ttl=math.max(ttl,redis.call('PTTL',key))
  redis.call('DEL',key)
 end
end
redis.call('DEL',KEYS[1])
redis.call('SADD',KEYS[1],ARGV[2])
redis.call('PEXPIRE',KEYS[1],ttl)
return 1
`)

var revokeUserRefreshFamilies = redis.NewScript(`
local families={}
for _,family in ipairs(redis.call('SMEMBERS',KEYS[2])) do families[family]=true end
for _,hash in ipairs(redis.call('SMEMBERS',KEYS[1])) do
 local raw=redis.call('GET',ARGV[1]..hash)
 if raw then local data=cjson.decode(raw);families[data.family_id]=true end
end
for family,_ in pairs(families) do
 local familyKey=ARGV[2]..family
 local ttl=math.max(redis.call('PTTL',familyKey),1)
 for _,hash in ipairs(redis.call('SMEMBERS',familyKey)) do
  if hash~=ARGV[3] then
   ttl=math.max(ttl,redis.call('PTTL',ARGV[1]..hash))
   redis.call('DEL',ARGV[1]..hash)
  end
 end
 redis.call('DEL',familyKey)
 redis.call('SADD',familyKey,ARGV[3])
 redis.call('PEXPIRE',familyKey,ttl)
end
for _,hash in ipairs(redis.call('SMEMBERS',KEYS[1])) do redis.call('DEL',ARGV[1]..hash) end
redis.call('DEL',KEYS[1],KEYS[2])
return 1
`)

var activeRefreshFamily = redis.NewScript(`
if redis.call('SISMEMBER',KEYS[1],ARGV[3])==1 then return 0 end
local remaining=0
for _,hash in ipairs(redis.call('SMEMBERS',KEYS[1])) do
 local key=ARGV[1]..hash
 local raw=redis.call('GET',key)
 if raw then
  local data=cjson.decode(raw)
  if tostring(data.user_id)==ARGV[2] then remaining=math.max(remaining,redis.call('PTTL',key)) end
 end
end
return remaining
`)

func (c *refreshTokenCache) ActiveFamilyDeadline(ctx context.Context, userID int64, family string) (time.Time, error) {
	ms, err := activeRefreshFamily.Run(ctx, c.rdb, []string{tokenFamilyKey(family)}, refreshTokenKeyPrefix, fmt.Sprint(userID), revokedFamilyMember).Int64()
	if err != nil {
		return time.Time{}, err
	}
	if ms <= 0 {
		return time.Time{}, nil
	}
	return time.Now().Add(time.Duration(ms) * time.Millisecond), nil
}

func (c *refreshTokenCache) storeIndexedRefreshToken(ctx context.Context, hash string, data *service.RefreshTokenData, value string, ttl time.Duration) error {
	return c.replaceIndexedRefreshToken(ctx, "", hash, data, value, ttl)
}

func (c *refreshTokenCache) replaceIndexedRefreshToken(ctx context.Context, oldHash, hash string, data *service.RefreshTokenData, value string, ttl time.Duration) error {
	expires := time.Now().Add(ttl)
	if !data.ExpiresAt.IsZero() && data.ExpiresAt.Before(expires) {
		expires = data.ExpiresAt
	}
	oldKey := ""
	if oldHash != "" {
		oldKey = refreshTokenKey(oldHash)
	}
	stored, err := storeRefreshFamily.Run(ctx, c.rdb, []string{refreshTokenKey(hash), tokenFamilyKey(data.FamilyID), userRefreshTokensKey(data.UserID), userTokenFamiliesKey(data.UserID), "refresh_token_family:" + hash}, value, hash, data.FamilyID, revokedFamilyMember, expires.UnixMilli(), oldKey).Int()
	if err != nil {
		return err
	}
	if stored != 1 {
		return service.ErrRefreshTokenInvalid
	}
	return nil
}

func (c *refreshTokenCache) FamilyForRefreshHash(ctx context.Context, hash string) (string, error) {
	family, err := c.rdb.Get(ctx, "refresh_token_family:"+hash).Result()
	if err == redis.Nil {
		return "", nil
	}
	return family, err
}
