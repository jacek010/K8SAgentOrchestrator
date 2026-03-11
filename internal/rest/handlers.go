package rest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestratorv1alpha1 "github.com/jacekmyjkowski/k8s-agent-orchestrator/api/v1alpha1"
)

// ─────────────────────────────── helpers ─────────────────────────────────────

// ts returns a compact UTC timestamp used in Event messages to prevent
// Kubernetes from deduplicating distinct lifecycle events (k8s merges events
// with identical reason+message into a single entry with an increased count).
func ts() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05")
}

// appendHistory records a lifecycle event via the history store.
// It is intentionally best-effort: errors are silently swallowed so that a
// history failure never surfaces in the REST response.
func (s *Server) appendHistory(ctx context.Context, namespace, name, eventType, reason, message string) {
	if s.history != nil {
		s.history.Append(ctx, namespace, name, eventType, reason, message)
	}
}
func respondOK(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "data": data})
}

func respondCreated(c *gin.Context, data interface{}) {
	c.JSON(http.StatusCreated, gin.H{"status": "created", "data": data})
}

func respondError(c *gin.Context, code int, msg string) {
	c.JSON(code, gin.H{"status": "error", "message": msg})
}

func (s *Server) nsName(c *gin.Context) (namespace, name string) {
	namespace = c.Param("namespace")
	if namespace == "" {
		namespace = s.namespace
	}
	name = c.Param("name")
	return
}

