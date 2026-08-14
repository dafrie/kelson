package renderer

import (
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
)

// The `externalSecrets` backend, rendered as one ExternalSecret per referenced
// Secret and nothing else (issue #80, ADR-0020).
//
// # What changes, and what deliberately does not
//
// The spec text does not change when the backend does. `{secret: payments,
// key: api-key}` is what an author writes under every backend, and it still
// renders into the same `valueFrom.secretKeyRef` on the workload — ADR-0018's
// promise that migrating backends is one field on one Environment. What the
// `externalSecrets` backend adds is the resource that *populates* the Secret
// the reference addresses: an ExternalSecret whose `target.name` is exactly the
// name the reference names, so the two halves meet at a name the author wrote
// and kelson never invents.
//
// One ExternalSecret per Secret name, not per reference. Two variables reading
// two keys of `payments` are one remote secret with two properties, and
// rendering two ExternalSecrets against one target would have them fight over
// the Secret they both own.
//
// # kelson has no per-backend code, and that is the ESO API's doing
//
// Vault, AWS Secrets Manager, GCP Secret Manager and Azure Key Vault are
// backends *of external-secrets*, configured in the SecretStore's
// `spec.provider` — a union with one arm per provider, carrying that provider's
// endpoints and auth. An ExternalSecret names a store and a remote key and is
// provider-agnostic apart from what a `remoteRef` means to the provider on the
// other end. So kelson renders the same document for all four, and adding a
// fifth is somebody installing a newer external-secrets, not a kelson release.
// ADR-0020 records the API reading this rests on.
//
// # No value can pass through here
//
// The same structural property as the rest of internal/renderer, and for the
// same reason: an ExternalSecret carries *addresses* — a store name, a remote
// key, a property — and there is no field on it a value could be placed in.
// kelson never contacts the store, never sees a response, and the Secret is
// written by the external-secrets controller directly into the cluster. The
// backend is, if anything, a stronger version of the guarantee than `cluster`:
// under `cluster` a human types the value into `kelson secret set`, and here no
// human or process on kelson's side ever holds it at all.

const (
	// externalSecretAPIVersion is external-secrets' stable API. It matches the
	// version detection lists SecretStores and ClusterSecretStores at
	// (internal/clusterprofile/detect) — one API version per group across
	// kelson, or the reader and the writer would eventually disagree.
	externalSecretAPIVersion = "external-secrets.io/v1"

	// storeKindNamespaced and storeKindCluster are external-secrets'
	// `secretStoreRef.kind` values. kelson writes one or the other explicitly:
	// the field defaults to SecretStore upstream, and letting the default stand
	// would make a ClusterSecretStore reference silently resolve to a
	// namespaced store nobody created.
	storeKindNamespaced = "SecretStore"
	storeKindCluster    = "ClusterSecretStore"

	// creationPolicyOwner makes the ExternalSecret own the Secret it produces:
	// the controller creates it, keeps it in sync, and garbage-collects it when
	// the ExternalSecret goes. It is external-secrets' own default and is
	// written out anyway, because it is the field that decides whether
	// `kelson uninstall` leaves a credential behind (ADR-0001's deletability
	// requirement) and a reader should not have to know a controller's defaults
	// to answer that.
	creationPolicyOwner = "Owner"
)

// storeRef is the resolved store: which one, and which kind of one.
type storeRef struct {
	name string
	kind string
}

// externalSecretRef is one Secret the environment references, and every key of
// it that something reads. Keys are sorted, so the rendered `data` list is
// deterministic.
type externalSecretRef struct {
	name string
	keys []string
}

