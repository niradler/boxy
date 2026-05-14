package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion = schema.GroupVersion{Group: "boxy.dev", Version: "v1alpha1"}

	schemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	AddToScheme = schemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&Sandbox{},
		&SandboxList{},
	)
	return nil
}
