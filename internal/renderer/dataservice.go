package renderer

import (
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/postgres"
	"github.com/dafrie/kelson/internal/model"
)

// Managed data services, rendered as CloudNativePG resources (ADR-0005,
// ADR-0007, issue #89). docs/data-services.md is the design reference and
// carries the rationale for every number and field name below; this file is
// the implementation and states only what the code needs.

// cnpgAPIVersion is the CloudNativePG API group kelson writes. kelson targets
// the latest operator release and relies on its declarative surface with no
// imperative fallback (owner decision 2026-08-13, internal/clusterprofile/postgres).
const cnpgAPIVersion = "postgresql.cnpg.io/v1"

// SharedClusterNamespace is where the `shared` preset's Database resources are
// rendered, because that is where the shared cluster lives.
//
// CloudNativePG's Database.spec.cluster is a corev1.LocalObjectReference: a
// name and nothing else, with no namespace field and no cross-namespace form.
// A Database must therefore sit in its cluster's namespace, and the shared
// cluster is kelson-owned infrastructure serving every project in an
// environment tier (ADR-0007's resource arithmetic only works that way), so it
// cannot live in a project's namespace. Provisioning it is issue #93; this is
// only the name the rendered Database points at.
const SharedClusterNamespace = "kelson-data"

// maxServiceResourceName caps the name kelson derives for a service resource.
// CloudNativePG derives its own object names from the cluster's — <cluster>-app,
// <cluster>-superuser, <cluster>-rw, <cluster>-1 — and the longest of those
// suffixes is ten characters, which has to fit inside the 63-character DNS
// label limit. Refusing here beats an API server rejecting the manifest with
// an arithmetic complaint about a name the author never wrote.
const maxServiceResourceName = 53

// postgresBindingKeys maps kelson's well-known binding keys (model.ServiceKeys)
// onto the keys CloudNativePG actually writes into the <cluster>-app Secret.
// Verified against pkg/specs/secrets.go in CloudNativePG release-1.30, which
// writes username, user, password, dbname, host, port, pgpass, uri, jdbc-uri,
// fqdn-uri and fqdn-jdbc-uri.
//
// Only `database` differs, and that is exactly the kind of mismatch a mapping
// exists for: kelson's key names are the spec's contract and CNPG's are the
// operator's, so neither side has to rename anything.
var postgresBindingKeys = map[string]string{
	"uri":      "uri",
	"host":     "host",
	"port":     "port",
	"database": "dbname",
	"username": "username",
	"password": "password",
}

// dedicatedPreset is the rendered shape of one dedicated-cluster preset. The
// preset *is* the sizing: there is no per-service resources override in v0
// (docs/data-services.md, "Tuning without leaving the preset model").
type dedicatedPreset struct {
	instances int
	cpu       string
	memory    string
	storage   string
	// syncReplicas is the min and max synchronous replica count. Zero means
	// asynchronous replication, which is the only honest setting for a
	// single-instance cluster.
	syncReplicas int
}

// dedicatedPresets is the sizing table of docs/data-services.md. Memory limit
// equals the request (an OOM-killed primary is a failover) and there is no CPU
// limit (throttling a database turns a slow query into a slow cluster).
var dedicatedPresets = map[model.ServicePreset]dedicatedPreset{
	model.PresetSmall:    {instances: 1, cpu: "500m", memory: "1Gi", storage: "5Gi"},
	model.PresetHASmall:  {instances: 3, cpu: "500m", memory: "1Gi", storage: "5Gi", syncReplicas: 1},
	model.PresetHAMedium: {instances: 3, cpu: "2", memory: "4Gi", storage: "20Gi", syncReplicas: 1},
}

// initdbDatabase is the application database and owner role name inside a
// dedicated cluster. CloudNativePG's own defaults, written explicitly: one
// database per cluster is CNPG's guidance, and the cluster is already scoped by
// its name and namespace, so nothing is gained by qualifying it again.
const initdbDatabase = "app"

