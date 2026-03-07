package v1alpha1

// This file contains hand-written DeepCopy implementations for types that
// controller-gen v0.20+ omits when they are not CRD root types.
// Intentionally NOT named zz_generated_* so it is never overwritten by make generate.

import (
	corev1 "k8s.io/api/core/v1"
)

// DeepCopyInto copies AgentCondition into out.
func (in *AgentCondition) DeepCopyInto(out *AgentCondition) {
	*out = *in
	in.LastTransitionTime.DeepCopyInto(&out.LastTransitionTime)
}

// DeepCopy returns a deep copy of AgentCondition.
func (in *AgentCondition) DeepCopy() *AgentCondition {
	if in == nil {
		return nil
	}
	out := new(AgentCondition)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies LifecycleEvent into out.
func (in *LifecycleEvent) DeepCopyInto(out *LifecycleEvent) {
	*out = *in
	in.Time.DeepCopyInto(&out.Time)
}

// DeepCopy returns a deep copy of LifecycleEvent.
func (in *LifecycleEvent) DeepCopy() *LifecycleEvent {
	if in == nil {
		return nil
	}
	out := new(LifecycleEvent)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies AgentSpec into out.
func (in *AgentSpec) DeepCopyInto(out *AgentSpec) {
	*out = *in
	if in.ImagePullSecrets != nil {
		in, out := &in.ImagePullSecrets, &out.ImagePullSecrets
		*out = make([]corev1.LocalObjectReference, len(*in))
		copy(*out, *in)
	}
	if in.Env != nil {
		in, out := &in.Env, &out.Env
		*out = make([]corev1.EnvVar, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.EnvFrom != nil {
		in, out := &in.EnvFrom, &out.EnvFrom
		*out = make([]corev1.EnvFromSource, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	in.Resources.DeepCopyInto(&out.Resources)
	if in.Command != nil {
		in, out := &in.Command, &out.Command
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.Args != nil {
		in, out := &in.Args, &out.Args
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
	if in.Volumes != nil {
		in, out := &in.Volumes, &out.Volumes
		*out = make([]corev1.Volume, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.VolumeMounts != nil {
		in, out := &in.VolumeMounts, &out.VolumeMounts
		*out = make([]corev1.VolumeMount, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.PodLabels != nil {
		in, out := &in.PodLabels, &out.PodLabels
		*out = make(map[string]string, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
	if in.PodAnnotations != nil {
		in, out := &in.PodAnnotations, &out.PodAnnotations
		*out = make(map[string]string, len(*in))
		for k, v := range *in {
			(*out)[k] = v
		}
	}
}

// DeepCopy returns a deep copy of AgentSpec.
func (in *AgentSpec) DeepCopy() *AgentSpec {
	if in == nil {
		return nil
	}
	out := new(AgentSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies AgentStatus into out.
func (in *AgentStatus) DeepCopyInto(out *AgentStatus) {
	*out = *in
	if in.LastUpdated != nil {
		in, out := &in.LastUpdated, &out.LastUpdated
		*out = (*in).DeepCopy()
	}
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]AgentCondition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.RestoredAt != nil {
		in, out := &in.RestoredAt, &out.RestoredAt
		*out = (*in).DeepCopy()
	}
	if in.History != nil {
		in, out := &in.History, &out.History
		*out = make([]LifecycleEvent, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy returns a deep copy of AgentStatus.
func (in *AgentStatus) DeepCopy() *AgentStatus {
	if in == nil {
		return nil
	}
	out := new(AgentStatus)
	in.DeepCopyInto(out)
	return out
}
