package serverstate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// Spec-store layout (ADR-0013 §1): one ConfigMap per project holding the
// authored YAML documents verbatim.
const (
	specNamePrefix = "kelson-spec-"
	projectDocKey  = "project.yaml"
	docKeySuffix   = ".yaml"
)

// writeAttempts bounds the read-modify-write retry a forced write performs when
// something else changed the object between the read and the update.
const writeAttempts = 3

// Documents are the authored YAML documents of one project, exactly as the
// user wrote them.
//
// The store keeps bytes, not a parsed model. The spec is the user's document:
// round-trips must be byte-faithful, comments and key order survive, and
// storing a normalized form would recreate the two-homes problem ADR-0002
// rejected for CRDs — the server would own a second version of a spec whose
// home is the user's file or their Git repository.
type Documents struct {
	// Project is the project document (project.yaml).
	Project []byte
	// Environments maps an environment name to its document. The name is the
	// key without the ".yaml" suffix the ConfigMap stores it under.
	Environments map[string][]byte
}

// Stored is one project's spec as the store holds it.
type Stored struct {
	// Project is the project name.
	Project string
	// Documents are the stored bytes.
	Documents Documents
	// Version is the ConfigMap's resourceVersion, opaque to callers and passed
	// back on the next write as the optimistic-concurrency token (ADR-0013 §1:
	// what Kubernetes already guarantees, kelson does not reimplement).
	Version string
	// Environments are the environment names present, sorted, so a caller can
	// list environments without decoding any YAML.
	Environments []string
}

// PutOptions carries the write-time controls every mutating RPC shares
// (ADR-0013 §2).
type PutOptions struct {
	// ExpectedVersion is the Version the caller read before editing. It is
	// required to overwrite an existing project and must be empty to create
	// one: a write with neither a version nor Force is a blind overwrite, and
	// blind overwrites are exactly what optimistic concurrency exists to
	// refuse.
	ExpectedVersion string
	// Force overrides the version check. It is the deliberate "I know, take my
	// version" escape hatch, never a default.
	Force bool
	// IdempotencyKey, when it matches the key recorded on the stored object,
	// makes this call a replay: the stored state is returned as success and
	// nothing is written.
	IdempotencyKey string
}

// DeleteOptions carries the same controls for a delete.
type DeleteOptions struct {
	ExpectedVersion string
	Force           bool
	// IdempotencyKey makes a delete replayable. A deleted ConfigMap takes its
	// annotations with it, so a replayed delete is indistinguishable from a
	// first delete of a project that never existed; with a key present the
	// absence is therefore reported as success — the recorded outcome of a
	// completed delete — and without one it is a store/not-found.
	IdempotencyKey string
}

// SpecStoreOptions configures a SpecStore.
type SpecStoreOptions struct {
	// Client is the typed client for the namespace the server runs against.
	Client kubernetes.Interface
	// Namespace is where state ConfigMaps live (default kelson-system,
	// flag-configurable on the server).
	Namespace string
}

// SpecStore is the cluster-backed store of project specs.
type SpecStore struct {
	client    kubernetes.Interface
	namespace string
}

// NewSpecStore returns a SpecStore reading and writing ConfigMaps in one
// namespace.
func NewSpecStore(opts SpecStoreOptions) (*SpecStore, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("serverstate: a Kubernetes client is required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("serverstate: a namespace is required")
	}
	return &SpecStore{client: opts.Client, namespace: opts.Namespace}, nil
}

