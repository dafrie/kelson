package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dafrie/kelson/internal/model"
)

// Kind names, matching the document kinds users already write
// (model.KindProject, model.KindEnvironment, model.KindGitConnection).
// ADR-0027 decision 1: a CRD that did not mirror the authoring documents would
// be a third spelling of the same thing.
const (
	KindProject           = "Project"
	KindProjectList       = "ProjectList"
	KindEnvironment       = "Environment"
	KindEnvironmentList   = "EnvironmentList"
	KindGitConnection     = "GitConnection"
	KindGitConnectionList = "GitConnectionList"
)

// Project is the shared-configuration document as a custom resource: the
// image/build coordinates, the shared environment and the components that
// deploy together, with nothing environment-specific in it.
//
// Spec is model.ProjectSpec verbatim. The document a user writes for
// `kelson render -f project.yaml` and the document `kubectl apply -f
// project.yaml` accepts are the same bytes.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   model.ProjectSpec `json:"spec,omitempty"`
	Status ProjectStatus     `json:"status,omitempty"`
}

// ProjectList is a list of Projects.
//
// +kubebuilder:object:root=true
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Project `json:"items"`
}

// Environment is where a Project runs and what differs there, as a custom
// resource: target namespace, routing, policy, secret backend and the
// per-component overrides.
//
// It binds to its Project by name in the same namespace (spec.project). The
// reference is namespace-local and deliberately not a cross-namespace one: a
// Project and its Environments are one unit of ownership, and a reference that
// could reach across namespaces would make "who may deploy this" a question
// about two RBAC scopes instead of one.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type Environment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   model.EnvironmentSpec `json:"spec,omitempty"`
	Status EnvironmentStatus     `json:"status,omitempty"`
}

// EnvironmentList is a list of Environments.
//
// +kubebuilder:object:root=true
type EnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Environment `json:"items"`
}

// GitConnection is the forge kelson can talk to and the Secret it talks with,
// as a custom resource (ADR-0033 decision 1).
//
// It is the one kind here that is not an authoring document. A Project and an
// Environment describe something to run; a connection describes something
// kelson may *do* — clone a private repository, mint an installation token,
// call a forge API — and the renderer never sees one. It lives in kelson's own
// namespace beside the Secrets it names, rather than in an application's.
//
// The spec carries identifiers and Secret names and never a credential value,
// which is ADR-0009 unchanged: `kubectl get gitconnection -o yaml` shows an
// operator what this cluster can pull from and reveals nothing that could pull
// from it.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type GitConnection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   model.GitConnectionSpec `json:"spec,omitempty"`
	Status GitConnectionStatus     `json:"status,omitempty"`
}

// GitConnectionList is a list of GitConnections.
//
// +kubebuilder:object:root=true
type GitConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GitConnection `json:"items"`
}
