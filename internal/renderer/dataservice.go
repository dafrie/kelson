package renderer

import (
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/postgres"
	"github.com/dafrie/kelson/internal/clusterprofile/valkey"
	"github.com/dafrie/kelson/internal/model"
)

// Managed data services, rendered as the resources of the operator each kind
// delegates to: CloudNativePG for `kind: postgres` (ADR-0005, ADR-0007, issue
// #89) and valkey-io/valkey-operator for `kind: valkey` (ADR-0015, issue #98).
// docs/data-services.md is the design reference and carries the rationale for
// every number and field name below; this file is the implementation and states
// only what the code needs.

// cnpgAPIVersion is the CloudNativePG API group kelson writes. kelson targets
// the latest operator release and relies on its declarative surface with no
// imperative fallback (owner decision 2026-08-13, internal/clusterprofile/postgres).
const cnpgAPIVersion = "postgresql.cnpg.io/v1"

// valkeyAPIVersion is the Valkey operator's API group. It is v1alpha1 and the
// operator says so: ADR-0015 accepts that deliberately, because the type whose
// failure mode is "the cache refills" is the one where an alpha API is an
// affordable risk.
const valkeyAPIVersion = valkey.Group + "/v1alpha1"

// maxServiceResourceName caps the name kelson derives for a postgres service.
// CloudNativePG derives its own object names from the cluster's — <cluster>-app,
// <cluster>-superuser, <cluster>-rw, <cluster>-1 — and the longest of those
// suffixes is ten characters, which has to fit inside the 63-character DNS
// label limit. Refusing here beats an API server rejecting the manifest with
// an arithmetic complaint about a name the author never wrote.
const maxServiceResourceName = 53

// maxValkeyResourceName is the same rule against a longer derivation. The Valkey
// operator writes a Secret called internal-<cluster>-system-passwords for its
// own system users (read from internal/controller/users.go), which is 26
// characters of prefix and suffix around the name kelson chose — so the cap is
// 63-26. It is tighter than the postgres one for a real reason, not a different
// opinion about names.
const maxValkeyResourceName = 37

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

// valkeyPreset is the rendered shape of one cache preset: the operator's
// topology fields, the container's resources, and the two cache settings that
// decide what happens when it fills up.
type valkeyPreset struct {
	// shards is spec.shards: how many primaries the keyspace is split across.
	shards int
	// replicas is spec.replicas: replicas *per shard*, not in total.
	replicas int
	cpu      string
	memory   string
	// maxMemory is the valkey `maxmemory` directive. It is deliberately below
	// the container's memory limit — see docs/data-services.md for the ratio and
	// why the two numbers cannot be the same one.
	maxMemory string
}

// valkeyPresets is the cache sizing table of docs/data-services.md.
//
// The HA presets are three shards rather than one primary with one replica, and
// that is not a sizing choice. The operator always runs Valkey in cluster mode
// (`cluster-enabled yes`, read from its internal/controller/config.go), and
// cluster-mode failover is decided by a vote among primaries. With a single
// primary there is no quorum left to promote anything once it dies, so a
// one-shard "replicated" cache would have a replica and no failover — a promise
// kelson would be making on the operator's behalf and the operator would not
// keep.
var valkeyPresets = map[model.ServicePreset]valkeyPreset{
	model.PresetSmall:    {shards: 1, replicas: 0, cpu: "250m", memory: "512Mi", maxMemory: "384mb"},
	model.PresetHASmall:  {shards: 3, replicas: 1, cpu: "250m", memory: "512Mi", maxMemory: "384mb"},
	model.PresetHAMedium: {shards: 3, replicas: 1, cpu: "1", memory: "2Gi", maxMemory: "1536mb"},
}

// valkeyEvictionPolicy is the `maxmemory-policy` every cache preset renders.
// A kelson valkey component is a cache (ADR-0015, issue #98): the default
// `noeviction` turns a full cache into write errors in the application, which is
// the wrong failure for something whose whole promise is that losing it is
// cheap. allkeys-lru evicts the least recently used key instead, across the
// whole keyspace rather than only keys someone remembered to set a TTL on.
const valkeyEvictionPolicy = "allkeys-lru"

// valkeyPort is the port the operator's Service exposes (DefaultPort in its
// controller). Written into the `port` and `uri` bindings.
const valkeyPort = "6379"