// rawClientset returns a typed Kubernetes clientset for operations not supported by
// controller-runtime (e.g., pod log streaming).
func (s *Server) rawClientset() (kubernetes.Interface, error) {
	cfg, err := getRestConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// ─────────────────────────────── health ──────────────────────────────────────

// handleHealthz godoc
// @Summary      Liveness probe
// @Description  Returns 200 OK if the process is alive
// @Tags         health
// @Produce      json
// @Success      200  {object}  map[string]string  "ok"
// @Router       /healthz [get]
func (s *Server) handleHealthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleReadyz godoc
// @Summary      Readiness probe
// @Description  Returns 200 if the Kubernetes API server is reachable, 503 otherwise
// @Tags         health
// @Produce      json
// @Success      200  {object}  map[string]string  "ready"
// @Failure      503  {object}  map[string]string  "k8s api not reachable"
// @Router       /readyz [get]
func (s *Server) handleReadyz(c *gin.Context) {
	ctx, cancel := apiCtx()
	defer cancel()

	agentList := &orchestratorv1alpha1.AgentList{}
	if err := s.client.List(ctx, agentList, client.Limit(1)); err != nil {
		respondError(c, http.StatusServiceUnavailable, "k8s api not reachable: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

// ─────────────────────────────── Agent CRUD ───────────────────────────────────

// CreateAgentRequest is the JSON body for POST /agents.
type CreateAgentRequest struct {
	Name            string                      `json:"name" binding:"required"`
	Image           string                      `json:"image" binding:"required"`
	ImagePullPolicy string                      `json:"imagePullPolicy,omitempty"`
	Env             []corev1.EnvVar             `json:"env,omitempty"`
	EnvFrom         []corev1.EnvFromSource      `json:"envFrom,omitempty"`
	Resources       corev1.ResourceRequirements `json:"resources,omitempty"`
	Command         []string                    `json:"command,omitempty"`
	Args            []string                    `json:"args,omitempty"`
	ServiceAccount  string                      `json:"serviceAccountName,omitempty"`
	RestartPolicy   string                      `json:"restartPolicy,omitempty"`
	PodLabels       map[string]string           `json:"podLabels,omitempty"`
	PodAnnotations  map[string]string           `json:"podAnnotations,omitempty"`
	Labels          map[string]string           `json:"labels,omitempty"`
	Annotations     map[string]string           `json:"annotations,omitempty"`
	// SelfHealingDisabled disables automatic resurrection of this Agent CR when deleted externally.
	// Default false means self-healing is ON.
	SelfHealingDisabled bool `json:"selfHealingDisabled,omitempty"`
	// Paused prevents pod creation when true.
	Paused bool `json:"paused,omitempty"`
	// ServicePort, if non-zero, causes the controller to create a ClusterIP Service on that port.
	ServicePort int32 `json:"servicePort,omitempty"`
	// ServiceProtocol is the protocol of the service port (TCP/UDP/SCTP). Default: TCP.
	ServiceProtocol string `json:"serviceProtocol,omitempty"`
	// IdleTimeout is the number of seconds of inactivity after which the orchestrator
	// automatically pauses this agent. 0 disables idle tracking (uses global default).
	IdleTimeout int32 `json:"idleTimeout,omitempty"`
}

// handleCreateAgent godoc
// @Summary      Create agent
// @Description  Creates a new Agent CR; the controller then creates a Pod for it
// @Tags         agents
// @Accept       json
// @Produce      json
// @Param        body       body      CreateAgentRequest  true   "Agent specification"
// @Success      201        {object}  map[string]interface{}   "created"
// @Failure      400        {object}  map[string]string        "invalid body"
// @Failure      409        {object}  map[string]string        "agent already exists"
// @Failure      500        {object}  map[string]string        "internal error"
// @Router       /api/v1/agents [post]
func (s *Server) handleCreateAgent(c *gin.Context) {
	namespace, _ := s.nsName(c)

	var req CreateAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}

	pullPolicy := corev1.PullPolicy(req.ImagePullPolicy)
	restartPolicy := corev1.RestartPolicy(req.RestartPolicy)

	agent := &orchestratorv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:        req.Name,
			Namespace:   namespace,
			Labels:      req.Labels,
			Annotations: req.Annotations,
		},
		Spec: orchestratorv1alpha1.AgentSpec{
			Image:               req.Image,
			ImagePullPolicy:     pullPolicy,
			Env:                 req.Env,
			EnvFrom:             req.EnvFrom,
			Resources:           req.Resources,
			Command:             req.Command,
			Args:                req.Args,
			ServiceAccountName:  req.ServiceAccount,
			RestartPolicy:       restartPolicy,
			PodLabels:           req.PodLabels,
			PodAnnotations:      req.PodAnnotations,
			SelfHealingDisabled: req.SelfHealingDisabled,
			Paused:              req.Paused,
			ServicePort:         req.ServicePort,
			ServiceProtocol:     corev1.Protocol(req.ServiceProtocol),
			IdleTimeout:         req.IdleTimeout,
		},
	}

	ctx, cancel := apiCtx()
	defer cancel()

	if err := s.client.Create(ctx, agent); err != nil {
		if apierrors.IsAlreadyExists(err) {
			respondError(c, http.StatusConflict, "agent already exists")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "Created",
		"[%s] Agent created via REST API (image: %s)", ts(), agent.Spec.Image)
	s.appendHistory(ctx, agent.Namespace, agent.Name, corev1.EventTypeNormal, "Created",
		fmt.Sprintf("[%s] Agent created via REST API (image: %s)", ts(), agent.Spec.Image))
	respondCreated(c, agent)
}

// AgentServiceInfo holds the agent name and its ClusterIP service URL.
type AgentServiceInfo struct {
	Agent     string `json:"agent"`
	Namespace string `json:"namespace"`
	Port      int32  `json:"port"`
	Protocol  string `json:"protocol"`
	URL       string `json:"url"`
}

// handleListAgentServices godoc
// @Summary      List agent service URLs
// @Description  Returns the ClusterIP DNS URL for every Agent that has a ServicePort configured
// @Tags         agents
// @Produce      json
// @Success      200        {object}  map[string]interface{}  "list of agent service URLs"
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/services [get]
func (s *Server) handleListAgentServices(c *gin.Context) {
	namespace, _ := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	agentList := &orchestratorv1alpha1.AgentList{}
	if err := s.client.List(ctx, agentList, client.InNamespace(namespace)); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	var results []AgentServiceInfo
	for _, a := range agentList.Items {
		if a.Spec.ServicePort <= 0 {
			continue
		}
		proto := string(a.Spec.ServiceProtocol)
		if proto == "" {
			proto = "TCP"
		}
		results = append(results, AgentServiceInfo{
			Agent:     a.Name,
			Namespace: namespace,
			Port:      a.Spec.ServicePort,
			Protocol:  proto,
			URL:       fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", a.Name, namespace, a.Spec.ServicePort),
		})
	}
	if results == nil {
		results = []AgentServiceInfo{}
	}
	respondOK(c, results)
}

// handleListAgents godoc
// @Summary      List agents
// @Description  Returns all Agent CRs in the given namespace
// @Tags         agents
// @Produce      json
// @Success      200        {object}  map[string]interface{}  "list of agents"
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents [get]
func (s *Server) handleListAgents(c *gin.Context) {
	namespace, _ := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	agentList := &orchestratorv1alpha1.AgentList{}
	if err := s.client.List(ctx, agentList, client.InNamespace(namespace)); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, agentList.Items)
}

// handleGetAgent godoc
// @Summary      Get agent
// @Description  Returns a single Agent CR with its current status (phase, podName, conditions)
// @Tags         agents
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]interface{}
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name} [get]
func (s *Server) handleGetAgent(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, agent)
}

