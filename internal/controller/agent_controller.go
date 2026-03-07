// Package controller implements the reconciliation loop for Agent custom resources.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	orchestratorv1alpha1 "github.com/jacekmyjkowski/k8s-agent-orchestrator/api/v1alpha1"
)

const (
	agentFinalizer    = "orchestrator.dev/finalizer"
	podOwnerLabel     = "orchestrator.dev/agent"
	podNamespaceLabel = "orchestrator.dev/namespace"
	managedByLabel    = "app.kubernetes.io/managed-by"
	managedByValue    = "k8s-agent-orchestrator"
)

// AgentReconciler reconciles an Agent object.
type AgentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=orchestrator.dev,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestrator.dev,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestrator.dev,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile is the main reconciliation loop for the Agent resource.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agent", req.NamespacedName)

	// Fetch the Agent CR.
	agent := &orchestratorv1alpha1.Agent{}
	if err := r.Get(ctx, req.NamespacedName, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching agent: %w", err)
	}

	// Handle deletion via finalizer.
	if !agent.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, agent)
	}

	// Register finalizer.
	if !controllerutil.ContainsFinalizer(agent, agentFinalizer) {
		controllerutil.AddFinalizer(agent, agentFinalizer)
		if err := r.Update(ctx, agent); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Find the Pod owned by this Agent.
	pod, err := r.findAgentPod(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}

	// If agent is paused, ensure pod is deleted and set Stopped phase.
	if agent.Spec.Paused {
		if pod != nil {
			logger.Info("Agent is paused, deleting Pod")
			if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("deleting pod for paused agent: %w", err)
			}
			r.Recorder.Event(agent, corev1.EventTypeNormal, "Paused", "Agent paused: Pod deleted and reconciliation halted")
		}
		_ = r.updateStatus(ctx, agent, orchestratorv1alpha1.AgentPhaseStopped, "", "Agent is paused")
		return ctrl.Result{}, nil
	}

	// If no Pod exists, create one.
	if pod == nil {
		logger.Info("Creating Pod for Agent")
		if err := r.createPod(ctx, agent); err != nil {
			r.Recorder.Eventf(agent, corev1.EventTypeWarning, "PodCreateFailed", "Failed to create Pod: %v", err)
			r.setCondition(agent, orchestratorv1alpha1.AgentConditionFailed, corev1.ConditionTrue, "PodCreateFailed", err.Error())
			_ = r.updateStatus(ctx, agent, orchestratorv1alpha1.AgentPhaseFailed, "", err.Error())
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		r.Recorder.Eventf(agent, corev1.EventTypeNormal, "PodCreated", "Pod created successfully for Agent %s", agent.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Detect spec drift (env changes → recreate pod).
	if r.specChanged(agent, pod) {
		logger.Info("Agent spec changed, recreating Pod")
		r.Recorder.Eventf(agent, corev1.EventTypeNormal, "PodRecreated", "Spec change detected: deleting Pod %s for recreation", pod.Name)
		_ = r.updateStatus(ctx, agent, orchestratorv1alpha1.AgentPhaseUpdating, pod.Name, "Applying spec update")
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting stale pod: %w", err)
		}
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
	}

	// Sync status from Pod phase.
	agentPhase := podPhaseToAgentPhase(pod.Status.Phase)
	message := fmt.Sprintf("Pod %s is %s", pod.Name, pod.Status.Phase)
	if err := r.updateStatus(ctx, agent, agentPhase, pod.Name, message); err != nil {
		return ctrl.Result{}, err
	}

	// Re-check after 30s to handle transient pod failures.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// handleDeletion removes the owned Pod and then removes the finalizer.
// If self-healing is not disabled, it enqueues a resurrection of the Agent CR
// AFTER the finalizer is removed — this ensures the old CR is actually gone from
// the API before the new one is created.
func (r *AgentReconciler) handleDeletion(ctx context.Context, agent *orchestratorv1alpha1.Agent) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agent", agent.Name, "namespace", agent.Namespace)

	// 1. Delete the owned Pod.
	pod, err := r.findAgentPod(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod != nil {
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting agent pod: %w", err)
		}
	}

	// 2. Capture resurrection data BEFORE removing the finalizer (agent object
	//    becomes invalid after Update removes the finalizer).
	shouldResurrect := !agent.Spec.SelfHealingDisabled
	var resurrectedSpec *orchestratorv1alpha1.AgentSpec
	var originalUID, agentName, agentNamespace string
	var agentLabels, agentAnnotations map[string]string
	if shouldResurrect {
		resurrectedSpec = agent.Spec.DeepCopy()
		originalUID = string(agent.UID)
		agentName = agent.Name
		agentNamespace = agent.Namespace
		agentLabels = make(map[string]string, len(agent.Labels))
		for k, v := range agent.Labels {
			agentLabels[k] = v
		}
		agentAnnotations = make(map[string]string, len(agent.Annotations))
		for k, v := range agent.Annotations {
			agentAnnotations[k] = v
		}
	}

	// 3. Remove finalizer — K8s will GC the old CR after this Update.
	controllerutil.RemoveFinalizer(agent, agentFinalizer)
	if err := r.Update(ctx, agent); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	// Resurrect AFTER finalizer removal, in a goroutine with a long-lived context
	//    so it is not cancelled when the reconcile request completes.
	if shouldResurrect {
		logger.Info("Self-healing: scheduling resurrection after finalizer removal")
		go r.resurrectAgentAsync(agentNamespace, agentName, originalUID, resurrectedSpec, agentLabels, agentAnnotations)
	}

	return ctrl.Result{}, nil
}

// resurrectAgentAsync runs in a goroutine: it waits for the old CR to be fully
// garbage-collected, then creates a new Agent CR with the same spec.
// It distinguishes between "old CR still terminating" (same UID → retry) and
// "a brand-new CR was already created" (different UID → skip).
func (r *AgentReconciler) resurrectAgentAsync(
	namespace, name, originalUID string,
	spec *orchestratorv1alpha1.AgentSpec,
	labels, annotations map[string]string,
) {
	bgCtx := context.Background()
	logger := log.FromContext(bgCtx).WithValues("agent", name, "namespace", namespace)
	now := metav1.Now()

	// Build resurrection annotations (preserve user annotations, add meta).
	resurrectedAnnotations := make(map[string]string, len(annotations)+2)
	for k, v := range annotations {
		resurrectedAnnotations[k] = v
	}
	resurrectedAnnotations["orchestrator.dev/resurrected-at"] = now.UTC().Format(time.RFC3339)
	resurrectedAnnotations["orchestrator.dev/resurrection-uid"] = originalUID

	resurrected := &orchestratorv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: resurrectedAnnotations,
		},
		Spec: *spec,
	}

	// Retry loop: the old CR may still be present in the API for a short time
	// after finalizer removal (Kubernetes GC is async).
	for attempt := 1; attempt <= 15; attempt++ {
		// Back off before each attempt; first attempt has a short delay to let GC run.
		time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)

		newAgent := resurrected.DeepCopy()
		err := r.Create(bgCtx, newAgent)
		if err == nil {
			logger.Info("Resurrected agent successfully", "attempt", attempt)
			r.Recorder.Eventf(newAgent, corev1.EventTypeNormal, "Resurrected",
				"Agent self-healed: recreated after external deletion (original UID %s, attempt %d)", originalUID, attempt)
			return
		}

		if !apierrors.IsAlreadyExists(err) {
			logger.Error(err, "Resurrection attempt failed, will retry", "attempt", attempt)
			continue
		}

		// AlreadyExists: check whether it's the OLD terminating CR (same UID)
		// or a genuinely new CR created by someone else.
		existing := &orchestratorv1alpha1.Agent{}
		if getErr := r.Get(bgCtx, types.NamespacedName{Namespace: namespace, Name: name}, existing); getErr != nil {
			// Can't read — might be a transient error; retry.
			continue
		}
		if string(existing.UID) == originalUID {
			// Same CR, still being garbage-collected — retry.
			logger.Info("Old CR still terminating, retrying", "attempt", attempt)
			continue
		}
		// Different UID: a new CR already exists (user re-created it manually).
		logger.Info("Resurrection skipped: new agent CR already exists with different UID")
		return
	}
	logger.Error(fmt.Errorf("gave up after 15 attempts"), "Failed to resurrect agent")
	// Emit a warning event on a stub object so it surfaces in the namespace event stream.
	stub := &orchestratorv1alpha1.Agent{}
	stub.Name = name
	stub.Namespace = namespace
	r.Recorder.Eventf(stub, corev1.EventTypeWarning, "ResurrectionFailed",
		"Self-healing failed: could not recreate Agent %s/%s after 15 attempts (original UID %s)", namespace, name, originalUID)
}