// Put stores a project's documents, honouring optimistic concurrency and the
// idempotency-key replay contract.
func (s *SpecStore) Put(ctx context.Context, project string, docs Documents, opts PutOptions) (Stored, error) {
	if err := validSegment("project", project); err != nil {
		return Stored{}, err
	}
	data, err := specData(project, docs)
	if err != nil {
		return Stored{}, err
	}

	ref := specRef(project)
	cms := s.client.CoreV1().ConfigMaps(s.namespace)

	for attempt := 0; attempt < writeAttempts; attempt++ {
		existing, err := cms.Get(ctx, specName(project), metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if opts.ExpectedVersion != "" && !opts.Force {
				return Stored{}, NotFound(ref,
					fmt.Sprintf("no spec is stored for project %q, so version %q cannot be updated", project, opts.ExpectedVersion),
					"put without a version to create the project")
			}
			created, err := cms.Create(ctx, s.configMap(project, data, opts.IdempotencyKey, ""), metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				// Another writer created the project between the read and the
				// create; that is the same lost update a stale version is.
				return Stored{}, withCause(VersionConflict(ref,
					fmt.Sprintf("project %q was created concurrently", project),
					"re-read the spec with GetSpec and retry the write with the version it returns"), err)
			}
			if err != nil {
				return Stored{}, fmt.Errorf("serverstate: create spec for %s: %w", ref, err)
			}
			return decodeStored(created)
		case err != nil:
			return Stored{}, fmt.Errorf("serverstate: read spec for %s: %w", ref, err)
		}

		// The replay check comes before the version check: a retried request
		// carries the version its first attempt was written against, which the
		// first attempt has already superseded.
		if opts.IdempotencyKey != "" && existing.Annotations[annIdempotencyKey] == opts.IdempotencyKey {
			return decodeStored(existing)
		}
		if !opts.Force {
			if err := checkVersion(ref, project, opts.ExpectedVersion, existing.ResourceVersion); err != nil {
				return Stored{}, err
			}
		}

		updated, err := cms.Update(ctx, s.configMap(project, data, opts.IdempotencyKey, existing.ResourceVersion), metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			if opts.Force {
				continue // Something wrote in between; force means take it anyway.
			}
			return Stored{}, withCause(VersionConflict(ref,
				fmt.Sprintf("project %q changed while this write was in flight", project),
				"re-read the spec with GetSpec and retry the write with the version it returns"), err)
		}
		if err != nil {
			return Stored{}, fmt.Errorf("serverstate: write spec for %s: %w", ref, err)
		}
		return decodeStored(updated)
	}
	return Stored{}, VersionConflict(ref,
		fmt.Sprintf("project %q is being written concurrently and did not settle in %d attempts", project, writeAttempts),
		"retry the write; if it persists, another client is writing this project in a loop")
}

// Get returns one project's stored documents.
func (s *SpecStore) Get(ctx context.Context, project string) (Stored, error) {
	if err := validSegment("project", project); err != nil {
		return Stored{}, err
	}
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, specName(project), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Stored{}, notStored(project)
	}
	if err != nil {
		return Stored{}, fmt.Errorf("serverstate: read spec for %s: %w", specRef(project), err)
	}
	return decodeStored(cm)
}

// List returns every stored project, ordered by name.
func (s *SpecStore) List(ctx context.Context) ([]Stored, error) {
	list, err := s.client.CoreV1().ConfigMaps(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			labelManagedBy: managedByKelson,
			labelState:     stateSpec,
		}).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("serverstate: list specs in %s: %w", s.namespace, err)
	}
	out := make([]Stored, 0, len(list.Items))
	for i := range list.Items {
		stored, err := decodeStored(&list.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, stored)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out, nil
}

// Delete removes a project's spec.
func (s *SpecStore) Delete(ctx context.Context, project string, opts DeleteOptions) error {
	if err := validSegment("project", project); err != nil {
		return err
	}
	ref := specRef(project)
	cms := s.client.CoreV1().ConfigMaps(s.namespace)

	for attempt := 0; attempt < writeAttempts; attempt++ {
		existing, err := cms.Get(ctx, specName(project), metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if opts.IdempotencyKey != "" {
				return nil
			}
			return notStored(project)
		case err != nil:
			return fmt.Errorf("serverstate: read spec for %s: %w", ref, err)
		}
		if !opts.Force {
			if err := checkVersion(ref, project, opts.ExpectedVersion, existing.ResourceVersion); err != nil {
				return err
			}
		}

		rv := existing.ResourceVersion
		delOpts := metav1.DeleteOptions{}
		if rv != "" {
			delOpts.Preconditions = &metav1.Preconditions{ResourceVersion: &rv}
		}
		err = cms.Delete(ctx, specName(project), delOpts)
		switch {
		case apierrors.IsNotFound(err):
			return nil // Someone else deleted it; the requested end state holds.
		case apierrors.IsConflict(err):
			if opts.Force {
				continue
			}
			return withCause(VersionConflict(ref,
				fmt.Sprintf("project %q changed while the delete was in flight", project),
				"re-read the spec with GetSpec and retry the delete with the version it returns"), err)
		case err != nil:
			return fmt.Errorf("serverstate: delete spec for %s: %w", ref, err)
		}
		return nil
	}
	return VersionConflict(ref,
		fmt.Sprintf("project %q is being written concurrently and did not settle in %d attempts", project, writeAttempts),
		"retry the delete; if it persists, another client is writing this project in a loop")
}

