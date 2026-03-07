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
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=orchestrator.dev,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestrator.dev,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestrator.dev,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get

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

	// If no Pod exists, create one.
	if pod == nil {
		logger.Info("Creating Pod for Agent")
		if err := r.createPod(ctx, agent); err != nil {
			r.setCondition(agent, orchestratorv1alpha1.AgentConditionFailed, corev1.ConditionTrue, "PodCreateFailed", err.Error())
			_ = r.updateStatus(ctx, agent, orchestratorv1alpha1.AgentPhaseFailed, "", err.Error())
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Detect spec drift (env changes → recreate pod).
	if r.specChanged(agent, pod) {
		logger.Info("Agent spec changed, recreating Pod")
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
func (r *AgentReconciler) handleDeletion(ctx context.Context, agent *orchestratorv1alpha1.Agent) (ctrl.Result, error) {
	pod, err := r.findAgentPod(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod != nil {
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting agent pod: %w", err)
		}
	}
	controllerutil.RemoveFinalizer(agent, agentFinalizer)
	if err := r.Update(ctx, agent); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
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
