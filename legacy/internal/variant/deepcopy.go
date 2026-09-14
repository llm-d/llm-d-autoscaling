package variant

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto copies the receiver into out.
func (in *ActuationStatus) DeepCopyInto(out *ActuationStatus) {
	*out = *in
}

// DeepCopy creates a new ActuationStatus copy of the receiver.
func (in *ActuationStatus) DeepCopy() *ActuationStatus {
	if in == nil {
		return nil
	}
	out := new(ActuationStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *OptimizedAlloc) DeepCopyInto(out *OptimizedAlloc) {
	*out = *in
	in.LastRunTime.DeepCopyInto(&out.LastRunTime)
	if in.NumReplicas != nil {
		in, out := &in.NumReplicas, &out.NumReplicas
		*out = new(int32)
		**out = **in
	}
}

// DeepCopy creates a new OptimizedAlloc copy of the receiver.
func (in *OptimizedAlloc) DeepCopy() *OptimizedAlloc {
	if in == nil {
		return nil
	}
	out := new(OptimizedAlloc)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VariantAutoscalingConfigSpec) DeepCopyInto(out *VariantAutoscalingConfigSpec) {
	*out = *in
}

// DeepCopy creates a new VariantAutoscalingConfigSpec copy of the receiver.
func (in *VariantAutoscalingConfigSpec) DeepCopy() *VariantAutoscalingConfigSpec {
	if in == nil {
		return nil
	}
	out := new(VariantAutoscalingConfigSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VariantAutoscalingSpec) DeepCopyInto(out *VariantAutoscalingSpec) {
	*out = *in
	out.ScaleTargetRef = in.ScaleTargetRef
	if in.MinReplicas != nil {
		in, out := &in.MinReplicas, &out.MinReplicas
		*out = new(int32)
		**out = **in
	}
	out.VariantAutoscalingConfigSpec = in.VariantAutoscalingConfigSpec
}

// DeepCopy creates a new VariantAutoscalingSpec copy of the receiver.
func (in *VariantAutoscalingSpec) DeepCopy() *VariantAutoscalingSpec {
	if in == nil {
		return nil
	}
	out := new(VariantAutoscalingSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VariantAutoscalingStatus) DeepCopyInto(out *VariantAutoscalingStatus) {
	*out = *in
	in.DesiredOptimizedAlloc.DeepCopyInto(&out.DesiredOptimizedAlloc)
	out.Actuation = in.Actuation
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a new VariantAutoscalingStatus copy of the receiver.
func (in *VariantAutoscalingStatus) DeepCopy() *VariantAutoscalingStatus {
	if in == nil {
		return nil
	}
	out := new(VariantAutoscalingStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VariantAutoscaling) DeepCopyInto(out *VariantAutoscaling) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy creates a new VariantAutoscaling copy of the receiver.
func (in *VariantAutoscaling) DeepCopy() *VariantAutoscaling {
	if in == nil {
		return nil
	}
	out := new(VariantAutoscaling)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *VariantAutoscaling) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *VariantAutoscalingList) DeepCopyInto(out *VariantAutoscalingList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]VariantAutoscaling, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy creates a new VariantAutoscalingList copy of the receiver.
func (in *VariantAutoscalingList) DeepCopy() *VariantAutoscalingList {
	if in == nil {
		return nil
	}
	out := new(VariantAutoscalingList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *VariantAutoscalingList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