// findAgentPod returns the Pod owned by this Agent, or nil if not found.
func (r *AgentReconciler) findAgentPod(ctx context.Context, agent *orchestratorv1alpha1.Agent) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(agent.Namespace),
		client.MatchingLabels{
			podOwnerLabel:  agent.Name,
			managedByLabel: managedByValue,
		},
	); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.DeletionTimestamp.IsZero() {
			return p, nil
		}
	}
	return nil, nil
}

// createPod builds and creates a Pod for the given Agent.
func (r *AgentReconciler) createPod(ctx context.Context, agent *orchestratorv1alpha1.Agent) error {
	pod := r.buildPod(agent)
	if err := controllerutil.SetControllerReference(agent, pod, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference: %w", err)
	}
	return r.Create(ctx, pod)
}

// buildPod constructs the Pod spec from the Agent spec.
func (r *AgentReconciler) buildPod(agent *orchestratorv1alpha1.Agent) *corev1.Pod {
	labels := map[string]string{
		podOwnerLabel:  agent.Name,
		managedByLabel: managedByValue,
		"app":          agent.Name,
	}
	for k, v := range agent.Spec.PodLabels {
		labels[k] = v
	}

	annotations := map[string]string{
		"orchestrator.dev/generation": fmt.Sprintf("%d", agent.Generation),
	}
	for k, v := range agent.Spec.PodAnnotations {
		annotations[k] = v
	}

	pullPolicy := agent.Spec.ImagePullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}

	restartPolicy := agent.Spec.RestartPolicy
	if restartPolicy == "" {
		restartPolicy = corev1.RestartPolicyAlways
	}

	container := corev1.Container{
		Name:            "agent",
		Image:           agent.Spec.Image,
		ImagePullPolicy: pullPolicy,
		Env:             agent.Spec.Env,
		EnvFrom:         agent.Spec.EnvFrom,
		Resources:       agent.Spec.Resources,
		Command:         agent.Spec.Command,
		Args:            agent.Spec.Args,
		VolumeMounts:    agent.Spec.VolumeMounts,
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: agent.Name + "-",
			Namespace:    agent.Namespace,
			Labels:       labels,
			Annotations:  annotations,
		},
		Spec: corev1.PodSpec{
			Containers:         []corev1.Container{container},
			RestartPolicy:      restartPolicy,
			ServiceAccountName: agent.Spec.ServiceAccountName,
			ImagePullSecrets:   agent.Spec.ImagePullSecrets,
			Volumes:            agent.Spec.Volumes,
		},
	}
}