// boundService is what an application binding resolves against: the Secret
// holding the service's credentials and the key mapping into it. A service
// that renders but cannot yet be bound to carries the reason instead, so the
// binding error can name it rather than emitting a reference to a Secret
// nothing creates (issue #141).
type boundService struct {
	name   string
	secret string
	keys   map[string]string

	reason      string
	remediation string
}

// serviceResourceName is the name every resource kelson renders for a service
// carries: <project>-<environment>-<service>.
//
// It is qualified even for the dedicated presets, where the namespace already
// disambiguates, so that one rule covers both topologies — in the shared
// cluster the qualification is not optional, because a database called `db`
// collides with the first other project that wants one.
func serviceResourceName(project, environment, service string) string {
	return project + "-" + environment + "-" + service
}

// sharedClusterName is the CNPG Cluster the `shared` preset's databases are
// created in: one per environment tier, so two projects' `development`
// environments share a cluster, which is the whole point of the preset. The
// environment name is the only pure input that identifies the tier. Issue #93
// provisions it and may make this configurable.
func sharedClusterName(environment string) string {
	return "kelson-shared-" + environment
}

// appSecretName is the Secret CloudNativePG generates for a cluster's
// application owner during initdb bootstrap: <cluster>-app, of type
// basic-auth. kelson never renders it — it is the operator's output, and a
// rendered Secret would put a credential in a manifest (ADR-0009).
func appSecretName(cluster string) string { return cluster + "-app" }

// serviceManifests renders one resolved service, and returns what applications
// binding to it should reference.
//
// Every refusal here is structured and names the issue that would lift it. A
// service kelson cannot render must never render *something else* quietly:
// that is the failure mode issue #141 exists to prevent, and a database is the
// worst possible place to reintroduce it.
func serviceManifests(
	resolved *model.Resolved,
	svc *model.ResolvedService,
	profile clusterprofile.ClusterProfile,
) ([]Manifest, boundService, error) {
	if svc.Type != "postgres" {
		return nil, boundService{}, Errors{{
			Code: ErrServiceNotImplemented,
			Message: "service " + quoted(svc.Name) + " has type " + quoted(svc.Type) +
				", which kelson does not render yet",
			Remediation: "only type: postgres renders today; valkey is tracked by " +
				"milestone M9b · Data services, issue #98. Remove the service, or install the " +
				"engine yourself and bind to it as an ordinary workload (ADR-0005)",
		}}
	}
	if svc.Preset == model.PresetBranch {
		return nil, boundService{}, Errors{{
			Code: ErrServiceNotImplemented,
			Message: "service " + quoted(svc.Name) + " requests preset " + quoted(string(model.PresetBranch)) +
				", which kelson does not render yet",
			Remediation: "a branch is bootstrapped from a source cluster with a snapshot mechanism " +
				"chosen from the cluster profile and a TTL, none of which exists yet (issue #99). " +
				"Use small, ha-small or ha-medium, or shared",
		}}
	}

	name := serviceResourceName(resolved.Project, resolved.Environment.Name, svc.Name)
	if len(name) > maxServiceResourceName {
		return nil, boundService{}, Errors{{
			Code: ErrServiceName,
			Message: "the CloudNativePG resource name for service " + quoted(svc.Name) + " would be " +
				quoted(name) + ", " + strconv.Itoa(len(name)) + " characters",
			Remediation: "shorten the project, environment or service name so that " +
				"<project>-<environment>-<service> is at most " + strconv.Itoa(maxServiceResourceName) +
				" characters; CloudNativePG derives longer names from it (<cluster>-superuser) that must " +
				"stay inside the 63-character DNS label limit",
		}}
	}

	if err := supportedPreset(svc, profile, name); err != nil {
		return nil, boundService{}, err
	}

	hash, herr := serviceHash(resolved, svc, name)
	if herr != nil {
		return nil, boundService{}, Errors{{Code: ErrInternal, Message: herr.Error()}}
	}

	if svc.Preset == model.PresetShared {
		m := sharedDatabase(resolved, name, hash)
		return []Manifest{m}, boundService{
			name: svc.Name,
			reason: "the shared cluster's credentials live in namespace " + quoted(SharedClusterNamespace) +
				" beside the cluster itself, and a pod cannot reference a Secret across a namespace boundary",
			remediation: "provisioning the shared cluster's per-project role and projecting its credentials " +
				"into this namespace is issue #93. Until it lands, use preset small, ha-small or ha-medium " +
				"for a service applications bind to — the shared database is still created",
		}, nil
	}

	m := dedicatedCluster(resolved, svc, name, hash)
	return []Manifest{m}, boundService{
		name:   svc.Name,
		secret: appSecretName(name),
		keys:   postgresBindingKeys,
	}, nil
}

