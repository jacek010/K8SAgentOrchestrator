# K8s Agent Orchestrator

A Kubernetes operator written in **Go** that manages **Agent** custom resources — each Agent maps 1-to-1 to a Pod. The orchestrator exposes a **REST API** for full agent lifecycle control, env-var hot-patching, log streaming, and a per-agent key-value cache (Redis-backed or in-memory).

---

## Table of Contents

- [Architecture](#architecture)
  - [Agent lifecycle phases](#agent-lifecycle-phases)
- [Prerequisites](#prerequisites)
- [Running locally](#running-locally)
- [Web UI](#web-ui)
  - [Features](#features)
  - [CLI flag](#cli-flag)
- [Deploying with Helm (production)](#deploying-with-helm-production)
- [REST API Reference](#rest-api-reference)
  - [Namespace in URLs](#namespace-in-urls)
  - [Health](#health)
  - [Agents — CRUD](#agents--crud)
  - [Lifecycle control](#lifecycle-control)
  - [Idle Auto-Stop & Keepalive](#idle-auto-stop--keepalive)
  - [Self-healing](#self-healing)
  - [Environment variables](#environment-variables)
  - [Connectivity — Services](#connectivity--services)
  - [Lifecycle History](#lifecycle-history)
  - [Logs](#logs)
  - [Cache](#cache)
- [Agent CR — direct kubectl usage](#agent-cr--direct-kubectl-usage)
- [Makefile targets](#makefile-targets)
- [Security](#security)
- [Project structure](#project-structure)
- [TODO](#todo)

---

## Architecture

```
┌─────────────┐   REST :8082   ┌─────────────────────────-─┐
│   Client    │ ◄────────────► │   REST API (Gin)          │
│   (A2A/CLI) │                │   internal/rest/          │
└──────┬──────┘                └────────────┬─────────────-┘
       │  POST /keepalive                   │ controller-runtime client
       │  (wake + idle reset)  ┌────────────▼─────────────--┐
       └──────────────────────►│   Agent CRD                │
                               │   orchestrator.dev/v1alpha1│
                               └────────────┬─────────────--┘
                                            │ reconcile loop
                          ┌─────────────────┴─────────────────┐
                          │                                   │
               ┌──────────▼──────────┐            ┌───────────▼──────────-┐
               │   Pod  (1 per Agent)│◄──────────►│  ClusterIP Service    │
               │                     │  direct    │  (optional, per Agent)│
               └─────────────────────┘ traffic    └──────────────────────-┘

┌──────────────────────────────────────────────────────────────┐
│  Idle Watcher (goroutine)   internal/idle/watcher.go         │
│  Polls all Agents every --idle-check-interval seconds.       │
│  Sets spec.paused=true when inactivity > effectiveTimeout.   │
└──────────────────────────────────────────────────────────────┘

┌─────────────┐   HTTP :8083   ┌──────────────────────────────┐
│  Browser    │ ◄────────────► │  Web UI (embedded SPA)       │
│             │                │  internal/ui/                │
│             │   REST :8082   └──────────────────────────────┘
│             │ ◄────────────► REST API (Gin)
└─────────────┘

Sidecar services:
  :8080  Prometheus metrics
  :8081  Health / readiness probes
  :8083  Web UI dashboard
```

| Component | Description |
|-----------|-------------|
| **Agent CRD** | Declares desired state (image, env, resources, restart policy, servicePort, idleTimeout…) |
| **Controller** | Reconcile loop: creates/deletes/restarts Pods and Services from CR; handles finalizer and self-healing |
| **REST API** | Gin HTTP server on `:8082` — CRUD agents, patch env, stream logs, manage cache, list service URLs, view history, keepalive |
| **Idle Watcher** | Background goroutine: pauses agents that exceeded their idle timeout; activity tracked via cache |
| **Cache** | Thread-safe, per-agent, TTL key-value store backed by **Redis** (or in-process memory fallback) |
| **Lifecycle History** | Append-only event log stored in **Redis** (or in-memory); persists across restarts; cap via `--history-max-entries` |
| **Redis** | Optional sidecar (`--redis-addr`): shared cache + persistent history; Bitnami subchart bundled in Helm chart |
| **Web UI** | Browser-based dashboard on `:8083` — full CRUD, phase badges, history, keepalive; pure vanilla JS, no build step |
| **Helm chart** | Deployment, Service, ServiceAccount, namespaced Role/RoleBinding, CRD |

### Agent lifecycle phases

| Phase | Meaning |
|-------|---------|
| `Pending` | CR created, Pod not yet scheduled |
| `Running` | Pod is running |
| `Failed` | Pod exited with error |
| `Stopped` | `spec.paused=true`, pod deleted and not re-created |
| `Updating` | Spec change detected, pod being replaced |
| `Restoring` | Agent CR deleted externally, self-healing resurrection in progress |

---

## Prerequisites

| Tool | Version |
|------|---------|
| Go | ≥ 1.22 |
| kubectl | any recent |
| Running K8s cluster | (local: `kind`, `minikube`, `k3d`, …) |
| Helm | ≥ 3 _(optional, production deploy)_ |

```bash
# Install Go (macOS)
brew install go
echo 'export PATH="$PATH:$(go env GOPATH)/bin"' >> ~/.zshrc && source ~/.zshrc
```

---

## Running locally

```bash
make dev
```

This single target:
1. Applies the `Agent` CRD to the cluster.
2. Runs `go mod tidy -e` (resolves dependencies, tolerates unreachable test-only deps).
3. Starts the controller manager + REST API + idle watcher.

For testing idle auto-stop with a short timeout:
```bash
go run ./cmd/main.go --debug=true --idle-timeout-default=30 --idle-check-interval=10
```

Ports after startup:

| Port | Purpose |
|------|---------|
| `8082` | REST API || `8083` | Web UI dashboard || `8080` | Prometheus metrics (`/metrics`) |
| `8081` | Health probes (`/healthz`, `/readyz`) |

---

## Web UI

Open **http://localhost:8083** in a browser after starting the orchestrator.

The dashboard is a single HTML file embedded in the binary (`internal/ui/static/index.html`) — no Node.js, no CDN, no build step required.

### Features

| Feature | Description |
|---------|-------------|
| **Agents table** | Lists all agents with phase badge, pod name, image, service port, idle timeout and age. Auto-refreshes every 5 s. |
| **Phase badges** | Color-coded: Running (green), Stopped (gray), Pending (yellow), Failed (red), Updating (blue), Restoring (purple) |
| **Create agent** | Modal form — name, image, command, args, env vars (`KEY=VALUE` per line), service port, idle timeout, resource requests/limits, self-healing toggle |
| **Stop / Start / Restart** | Per-row action buttons; contextually enabled based on current phase |
| **Delete** | Confirmation dialog before deletion |
| **Detail modal** | Three-tab view: Overview (full spec + actions), History (lifecycle events, newest first), Keepalive (wake agent, reset idle timer, display SVC URL) |
| **Search** | Client-side filter by name or image |
| **Stats bar** | Running / Stopped / other counts |

### CLI flag

| Flag | Default | Description |
|------|---------|-------------|
| `--ui-bind-address` | `:8083` | Address the Web UI listens on. Set to empty string to disable. |

```bash
# Custom port
go run ./cmd/main.go --ui-bind-address=:9090
```

> **CORS:** The REST API on `:8082` is configured with `Access-Control-Allow-Origin: *`, so the browser can call it directly from any origin (including `:8083`).

---

## Deploying with Helm (production)

```bash
# Build and push image
make docker-build-push IMAGE_REPO=your-registry/k8s-agent-orchestrator IMAGE_TAG=v1.0.0

# Install into the 'orchestrator' namespace
make helm-install IMAGE_REPO=your-registry/k8s-agent-orchestrator IMAGE_TAG=v1.0.0

# Uninstall (CRD is preserved)
make helm-uninstall
```

Key Helm values:

| Value | Default | Description |
|-------|---------|-------------|
| `image.repository` | `ghcr.io/jacekmyjkowski/k8s-agent-orchestrator` | Image repo |
| `image.tag` | chart `appVersion` | Image tag |
| `watchNamespace` | release namespace | Namespace to watch **and** default namespace for short REST API URLs (`/api/v1/agents/...`) |
| `leaderElect` | `false` | Enable leader election |
| `rbac.create` | `true` | Create Role + RoleBinding |
| `service.type` | `ClusterIP` | Service type |
| `service.restPort` | `8082` | REST API port |

---

## REST API Reference

**Base URL:** `http://localhost:8082`

All JSON responses use the envelope format:
```json
{ "status": "ok|created|deleted|error", "data": <payload> }
```
Error responses replace `"data"` with `"message": "<reason>"`.

### Namespace in URLs

Every agent endpoint is available in **two equivalent forms**:

| Form | URL pattern | Namespace used |
|------|-------------|----------------|
| **Short** (recommended) | `/api/v1/agents/...` | Default namespace (`default` locally, `.Release.Namespace` via Helm) |
| **Explicit** | `/api/v1/namespaces/{namespace}/agents/...` | Namespace from the URL |

Both forms accept identical bodies and return identical responses. Use the explicit form only when you need to target a namespace different from the default.

```bash
# Short form — uses default namespace
curl http://localhost:8082/api/v1/agents

# Explicit form — targets 'production' namespace
curl http://localhost:8082/api/v1/namespaces/production/agents
```

All examples below use the **short form**. Replace `/api/v1/agents` with `/api/v1/namespaces/{namespace}/agents` whenever you need a specific namespace.

---

### Health

#### `GET /healthz`
Liveness check — always `200` if the process is alive.
```bash
curl http://localhost:8082/healthz
# {"status":"ok"}
```

#### `GET /readyz`
Readiness check — `200` if the K8s API is reachable, `503` otherwise.
```bash
curl http://localhost:8082/readyz
```

---

### Agents — CRUD

#### `POST /api/v1/agents`
Create a new Agent (and its Pod).

**Required:** `name`, `image`

```bash
curl -X POST http://localhost:8082/api/v1/agents \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "my-agent",
    "image": "busybox:1.36",
    "command": ["sh", "-c", "while true; do echo hello; sleep 5; done"],
    "restartPolicy": "Always",
    "env": [
      {"name": "LOG_LEVEL", "value": "debug"},
      {"name": "REGION",    "value": "eu-west-1"}
    ],
    "resources": {
      "requests": {"cpu": "50m", "memory": "64Mi"},
      "limits":   {"cpu": "200m","memory": "128Mi"}
    },
    "podLabels": {"team": "platform"},
    "servicePort": 8080
}'
```

Full body schema:

| Field | Type | Required | Description |
|-------|------|:--------:|-------------|
| `name` | string | ✅ | Agent / Pod name |
| `image` | string | ✅ | Container image |
| `imagePullPolicy` | `Always`\|`IfNotPresent`\|`Never` | | Default: `IfNotPresent` |
| `command` | `[]string` | | Overrides container `ENTRYPOINT` |
| `args` | `[]string` | | Overrides container `CMD` |
| `env` | `[]EnvVar` | | `[{"name":"X","value":"Y"}]` |
| `envFrom` | `[]EnvFromSource` | | ConfigMap / Secret bulk env |
| `resources` | `ResourceRequirements` | | `requests` / `limits` |
| `serviceAccountName` | string | | ServiceAccount for the Pod |
| `restartPolicy` | `Always`\|`OnFailure`\|`Never` | | Default: `Always` |
| `podLabels` | `map[string]string` | | Extra labels on the Pod |
| `podAnnotations` | `map[string]string` | | Extra annotations on the Pod |
| `labels` | `map[string]string` | | Labels on the Agent CR |
| `annotations` | `map[string]string` | | Annotations on the Agent CR |
| `paused` | bool | | `true` = delete Pod and halt reconciliation (stop the agent) |
| `selfHealingDisabled` | bool | | `true` = disable automatic resurrection. **Default: false (self-healing ON)** |
| `servicePort` | int (1-65535) | | When non-zero the controller creates a **ClusterIP Service** on this port. Set to `0` or omit to disable. |
| `serviceProtocol` | `TCP`\|`UDP`\|`SCTP` | | Protocol for the Service port. Default: `TCP` |
| `idleTimeout` | int (seconds) | | Auto-pause agent after N seconds of inactivity. `0` = use global `--idle-timeout-default` (or disabled if that is also `0`). |

**Response:** `201 Created`

---

#### `GET /api/v1/agents`
List all Agents in the default namespace.
```bash
curl http://localhost:8082/api/v1/agents
```

---

#### `GET /api/v1/agents/:name`
Get a single Agent with its current status (`.status.phase`, `.status.podName`, `.status.conditions`, `.status.serviceName`).
```bash
curl http://localhost:8082/api/v1/agents/my-agent
```

---

#### `PUT /api/v1/agents/:name`
Update Agent spec. Only non-zero fields in the body are applied. Triggers pod recreation.
```bash
curl -X PUT http://localhost:8082/api/v1/agents/my-agent \
  -H 'Content-Type: application/json' \
  -d '{"image": "busybox:1.37"}'
```

---

#### `DELETE /api/v1/agents/:name`
Delete the Agent CR (Pod is garbage-collected via finalizer). Also clears the agent's cache entries.
```bash
curl -X DELETE http://localhost:8082/api/v1/agents/my-agent
# {"status":"deleted","name":"my-agent"}
```

---

### Lifecycle control

#### `POST /api/v1/agents/:name/restart`
Force pod recreation by bumping the `orchestrator.dev/restart-at` annotation.
```bash
curl -X POST http://localhost:8082/api/v1/agents/my-agent/restart
# {"status":"ok","data":{"restarted":true}}
```

---

#### `POST /api/v1/agents/:name/stop`
Stop the agent: sets `spec.paused=true`. The controller deletes the Pod and does not recreate it until `/start` is called.
```bash
curl -X POST http://localhost:8082/api/v1/agents/my-agent/stop
# {"status":"ok","data":{"stopped":true}}
```

---

#### `POST /api/v1/agents/:name/start`
Start (resume) a paused agent. Sets `spec.paused=false` and forces pod recreation.
```bash
curl -X POST http://localhost:8082/api/v1/agents/my-agent/start
# {"status":"ok","data":{"started":true}}
```

---

### Idle Auto-Stop & Keepalive

The orchestrator can automatically pause (stop) agents that have been inactive for a configurable period. This is useful with A2A agents that should not consume cluster resources while idle.

**How it works:**
1. Every REST API call on a named agent (`/:name/*`) resets a per-agent activity timestamp stored in the cache store.
2. A background **Idle Watcher** goroutine polls all Agents every `--idle-check-interval` seconds.
3. If `now − lastActivity > effectiveTimeout`, the watcher sets `spec.paused=true` (Pod is deleted).
4. The **effective timeout** per agent is: `spec.idleTimeout` if `> 0`, otherwise `--idle-timeout-default`; `0` disables idle tracking.

**A2A session pattern:**
- Before initiating conversation: call `POST /keepalive` → orchestrator wakes the agent and returns `svcUrl`.
- Communicate directly with the agent via the ClusterIP Service (`svcUrl`) — orchestrator is **not** in the data path.
- Periodically call `POST /keepalive?wait=0` during the session (every `idleTimeout/2` seconds) to prevent auto-stop.
- After the session ends: do nothing — agent stops automatically after `idleTimeout`.

#### CLI flags

| Flag | Default | Description |
|------|---------|-------------|
| `--idle-timeout-default` | `0` | Global idle timeout in seconds. `0` = disabled globally (per-agent `spec.idleTimeout` still works). |
| `--idle-check-interval` | `30` | How often (seconds) the watcher scans all agents. |
| `--ui-bind-address` | `:8083` | Address the Web UI dashboard listens on. |
| `--redis-addr` | `""` | Redis server address (`host:port`). Leave empty to use in-memory fallback for cache and history. |
| `--redis-password` | `""` | Redis password. Leave empty if auth is disabled. |
| `--redis-db` | `0` | Redis database number. |
| `--history-max-entries` | `1000` | Maximum history entries per agent. Oldest are evicted when the cap is reached. |

#### `POST /api/v1/agents/:name/keepalive`
Resets the idle timer. If the agent is paused, wakes it and waits up to `?wait` seconds for `Running` phase.
Returns current status and the ClusterIP service URL for direct A2A communication.

| Query param | Type | Default | Description |
|-------------|------|---------|-------------|
| `wait` | int (0-120) | `30` | Max seconds to wait for `Running`. Use `0` to only reset the timer without blocking. |

```bash
# Wake agent and wait up to 30s for Running
curl -X POST "http://localhost:8082/api/v1/agents/my-agent/keepalive?wait=30"
```
```json
{
  "status": "ok",
  "data": {
    "status":  "running",
    "phase":   "Running",
    "svcUrl":  "http://my-agent.default.svc.cluster.local:8080",
    "elapsed": "4.2s"
  }
}
```

Possible `status` values:

| Value | Meaning |
|-------|---------|
| `running` | Agent is Running; `svcUrl` is ready for use |
| `starting` | Agent was woken but did not reach Running within `wait` seconds — retry |
| `accepted` | `wait=0` was used; wake command sent but no polling performed |

```bash
# Only reset idle timer, don't wait (fire-and-forget during active A2A session)
curl -X POST "http://localhost:8082/api/v1/agents/my-agent/keepalive?wait=0"
```

---

### Self-healing

Every agent has **self-healing enabled by default**. If the Agent CR is deleted externally (e.g. `kubectl delete agt my-agent`), the orchestrator automatically recreates it with the same spec after the deletion is finalised.

Self-healing operates at two levels:

| Level | Mechanism | Always active |
|-------|-----------|:-:|
| **Pod** | `Owns(&corev1.Pod{})` — pod deletion triggers immediate reconcile → pod recreated | ✅ |
| **Agent CR** | Finalizer-based resurrection — CR deleted → goroutine recreates new CR with same spec | ✅ (unless disabled) |

**Lifecycle history survives resurrection.** Events are written to the history store (Redis or in-memory) under a stable key scoped to `{namespace}/{name}`. When the controller recreates the CR during self-healing, it appends a `Resurrected` event to the same history — the `/history` endpoint always shows a complete audit trail, even across multiple self-healing cycles. With Redis, history also survives orchestrator restarts.

#### `POST /api/v1/agents/:name/disable-healing`
Disable automatic resurrection for this agent (`spec.selfHealingDisabled=true`). The Agent CR will be permanently deleted when `kubectl delete` is called.
```bash
curl -X POST http://localhost:8082/api/v1/agents/my-agent/disable-healing
# {"status":"ok","data":{"selfHealingDisabled":true}}
```

#### `POST /api/v1/agents/:name/enable-healing`
Re-enable automatic resurrection (`spec.selfHealingDisabled=false`). This is the default state for all newly created agents.
```bash
curl -X POST http://localhost:8082/api/v1/agents/my-agent/enable-healing
# {"status":"ok","data":{"selfHealingDisabled":false}}
```

> **Tip:** To permanently delete a self-healing agent, either call `/disable-healing` first, or use the REST `DELETE` endpoint which bypasses self-healing.

---

### Environment variables

All env changes are applied to the Agent spec immediately and trigger a pod recreation.

#### `GET /api/v1/agents/:name/env`
Return the current env list.
```bash
curl http://localhost:8082/api/v1/agents/my-agent/env
# {"status":"ok","data":[{"name":"LOG_LEVEL","value":"debug"}]}
```

---

#### `PUT /api/v1/agents/:name/env`
**Replace** the entire env list (destructive).
```bash
curl -X PUT http://localhost:8082/api/v1/agents/my-agent/env \
  -H 'Content-Type: application/json' \
  -d '{"env":[{"name":"LOG_LEVEL","value":"info"},{"name":"PORT","value":"8080"}]}'
```

---

#### `PATCH /api/v1/agents/:name/env`
**Merge / upsert** individual env vars — existing keys are updated, new keys are appended, unmentioned keys are left intact.
```bash
curl -X PATCH http://localhost:8082/api/v1/agents/my-agent/env \
  -H 'Content-Type: application/json' \
  -d '{"env":[{"name":"LOG_LEVEL","value":"warn"},{"name":"NEW_VAR","value":"hello"}]}'
```

---

#### `DELETE /api/v1/agents/:name/env/:key`
Remove a single env var by name.
```bash
curl -X DELETE http://localhost:8082/api/v1/agents/my-agent/env/LOG_LEVEL
```

---

### Connectivity — Services

When an Agent is created with `servicePort > 0`, the controller automatically creates a **ClusterIP Service** named after the agent. The Service selector matches the agent's Pod, enabling stable in-cluster DNS access without knowing the Pod IP.

#### `GET /api/v1/agents/services`
Return the ClusterIP DNS URL for every Agent that has a `servicePort` configured.

```bash
curl http://localhost:8082/api/v1/agents/services
```
```json
{
  "status": "ok",
  "data": [
    {
      "agent":     "my-agent",
      "namespace": "default",
      "port":      8080,
      "protocol":  "TCP",
      "url":       "http://my-agent.default.svc.cluster.local:8080"
    }
  ]
}
```

The URL follows standard Kubernetes DNS: `http://{agent-name}.{namespace}.svc.cluster.local:{port}`.

The Service name is reflected in `status.serviceName` on the Agent CR.

---

### Lifecycle History

Every significant event (pod creation, spec update, restart, resurrection, env changes…) is stored in the **history store** (Redis by default; in-memory when `--redis-addr` is not set). The list is capped at `--history-max-entries` entries (default **1000**; oldest are evicted first). History persists across orchestrator restarts when Redis is used.

#### `GET /api/v1/agents/:name/history`
Retrieve the full event history for an agent.

```bash
curl http://localhost:8082/api/v1/agents/my-agent/history
```
```json
{
  "status": "ok",
  "data": {
    "agent": "my-agent",
    "count": 4,
    "history": [
      { "time": "2026-03-07T13:21:40Z", "type": "Normal",  "reason": "Created",      "message": "[2026-03-07T13:21:40] Agent created via REST API (image: busybox:1.36)" },
      { "time": "2026-03-07T13:21:40Z", "type": "Normal",  "reason": "PodCreated",   "message": "[2026-03-07T13:21:40] Pod created successfully for Agent my-agent" },
      { "time": "2026-03-07T13:22:10Z", "type": "Normal",  "reason": "Resurrected",  "message": "[2026-03-07T13:22:10] Agent self-healed: recreated after external deletion (original UID abc-123, attempt 1)" },
      { "time": "2026-03-07T13:22:11Z", "type": "Normal",  "reason": "PodCreated",   "message": "[2026-03-07T13:22:11] Pod created successfully for Agent my-agent" }
    ]
  }
}
```

History is also accessible via the Web UI **History** tab in the agent detail modal.

---

### Logs

#### `GET /api/v1/agents/:name/logs`

| Query param | Type | Default | Description |
|-------------|------|---------|-------------|
| `tailLines` | int | `100` | Lines from the end |
| `sinceSeconds` | int | — | Only lines newer than N seconds |
| `container` | string | `agent` | Container name inside the Pod |
| `follow` | bool | `false` | Stream logs (chunked transfer) |

```bash
# Last 200 lines
curl "http://localhost:8082/api/v1/agents/my-agent/logs?tailLines=200"

# Live stream
curl -N "http://localhost:8082/api/v1/agents/my-agent/logs?follow=true"

# Lines from the last 60 seconds
curl "http://localhost:8082/api/v1/agents/my-agent/logs?sinceSeconds=60"
```

Response: `text/plain` — raw log lines.

---

### Cache

Per-agent, namespaced key-value store with optional TTL. Cache is backed by **Redis** when `--redis-addr` is set, or **in-process memory** otherwise. Useful for temporary state shared between operator logic and external callers.

#### `GET /api/v1/agents/:name/cache`
List all cache entries for the agent.
```bash
curl http://localhost:8082/api/v1/agents/my-agent/cache
```
```json
{
  "status": "ok",
  "data": {
    "last-result":   {"value": "success", "expires_at": "2026-03-07T12:00:00Z"},
    "counter":       {"value": 42,        "expires_at": null}
  }
}
```

---

#### `GET /api/v1/agents/:name/cache/:field`
Get a single field. Returns `404` if missing or expired.
```bash
curl http://localhost:8082/api/v1/agents/my-agent/cache/last-result
```

---

#### `PUT /api/v1/agents/:name/cache/:field`
Set a field with an optional TTL.

| Field | Type | Required | Description |
|-------|------|:--------:|-------------|
| `value` | any | ✅ | JSON value — string, number, bool, object, array |
| `ttl_seconds` | int | | `0` = no expiry (default) |

```bash
# Permanent
curl -X PUT http://localhost:8082/api/v1/agents/my-agent/cache/counter \
  -H 'Content-Type: application/json' \
  -d '{"value": 42}'

# Expires after 5 minutes
curl -X PUT http://localhost:8082/api/v1/agents/my-agent/cache/session-token \
  -H 'Content-Type: application/json' \
  -d '{"value": "abc123", "ttl_seconds": 300}'
```

---

#### `DELETE /api/v1/agents/:name/cache/:field`
Delete a single field.
```bash
curl -X DELETE http://localhost:8082/api/v1/agents/my-agent/cache/counter
```

---

#### `DELETE /api/v1/agents/:name/cache`
Clear all cache entries for this agent.
```bash
curl -X DELETE http://localhost:8082/api/v1/agents/my-agent/cache
```

---

## Agent CR — direct kubectl usage

```yaml
# agent-example.yaml
apiVersion: orchestrator.dev/v1alpha1
kind: Agent
metadata:
  name: data-processor
  namespace: default
spec:
  image: python:3.12-slim
  restartPolicy: Always
  servicePort: 8080          # creates a ClusterIP Service named 'data-processor'
  idleTimeout: 300           # pause after 5 minutes of inactivity (0 = use global default)
  env:
    - name: QUEUE_URL
      value: "amqp://rabbitmq:5672"
    - name: DB_PASSWORD
      valueFrom:
        secretKeyRef:
          name: db-secret
          key: password
  resources:
    requests:
      cpu: "100m"
      memory: "128Mi"
    limits:
      cpu: "500m"
      memory: "256Mi"
  podLabels:
    team: data-engineering
```

```bash
kubectl apply   -f agent-example.yaml
kubectl get     agents -n default          # shortname: agt
kubectl describe agent data-processor -n default
kubectl delete  -f agent-example.yaml
```

---

## Makefile targets

| Target | Description |
|--------|-------------|
| `make dev` | Install CRD + run locally _(main dev workflow)_ |
| `make build` | Build binary → `bin/orchestrator` |
| `make run` | `go run ./cmd/main.go --debug=true` |
| `make test` | Unit tests |
| `make lint` | `golangci-lint` |
| `make fmt` | `gofmt` + `goimports` |
| `make vet` | `go vet` |
| `make tidy` | `go mod tidy -e` |
| `make docker-build` | Build Docker image |
| `make docker-push` | Push Docker image |
| `make docker-build-push` | Build + push |
| `make helm-lint` | Lint Helm chart |
| `make helm-template` | Render templates to stdout |
| `make helm-install` | Install / upgrade Helm release |
| `make helm-uninstall` | Uninstall Helm release _(CRD preserved)_ |
| `make install-crd` | `kubectl apply` CRD directly |
| `make uninstall-crd` | `kubectl delete` CRD _(destroys all Agents!)_ |

---

## Security

- Orchestrator runs with minimal RBAC — namespaced **Role**, not **ClusterRole**.
- Container runs as non-root (`runAsUser: 65532`).
- `readOnlyRootFilesystem: true`, all Linux capabilities dropped.
- Distroless base image (`gcr.io/distroless/static:nonroot`).

---

## Project structure

```
K8SAgentOrchestrator/
├── api/v1alpha1/
│   ├── agent_types.go           # CRD Go types
│   ├── deepcopy_extra.go        # Hand-written DeepCopy (not overwritten by controller-gen)
│   ├── groupversion_info.go     # Group/Version registration
│   └── zz_generated.deepcopy.go
├── cmd/
│   └── main.go                  # Entrypoint — manager + REST server
├── config/crd/bases/
│   └── orchestrator.dev_agents.yaml
├── internal/
│   ├── cache/
│   │   ├── cache.go             # CacheStore interface + in-memory TTL implementation
│   │   └── redis_cache.go       # Redis-backed CacheStore implementation
│   ├── history/
│   │   └── history.go           # HistoryStore interface + Redis and in-memory implementations
│   ├── controller/
│   │   └── agent_controller.go  # Reconcile loop
│   ├── idle/
│   │   └── watcher.go           # Idle auto-stop goroutine
│   ├── rest/
│   │   ├── server.go            # Gin server + route registration (CORS middleware)
│   │   ├── handlers.go          # All HTTP handlers
│   │   └── kubeconfig.go        # in-cluster / KUBECONFIG helper
│   └── ui/
│       ├── server.go            # Embeds static/ via go:embed
│       └── static/
│           └── index.html       # Single-page dashboard (vanilla JS + CSS, no build step)
├── helm/k8s-agent-orchestrator/
│   ├── Chart.yaml
│   ├── values.yaml
│   ├── crds/                    # CRD installed before templates
│   └── templates/
│       ├── deployment.yaml
│       ├── service.yaml
│       ├── serviceaccount.yaml
│       ├── role.yaml            # Namespaced RBAC
│       └── rolebinding.yaml
├── Dockerfile                   # Multi-stage, distroless
├── Makefile
└── go.mod
```

---

## TODO

- [x] **Persistent cache** — replaced the in-process key-value store with Redis (`--redis-addr`); in-memory fallback retained
- [ ] **Multi-namespace watch** — support watching all namespaces via `--watch-all-namespaces` flag instead of a single namespace
- [ ] **Leader election** — enable and test HA mode with multiple orchestrator replicas and proper leader election
- [ ] **Agent scaling** — allow `spec.replicas > 1` so a single Agent CR can back multiple Pods behind the ClusterIP Service
- [ ] **Webhook validation** — add a validating/mutating webhook to reject invalid Agent specs at creation time
- [ ] **gRPC / WebSocket logs** — replace chunked HTTP log streaming with a proper WebSocket or gRPC stream endpoint
- [ ] **Auth on REST API** — add token-based (Bearer) or mTLS authentication to the REST API
- [x] **Persistent lifecycle history** — history stored in Redis (or in-memory) via `HistoryStore`; survives orchestrator restarts and CR resurrections
- [ ] **Grafana dashboard** — ship a pre-built Grafana dashboard JSON for the Prometheus metrics exposed on `:8080`
- [ ] **CLI client** — implement a `kubectl`-style CLI (`orchctl`) wrapping the REST API for scripting and CI usage
- [ ] **E2E test suite** — add end-to-end tests using `envtest` or a real `kind` cluster in CI