// configMap builds the object one Put writes. resourceVersion is empty on
// create and the read version on update, so the API server enforces the same
// check the store just made.
func (s *SpecStore) configMap(project string, data map[string]string, idempotencyKey, resourceVersion string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            specName(project),
			Namespace:       s.namespace,
			ResourceVersion: resourceVersion,
			Labels: map[string]string{
				labelManagedBy: managedByKelson,
				labelProject:   project,
				labelState:     stateSpec,
			},
		},
		Data: data,
	}
	// The annotation describes the write that produced the current contents,
	// so a write without a key clears a predecessor's rather than inheriting
	// it and making an unrelated request look like a replay.
	if idempotencyKey != "" {
		cm.Annotations = map[string]string{annIdempotencyKey: idempotencyKey}
	}
	return cm
}

// specData lays the documents out as ConfigMap keys.
//
// Data (string values) rather than BinaryData: the ADR's argument for
// ConfigMaps is that `kubectl get configmap kelson-spec-shop -o yaml` is a
// readable audit trail, which base64 would destroy. YAML is UTF-8 by
// specification, so the string constraint costs nothing a valid spec cares
// about — and the API server rejects the invalid case loudly rather than
// storing mojibake.
func specData(project string, docs Documents) (map[string]string, error) {
	if len(docs.Project) == 0 {
		return nil, fmt.Errorf("serverstate: project %q has no project document", project)
	}
	data := map[string]string{projectDocKey: string(docs.Project)}
	for env, doc := range docs.Environments {
		if err := validSegment("environment", env); err != nil {
			return nil, err
		}
		key := env + docKeySuffix
		if key == projectDocKey {
			return nil, fmt.Errorf("serverstate: environment %q collides with the project document key %q", env, projectDocKey)
		}
		if len(doc) == 0 {
			return nil, fmt.Errorf("serverstate: environment %q has an empty document", env)
		}
		data[key] = string(doc)
	}
	return data, nil
}

// decodeStored is the inverse of specData.
func decodeStored(cm *corev1.ConfigMap) (Stored, error) {
	project := cm.Labels[labelProject]
	if project == "" {
		return Stored{}, fmt.Errorf("serverstate: ConfigMap %s/%s carries no %s label", cm.Namespace, cm.Name, labelProject)
	}
	stored := Stored{
		Project: project,
		Version: cm.ResourceVersion,
		Documents: Documents{
			Project:      []byte(cm.Data[projectDocKey]),
			Environments: map[string][]byte{},
		},
	}
	for key, doc := range cm.Data {
		if key == projectDocKey || !strings.HasSuffix(key, docKeySuffix) {
			continue
		}
		env := strings.TrimSuffix(key, docKeySuffix)
		stored.Documents.Environments[env] = []byte(doc)
		stored.Environments = append(stored.Environments, env)
	}
	sort.Strings(stored.Environments)
	return stored, nil
}

// checkVersion is the optimistic-concurrency gate. An empty expected version
// against an existing object is refused rather than treated as "create or
// overwrite": the caller that has not read the spec cannot know what it is
// about to replace.
func checkVersion(ref, project, expected, actual string) error {
	if expected == "" {
		return VersionConflict(ref,
			fmt.Sprintf("project %q already exists and the write carried no version", project),
			"read the spec with GetSpec and put it back with the version it returned, or set force to overwrite deliberately")
	}
	if expected != actual {
		return VersionConflict(ref,
			fmt.Sprintf("project %q is at version %q, the write expected %q", project, actual, expected),
			"re-read the spec with GetSpec, re-apply the edit and retry with the version it returns")
	}
	return nil
}

func notStored(project string) Error {
	return NotFound(specRef(project),
		fmt.Sprintf("no spec is stored for project %q", project),
		"list the stored projects with ListSpecs, or create this one with PutSpec")
}

func specName(project string) string { return specNamePrefix + project }

// specRef is the resource identity carried on errors: the project, not the
// ConfigMap, because the ConfigMap is an implementation detail the CRD
// successor replaces.
func specRef(project string) string { return "spec/" + project }