// supportedPreset applies the capability verdict from issue #90's judgement.
//
// The check lives here rather than in model.Validate* because validation is
// profile-free by design: a spec is valid on its own terms and the same spec
// targets several clusters (ADR-0001). Every surface that can reach a cluster
// goes through Render, so nothing accepts an unsupportable preset and finds out
// at apply time.
//
// Only OutcomeNo refuses. OutcomeUnknown renders: Unknown means the operator's
// version could not be read or CNPG sat behind a detection gap — nobody looked
// — and refusing on it would turn every hand-written profile into a refusal for
// a cluster that very likely works. The cost is an apply-time failure from the
// API server, which is a specific error from the component that actually knows.
// Unknown is not No (docs/data-services.md, issue #144).
func supportedPreset(svc *model.ResolvedService, profile clusterprofile.ClusterProfile, name string) error {
	verdict := postgres.SupportsPreset(profile, postgres.Preset(svc.Preset))
	if verdict.Outcome != clusterprofile.OutcomeNo {
		return nil
	}
	blocking := make([]string, len(verdict.Blocking))
	for i, c := range verdict.Blocking {
		blocking[i] = string(c)
	}
	return Errors{{
		Code:    ErrPostgresUnsupported,
		Target:  "Cluster/" + name,
		Message: "service " + quoted(svc.Name) + ": " + verdict.Message,
		Remediation: "blocking capabilities: " + strings.Join(blocking, ", ") +
			". Upgrade the CloudNativePG operator this cluster already runs — its resources are " +
			"cluster-scoped and a second install fights the first (ADR-0005) — then re-detect the " +
			"cluster profile. A preset with lighter requirements may also work: shared needs the " +
			"Database CRD, the dedicated presets do not",
	}}
}

// dedicatedCluster renders the CNPG Cluster for the small/ha-* presets.
//
// The application database and its owner come from bootstrap.initdb rather
// than managed.roles, because initdb generates their credentials and publishes
// them as <cluster>-app while a managed role requires a passwordSecret the
// author supplies — and a pure renderer has no random source (issue #20).
// managed.roles enters when a spec needs more than the owner role; nothing in
// the model expresses that yet.
func dedicatedCluster(resolved *model.Resolved, svc *model.ResolvedService, name, hash string) Manifest {
	p := dedicatedPresets[svc.Preset]
	prov := serviceProvenance(resolved, name, hash)

	specKV := []any{"instances", p.instances}
	if p.syncReplicas > 0 {
		// The legacy quorum fields rather than .spec.postgresql.synchronous
		// (CNPG 1.24+): kelson's declared cnpg support floor is 1.23.0, and
		// these are still fully supported in 1.30. See docs/data-services.md.
		specKV = append(specKV,
			"minSyncReplicas", p.syncReplicas,
			"maxSyncReplicas", p.syncReplicas,
		)
	}
	specKV = append(specKV,
		// storageClass is deliberately unset: CNPG falls back to the cluster
		// default, and naming a detected class here would bake a detection
		// snapshot into a manifest that outlives it.
		"storage", mapNode("size", p.storage),
		"resources", mapNode(
			"requests", mapNode("cpu", p.cpu, "memory", p.memory),
			"limits", mapNode("memory", p.memory),
		),
		"bootstrap", mapNode(
			"initdb", mapNode("database", initdbDatabase, "owner", initdbDatabase),
		),
	)
	return baseManifest(cnpgAPIVersion, "Cluster", prov, mapNode(specKV...))
}