// handleGetAgentHistory godoc
// @Summary      Get agent lifecycle history
// @Description  Returns the full lifecycle event history stored in Agent status
// @Tags         agents
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]interface{}
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name}/history [get]
func (s *Server) handleGetAgentHistory(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	// Verify the agent exists before returning history (proper 404 semantics).
	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	var history []orchestratorv1alpha1.LifecycleEvent
	if s.history != nil {
		history = s.history.List(ctx, namespace, name)
	}
	if history == nil {
		history = []orchestratorv1alpha1.LifecycleEvent{}
	}
	respondOK(c, gin.H{
		"agent":   name,
		"count":   len(history),
		"history": history,
	})
}

// handleUpdateAgent godoc
// @Summary      Update agent
// @Description  Updates Agent spec fields; only non-zero fields are applied. Triggers pod recreation.
// @Tags         agents
// @Accept       json
// @Produce      json
// @Param        name       path      string              true  "Agent name"
// @Param        body       body      CreateAgentRequest  true  "Fields to update"
// @Success      200        {object}  map[string]interface{}
// @Failure      400        {object}  map[string]string
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name} [put]
func (s *Server) handleUpdateAgent(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	var req CreateAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	// Update spec fields.
	if req.Image != "" {
		agent.Spec.Image = req.Image
	}
	if req.ImagePullPolicy != "" {
		agent.Spec.ImagePullPolicy = corev1.PullPolicy(req.ImagePullPolicy)
	}
	if req.Env != nil {
		agent.Spec.Env = req.Env
	}
	if req.EnvFrom != nil {
		agent.Spec.EnvFrom = req.EnvFrom
	}
	if req.Command != nil {
		agent.Spec.Command = req.Command
	}
	if req.Args != nil {
		agent.Spec.Args = req.Args
	}
	if req.ServiceAccount != "" {
		agent.Spec.ServiceAccountName = req.ServiceAccount
	}
	if req.RestartPolicy != "" {
		agent.Spec.RestartPolicy = corev1.RestartPolicy(req.RestartPolicy)
	}
	if req.PodLabels != nil {
		agent.Spec.PodLabels = req.PodLabels
	}
	if req.PodAnnotations != nil {
		agent.Spec.PodAnnotations = req.PodAnnotations
	}
	if req.IdleTimeout >= 0 && req.IdleTimeout != agent.Spec.IdleTimeout {
		agent.Spec.IdleTimeout = req.IdleTimeout
	}

	if err := s.client.Update(ctx, agent); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "SpecUpdated",
		"[%s] Agent spec updated via REST API", ts())
	s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "SpecUpdated",
		fmt.Sprintf("[%s] Agent spec updated via REST API", ts()))
	respondOK(c, agent)
}

