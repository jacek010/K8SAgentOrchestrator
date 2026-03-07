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

func (s *Server) handleHealthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

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
	Name            string                        `json:"name" binding:"required"`
	Image           string                        `json:"image" binding:"required"`
	ImagePullPolicy string                        `json:"imagePullPolicy,omitempty"`
	Env             []corev1.EnvVar               `json:"env,omitempty"`
	EnvFrom         []corev1.EnvFromSource        `json:"envFrom,omitempty"`
	Resources       corev1.ResourceRequirements   `json:"resources,omitempty"`
	Command         []string                      `json:"command,omitempty"`
	Args            []string                      `json:"args,omitempty"`
	ServiceAccount  string                        `json:"serviceAccountName,omitempty"`
	RestartPolicy   string                        `json:"restartPolicy,omitempty"`
	PodLabels       map[string]string             `json:"podLabels,omitempty"`
	PodAnnotations  map[string]string             `json:"podAnnotations,omitempty"`
	Labels          map[string]string             `json:"labels,omitempty"`
	Annotations     map[string]string             `json:"annotations,omitempty"`
}

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
			Image:              req.Image,
			ImagePullPolicy:    pullPolicy,
			Env:                req.Env,
			EnvFrom:            req.EnvFrom,
			Resources:          req.Resources,
			Command:            req.Command,
			Args:               req.Args,
			ServiceAccountName: req.ServiceAccount,
			RestartPolicy:      restartPolicy,
			PodLabels:          req.PodLabels,
			PodAnnotations:     req.PodAnnotations,
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
	respondCreated(c, agent)
}

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

	if err := s.client.Update(ctx, agent); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, agent)
}

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

	if err := s.client.Delete(ctx, agent); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}

	// Clean up the in-memory cache for this agent.
	s.cache.ClearAgent(namespace, name)

	c.JSON(http.StatusOK, gin.H{"status": "deleted", "name": name})
}

// ─────────────────────────────── Lifecycle ───────────────────────────────────

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
	respondOK(c, gin.H{"restarted": true})
}

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
	// Use a Never restart policy so the pod stops and is not restarted.
	agent.Spec.RestartPolicy = corev1.RestartPolicyNever
	if agent.Annotations == nil {
		agent.Annotations = make(map[string]string)
	}
	agent.Annotations["orchestrator.dev/stopped-at"] = time.Now().UTC().Format(time.RFC3339)

	if err := s.client.Patch(ctx, agent, patch); err != nil {
		respondError(c, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(c, gin.H{"stopped": true})
}

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
	if agent.Spec.RestartPolicy == corev1.RestartPolicyNever {
		agent.Spec.RestartPolicy = corev1.RestartPolicyAlways
	}
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
	respondOK(c, gin.H{"started": true})
}

// ─────────────────────────────── Env management ───────────────────────────────

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
	respondOK(c, agent.Spec.Env)
}

// EnvMergeRequest merges/overrides individual env vars (upsert by name).
type EnvMergeRequest struct {
	Env []corev1.EnvVar `json:"env" binding:"required"`
}

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
	respondOK(c, agent.Spec.Env)
}

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

func (s *Server) handleListCache(c *gin.Context) {
	namespace, name := s.nsName(c)
	entries := s.cache.List(namespace, name)
	respondOK(c, entries)
}

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

func (s *Server) handleDeleteCacheField(c *gin.Context) {
	namespace, name := s.nsName(c)
	field := c.Param("field")
	s.cache.Delete(namespace, name, field)
	respondOK(c, gin.H{"field": field, "deleted": true})
}

func (s *Server) handleClearCache(c *gin.Context) {
	namespace, name := s.nsName(c)
	s.cache.ClearAgent(namespace, name)
	respondOK(c, gin.H{"cleared": true})
}
