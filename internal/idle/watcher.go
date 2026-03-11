// Package idle implements automatic agent pausing after a configurable idle timeout.
// Activity is tracked via the shared AgentCacheManager using the reserved key
// "_idle_last_activity". The Watcher runs as a background goroutine and calls
// client.Patch to set spec.paused=true on idle agents.
package idle

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestratorv1alpha1 "github.com/jacekmyjkowski/k8s-agent-orchestrator/api/v1alpha1"
	"github.com/jacekmyjkowski/k8s-agent-orchestrator/internal/cache"
)

// ActivityKey is the cache field used to record the last activity timestamp.
const ActivityKey = "_idle_last_activity"

var log = ctrl.Log.WithName("idle-watcher")

// Watcher periodically checks all Agent CRs and pauses those that have been
// idle for longer than their effective timeout.
type Watcher struct {
	// Client is the controller-runtime client used to list and patch Agent CRs.
	Client client.Client
	// Cache is the shared cache where activity timestamps are stored.
	Cache cache.CacheStore
	// GlobalTimeout is the fallback idle timeout applied to every agent that does
	// not specify spec.idleTimeout. Zero disables idle tracking globally.
	GlobalTimeout time.Duration
	// CheckInterval controls how often the watcher scans all agents.
	CheckInterval time.Duration
}

// Start runs the idle check loop until ctx is cancelled.
// Intended to be called in a goroutine.
func (w *Watcher) Start(ctx context.Context) {
	if w.CheckInterval <= 0 {
		w.CheckInterval = 30 * time.Second
	}
	ticker := time.NewTicker(w.CheckInterval)
	defer ticker.Stop()

	log.Info("Idle watcher started",
		"globalTimeout", w.GlobalTimeout,
		"checkInterval", w.CheckInterval,
	)

	for {
		select {
		case <-ctx.Done():
			log.Info("Idle watcher stopped")
			return
		case <-ticker.C:
			w.check(ctx)
		}
	}
}

// TouchActivity records the current time as the last activity for an agent.
// The timestamp is stored as Unix nanoseconds (int64) so it round-trips cleanly
// through JSON when using a Redis-backed CacheStore.
func (w *Watcher) TouchActivity(namespace, name string) {
	w.Cache.Set(namespace, name, ActivityKey, time.Now().UnixNano(), 0)
}

// check iterates over all Agent CRs and pauses those that exceeded their timeout.
func (w *Watcher) check(ctx context.Context) {
	agentList := &orchestratorv1alpha1.AgentList{}
	if err := w.Client.List(ctx, agentList); err != nil {
		log.Error(err, "idle watcher: failed to list agents")
		return
	}

	now := time.Now()

	for i := range agentList.Items {
		agent := &agentList.Items[i]

		// Already paused — nothing to do.
		if agent.Spec.Paused {
			continue
		}

		// Determine effective timeout.
		timeout := w.GlobalTimeout
		if agent.Spec.IdleTimeout > 0 {
			timeout = time.Duration(agent.Spec.IdleTimeout) * time.Second
		}
		if timeout == 0 {
			continue // idle tracking disabled for this agent
		}

		val, ok := w.Cache.Get(agent.Namespace, agent.Name, ActivityKey)
		if !ok {
			// No activity recorded yet — seed with now and check next tick.
			w.TouchActivity(agent.Namespace, agent.Name)
			continue
		}

		// The value is stored as int64 UnixNano. When read back through a Redis
		// CacheStore the JSON round-trip yields float64, so handle both.
		var lastActivityNano int64
		switch v := val.(type) {
		case int64:
			lastActivityNano = v
		case float64:
			lastActivityNano = int64(v)
		default:
			w.TouchActivity(agent.Namespace, agent.Name)
			continue
		}
		lastActivity := time.Unix(0, lastActivityNano)

		if now.Sub(lastActivity) < timeout {
			continue // still within timeout window
		}

		log.Info("Agent idle — pausing",
			"agent", agent.Namespace+"/"+agent.Name,
			"idleDuration", now.Sub(lastActivity).Round(time.Second),
			"timeout", timeout,
		)
		w.pauseAgent(ctx, agent)
	}
}

// pauseAgent patches the Agent CR to set spec.paused=true.
func (w *Watcher) pauseAgent(ctx context.Context, agent *orchestratorv1alpha1.Agent) {
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.Paused = true
	if agent.Annotations == nil {
		agent.Annotations = make(map[string]string)
	}
	agent.Annotations["orchestrator.dev/stopped-at"] = time.Now().UTC().Format(time.RFC3339)
	agent.Annotations["orchestrator.dev/stop-reason"] = "idle-timeout"

	if err := w.Client.Patch(ctx, agent, patch); err != nil {
		log.Error(err, "idle watcher: failed to pause agent",
			"agent", agent.Namespace+"/"+agent.Name)
	}
}