// handleDeleteAgent godoc
// @Summary      Delete agent
// @Description  Permanently deletes the Agent CR and its Pod. Self-healing is disabled before deletion so the agent is not resurrected. Also clears the in-memory cache.
// @Tags         agents
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]string  "deleted"
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name} [delete]
func (s *Server) handleDeleteAgent(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	// Disable self-healing before deleting so the agent is not resurrected.
	if !agent.Spec.SelfHealingDisabled {
		patch := client.MergeFrom(agent.DeepCopy())
		agent.Spec.SelfHealingDisabled = true
		if err := s.client.Patch(ctx, agent, patch); err != nil {
			respondError(c, http.StatusInternalServerError, "disabling self-healing: "+err.Error())
			return
		}
	}

	// Emit history BEFORE delete while the object still exists for status patch.
	s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "Deleted",
		fmt.Sprintf("[%s] Agent permanently deleted via REST API (self-healing was disabled before deletion)", ts()))

	if err := s.client.Delete(ctx, agent); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "Deleted",
		"[%s] Agent permanently deleted via REST API (self-healing was disabled before deletion)", ts())

	// Clean up the in-memory cache for this agent.
	s.cache.ClearAgent(namespace, name)

	c.JSON(http.StatusOK, gin.H{"status": "deleted", "name": name})
}

// ─────────────────────────────── Lifecycle ───────────────────────────────────

// handleRestartAgent godoc
// @Summary      Restart agent
// @Description  Forces pod recreation by bumping the orchestrator.dev/restart-at annotation
// @Tags         lifecycle
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]interface{}  "restarted: true"
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name}/restart [post]
func (s *Server) handleRestartAgent(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	// Trigger pod recreation by bumping a restart annotation.
	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	if agent.Annotations == nil {
		agent.Annotations = make(map[string]string)
	}
	agent.Annotations["orchestrator.dev/restart-at"] = time.Now().UTC().Format(time.RFC3339)

	if err := s.client.Patch(ctx, agent, patch); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "Restarted",
		"[%s] Agent pod recreation triggered via REST API", ts())
	s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "Restarted",
		fmt.Sprintf("[%s] Agent pod recreation triggered via REST API", ts()))
	respondOK(c, gin.H{"restarted": true})
}

// handleStopAgent godoc
// @Summary      Stop agent
// @Description  Pauses the agent: sets spec.paused=true, which causes the controller to delete the Pod and stop reconciling
// @Tags         lifecycle
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]interface{}  "stopped: true"
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name}/stop [post]
func (s *Server) handleStopAgent(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.Paused = true
	if agent.Annotations == nil {
		agent.Annotations = make(map[string]string)
	}
	agent.Annotations["orchestrator.dev/stopped-at"] = time.Now().UTC().Format(time.RFC3339)

	if err := s.client.Patch(ctx, agent, patch); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "Stopped",
		"[%s] Agent paused via REST API (spec.paused=true, pod will be deleted)", ts())
	s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "Stopped",
		fmt.Sprintf("[%s] Agent paused via REST API (spec.paused=true, pod will be deleted)", ts()))
	respondOK(c, gin.H{"stopped": true})
}

// handleStartAgent godoc
// @Summary      Start agent
// @Description  Resumes a paused agent: sets spec.paused=false and forces pod recreation
// @Tags         lifecycle
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]interface{}  "started: true"
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name}/start [post]
func (s *Server) handleStartAgent(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.Paused = false
	if agent.Annotations == nil {
		agent.Annotations = make(map[string]string)
	}
	agent.Annotations["orchestrator.dev/started-at"] = time.Now().UTC().Format(time.RFC3339)
	// Bump restart annotation to force pod recreation.
	agent.Annotations["orchestrator.dev/restart-at"] = time.Now().UTC().Format(time.RFC3339)

	if err := s.client.Patch(ctx, agent, patch); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "Started",
		"[%s] Agent resumed via REST API (spec.paused=false, pod recreation triggered)", ts())
	s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "Started",
		fmt.Sprintf("[%s] Agent resumed via REST API (spec.paused=false, pod recreation triggered)", ts()))
	respondOK(c, gin.H{"started": true})
}