// boundService is what an application binding resolves against.
//
// A binding key is answered from exactly one of three maps, and which one it is
// says something real about the service. `keys` are credentials: they resolve to
// a secretKeyRef, because a spec never carries a value (ADR-0009). Which Secret
// depends on the kind — CloudNativePG's generated <cluster>-app for postgres,
// the one an author named under `auth:` for a valkey component that declares
// one — and the difference stops here: both are a name and a key, and the
// binding path below cannot tell them apart. `values` are addresses: a hostname
// and a port are not secrets, they are derived from names the renderer already
// knows, and wrapping them in a Secret nobody wrote would be theatre.
// `withheld` is the third answer — a key the kind declares that this service
// *as configured* cannot supply — and it exists so the refusal can say why
// instead of pretending the key was misspelled.
type boundService struct {
	name   string
	secret string
	keys   map[string]string
	values map[string]string
	// withheld maps a well-known key to the reason it cannot be answered, in a
	// form the error's remediation can print directly.
	withheld map[string]string
}

// bindable lists every key this service can actually answer, sorted, for the
// remediation of an unknown-key error. Withheld keys are excluded: suggesting a
// key that refuses would send the reader in a circle.
func (b boundService) bindable() []string {
	out := make([]string, 0, len(b.keys)+len(b.values))
	for k := range b.keys {
		out = append(out, k)
	}
	for k := range b.values {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// scopedResourceName is the name every resource kelson renders on a
// component's behalf into a shared API group carries:
// <project>-<environment>-<component>.
//
// Workloads do not use it — a Deployment is named after its component and
// scoped by its namespace. These are: an operator's CRs and a HelmRelease live
// in a group whose other tenants kelson does not know about, and colliding with
// one of them would be a collision between two projects rather than a
// misconfiguration inside one.
func scopedResourceName(project, environment, component string) string {
	return project + "-" + environment + "-" + component
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
	svc *model.ResolvedDataService,
	profile clusterprofile.ClusterProfile,
) ([]Manifest, boundService, error) {
	switch svc.Kind {
	case model.ComponentPostgres:
		return postgresManifests(resolved, svc, profile)
	case model.ComponentValkey:
		return valkeyManifests(resolved, svc, profile)
	}
	// Unreachable for the kinds model.ComponentKind.IsData() admits today, and
	// deliberately kept: a kind added to the enum before this switch learns it
	// must refuse loudly rather than render nothing at all (issue #141).
	return nil, boundService{}, Errors{{
		Code: ErrServiceNotImplemented,
		Message: "component " + quoted(svc.Name) + " has kind " + quoted(string(svc.Kind)) +
			", which kelson does not render yet",
		Remediation: "kind: postgres and kind: valkey render today. Remove the component, or install the " +
			"engine yourself and bind to it as an ordinary workload (ADR-0005)",
	}}
}

// postgresManifests renders one `kind: postgres` component as a CloudNativePG
// Cluster.
func postgresManifests(
	resolved *model.Resolved,
	svc *model.ResolvedDataService,
	profile clusterprofile.ClusterProfile,
) ([]Manifest, boundService, error) {
	if svc.Preset == model.PresetBranch {
		return nil, boundService{}, Errors{{
			Code: ErrServiceNotImplemented,
			Message: "service " + quoted(svc.Name) + " requests preset " + quoted(string(model.PresetBranch)) +
				", which kelson does not render yet",
			Remediation: "a branch is bootstrapped from a source cluster with a snapshot mechanism " +
				"chosen from the cluster profile and a TTL, none of which exists yet (issue #99). " +
				"Use small, ha-small or ha-medium",
		}}
	}
	// shared was ADR-0007's cost optimization for a shared CNPG cluster — never
	// an ask, and dedicated clusters work today including bindings at an
	// acceptable pod cost (owner decision, 2026-08-13, issue #93). What used to
	// render here was one Database CR into SharedClusterNamespace and a
	// binding refusal naming the same issue; see git history (the commit that
	// added this comment) for the removed sharedDatabase/sharedClusterName code
	// and its golden fixture.
	if svc.Preset == model.PresetShared {
		return nil, boundService{}, Errors{{
			Code: ErrServiceNotImplemented,
			Message: "service " + quoted(svc.Name) + " requests preset " + quoted(string(model.PresetShared)) +
				": the shared preset is deferred — dedicated presets (small, ha-small, ha-medium) work today",
			Remediation: "the shared cluster was ADR-0007's cost optimization, never an ask; dedicated " +
				"clusters per postgres component are simpler, work today including bindings, and the pod " +
				"cost is acceptable at this stage (owner decision, issue #93, which tracks any return of " +
				"shared). Use preset: small",
		}}
	}

	name := scopedResourceName(resolved.Project, resolved.Environment.Name, svc.Name)
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

	m := dedicatedCluster(resolved, svc, name, hash)
	return []Manifest{m}, boundService{
		name:   svc.Name,
		secret: appSecretName(name),
		keys:   postgresBindingKeys,
	}, nil
}

// valkeyManifests renders one `kind: valkey` component as a ValkeyCluster
// (ADR-0015, issue #98).
//
// One resource and no more. The operator owns the Service, the ConfigMap, the
// ValkeyNodes, the PodDisruptionBudget and the ACL file; kelson owns the
// topology request and the two cache settings that decide what a full cache
// does. That division is the whole point of ADR-0005.
func valkeyManifests(
	resolved *model.Resolved,
	svc *model.ResolvedDataService,
	profile clusterprofile.ClusterProfile,
) ([]Manifest, boundService, error) {
	if err := valkeyPresetSupported(svc); err != nil {
		return nil, boundService{}, err
	}

	name := scopedResourceName(resolved.Project, resolved.Environment.Name, svc.Name)
	if len(name) > maxValkeyResourceName {
		return nil, boundService{}, Errors{{
			Code: ErrServiceName,
			Message: "the ValkeyCluster resource name for component " + quoted(svc.Name) + " would be " +
				quoted(name) + ", " + strconv.Itoa(len(name)) + " characters",
			Remediation: "shorten the project, environment or component name so that " +
				"<project>-<environment>-<component> is at most " + strconv.Itoa(maxValkeyResourceName) +
				" characters; the Valkey operator derives longer names from it " +
				"(internal-<cluster>-system-passwords) that must stay inside the 63-character DNS label limit",
		}}
	}

	if err := valkeyCapable(svc, profile, name); err != nil {
		return nil, boundService{}, err
	}

	hash, herr := serviceHash(resolved, svc, name)
	if herr != nil {
		return nil, boundService{}, Errors{{Code: ErrInternal, Message: herr.Error()}}
	}

	m := valkeyCluster(resolved, svc, name, hash)
	bound := boundService{
		name:   svc.Name,
		values: valkeyBindingValues(name, resolved.Environment.Namespace),
	}
	if svc.Auth != nil {
		// The same Secret the ValkeyCluster's ACL user reads its password from,
		// pointed at from the other side. Nothing about it is derived: kelson
		// writes the author's name and key into both places and reads neither.
		bound.secret = svc.Auth.Name
		bound.keys = map[string]string{"password": svc.Auth.Key}
	} else {
		bound.withheld = valkeyWithheldKeys(svc.Name)
	}
	return []Manifest{m}, bound, nil
}

// valkeyPresetSupported refuses the two presets that are not cache topologies.
//
// The preset vocabulary is shared by every data kind (ADR-0007) and that is
// worth keeping — `preset: small` means the same thing whichever engine reads
// it — but two of its members describe things a cache does not have.
func valkeyPresetSupported(svc *model.ResolvedDataService) error {
	switch svc.Preset {
	case model.PresetShared:
		return Errors{{
			Code: ErrServiceNotImplemented,
			Message: "component " + quoted(svc.Name) + " requests preset " + quoted(string(model.PresetShared)) +
				": the shared preset is deferred for every data kind — small, ha-small and ha-medium work today",
			Remediation: "a shared cache instance carved up between projects was issue #98's cost " +
				"optimization and shares the fate of the shared Postgres cluster (owner decision, issue #93): " +
				"one instance per component is simpler, works today, and a cache pod is cheap. Use preset: small",
		}}
	case model.PresetBranch:
		return Errors{{
			Code: ErrServiceNotImplemented,
			Message: "component " + quoted(svc.Name) + " requests preset " + quoted(string(model.PresetBranch)) +
				", which is not a valkey topology",
			Remediation: "branching bootstraps a copy of a database from a snapshot of its durable state " +
				"(ADR-0007, issue #99), and a kelson cache has no durable state to copy — it renders with " +
				"persistence off, and an empty cache is what starting one already gives you. " +
				"Use small, ha-small or ha-medium",
		}}
	}
	if _, ok := valkeyPresets[svc.Preset]; !ok {
		return Errors{{
			Code: ErrServiceNotImplemented,
			Message: "component " + quoted(svc.Name) + " requests preset " + quoted(string(svc.Preset)) +
				", which kelson does not render for kind: valkey",
			Remediation: "use small, ha-small or ha-medium (docs/data-services.md)",
		}}
	}
	return nil
}

// valkeyCapable applies the capability verdict of internal/clusterprofile/valkey
// with exactly the tri-state reading supportedPreset uses for CloudNativePG:
// only OutcomeNo refuses, because Unknown means nobody could look and refusing
// on it would turn every hand-written profile into a refusal (issue #144).
func valkeyCapable(svc *model.ResolvedDataService, profile clusterprofile.ClusterProfile, name string) error {
	verdict := valkey.SupportsPreset(profile, valkey.Preset(svc.Preset))
	if verdict.Outcome != clusterprofile.OutcomeNo {
		return nil
	}
	blocking := make([]string, len(verdict.Blocking))
	for i, c := range verdict.Blocking {
		blocking[i] = string(c)
	}
	return Errors{{
		Code:    ErrValkeyUnsupported,
		Target:  "ValkeyCluster/" + name,
		Message: "component " + quoted(svc.Name) + ": " + verdict.Message,
		Remediation: "blocking capabilities: " + strings.Join(blocking, ", ") +
			". Install or upgrade valkey-io/valkey-operator — kelson delegates every managed valkey " +
			"component to it and never installs it as a side effect (ADR-0005, ADR-0015) — then re-detect " +
			"the cluster profile. All three cache presets need the same capabilities, so no lighter one exists",
	}}
}

// valkeyCluster renders the ValkeyCluster for the small/ha-* presets.
//
// Two omissions carry as much intent as the fields that are here.
//
// `persistence` is absent, which is what makes the cache a cache: with no
// PersistenceSpec the operator gives each node an emptyDir instead of a PVC, so
// a restart starts empty and refills. Issue #98's framing is that this is the
// point, not a limitation, and docs/data-services.md states what using one as a
// durable store would take instead of leaving it to be assumed either way.
//
// `users` is absent unless the component declares `auth:`. Without it the
// operator leaves Valkey's own `default` user in place and the cache accepts
// connections from anything that can reach its Service — the operator's own
// default, and the documented negative of ADR-0015. With it, kelson renders the
// `default` user against the author's Secret; see valkeyUsers.
func valkeyCluster(resolved *model.Resolved, svc *model.ResolvedDataService, name, hash string) Manifest {
	p := valkeyPresets[svc.Preset]
	prov := serviceProvenance(resolved, name, hash)

	specKV := []any{
		"shards", p.shards,
		// Written even when it is zero, which is also the operator's default:
		// replicas is the field that separates one preset from another, and a
		// reader should not have to know a default to know what they deployed.
		"replicas", p.replicas,
		"resources", mapNode(
			"requests", mapNode("cpu", p.cpu, "memory", p.memory),
			"limits", mapNode("memory", p.memory),
		),
	}
	if svc.Auth != nil {
		// Between `resources` and `config`, which is where `users` sits in the
		// operator's own ValkeyClusterSpec.
		specKV = append(specKV, "users", valkeyUsers(svc.Auth))
	}
	specKV = append(specKV,
		// spec.config is written verbatim into valkey.conf ahead of the
		// operator's own directives, and both of these keys are on the
		// operator's live-settable allow-list — changing a preset re-tunes a
		// running cache with CONFIG SET instead of rolling the pods.
		"config", mapNode(
			"maxmemory", p.maxMemory,
			"maxmemory-policy", valkeyEvictionPolicy,
		),
	)
	return baseManifest(valkeyAPIVersion, "ValkeyCluster", prov, mapNode(specKV...))
}

// valkeyAuthUser is the ACL user kelson renders for an authenticated cache:
// Valkey's own `default`, given a password and the permissions it already had.
//
// Naming a *new* user would leave `default` exactly as it is — an ACL file that
// does not mention `default` does not change it, so the built-in
// `on nopass ~* &* +@all` survives and the cache stays open to anything that
// skips the AUTH. Redefining `default` is therefore not a shortcut around
// inventing a username: it is the only edit that closes the hole this field
// exists to close. It also keeps the binding vocabulary honest —
// model.ServiceKeys[valkey] has no `username` key, and `AUTH <password>` against
// `default` is what every Redis-compatible client can do without one.
const valkeyAuthUser = "default"

// valkeyUsers renders spec.users, verified against the operator's API at
// https://github.com/valkey-io/valkey-operator/blob/main/api/v1alpha1/valkeyacls_types.go
// (read 2026-08-14) and the field reference at
// https://github.com/valkey-io/valkey-operator/blob/main/docs/valkeycluster.md#users:
//
//	type UserAclSpec struct {
//	    Name           string             `json:"name"`             // may not start with "_"
//	    Enabled        bool               `json:"enabled,omitempty"`  // +kubebuilder:default=true
//	    PasswordSecret PasswordSecretSpec `json:"passwordSecret,omitempty"`
//	    NoPassword     bool               `json:"nopass,omitempty"`
//	    ResetPass      bool               `json:"resetpass,omitempty"`
//	    Commands       CommandsAclSpec    `json:"commands,omitempty"` // allow/deny []string
//	    Keys           KeysAclSpec        `json:"keys,omitempty"`     // readWrite/readOnly/writeOnly []string
//	    Channels       ChannelsAclSpec    `json:"channels,omitempty"` // patterns []string
//	    RawAcl         string             `json:"permissions,omitempty"`
//	}
//	type PasswordSecretSpec struct {
//	    Name string   `json:"name,omitempty"`
//	    Keys []string `json:"keys,omitempty"`
//	}
//
// Three details of the operator's behaviour decide what is written here, all
// read from its internal/controller/users.go at the same commit:
//
//   - `passwordSecret.name` defaults to <cluster>-users and `keys` defaults to
//     the *username*. kelson writes both explicitly, because both defaults are
//     names the author did not choose and one of them would silently be the
//     literal string "default".
//   - the Secret's value may be plaintext — the operator SHA-256-hashes
//     whatever it reads, unless the value is already a `#`-prefixed 64-hex
//     digest. So `kelson secret set` needs no special format, and kelson does
//     not have to hash anything (it could not: the renderer is pure and the
//     value is not in the spec).
//   - the ACL is applied with `ACL LOAD` against a running node, so adding
//     `auth:` to an existing cache does not roll its pods.
//
// The permissions are the built-in `default` user's own — all keys, all
// channels, all commands — restated because an ACL-file line replaces a user's
// rules rather than adding to them. The change this field makes is
// authentication and nothing else: the same cache, with a password.
func valkeyUsers(auth *model.SecretRef) *yaml.Node {
	return seqNode(mapNode(
		"name", valkeyAuthUser,
		"passwordSecret", mapNode(
			"name", auth.Name,
			"keys", seqNode(strNode(auth.Key)),
		),
		"keys", mapNode("readWrite", seqNode(strNode("*"))),
		"channels", mapNode("patterns", seqNode(strNode("*"))),
		"commands", mapNode("allow", seqNode(strNode("@all"))),
	))
}

// valkeyServiceName is the Service the operator creates for a cluster:
// valkey-<cluster>, headless, on 6379 (its upsertService). kelson renders no
// Service of its own — that would be a second object competing with the
// operator's for the same name.
func valkeyServiceName(cluster string) string { return "valkey-" + cluster }

// valkeyBindingValues are the connection details a workload binds to.
//
// They are plain env values rather than secretKeyRefs because they are plain
// facts: a Service name kelson derived and a port the operator fixes. ADR-0009
// forbids a *secret* in a spec, and inventing a Secret to hold a hostname would
// obey the letter of that while making the manifest harder to read.
//
// The URI is `redis://` on purpose. Valkey is wire- and URL-compatible with
// Redis and that is the scheme the client libraries an application already has
// will parse; emitting `valkey://` would name the product correctly and be
// rejected by most of them, which is the wrong trade for a value whose only job
// is to be handed to a client constructor.
func valkeyBindingValues(cluster, namespace string) map[string]string {
	host := valkeyServiceName(cluster) + "." + namespace + ".svc"
	return map[string]string{
		"host": host,
		"port": valkeyPort,
		"uri":  "redis://" + host + ":" + valkeyPort,
	}
}

// valkeyWithheldKeys is the `password` key of model.ServiceKeys[valkey] and the
// reason it has no answer *for a component that declares no `auth:`*. A
// component that declares one answers the key from `keys` instead, through the
// ordinary secretKeyRef path, and this map is never built for it.
//
// It stays a map rather than a special case in bindingRef because the shape is
// what lets a service withhold a key with a reason at all, and because the
// remediation now has somewhere to send the reader: the missing piece is a
// field they can add, not an upstream change they can only wait for.
func valkeyWithheldKeys(component string) map[string]string {
	return map[string]string{
		"password": "this cache renders no ACL user, so Valkey's own `default` user stays in place and anything " +
			"that can reach the Service in this namespace can read and write the cache without a password. " +
			"kelson cannot invent one — the operator reads user passwords from a Secret it never creates, and a " +
			"pure renderer has no random source (issue #20) — but it can reference one you write. Two steps: " +
			"`kelson secret set " + component + "-auth --project <project> --env <environment> password=<value>` " +
			"(or --from-stdin password, to keep it out of your shell history), then add " +
			"`auth: {secret: " + component + "-auth, key: password}` to the " + quoted(component) + " component. " +
			"kelson then renders the ACL user against that Secret and this binding becomes a secretKeyRef " +
			"against the same one (ADR-0015 amendment 2026-08-14, ADR-0018, docs/data-services.md)",
	}
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
func supportedPreset(svc *model.ResolvedDataService, profile clusterprofile.ClusterProfile, name string) error {
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
			"cluster profile. small, ha-small and ha-medium all need the same capabilities, so no " +
			"lighter dedicated preset exists",
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
func dedicatedCluster(resolved *model.Resolved, svc *model.ResolvedDataService, name, hash string) Manifest {
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
func serviceHash(resolved *model.Resolved, svc *model.ResolvedDataService, name string) (string, error) {
	return hashJSON(struct {
		Project     string                    `json:"project"`
		Environment string                    `json:"environment"`
		Namespace   string                    `json:"namespace"`
		Resource    string                    `json:"resource"`
		Service     model.ResolvedDataService `json:"service"`
	}{
		Project:     resolved.Project,
		Environment: resolved.Environment.Name,
		Namespace:   resolved.Environment.Namespace,
		Resource:    name,
		Service:     *svc,
	})
}

// bindingRef resolves one {from: {service, key}} binding, or explains why it
// cannot.
//
// It returns the container-env field to write — "valueFrom" for a credential
// that resolves to a secretKeyRef, "value" for a plain connection detail — and
// the node to write there. Two return values rather than one because the two
// answers sit at different keys of the same env entry, and hiding that behind a
// wrapper node would mean rendering `valueFrom` around something that is not a
// reference.
func bindingRef(app string, field string, b *model.ServiceBinding, services map[string]boundService) (string, *yaml.Node, *Error) {
	svc, ok := services[b.Service]
	if !ok {
		return "", nil, &Error{
			Code:        ErrBindingUnknownService,
			Application: app,
			Message: "environment variable " + quoted(field) + " binds to service " + quoted(b.Service) +
				", which the resolved spec does not declare",
			Remediation: "declare it under spec.components on the Project with kind: postgres or kind: valkey, " +
				"or bind to one of: " + strings.Join(serviceNames(services), ", "),
		}
	}
	if key, ok := svc.keys[b.Key]; ok {
		return "valueFrom", secretKeyRefNode(svc.secret, key), nil
	}
	if value, ok := svc.values[b.Key]; ok {
		return "value", strNode(value), nil
	}
	if why, ok := svc.withheld[b.Key]; ok {
		return "", nil, &Error{
			Code:        ErrBindingUnavailableKey,
			Application: app,
			Message: "environment variable " + quoted(field) + " binds to key " + quoted(b.Key) +
				" of service " + quoted(b.Service) + ", which kelson cannot supply for this service type",
			Remediation: why,
		}
	}
	return "", nil, &Error{
		Code:        ErrBindingUnknownKey,
		Application: app,
		Message: "environment variable " + quoted(field) + " binds to key " + quoted(b.Key) +
			" of service " + quoted(b.Service) + ", which kelson does not map to a connection detail",
		Remediation: "use one of: " + strings.Join(svc.bindable(), ", "),
	}
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
