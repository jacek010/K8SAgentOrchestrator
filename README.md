# K8s Agent Orchestrator

A Kubernetes operator written in **Go** that manages **Agent** custom resources — each Agent maps 1-to-1 to a Pod. The orchestrator exposes a **REST API** for full agent lifecycle control, env-var hot-patching, log streaming, and a per-agent in-memory key-value cache.

---

## Architecture

```
┌─────────────┐   REST :8082   ┌──────────────────────────┐
│   Client    │ ◄────────────► │   REST API (Gin)          │
└─────────────┘                │   internal/rest/          │
                               └────────────┬─────────────┘
                                            │ controller-runtime client
                               ┌────────────▼─────────────┐
                               │   Agent CRD               │
                               │   orchestrator.dev/v1alpha1│
                               └────────────┬─────────────┘
                                            │ reconcile loop
                               ┌────────────▼─────────────┐
                               │   Pod  (1 per Agent)      │
                               └──────────────────────────┘
Sidecar services:
  :8080  Prometheus metrics
  :8081  Health / readiness probes
```

| Component | Description |
|-----------|-------------|
| **Agent CRD** | Declares desired state (image, env, resources, restart policy…) |
| **Controller** | Reconcile loop: creates/deletes/restarts Pods from CR; handles finalizer |
| **REST API** | Gin HTTP server on `:8082` — CRUD agents, patch env, stream logs, manage cache |
| **Cache** | Thread-safe, per-agent, TTL key-value store (in-process memory) |
| **Helm chart** | Deployment, Service, ServiceAccount, namespaced Role/RoleBinding, CRD |

### Agent lifecycle phases

| Phase | Meaning |
|-------|---------|
| `Pending` | CR created, Pod not yet scheduled |
| `Running` | Pod is running |
| `Failed` | Pod exited with error |
| `Stopped` | `RestartPolicy=Never`, pod completed / not re-created |
| `Updating` | Spec change detected, pod being replaced |

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
3. Starts the controller manager + REST API.

Ports after startup:

| Port | Purpose |
|------|---------|
| `8082` | REST API |
| `8080` | Prometheus metrics (`/metrics`) |
| `8081` | Health probes (`/healthz`, `/readyz`) |

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
| `watchNamespace` | release namespace | Namespace to watch |
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

#### `POST /api/v1/namespaces/:namespace/agents`
Create a new Agent (and its Pod).

**Required:** `name`, `image`

```bash
curl -X POST http://localhost:8082/api/v1/namespaces/default/agents \
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
    "podLabels": {"team": "platform"}
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

**Response:** `201 Created`

---

#### `GET /api/v1/namespaces/:namespace/agents`
List all Agents in a namespace.
```bash
curl http://localhost:8082/api/v1/namespaces/default/agents
```

---

#### `GET /api/v1/namespaces/:namespace/agents/:name`
Get a single Agent with its current status (`.status.phase`, `.status.podName`, `.status.conditions`).
```bash
curl http://localhost:8082/api/v1/namespaces/default/agents/my-agent
```

---

#### `PUT /api/v1/namespaces/:namespace/agents/:name`
Update Agent spec. Only non-zero fields in the body are applied. Triggers pod recreation.
```bash
curl -X PUT http://localhost:8082/api/v1/namespaces/default/agents/my-agent \
  -H 'Content-Type: application/json' \
  -d '{"image": "busybox:1.37"}'
```

---

#### `DELETE /api/v1/namespaces/:namespace/agents/:name`
Delete the Agent CR (Pod is garbage-collected via finalizer). Also clears the in-memory cache.
```bash
curl -X DELETE http://localhost:8082/api/v1/namespaces/default/agents/my-agent
# {"status":"deleted","name":"my-agent"}
```

---

### Lifecycle control

#### `POST /api/v1/namespaces/:namespace/agents/:name/restart`
Force pod recreation by bumping the `orchestrator.dev/restart-at` annotation.
```bash
curl -X POST http://localhost:8082/api/v1/namespaces/default/agents/my-agent/restart
# {"status":"ok","data":{"restarted":true}}
```

---

#### `POST /api/v1/namespaces/:namespace/agents/:name/stop`
Stop the agent: sets `restartPolicy=Never` and records `orchestrator.dev/stopped-at`.  
The pod finishes and is not restarted.
```bash
curl -X POST http://localhost:8082/api/v1/namespaces/default/agents/my-agent/stop
# {"status":"ok","data":{"stopped":true}}
```

---

#### `POST /api/v1/namespaces/:namespace/agents/:name/start`
Start (resume) a stopped agent. Resets `restartPolicy=Always` and forces pod recreation.
```bash
curl -X POST http://localhost:8082/api/v1/namespaces/default/agents/my-agent/start
# {"status":"ok","data":{"started":true}}
```

---

### Environment variables

All env changes are applied to the Agent spec immediately and trigger a pod recreation.

#### `GET /api/v1/namespaces/:namespace/agents/:name/env`
Return the current env list.
```bash
curl http://localhost:8082/api/v1/namespaces/default/agents/my-agent/env
# {"status":"ok","data":[{"name":"LOG_LEVEL","value":"debug"}]}
```

---

#### `PUT /api/v1/namespaces/:namespace/agents/:name/env`
**Replace** the entire env list (destructive).
```bash
curl -X PUT http://localhost:8082/api/v1/namespaces/default/agents/my-agent/env \
  -H 'Content-Type: application/json' \
  -d '{"env":[{"name":"LOG_LEVEL","value":"info"},{"name":"PORT","value":"8080"}]}'