// wakeAgent ensures the agent is unpaused and waits up to waitTimeout for it to
// reach Running phase. If the agent is already running it returns immediately.
// Returns the current phase and the ClusterIP service URL (empty when servicePort==0).
func (s *Server) wakeAgent(ctx context.Context, namespace, name string, waitTimeout time.Duration) (phase string, svcURL string, err error) {
	agent := &orchestratorv1alpha1.Agent{}
	if err = s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		return
	}

	// Build svcURL when a ServicePort is configured.
	if agent.Spec.ServicePort > 0 {
		svcName := agent.Name
		if agent.Status.ServiceName != "" {
			svcName = agent.Status.ServiceName
		}
		svcURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svcName, namespace, agent.Spec.ServicePort)
	}

	// Already running — nothing to do.
	if !agent.Spec.Paused && agent.Status.Phase == orchestratorv1alpha1.AgentPhaseRunning {
		return string(agent.Status.Phase), svcURL, nil
	}

	// Unpause if needed.
	if agent.Spec.Paused {
		patch := client.MergeFrom(agent.DeepCopy())
		agent.Spec.Paused = false
		if agent.Annotations == nil {
			agent.Annotations = make(map[string]string)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		agent.Annotations["orchestrator.dev/started-at"] = now
		agent.Annotations["orchestrator.dev/restart-at"] = now
		agent.Annotations["orchestrator.dev/wake-reason"] = "keepalive"
		if err = s.client.Patch(ctx, agent, patch); err != nil {
			return
		}
		s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "WokeUp",
			fmt.Sprintf("[%s] Agent woken via keepalive (was idle-paused)", ts()))
	}

	// Poll until Running or timeout.
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		fresh := &orchestratorv1alpha1.Agent{}
		if gerr := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, fresh); gerr == nil {
			if fresh.Status.Phase == orchestratorv1alpha1.AgentPhaseRunning {
				return string(fresh.Status.Phase), svcURL, nil
			}
			phase = string(fresh.Status.Phase)
		}
		time.Sleep(2 * time.Second)
	}

	// Timed out — return last known phase (not an error; caller decides).
	return phase, svcURL, nil
}

// handleKeepalive godoc
// @Summary      Agent keepalive / wake-on-demand
// @Description  Resets the idle timer for the agent. If the agent is paused (idle-stopped), wakes it and waits up to 30s for it to reach Running phase. Returns current status and the ClusterIP service URL for direct A2A communication. Call this endpoint periodically (e.g. every idleTimeout/2 seconds) during active A2A sessions to prevent auto-stop.
// @Tags         lifecycle
// @Produce      json
// @Param        name    path      string  true   "Agent name"
// @Param        wait    query     integer false  "Max seconds to wait for Running (default 30, max 120)"
// @Success      200     {object}  map[string]interface{}  "status + svcUrl"
// @Failure      404     {object}  map[string]string
// @Failure      500     {object}  map[string]string
// @Router       /api/v1/agents/{name}/keepalive [post]
func (s *Server) handleKeepalive(c *gin.Context) {
	namespace, name := s.nsName(c)

	// Parse optional ?wait= query param (seconds).
	waitSec := 30
	if ws := c.Query("wait"); ws != "" {
		if n, err := strconv.Atoi(ws); err == nil && n >= 0 && n <= 120 {
			waitSec = n
		}
	}
	waitTimeout := time.Duration(waitSec) * time.Second

	// The activity middleware has already touched the idle timer.
	// Run wakeAgent with a context that permits the full wait duration.
	wakeCtx, cancel := context.WithTimeout(context.Background(), waitTimeout+10*time.Second)
	defer cancel()

	t0 := time.Now()
	phase, svcURL, err := s.wakeAgent(wakeCtx, namespace, name, waitTimeout)
	if err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	status := "running"
	if phase != string(orchestratorv1alpha1.AgentPhaseRunning) {
		status = "starting"
		if waitSec == 0 {
			status = "accepted"
		}
	}

	resp := gin.H{
		"status":  status,
		"phase":   phase,
		"elapsed": time.Since(t0).Round(time.Millisecond).String(),
	}
	if svcURL != "" {
		resp["svcUrl"] = svcURL
	}
	respondOK(c, resp)
}

