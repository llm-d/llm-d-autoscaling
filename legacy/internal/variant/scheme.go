package variant

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion identifies the (historical) VariantAutoscaling group/version.
//
// VariantAutoscaling is no longer a served CRD; it exists only as an in-memory
// type synthesized from annotated HPAs/ScaledObjects. GroupVersion and AddToScheme
// are retained solely so unit tests can register the type with a controller-runtime
// fake client, which requires every object it stores to be scheme-registered. The
// production manager scheme (cmd/main.go) does NOT register these types.
var GroupVersion = schema.GroupVersion{Group: "llmd.ai", Version: "v1alpha1"}

// SchemeBuilder registers the VariantAutoscaling types with a runtime.Scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the VariantAutoscaling types to the given scheme. Intended for
// test fake clients only — see GroupVersion.
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(&VariantAutoscaling{}, &VariantAutoscalingList{})
}

// ensure the types satisfy runtime.Object at compile time.
var (
	_ runtime.Object = (*VariantAutoscaling)(nil)
	_ runtime.Object = (*VariantAutoscalingList)(nil)
)