```

---

#### `PATCH /api/v1/namespaces/:namespace/agents/:name/env`
**Merge / upsert** individual env vars — existing keys are updated, new keys are appended, unmentioned keys are left intact.
```bash
curl -X PATCH http://localhost:8082/api/v1/namespaces/default/agents/my-agent/env \
  -H 'Content-Type: application/json' \
  -d '{"env":[{"name":"LOG_LEVEL","value":"warn"},{"name":"NEW_VAR","value":"hello"}]}'
```

---

#### `DELETE /api/v1/namespaces/:namespace/agents/:name/env/:key`
Remove a single env var by name.
```bash
curl -X DELETE http://localhost:8082/api/v1/namespaces/default/agents/my-agent/env/LOG_LEVEL
```

---

### Logs

#### `GET /api/v1/namespaces/:namespace/agents/:name/logs`

| Query param | Type | Default | Description |
|-------------|------|---------|-------------|
| `tailLines` | int | `100` | Lines from the end |
| `sinceSeconds` | int | — | Only lines newer than N seconds |
| `container` | string | `agent` | Container name inside the Pod |
| `follow` | bool | `false` | Stream logs (chunked transfer) |

```bash
# Last 200 lines
curl "http://localhost:8082/api/v1/namespaces/default/agents/my-agent/logs?tailLines=200"

# Live stream
curl -N "http://localhost:8082/api/v1/namespaces/default/agents/my-agent/logs?follow=true"

# Lines from the last 60 seconds
curl "http://localhost:8082/api/v1/namespaces/default/agents/my-agent/logs?sinceSeconds=60"
```

Response: `text/plain` — raw log lines.

---

### In-memory Cache

Per-agent, namespaced key-value store with optional TTL. Cache is **in-process only** — it resets when the orchestrator restarts. Useful for temporary state shared between operator logic and external callers.

#### `GET /api/v1/namespaces/:namespace/agents/:name/cache`
List all cache entries for the agent.
```bash
curl http://localhost:8082/api/v1/namespaces/default/agents/my-agent/cache
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

#### `GET /api/v1/namespaces/:namespace/agents/:name/cache/:field`
Get a single field. Returns `404` if missing or expired.
```bash
curl http://localhost:8082/api/v1/namespaces/default/agents/my-agent/cache/last-result
```

---

#### `PUT /api/v1/namespaces/:namespace/agents/:name/cache/:field`
Set a field with an optional TTL.

| Field | Type | Required | Description |
|-------|------|:--------:|-------------|
| `value` | any | ✅ | JSON value — string, number, bool, object, array |
| `ttl_seconds` | int | | `0` = no expiry (default) |

```bash
# Permanent
curl -X PUT http://localhost:8082/api/v1/namespaces/default/agents/my-agent/cache/counter \
  -H 'Content-Type: application/json' \
  -d '{"value": 42}'

# Expires after 5 minutes
curl -X PUT http://localhost:8082/api/v1/namespaces/default/agents/my-agent/cache/session-token \
  -H 'Content-Type: application/json' \
  -d '{"value": "abc123", "ttl_seconds": 300}'
```

---

#### `DELETE /api/v1/namespaces/:namespace/agents/:name/cache/:field`
Delete a single field.
```bash
curl -X DELETE http://localhost:8082/api/v1/namespaces/default/agents/my-agent/cache/counter
```

---

#### `DELETE /api/v1/namespaces/:namespace/agents/:name/cache`
Clear all cache entries for this agent.
```bash
curl -X DELETE http://localhost:8082/api/v1/namespaces/default/agents/my-agent/cache
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
│   ├── groupversion_info.go     # Group/Version registration
│   └── zz_generated.deepcopy.go
├── cmd/
│   └── main.go                  # Entrypoint — manager + REST server
├── config/crd/bases/
│   └── orchestrator.dev_agents.yaml
├── internal/
│   ├── cache/
│   │   └── cache.go             # In-memory TTL cache
│   ├── controller/
│   │   └── agent_controller.go  # Reconcile loop
│   └── rest/
│       ├── server.go            # Gin server + route registration
│       ├── handlers.go          # All HTTP handlers
│       └── kubeconfig.go        # in-cluster / KUBECONFIG helper
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