// handleDisableSelfHealing godoc
// @Summary      Disable self-healing
// @Description  Sets spec.selfHealingDisabled=true. The Agent CR will NOT be recreated automatically when deleted externally.
// @Tags         lifecycle
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]interface{}  "selfHealingDisabled: true"
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name}/disable-healing [post]
func (s *Server) handleDisableSelfHealing(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.SelfHealingDisabled = true

	if err := s.client.Patch(ctx, agent, patch); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "SelfHealingDisabled",
		"[%s] Self-healing disabled via REST API — agent will NOT be recreated on external deletion", ts())
	s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "SelfHealingDisabled",
		fmt.Sprintf("[%s] Self-healing disabled via REST API — agent will NOT be recreated on external deletion", ts()))
	respondOK(c, gin.H{"selfHealingDisabled": true})
}

// handleEnableSelfHealing godoc
// @Summary      Enable self-healing
// @Description  Sets spec.selfHealingDisabled=false. The Agent CR will be automatically recreated when deleted externally (this is the default behaviour).
// @Tags         lifecycle
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]interface{}  "selfHealingDisabled: false"
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name}/enable-healing [post]
func (s *Server) handleEnableSelfHealing(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.SelfHealingDisabled = false

	if err := s.client.Patch(ctx, agent, patch); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "SelfHealingEnabled",
		"[%s] Self-healing enabled via REST API — agent will be recreated automatically on external deletion", ts())
	s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "SelfHealingEnabled",
		fmt.Sprintf("[%s] Self-healing enabled via REST API — agent will be recreated automatically on external deletion", ts()))
	respondOK(c, gin.H{"selfHealingDisabled": false})
}

// ─────────────────────────────── Env management ───────────────────────────────

// handleGetEnv godoc
// @Summary      Get env vars
// @Description  Returns the current list of environment variables for the agent container
// @Tags         env
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]interface{}  "list of EnvVar"
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name}/env [get]
func (s *Server) handleGetEnv(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, agent.Spec.Env)
}

// EnvSetRequest replaces the entire env list.
type EnvSetRequest struct {
	Env []corev1.EnvVar `json:"env" binding:"required"`
}

// handleSetEnv godoc
// @Summary      Replace env vars
// @Description  Replaces the entire env list (destructive). Triggers pod recreation.
// @Tags         env
// @Accept       json
// @Produce      json
// @Param        name       path      string         true  "Agent name"
// @Param        body       body      EnvSetRequest  true  "New env list"
// @Success      200        {object}  map[string]interface{}
// @Failure      400        {object}  map[string]string
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name}/env [put]
func (s *Server) handleSetEnv(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	var req EnvSetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	agent.Spec.Env = req.Env
	if err := s.client.Patch(ctx, agent, patch); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "EnvReplaced",
		"[%s] Env vars replaced via REST API (%d vars, pod recreation triggered)", ts(), len(req.Env))
	s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "EnvReplaced",
		fmt.Sprintf("[%s] Env vars replaced via REST API (%d vars, pod recreation triggered)", ts(), len(req.Env)))
	respondOK(c, agent.Spec.Env)
}

// EnvMergeRequest merges/overrides individual env vars (upsert by name).
type EnvMergeRequest struct {
	Env []corev1.EnvVar `json:"env" binding:"required"`
}

// handleMergeEnv godoc
// @Summary      Merge env vars
// @Description  Upserts env vars by name — existing keys are updated, new keys are appended, unmentioned keys are left intact. Triggers pod recreation.
// @Tags         env
// @Accept       json
// @Produce      json
// @Param        name       path      string           true  "Agent name"
// @Param        body       body      EnvMergeRequest  true  "Env vars to merge"
// @Success      200        {object}  map[string]interface{}
// @Failure      400        {object}  map[string]string
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name}/env [patch]
func (s *Server) handleMergeEnv(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := apiCtx()
	defer cancel()

	var req EnvMergeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	merged := mergeEnvVars(agent.Spec.Env, req.Env)
	agent.Spec.Env = merged

	if err := s.client.Patch(ctx, agent, patch); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	// Collect names of upserted vars for the event message.
	upsertedNames := make([]string, 0, len(req.Env))
	for _, e := range req.Env {
		upsertedNames = append(upsertedNames, e.Name)
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "EnvMerged",
		"[%s] Env vars merged via REST API (upserted: %v, total: %d, pod recreation triggered)", ts(), upsertedNames, len(merged))
	s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "EnvMerged",
		fmt.Sprintf("[%s] Env vars merged via REST API (upserted: %v, total: %d, pod recreation triggered)", ts(), upsertedNames, len(merged)))
	respondOK(c, agent.Spec.Env)
}

