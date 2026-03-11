// Package history provides persistent lifecycle event storage for agents.
// Two implementations are available:
//   - RedisHistory    — persists to a Redis List; survives orchestrator restarts.
//   - InMemoryHistory — in-process fallback; data is lost on restart.
package history

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	redis "github.com/redis/go-redis/v9"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestratov1alpha1 "github.com/jacekmyjkowski/k8s-agent-orchestrator/api/v1alpha1"
)

// HistoryStore is the interface for persisting agent lifecycle events.
type HistoryStore interface {
	// Append records a new event. Best-effort: implementations must not panic on errors.
	Append(ctx context.Context, namespace, name, eventType, reason, message string)
	// List returns all recorded events for the agent, oldest first.
	List(ctx context.Context, namespace, name string) []orchestratov1alpha1.LifecycleEvent
	// Clear removes all events for the agent.
	Clear(ctx context.Context, namespace, name string)
}

// ─────────────────────────────── RedisHistory ────────────────────────────────

// RedisHistory persists lifecycle events in a Redis List per agent.
// Key pattern: history:{namespace}/{name}
//
// Events are appended with RPUSH (oldest first) and the list is trimmed to
// maxEntries with LTRIM so the newest events are retained.
type RedisHistory struct {
	client     *redis.Client
	maxEntries int
}

// NewRedisHistory creates a RedisHistory backed by client and capped at maxEntries.
func NewRedisHistory(client *redis.Client, maxEntries int) *RedisHistory {
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return &RedisHistory{client: client, maxEntries: maxEntries}
}

func (r *RedisHistory) key(namespace, name string) string {
	return fmt.Sprintf("history:%s/%s", namespace, name)
}

// Append saves a new event to the Redis List and trims the list to maxEntries.
func (r *RedisHistory) Append(ctx context.Context, namespace, name, eventType, reason, message string) {
	event := orchestratov1alpha1.LifecycleEvent{
		Time:    metav1.Now(),
		Type:    eventType,
		Reason:  reason,
		Message: message,
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	k := r.key(namespace, name)
	p := r.client.Pipeline()
	p.RPush(ctx, k, data)
	// Keep the last maxEntries events (negative start index = from the end).
	p.LTrim(ctx, k, -int64(r.maxEntries), -1)
	_, _ = p.Exec(ctx)
}

// List retrieves all events for the agent, oldest first.
func (r *RedisHistory) List(ctx context.Context, namespace, name string) []orchestratov1alpha1.LifecycleEvent {
	items, err := r.client.LRange(ctx, r.key(namespace, name), 0, -1).Result()
	if err != nil || len(items) == 0 {
		return []orchestratov1alpha1.LifecycleEvent{}
	}
	events := make([]orchestratov1alpha1.LifecycleEvent, 0, len(items))
	for _, item := range items {
		var ev orchestratov1alpha1.LifecycleEvent
		if json.Unmarshal([]byte(item), &ev) == nil {
			events = append(events, ev)
		}
	}
	return events
}

// Clear deletes the entire history list for the agent.
func (r *RedisHistory) Clear(ctx context.Context, namespace, name string) {
	_ = r.client.Del(ctx, r.key(namespace, name)).Err()
}

// ─────────────────────────── InMemoryHistory ─────────────────────────────────

// InMemoryHistory is the in-process fallback HistoryStore.
// Data is lost when the orchestrator restarts.
type InMemoryHistory struct {
	mu         sync.RWMutex
	events     map[string][]orchestratov1alpha1.LifecycleEvent // key: "namespace/name"
	maxEntries int
}

// NewInMemoryHistory creates an InMemoryHistory capped at maxEntries per agent.
func NewInMemoryHistory(maxEntries int) *InMemoryHistory {
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return &InMemoryHistory{
		events:     make(map[string][]orchestratov1alpha1.LifecycleEvent),
		maxEntries: maxEntries,
	}
}

func agentKey(namespace, name string) string {
	return namespace + "/" + name
}

// Append adds a new event for the agent, evicting the oldest if the cap is exceeded.
func (m *InMemoryHistory) Append(_ context.Context, namespace, name, eventType, reason, message string) {
	key := agentKey(namespace, name)
	event := orchestratov1alpha1.LifecycleEvent{
		Time:    metav1.Now(),
		Type:    eventType,
		Reason:  reason,
		Message: message,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events[key] = append(m.events[key], event)
	if len(m.events[key]) > m.maxEntries {
		m.events[key] = m.events[key][len(m.events[key])-m.maxEntries:]
	}
}

// List returns all events for the agent, oldest first.
func (m *InMemoryHistory) List(_ context.Context, namespace, name string) []orchestratov1alpha1.LifecycleEvent {
	key := agentKey(namespace, name)
	m.mu.RLock()
	defer m.mu.RUnlock()
	events := m.events[key]
	if len(events) == 0 {
		return []orchestratov1alpha1.LifecycleEvent{}
	}
	result := make([]orchestratov1alpha1.LifecycleEvent, len(events))
	copy(result, events)
	return result
}

// Clear removes all events for the agent.
func (m *InMemoryHistory) Clear(_ context.Context, namespace, name string) {
	key := agentKey(namespace, name)
	m.mu.Lock()
	delete(m.events, key)
	m.mu.Unlock()
}