// sharedDatabase renders the CNPG Database for the `shared` preset, into the
// shared cluster's namespace because spec.cluster is a same-namespace
// reference. The database, its owner role and the resource all take the
// qualified name: in a cluster shared across projects an unqualified `db`
// collides with the first other project that wants one.
func sharedDatabase(resolved *model.Resolved, name, hash string) Manifest {
	prov := serviceProvenance(resolved, name, hash)
	prov.namespace = SharedClusterNamespace

	spec := mapNode(
		"cluster", mapNode("name", sharedClusterName(resolved.Environment.Name)),
		"name", name,
		"owner", name,
		// CNPG's own default, written explicitly because it is ADR-0007's
		// asymmetric-retention promise: removing a service from a spec must
		// never drop a database.
		"databaseReclaimPolicy", "retain",
	)
	return baseManifest(cnpgAPIVersion, "Database", prov, spec)
}

// serviceProvenance stamps a service resource like every other manifest, with
// one difference: no kelson.dev/application label. A data service is not owned
// by one application — being bindable by several is the point.
func serviceProvenance(resolved *model.Resolved, name, hash string) provenance {
	return provenance{
		project:      resolved.Project,
		environment:  resolved.Environment.Name,
		resourceName: name,
		namespace:    resolved.Environment.Namespace,
		specHash:     hash,
	}
}

// serviceHash is the service's kelson.dev/spec-hash. It covers only what the
// service's own manifest is built from, so an unrelated spec edit — a new
// application, a changed image — leaves a database's annotation untouched.
func serviceHash(resolved *model.Resolved, svc *model.ResolvedService, name string) (string, error) {
	return hashJSON(struct {
		Project     string                `json:"project"`
		Environment string                `json:"environment"`
		Namespace   string                `json:"namespace"`
		Resource    string                `json:"resource"`
		Service     model.ResolvedService `json:"service"`
	}{
		Project:     resolved.Project,
		Environment: resolved.Environment.Name,
		Namespace:   resolved.Environment.Namespace,
		Resource:    name,
		Service:     *svc,
	})
}

// bindingRef turns one {from: {service, key}} binding into a secretKeyRef, or
// explains why it cannot.
func bindingRef(app string, field string, b *model.ServiceBinding, services map[string]boundService) (*yaml.Node, *Error) {
	svc, ok := services[b.Service]
	if !ok {
		return nil, &Error{
			Code:        ErrBindingUnknownService,
			Application: app,
			Message: "environment variable " + quoted(field) + " binds to service " + quoted(b.Service) +
				", which the resolved spec does not declare",
			Remediation: "declare the service under spec.services on the Project, or bind to one of: " +
				strings.Join(serviceNames(services), ", "),
		}
	}
	if svc.secret == "" {
		return nil, &Error{
			Code:        ErrBindingUnavailable,
			Application: app,
			Message: "environment variable " + quoted(field) + " binds to service " + quoted(b.Service) +
				", which renders but cannot be bound to yet: " + svc.reason,
			Remediation: svc.remediation,
		}
	}
	key, ok := svc.keys[b.Key]
	if !ok {
		return nil, &Error{
			Code:        ErrBindingUnknownKey,
			Application: app,
			Message: "environment variable " + quoted(field) + " binds to key " + quoted(b.Key) +
				" of service " + quoted(b.Service) + ", which kelson does not map to a credential",
			Remediation: "use one of: " + strings.Join(sortedKeys(svc.keys), ", "),
		}
	}
	return mapNode("secretKeyRef", mapNode("name", svc.secret, "key", key)), nil
}

func serviceNames(services map[string]boundService) []string {
	if len(services) == 0 {
		return []string{"(the resolved spec declares no services)"}
	}
	return sortedKeys(services)
}

// sortedKeys keeps every generated message deterministic: an error that lists
// what the author could have written instead must not depend on map iteration
// order, or the golden harness would catch it as non-determinism.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func quoted(s string) string { return `"` + s + `"` }
