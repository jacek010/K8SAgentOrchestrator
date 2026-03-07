package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentPhase represents the current lifecycle phase of an Agent.
type AgentPhase string

const (
	AgentPhasePending  AgentPhase = "Pending"
	AgentPhaseRunning  AgentPhase = "Running"
	AgentPhaseFailed   AgentPhase = "Failed"
	AgentPhaseStopped  AgentPhase = "Stopped"
	AgentPhaseUpdating AgentPhase = "Updating"
	// AgentPhaseRestoring is set while a protected Agent CR is being resurrected.
	AgentPhaseRestoring AgentPhase = "Restoring"
)

// AgentSpec defines the desired state of an Agent.
type AgentSpec struct {
	// Image is the container image to run.
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// ImagePullPolicy for the container. Defaults to IfNotPresent.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// ImagePullSecrets is an optional list of references to secrets to use for pulling the image.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Env is a list of environment variables to inject into the agent container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// EnvFrom lists sources to populate environment variables in the container.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// Resources specifies resource requirements for the agent container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// ServiceAccountName is the name of the ServiceAccount to use to run this agent Pod.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// Command overrides the ENTRYPOINT of the container.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args overrides the CMD of the container.
	// +optional
	Args []string `json:"args,omitempty"`

	// Volumes defines additional volumes to mount.
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// VolumeMounts defines additional volume mounts for the agent container.
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// RestartPolicy for the agent Pod. Defaults to Always.
	// +kubebuilder:validation:Enum=Always;OnFailure;Never
	// +optional
	RestartPolicy corev1.RestartPolicy `json:"restartPolicy,omitempty"`

	// Labels to add to the agent Pod.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// Annotations to add to the agent Pod.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// Paused, when true, prevents the controller from creating or recreating the Pod.
	// Use POST /stop to set this field via the REST API.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// SelfHealingDisabled, when true, turns off automatic resurrection of this Agent CR
	// when it is deleted externally (e.g. via kubectl delete agent).
	// By default (false) the orchestrator will recreate the Agent CR automatically.
	// +optional
	SelfHealingDisabled bool `json:"selfHealingDisabled,omitempty"`

	// ServicePort, when greater than 0, causes the controller to create a ClusterIP Service
	// named after the Agent that selects the agent Pod and exposes it on this port.
	// Set to 0 (or omit) to disable the Service.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	ServicePort int32 `json:"servicePort,omitempty"`

	// ServiceProtocol is the network protocol for the Service port.
	// Defaults to TCP.
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +optional
	ServiceProtocol corev1.Protocol `json:"serviceProtocol,omitempty"`

	// IdleTimeout is the number of seconds of inactivity after which the orchestrator
	// automatically pauses this agent (sets spec.paused=true, which deletes the Pod).
	// A value of 0 disables idle tracking for this agent.
	// When > 0: overrides the global --idle-timeout-default flag.
	// Activity is reset by any REST API call on this agent or by POST /agents/{name}/keepalive.
	// +kubebuilder:validation:Minimum=0
	// +optional
	IdleTimeout int32 `json:"idleTimeout,omitempty"`
}

// LifecycleEvent records a single lifecycle operation on this Agent.
// Entries are stored in Status.History and intentionally forwarded to each new CR
// created by the self-healing mechanism, giving kubectl describe a full audit
// trail that survives entity-UID churn from resurrections.
type LifecycleEvent struct {
	// Time when the event occurred.
	Time metav1.Time `json:"time"`
	// Reason is a short CamelCase identifier (e.g. Resurrected, Stopped, EnvMerged).
	Reason string `json:"reason"`
	// Type is "Normal" or "Warning".
	Type string `json:"type"`
	// Message is a human-readable description.
	Message string `json:"message"`
}

// AgentConditionType defines the type of condition.
type AgentConditionType string

const (
	AgentConditionReady   AgentConditionType = "Ready"
	AgentConditionSynced  AgentConditionType = "Synced"
	AgentConditionFailed  AgentConditionType = "Failed"
)

// AgentCondition describes the state of an Agent at a certain point.
type AgentCondition struct {
	Type               AgentConditionType     `json:"type"`
	Status             corev1.ConditionStatus `json:"status"`
	LastTransitionTime metav1.Time            `json:"lastTransitionTime,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	Message            string                 `json:"message,omitempty"`
}

// AgentStatus defines the observed state of an Agent.
type AgentStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase AgentPhase `json:"phase,omitempty"`

	// PodName is the name of the Pod currently managing this agent.
	// +optional
	PodName string `json:"podName,omitempty"`

	// PodIP is the IP address of the current agent Pod.
	// +optional
	PodIP string `json:"podIP,omitempty"`

	// Message provides a human-readable description of the current status.
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastUpdated is the time of the last status update.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// Conditions represents the latest available observations of the Agent's state.
	// +optional
	Conditions []AgentCondition `json:"conditions,omitempty"`

	// RestoredAt is set when this Agent CR was resurrected by the self-healing mechanism.
	// +optional
	RestoredAt *metav1.Time `json:"restoredAt,omitempty"`

	// RestoredFrom is the original Agent name that was deleted and triggered this resurrection.
	// +optional
	RestoredFrom string `json:"restoredFrom,omitempty"`

	// ServiceName is the name of the ClusterIP Service created for this Agent.
	// Populated only when spec.servicePort > 0.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// History contains an ordered list of lifecycle events for this Agent (newest last).
	// Unlike Kubernetes Events, entries survive resurrections because they are stored in
	// the Agent's status subresource and forwarded to each new CR on self-healing.
	// Capped at 100 entries; oldest are evicted first.
	// +optional
	History []LifecycleEvent `json:"history,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=agt,categories=all
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Pod",type="string",JSONPath=".status.podName"
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.image"
// +kubebuilder:printcolumn:name="HealingOff",type="boolean",JSONPath=".spec.selfHealingDisabled"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Agent is the Schema for the agents API.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec,omitempty"`
	Status AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
