// Package cache provides a thread-safe, namespaced cache for agent state.
// Each agent gets its own isolated key-value namespace.
// Use NewInMemoryCache for in-process storage or NewRedisCache for Redis-backed storage.
package cache

import (
	"sync"
	"time"
)

// CacheStore is the interface for the per-agent key-value cache.
// Implementations must be safe for concurrent use.
type CacheStore interface {
	Set(namespace, name, field string, value interface{}, ttl time.Duration)
	Get(namespace, name, field string) (interface{}, bool)
	GetEntry(namespace, name, field string) (*Entry, bool)
	Delete(namespace, name, field string)
	List(namespace, name string) map[string]*Entry
	ClearAgent(namespace, name string)
}

// Entry represents a single cached value with optional TTL.
type Entry struct {
	Value     interface{} `json:"value"`
	CreatedAt time.Time   `json:"created_at"`
	ExpiresAt *time.Time  `json:"expires_at,omitempty"`
}

// IsExpired returns true if the entry has a TTL and it has passed.
func (e *Entry) IsExpired() bool {
	if e.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*e.ExpiresAt)
}

// agentCache is the per-agent key-value store.
type agentCache struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

// InMemoryCache manages isolated cache namespaces per agent (in-process, volatile).
type InMemoryCache struct {
	mu     sync.RWMutex
	agents map[string]*agentCache // key: "<namespace>/<name>"
}

// NewInMemoryCache creates a new in-memory cache manager with a background GC loop.
func NewInMemoryCache() *InMemoryCache {
	m := &InMemoryCache{
		agents: make(map[string]*agentCache),
	}
	go m.gcLoop()
	return m
}

func agentKey(namespace, name string) string {
	return namespace + "/" + name
}

func (m *InMemoryCache) getOrCreate(namespace, name string) *agentCache {
	key := agentKey(namespace, name)
	m.mu.RLock()
	ac, ok := m.agents[key]
	m.mu.RUnlock()
	if ok {
		return ac
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// double-check
	if ac, ok = m.agents[key]; ok {
		return ac
	}
	ac = &agentCache{entries: make(map[string]*Entry)}
	m.agents[key] = ac
	return ac
}

// Set stores a value under the given field key for an agent.
// ttl = 0 means no expiration.
func (m *InMemoryCache) Set(namespace, name, field string, value interface{}, ttl time.Duration) {
	ac := m.getOrCreate(namespace, name)
	entry := &Entry{
		Value:     value,
		CreatedAt: time.Now(),
	}
	if ttl > 0 {
		exp := time.Now().Add(ttl)
		entry.ExpiresAt = &exp
	}
	ac.mu.Lock()
	ac.entries[field] = entry
	ac.mu.Unlock()
}

// Get retrieves a value from the agent's cache. Returns (value, found).
func (m *InMemoryCache) Get(namespace, name, field string) (interface{}, bool) {
	key := agentKey(namespace, name)
	m.mu.RLock()
	ac, ok := m.agents[key]
	m.mu.RUnlock()
	if !ok {
		return nil, false
	}
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	entry, exists := ac.entries[field]
	if !exists || entry.IsExpired() {
		return nil, false
	}
	return entry.Value, true
}

// GetEntry retrieves the full entry metadata.
func (m *InMemoryCache) GetEntry(namespace, name, field string) (*Entry, bool) {
	key := agentKey(namespace, name)
	m.mu.RLock()
	ac, ok := m.agents[key]
	m.mu.RUnlock()
	if !ok {
		return nil, false
	}
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	entry, exists := ac.entries[field]
	if !exists || entry.IsExpired() {
		return nil, false
	}
	return entry, true
}

// Delete removes a specific field from an agent's cache.
func (m *InMemoryCache) Delete(namespace, name, field string) {
	key := agentKey(namespace, name)
	m.mu.RLock()
	ac, ok := m.agents[key]
	m.mu.RUnlock()
	if !ok {
		return
	}
	ac.mu.Lock()
	delete(ac.entries, field)
	ac.mu.Unlock()
}

// List returns all non-expired key-entry pairs for an agent.
func (m *InMemoryCache) List(namespace, name string) map[string]*Entry {
	key := agentKey(namespace, name)
	m.mu.RLock()
	ac, ok := m.agents[key]
	m.mu.RUnlock()
	result := make(map[string]*Entry)
	if !ok {
		return result
	}
	ac.mu.RLock()
	defer ac.mu.RUnlock()
	for k, e := range ac.entries {
		if !e.IsExpired() {
			result[k] = e
		}
	}
	return result
}

// ClearAgent removes the entire cache namespace for an agent.
func (m *InMemoryCache) ClearAgent(namespace, name string) {
	key := agentKey(namespace, name)
	m.mu.Lock()
	delete(m.agents, key)
	m.mu.Unlock()
}

// gcLoop periodically removes expired entries from all caches.
func (m *InMemoryCache) gcLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.RLock()
		keys := make([]string, 0, len(m.agents))
		for k := range m.agents {
			keys = append(keys, k)
		}
		m.mu.RUnlock()

		for _, k := range keys {
			m.mu.RLock()
			ac := m.agents[k]
			m.mu.RUnlock()
			if ac == nil {
				continue
			}
			ac.mu.Lock()
			for field, e := range ac.entries {
				if e.IsExpired() {
					delete(ac.entries, field)
				}
			}
			ac.mu.Unlock()
		}
	}
}