// specChanged returns true if the running pod's spec differs from the Agent spec
// in a way that requires pod recreation (env vars, image, command, args).
func (r *AgentReconciler) specChanged(agent *orchestratorv1alpha1.Agent, pod *corev1.Pod) bool {
	// Check generation annotation.
	ann := pod.Annotations["orchestrator.dev/generation"]
	expected := fmt.Sprintf("%d", agent.Generation)
	return ann != expected
}

// updateStatus patches the Agent's status subresource.
func (r *AgentReconciler) updateStatus(ctx context.Context, agent *orchestratorv1alpha1.Agent, phase orchestratorv1alpha1.AgentPhase, podName, message string) error {
	now := metav1.Now()
	patch := client.MergeFrom(agent.DeepCopy())
	agent.Status.Phase = phase
	agent.Status.PodName = podName
	agent.Status.Message = message
	agent.Status.ObservedGeneration = agent.Generation
	agent.Status.LastUpdated = &now
	return r.Status().Patch(ctx, agent, patch)
}

// setCondition sets or updates a named condition on the Agent.
func (r *AgentReconciler) setCondition(agent *orchestratorv1alpha1.Agent, condType orchestratorv1alpha1.AgentConditionType, status corev1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range agent.Status.Conditions {
		if c.Type == condType {
			agent.Status.Conditions[i].Status = status
			agent.Status.Conditions[i].Reason = reason
			agent.Status.Conditions[i].Message = message
			agent.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	agent.Status.Conditions = append(agent.Status.Conditions, orchestratorv1alpha1.AgentCondition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// podPhaseToAgentPhase maps a Kubernetes Pod phase to an AgentPhase.
func podPhaseToAgentPhase(phase corev1.PodPhase) orchestratorv1alpha1.AgentPhase {
	switch phase {
	case corev1.PodPending:
		return orchestratorv1alpha1.AgentPhasePending
	case corev1.PodRunning:
		return orchestratorv1alpha1.AgentPhaseRunning
	case corev1.PodSucceeded:
		return orchestratorv1alpha1.AgentPhaseStopped
	case corev1.PodFailed:
		return orchestratorv1alpha1.AgentPhaseFailed
	default:
		return orchestratorv1alpha1.AgentPhasePending
	}
}

// SetupWithManager registers the controller with the Manager.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orchestratorv1alpha1.Agent{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

// GetPodForAgent returns the current Pod name for a given Agent (used by REST API).
func (r *AgentReconciler) GetPodForAgent(ctx context.Context, namespace, agentName string) (*corev1.Pod, error) {
	agent := &orchestratorv1alpha1.Agent{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: agentName}, agent); err != nil {
		return nil, err
	}
	return r.findAgentPod(ctx, agent)
}