// handleDeleteEnvKey godoc
// @Summary      Delete env var
// @Description  Removes a single environment variable by name. Triggers pod recreation.
// @Tags         env
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Param        key        path      string  true  "Env var name to delete"
// @Success      200        {object}  map[string]interface{}
// @Failure      404        {object}  map[string]string
// @Failure      500        {object}  map[string]string
// @Router       /api/v1/agents/{name}/env/{key} [delete]
func (s *Server) handleDeleteEnvKey(c *gin.Context) {
	namespace, name := s.nsName(c)
	key := c.Param("key")
	ctx, cancel := apiCtx()
	defer cancel()

	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	patch := client.MergeFrom(agent.DeepCopy())
	filtered := make([]corev1.EnvVar, 0, len(agent.Spec.Env))
	for _, e := range agent.Spec.Env {
		if e.Name != key {
			filtered = append(filtered, e)
		}
	}
	agent.Spec.Env = filtered

	if err := s.client.Patch(ctx, agent, patch); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	s.recorder.Eventf(agent, corev1.EventTypeNormal, "EnvDeleted",
		"[%s] Env var %q removed via REST API (pod recreation triggered)", ts(), key)
	s.appendHistory(ctx, namespace, name, corev1.EventTypeNormal, "EnvDeleted",
		fmt.Sprintf("[%s] Env var %q removed via REST API (pod recreation triggered)", ts(), key))
	respondOK(c, agent.Spec.Env)
}

// mergeEnvVars upserts newVars into existing (match by Name).
func mergeEnvVars(existing, newVars []corev1.EnvVar) []corev1.EnvVar {
	idx := make(map[string]int, len(existing))
	result := make([]corev1.EnvVar, len(existing))
	copy(result, existing)
	for i, e := range result {
		idx[e.Name] = i
	}
	for _, nv := range newVars {
		if i, ok := idx[nv.Name]; ok {
			result[i] = nv
		} else {
			result = append(result, nv)
			idx[nv.Name] = len(result) - 1
		}
	}
	return result
}

// ─────────────────────────────── Logs ────────────────────────────────────────

// handleGetLogs godoc
// @Summary      Get pod logs
// @Description  Returns or streams logs from the agent's pod. Use follow=true for live streaming (chunked transfer).
// @Tags         logs
// @Produce      plain
// @Param        name         path      string  true   "Agent name"
// @Param        tailLines    query     integer false  "Number of lines from the end" default(100)
// @Param        sinceSeconds query     integer false  "Only lines newer than N seconds"
// @Param        container    query     string  false  "Container name" default(agent)
// @Param        follow       query     boolean false  "Stream logs (chunked transfer)" default(false)
// @Success      200          {string}  string  "log lines"
// @Failure      404          {object}  map[string]string
// @Failure      500          {object}  map[string]string
// @Router       /api/v1/agents/{name}/logs [get]
func (s *Server) handleGetLogs(c *gin.Context) {
	namespace, name := s.nsName(c)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Parse query params.
	tailLines := int64(100)
	if t := c.Query("tailLines"); t != "" {
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			tailLines = n
		}
	}
	follow := c.Query("follow") == "true"
	since := c.Query("sinceSeconds")
	containerName := c.DefaultQuery("container", "agent")

	// Get the pod name from Agent status.
	agent := &orchestratorv1alpha1.Agent{}
	if err := s.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, "agent not found")
			return
		}
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	podName := agent.Status.PodName
	if podName == "" {
		respondError(c, http.StatusNotFound, "agent has no running pod")
		return
	}

	cs, err := s.rawClientset()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "failed to create k8s clientset: "+err.Error())
		return
	}

	opts := &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &tailLines,
		Follow:    follow,
	}
	if since != "" {
		if secs, err := strconv.ParseInt(since, 10, 64); err == nil {
			opts.SinceSeconds = &secs
		}
	}

	req := cs.CoreV1().Pods(namespace).GetLogs(podName, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		respondError(c, http.StatusInternalServerError, fmt.Sprintf("error opening log stream: %v", err))
		return
	}
	defer stream.Close()

	if follow {
		// Stream logs to client using chunked transfer.
		c.Writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		c.Writer.Header().Set("X-Content-Type-Options", "nosniff")
		c.Writer.WriteHeader(http.StatusOK)
		scanner := bufio.NewScanner(stream)
		for scanner.Scan() {
			_, _ = fmt.Fprintf(c.Writer, "%s\n", scanner.Text())
			c.Writer.Flush()
		}
	} else {
		data, err := io.ReadAll(stream)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "error reading logs: "+err.Error())
			return
		}
		c.Data(http.StatusOK, "text/plain; charset=utf-8", data)
	}
}

