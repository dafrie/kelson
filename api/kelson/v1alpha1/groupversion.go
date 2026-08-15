package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// Group is the API group kelson claims. Nothing else claims it, which is
	// the whole of ADR-0027's argument that installing these CRDs is additive:
	// registering a group nobody else owns mutates no existing object and
	// changes no existing behaviour.
	Group = "kelson.dev"

	// Version is the stored and served version. It matches the apiVersion the
	// authoring documents already carry (model.APIVersion), so a file a user
	// wrote for `kelson render` is the same document `kubectl apply` accepts.
	Version = "v1alpha1"
)

// GroupVersion is the group and version of every type in this package.
var GroupVersion = schema.GroupVersion{Group: Group, Version: Version}

// SchemeBuilder registers this package's types with a runtime.Scheme.
//
// It is apimachinery's builder rather than controller-runtime's
// (sigs.k8s.io/controller-runtime/pkg/scheme.Builder) on purpose: this package
// is the contract other people's code imports, and a consumer using
// client-go directly should not have to take a controller-runtime dependency to
// register the kinds. The controller takes controller-runtime; the API does not.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds the kelson.dev/v1alpha1 kinds to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&Project{}, &ProjectList{},
		&Environment{}, &EnvironmentList{},
		&GitConnection{}, &GitConnectionList{},
		&GitSource{}, &GitSourceList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// Resource qualifies an unqualified resource name with this group, for the
// errors client-go builds (`projects.kelson.dev "x" not found`).
func Resource(resource string) schema.GroupResource {
	return GroupVersion.WithResource(resource).GroupResource()
}