// referencedSecrets collects every `{secret, key}` reference in the resolved
// spec, grouped by Secret name.
//
// Two reference shapes are deliberately NOT here. A `{from: {service, key}}`
// binding names a Secret the component's own operator generates — CloudNativePG
// writes it, and an ExternalSecret pointed at the same name would fight the
// operator for it. And a bare Secret *name* with no key — `valuesFrom` on a
// helm component, `secretRef` on previews — cannot produce an ExternalSecret at
// all: an ExternalSecret's `data` is a list of keys, and kelson does not know
// which keys a chart's values file or a forge credential contains. ADR-0020
// records both as gaps rather than guesses.
func referencedSecrets(resolved *model.Resolved) []externalSecretRef {
	byName := map[string]map[string]bool{}
	add := func(ref *model.SecretRef) {
		if ref == nil || ref.Name == "" || ref.Key == "" {
			return
		}
		if byName[ref.Name] == nil {
			byName[ref.Name] = map[string]bool{}
		}
		byName[ref.Name][ref.Key] = true
	}
	for i := range resolved.Components {
		for _, v := range resolved.Components[i].Env {
			add(v.Secret)
		}
	}
	// A data component's `auth:` is a model.SecretRef too (ADR-0015 amendment),
	// and the operator reads that Secret exactly the way a workload does. It
	// gets an ExternalSecret for the same reason: under this backend nothing
	// else puts a value where the reference points.
	for i := range resolved.DataServices {
		add(resolved.DataServices[i].Auth)
	}

	out := make([]externalSecretRef, 0, len(byName))
	for _, name := range sortedKeys(byName) {
		keys := make([]string, 0, len(byName[name]))
		for k := range byName[name] {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out = append(out, externalSecretRef{name: name, keys: keys})
	}
	return out
}

// externalSecretsManifests renders the backend's resources, or refuses.
//
// It returns nothing at all — no error, no manifests — for every backend but
// `externalSecrets`, and for an `externalSecrets` environment that references
// no Secret. The second case is not an error: an environment can select the
// backend and have nothing to sync yet, and refusing that would make adding the
// backend before the first reference impossible.
func externalSecretsManifests(resolved *model.Resolved, profile clusterprofile.ClusterProfile) ([]Manifest, error) {
	if resolved.Environment.Secrets.Backend != model.SecretsExternalSecrets {
		return nil, nil
	}
	if errs := externalSecretsInstalled(resolved, profile); len(errs) > 0 {
		return nil, errs
	}
	refs := referencedSecrets(resolved)
	if len(refs) == 0 {
		return nil, nil
	}
	store, errs := resolveStore(resolved, profile)
	if len(errs) > 0 {
		return nil, errs
	}
	out := make([]Manifest, 0, len(refs))
	for _, ref := range refs {
		m, err := externalSecret(resolved, ref, store)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// externalSecretsInstalled is the capability gate: this backend delegates
// everything to a controller, and applying an ExternalSecret where none runs
// produces a resource nothing ever acts on — or, more often, a kind the API
// server does not even serve.
//
// It follows the flux gate of ADR-0016 in shape and the data-service gates of
// docs/data-services.md in its reading of the profile: only a definite No
// refuses. A profile with a Gap on `externalSecrets` could not see the API
// groups at all, and an Unknown must never be reported as a finding (issue
// #144) — so it renders, and the apply is where the answer comes from.
//
// A profile that says nothing therefore refuses, exactly as the CNPG and Valkey
// gates do for their operators: `nil` with no Gap beside it is the shape
// detection produces for "looked, not there" (internal/clusterprofile: absent,
// present and unknown are three different things), and reading it as Unknown
// would make every real absence render silently.
func externalSecretsInstalled(resolved *model.Resolved, profile clusterprofile.ClusterProfile) Errors {
	if profile.ExternalSecrets != nil {
		return nil
	}
	if _, gapped := profile.GapFor("externalSecrets"); gapped {
		return nil
	}
	return Errors{{
		Code: ErrExternalSecretsNotInstalled,
		Message: "environment " + quoted(resolved.Environment.Name) + " selects secret backend " +
			quoted(string(model.SecretsExternalSecrets)) + ", which renders an ExternalSecret per referenced " +
			"Secret, but the cluster profile reports no external-secrets operator",
		Remediation: "install external-secrets in the target cluster (it serves the external-secrets.io " +
			"API group and reconciles ExternalSecret into a Kubernetes Secret), or set secrets.backend: cluster " +
			"and write the Secret with kelson secret set. kelson delegates to the operator and does not install " +
			"it (ADR-0005); an ExternalSecret applied where nothing reconciles it is a manifest that does nothing",
	}}
}

// resolveStore turns the spec's store name — or its absence — into the
// `secretStoreRef` the ExternalSecrets will carry.
//
// Two questions, one answer. *Which store* comes from the spec when it names
// one and from the profile when exactly one is available; *which kind* is
// always the profile's, because whether `vault-backend` is a namespaced
// SecretStore or a cluster-scoped ClusterSecretStore is a fact about the
// cluster and not something an author should have to restate.
//
// Every case that is not exactly one candidate is refused by name. That is the
// whole design: a wrong store is not a render failure, it is a workload reading
// a credential from somewhere nobody intended, and the failure mode for
// guessing is worse than the failure mode for stopping.
func resolveStore(resolved *model.Resolved, profile clusterprofile.ClusterProfile) (storeRef, Errors) {
	env := resolved.Environment
	var namespaced, cluster []string
	if profile.ExternalSecrets != nil {
		namespaced = profile.ExternalSecrets.SecretStoresIn(env.Namespace)
		cluster = profile.ExternalSecrets.ClusterSecretStores
	}

	if name := env.Secrets.Store; name != "" {
		inNamespace := slices.Contains(namespaced, name)
		inCluster := slices.Contains(cluster, name)
		switch {
		case inNamespace && inCluster:
			return storeRef{}, Errors{{
				Code: ErrExternalSecretsStoreAmbiguous,
				Message: "secrets.store " + quoted(name) + " matches both a SecretStore in namespace " +
					quoted(env.Namespace) + " and a ClusterSecretStore of the same name",
				Remediation: "rename one of the two stores in the cluster, or move the reference to the one " +
					"you mean. kelson will not pick between them: an ExternalSecret names a store kind as well " +
					"as a name, and choosing the wrong one reads a credential from somewhere nobody intended",
			}}
		case inNamespace:
			return storeRef{name: name, kind: storeKindNamespaced}, nil
		case inCluster:
			return storeRef{name: name, kind: storeKindCluster}, nil
		}
		// A named store the profile has never heard of. Unknown-because-unread
		// is handled by the gate above (a Gap on externalSecrets renders), so
		// what is left here is a store the probe looked for and did not find.
		return storeRef{}, Errors{{
			Code: ErrExternalSecretsStoreUnknown,
			Message: "secrets.store " + quoted(name) + " is neither a SecretStore in namespace " +
				quoted(env.Namespace) + " nor a ClusterSecretStore" + availableStores(namespaced, cluster),
			Remediation: "name a store the cluster has, or create the SecretStore. kelson renders a reference " +
				"to a store and never configures one: the backend credentials (Vault address and role, an AWS " +
				"region and IRSA role, a GCP service account) live in the SecretStore's spec.provider, which is " +
				"the cluster administrator's to write (ADR-0005, ADR-0020)",
		}}
	}

	candidates := make([]string, 0, len(namespaced)+len(cluster))
	for _, n := range namespaced {
		candidates = append(candidates, storeKindNamespaced+"/"+n)
	}
	for _, n := range cluster {
		candidates = append(candidates, storeKindCluster+"/"+n)
	}
	switch len(candidates) {
	case 1:
		kind, name, _ := strings.Cut(candidates[0], "/")
		return storeRef{name: name, kind: kind}, nil
	case 0:
		return storeRef{}, Errors{{
			Code: ErrExternalSecretsStoreUnknown,
			Message: "environment " + quoted(env.Name) + " selects secret backend " +
				quoted(string(model.SecretsExternalSecrets)) + " and the cluster profile reports no SecretStore " +
				"in namespace " + quoted(env.Namespace) + " and no ClusterSecretStore",
			Remediation: "create a SecretStore or ClusterSecretStore pointing at your secret manager, then set " +
				"secrets.store to its name. An ExternalSecret with no store to read from stalls with an " +
				"unresolved reference rather than failing the apply, which is a failure far from its cause",
		}}
	default:
		return storeRef{}, Errors{{
			Code: ErrExternalSecretsStoreAmbiguous,
			Message: "environment " + quoted(env.Name) + " sets no secrets.store and the cluster offers " +
				strings.Join(candidates, ", "),
			Remediation: "set secrets.store to the one this environment should read from. kelson defaults the " +
				"store only when there is exactly one to default to: picking between several would bind every " +
				"credential in this environment to whichever store sorted first, which is not a decision a " +
				"renderer gets to make",
		}}
	}
}

// availableStores names what the cluster does offer, so a typo is a one-line
// fix rather than a round trip through kubectl. Empty when there is nothing to
// list — an error that ends in "available: " helps nobody.
func availableStores(namespaced, cluster []string) string {
	var parts []string
	if len(namespaced) > 0 {
		parts = append(parts, "SecretStores "+strings.Join(namespaced, ", "))
	}
	if len(cluster) > 0 {
		parts = append(parts, "ClusterSecretStores "+strings.Join(cluster, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (the cluster has " + strings.Join(parts, "; ") + ")"
}

// externalSecret renders one ExternalSecret: the store it reads, how often, the
// Secret it writes, and one entry per key something in this environment reads.
//
// The resource takes the Secret's own name. There is one ExternalSecret per
// target Secret, they live in the same namespace, and a second name — a
// `<project>-<environment>-` prefix, say — would only make the reader map
// between two spellings of one thing. A collision with an ExternalSecret
// somebody else wrote surfaces as a field-ownership conflict at apply, which is
// the loud outcome; silently producing a second writer for one Secret is not.
func externalSecret(resolved *model.Resolved, ref externalSecretRef, store storeRef) (Manifest, error) {
	hash, err := externalSecretHash(resolved, ref, store)
	if err != nil {
		return Manifest{}, Errors{{Code: ErrInternal, Message: err.Error()}}
	}
	prov := provenance{
		project:      resolved.Project,
		environment:  resolved.Environment.Name,
		resourceName: ref.name,
		namespace:    resolved.Environment.Namespace,
		specHash:     hash,
	}
	spec := mapNode(
		"refreshInterval", resolved.Environment.Secrets.RefreshInterval,
		"secretStoreRef", mapNode(
			"name", store.name,
			"kind", store.kind,
		),
		"target", mapNode(
			"name", ref.name,
			"creationPolicy", creationPolicyOwner,
		),
		"data", externalSecretData(ref),
	)
	return baseManifest(externalSecretAPIVersion, "ExternalSecret", prov, spec), nil
}

// externalSecretData maps each referenced key onto a remote address.
//
// The mapping is `remoteRef.key` = the Secret's name, `remoteRef.property` =
// the key — the spec's own two-part reference, carried through unchanged. It is
// the shape every provider already uses for a structured secret (a Vault KV
// entry with fields, an AWS Secrets Manager JSON secret, a GCP secret holding
// JSON), and it means an author who wrote `{secret: payments, key: api-key}`
// can find the value at `payments`.`api-key` in their secret manager without a
// translation table.
//
// Where the path *prefix* comes from is deliberately not kelson's question: a
// Vault mount and path, an AWS name prefix, a GCP project are all SecretStore
// configuration, and putting a second prefix in the spec would give an
// environment two places to be wrong.
//
// Listing each key explicitly rather than pulling the whole remote secret with
// `dataFrom` is what makes a missing key visible: external-secrets fails the
// sync and says which property it could not find, and the observation plane
// relays it. A `dataFrom` would sync happily and leave the workload to fail at
// pod start against a key that is simply absent.
func externalSecretData(ref externalSecretRef) *yaml.Node {
	items := make([]*yaml.Node, 0, len(ref.keys))
	for _, key := range ref.keys {
		items = append(items, mapNode(
			"secretKey", key,
			"remoteRef", mapNode(
				"key", ref.name,
				"property", key,
			),
		))
	}
	return seqNode(items...)
}

// externalSecretHash is this resource's kelson.dev/spec-hash. Like a data
// service's, it covers only what this resource is built from, so adding a
// reference to one Secret leaves the annotation of every other ExternalSecret —
// and therefore the resource — untouched.
func externalSecretHash(resolved *model.Resolved, ref externalSecretRef, store storeRef) (string, error) {
	return hashJSON(struct {
		Project         string   `json:"project"`
		Environment     string   `json:"environment"`
		Namespace       string   `json:"namespace"`
		Resource        string   `json:"resource"`
		Store           string   `json:"store"`
		StoreKind       string   `json:"storeKind"`
		RefreshInterval string   `json:"refreshInterval"`
		Keys            []string `json:"keys"`
	}{
		Project:         resolved.Project,
		Environment:     resolved.Environment.Name,
		Namespace:       resolved.Environment.Namespace,
		Resource:        ref.name,
		Store:           store.name,
		StoreKind:       store.kind,
		RefreshInterval: resolved.Environment.Secrets.RefreshInterval,
		Keys:            ref.keys,
	})
}
