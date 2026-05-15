package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		&ControllerPool{},
		&ControllerPoolList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
