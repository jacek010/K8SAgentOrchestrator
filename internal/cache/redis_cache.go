// Package cache — Redis-backed implementation of CacheStore.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCache implements CacheStore using Redis as the backend.
//
// Key layout:
//   - Field value:  cache:{namespace}/{name}:{field}  (SET with optional TTL)
//   - Field index:  cache-idx:{namespace}/{name}       (Redis Set of field names)
//
// The index set is used by List and ClearAgent to avoid a full SCAN.
type RedisCache struct {
	client *redis.Client
}

// NewRedisCache creates a RedisCache backed by the given client.
func NewRedisCache(client *redis.Client) *RedisCache {
	return &RedisCache{client: client}
}

// redisEntry is the JSON-serializable form of Entry stored in Redis.
type redisEntry struct {
	Value     json.RawMessage `json:"value"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt *time.Time      `json:"expires_at,omitempty"`
}

func (r *RedisCache) fieldKey(namespace, name, field string) string {
	return fmt.Sprintf("cache:%s/%s:%s", namespace, name, field)
}

func (r *RedisCache) indexKey(namespace, name string) string {
	return fmt.Sprintf("cache-idx:%s/%s", namespace, name)
}

// Set stores a value with an optional TTL.
func (r *RedisCache) Set(namespace, name, field string, value interface{}, ttl time.Duration) {
	ctx := context.Background()
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	now := time.Now()
	re := redisEntry{Value: raw, CreatedAt: now}
	if ttl > 0 {
		exp := now.Add(ttl)
		re.ExpiresAt = &exp
	}
	data, err := json.Marshal(re)
	if err != nil {
		return
	}
	key := r.fieldKey(namespace, name, field)
	idxKey := r.indexKey(namespace, name)

	p := r.client.Pipeline()
	p.Set(ctx, key, data, ttl)
	p.SAdd(ctx, idxKey, field)
	// Keep index alive at least as long as the longest TTL entry; for no-TTL
	// entries we set a long expiry (24 h) that updates on every write.
	if ttl > 0 {
		p.Expire(ctx, idxKey, ttl*2)
	} else {
		p.Expire(ctx, idxKey, 24*time.Hour)
	}
	_, _ = p.Exec(ctx)
}

// Get retrieves a value. Returns (nil, false) if missing or expired.
func (r *RedisCache) Get(namespace, name, field string) (interface{}, bool) {
	entry, ok := r.GetEntry(namespace, name, field)
	if !ok {
		return nil, false
	}
	return entry.Value, true
}

// GetEntry retrieves the full entry metadata.
func (r *RedisCache) GetEntry(namespace, name, field string) (*Entry, bool) {
	ctx := context.Background()
	data, err := r.client.Get(ctx, r.fieldKey(namespace, name, field)).Bytes()
	if err != nil {
		return nil, false
	}
	var re redisEntry
	if err := json.Unmarshal(data, &re); err != nil {
		return nil, false
	}
	var value interface{}
	_ = json.Unmarshal(re.Value, &value)
	return &Entry{Value: value, CreatedAt: re.CreatedAt, ExpiresAt: re.ExpiresAt}, true
}

// Delete removes a specific field.
func (r *RedisCache) Delete(namespace, name, field string) {
	ctx := context.Background()
	p := r.client.Pipeline()
	p.Del(ctx, r.fieldKey(namespace, name, field))
	p.SRem(ctx, r.indexKey(namespace, name), field)
	_, _ = p.Exec(ctx)
}

// List returns all non-expired entries for an agent.
func (r *RedisCache) List(namespace, name string) map[string]*Entry {
	ctx := context.Background()
	result := make(map[string]*Entry)
	fields, err := r.client.SMembers(ctx, r.indexKey(namespace, name)).Result()
	if err != nil || len(fields) == 0 {
		return result
	}
	for _, field := range fields {
		if entry, ok := r.GetEntry(namespace, name, field); ok {
			result[field] = entry
		} else {
			// Field expired or missing — prune stale index entry.
			_ = r.client.SRem(ctx, r.indexKey(namespace, name), field)
		}
	}
	return result
}

// ClearAgent removes all cache entries for an agent.
func (r *RedisCache) ClearAgent(namespace, name string) {
	ctx := context.Background()
	idxKey := r.indexKey(namespace, name)
	fields, err := r.client.SMembers(ctx, idxKey).Result()
	if err != nil {
		return
	}
	keys := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		keys = append(keys, r.fieldKey(namespace, name, f))
	}
	keys = append(keys, idxKey)
	_ = r.client.Del(ctx, keys...).Err()
}