// ─────────────────────────────── Cache ───────────────────────────────────────

// handleListCache godoc
// @Summary      List cache entries
// @Description  Returns all in-memory cache entries for the given agent
// @Tags         cache
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]interface{}
// @Router       /api/v1/agents/{name}/cache [get]
func (s *Server) handleListCache(c *gin.Context) {
	namespace, name := s.nsName(c)
	entries := s.cache.List(namespace, name)
	respondOK(c, entries)
}

// handleGetCacheField godoc
// @Summary      Get cache field
// @Description  Returns a single cache entry by field name. Returns 404 if missing or expired.
// @Tags         cache
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Param        field      path      string  true  "Cache field name"
// @Success      200        {object}  map[string]interface{}
// @Failure      404        {object}  map[string]string
// @Router       /api/v1/agents/{name}/cache/{field} [get]
func (s *Server) handleGetCacheField(c *gin.Context) {
	namespace, name := s.nsName(c)
	field := c.Param("field")
	entry, ok := s.cache.GetEntry(namespace, name, field)
	if !ok {
		respondError(c, http.StatusNotFound, "cache field not found or expired")
		return
	}
	respondOK(c, entry)
}

type CacheSetRequest struct {
	Value interface{} `json:"value" binding:"required"`
	TTL   int         `json:"ttl_seconds,omitempty"` // 0 = no expiry
}

// handleSetCacheField godoc
// @Summary      Set cache field
// @Description  Stores a value in the per-agent cache with an optional TTL. ttl_seconds=0 means no expiry.
// @Tags         cache
// @Accept       json
// @Produce      json
// @Param        name       path      string          true  "Agent name"
// @Param        field      path      string          true  "Cache field name"
// @Param        body       body      CacheSetRequest true  "Value and optional TTL"
// @Success      200        {object}  map[string]interface{}
// @Failure      400        {object}  map[string]string
// @Router       /api/v1/agents/{name}/cache/{field} [put]
func (s *Server) handleSetCacheField(c *gin.Context) {
	namespace, name := s.nsName(c)
	field := c.Param("field")

	var req CacheSetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}

	ttl := time.Duration(req.TTL) * time.Second
	s.cache.Set(namespace, name, field, req.Value, ttl)
	respondOK(c, gin.H{"field": field, "set": true})
}

// handleDeleteCacheField godoc
// @Summary      Delete cache field
// @Description  Removes a single field from the per-agent cache
// @Tags         cache
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Param        field      path      string  true  "Cache field name"
// @Success      200        {object}  map[string]interface{}  "deleted: true"
// @Router       /api/v1/agents/{name}/cache/{field} [delete]
func (s *Server) handleDeleteCacheField(c *gin.Context) {
	namespace, name := s.nsName(c)
	field := c.Param("field")
	s.cache.Delete(namespace, name, field)
	respondOK(c, gin.H{"field": field, "deleted": true})
}

// handleClearCache godoc
// @Summary      Clear agent cache
// @Description  Removes all cache entries for the given agent
// @Tags         cache
// @Produce      json
// @Param        name       path      string  true  "Agent name"
// @Success      200        {object}  map[string]interface{}  "cleared: true"
// @Router       /api/v1/agents/{name}/cache [delete]
func (s *Server) handleClearCache(c *gin.Context) {
	namespace, name := s.nsName(c)
	s.cache.ClearAgent(namespace, name)
	respondOK(c, gin.H{"cleared": true})
}
