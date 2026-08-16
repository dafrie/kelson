package secret

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/redact"
)

// FieldManager is the server-side-apply manager name every write here uses.
//
// One name for every surface — CLI, API, agent — is deliberate: the field
// manager is what makes a repeated set an idempotent update of the same fields
// rather than a conflict, and a per-surface manager would make "set this from
// the CLI, then from the UI" a conflict between two halves of kelson.
const FieldManager = "kelson"

// LabelManaged marks a Secret kelson wrote. Its presence is the whole of
// kelson's claim over a Secret: listing selects on it, and deleting or
// overwriting an unlabelled Secret is refused (see the package doc).
const LabelManaged = "kelson.dev/managed-secret"

// The provenance labels every kelson-written resource carries, matching what
// the renderer stamps (internal/renderer/renderer.go) so a Secret written here
// is selectable by the same queries as the workloads that reference it.
const (
	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelProject     = "kelson.dev/project"
	labelEnvironment = "kelson.dev/environment"
	managedByKelson  = "kelson"
)

// ManagedSelector is the label query that finds the Secrets kelson manages. It
// is exported so a caller can run the same query kelson runs — with kubectl,
// say — rather than reconstructing it from the label constant and getting the
// value wrong.
func ManagedSelector() string { return LabelManaged + "=true" }

// Target addresses the namespace a secret lives in.
//
// Project and Environment are always required, even when Namespace is set:
// they are what the provenance labels record, and a Secret written into an
// explicit namespace with no idea which environment it belongs to would be
// invisible to every query that finds the others.
type Target struct {
	Project     string
	Environment string
	// Namespace overrides the derived `<project>-<environment>`, for an
	// Environment whose spec sets `spec.namespace`. Empty derives.
	Namespace string
}

// Resolve returns the target namespace, or a structured refusal for a target
// that cannot address one. The derivation is the model's own
// (model.DefaultNamespace), so a Secret lands where the render will look for
// it rather than in a namespace only this package believes in.
func (t Target) Resolve() (string, error) {
	if t.Project == "" || t.Environment == "" {
		return "", newError(ErrInvalidTarget, "",
			fmt.Sprintf("a secret is addressed by project and environment, and this target has project=%q environment=%q",
				t.Project, t.Environment),
			"pass both --project and --env; the namespace is derived from them as <project>-<environment> "+
				"unless the Environment's spec.namespace says otherwise, in which case pass --namespace too")
	}
	if t.Namespace != "" {
		return t.Namespace, nil
	}
	return model.DefaultNamespace(t.Project, t.Environment), nil
}

// SetRequest is one `kelson secret set`: which Secret, in which environment,
// and the keys to write into it.
type SetRequest struct {
	Target
	// Name is the Secret's name — the same string a
	// `{secret: <name>, key: <key>}` reference would carry.
	Name string
	// Values are the keys to write. Existing keys of the same Secret that are
	// not named here are preserved (see the package doc: Set merges).
	Values map[string]string
	// DryRun asks the API server to validate and discard the write
	// (metav1.DryRunAll). It is a real server-side dry run: admission runs, the
	// merge is computed, and nothing is persisted.
	DryRun bool
}

// UnsetRequest is one `kelson secret unset`: which Secret, and which of its
// keys to remove.
type UnsetRequest struct {
	Target
	Name string
	// Keys are the keys to remove. Every one of them must be in the Secret —
	// see [Store.Unset] — and keys not named here are left alone.
	Keys []string
	// DryRun asks the API server to validate and discard the write, exactly as
	// [SetRequest.DryRun] does.
	DryRun bool
}

// DeleteRequest is one `kelson secret delete`.
type DeleteRequest struct {
	Target
	Name   string
	DryRun bool
}

// Secret is a masked read-back: what exists, not what is in it.
//
// There is deliberately no value field and no method that could produce one.
// ADR-0009's "reads back masked for display" is enforced by this type having
// nothing to mask — a formatter cannot leak what it was never handed.
type Secret struct {
	Name      string
	Namespace string
	// Keys are the Secret's data keys, sorted. A key name is a reference, and
	// referencing a secret is the whole point of the model, so keys are shown
	// everywhere values are not.
	Keys []string
	// CreatedAt is the object's creation timestamp, zero when the API server
	// reported none.
	CreatedAt time.Time
}

// Age is how old the Secret is at now, rounded to the second. It returns zero
// for an object with no creation timestamp rather than an age measured from the
// zero time, which would print as several thousand years.
func (s Secret) Age(now time.Time) time.Duration {
	if s.CreatedAt.IsZero() || now.Before(s.CreatedAt) {
		return 0
	}
	return now.Sub(s.CreatedAt).Round(time.Second)
}

// Validate checks everything about a Set that can be decided without a cluster
// and returns the target namespace.
//
// It is exported because a caller may need exactly that and nothing else: the
// API's DRY_RUN_RENDER means "validate and touch nothing" (#69), and a render
// dry run that opened a connection to answer would not be one. [Store.Set]
// calls it too, so the two cannot come to different conclusions about the same
// request.
func (r SetRequest) Validate() (string, error) {
	namespace, err := r.Resolve()
	if err != nil {
		return "", err
	}
	if err := checkName(namespace, r.Name); err != nil {
		return "", err
	}
	if len(r.Values) == 0 {
		return "", newError(ErrNoValues, resourceOf(namespace, r.Name),
			"a set with no keys would write nothing",
			"pass at least one key=value, --from-file <key>=<path> or --from-stdin <key>")
	}
	for _, k := range sortedKeys(r.Values) {
		if !model.ValidSecretKey(k) {
			return "", newError(ErrInvalidKey, resourceOf(namespace, r.Name),
				fmt.Sprintf("%q is not a key a Kubernetes Secret can hold", k),
				"use "+model.SecretKeyAlphabet+"; it is the same alphabet a "+
					"{secret: <name>, key: <key>} reference is held to, so a key kelson refuses here is "+
					"one no spec could reference")
		}
	}
	return namespace, nil
}

// Keys is the sorted key set a Set would write. It is what a caller reports as
// "written", and it exists so no caller has to sort a map and get the order
// non-deterministic.
func (r SetRequest) Keys() []string { return sortedKeys(r.Values) }

// Validate is [SetRequest.Validate] for an unset: everything decidable without
// a cluster, returning the target namespace.
//
// It deliberately does not check the key alphabet the way a set does. A key
// outside it cannot be in a Secret, so it is already covered by the
// present-or-refused rule — and [ErrKeyNotFound] is the better answer, because
// it lists the keys the Secret actually holds while `secret/invalid-key` would
// only restate the alphabet.
func (r UnsetRequest) Validate() (string, error) {
	namespace, err := r.Resolve()
	if err != nil {
		return "", err
	}
	if err := checkName(namespace, r.Name); err != nil {
		return "", err
	}
	if len(r.RemovedKeys()) == 0 {
		return "", newError(ErrNoKeys, resourceOf(namespace, r.Name),
			"an unset with no keys would remove nothing",
			"name at least one key, e.g. `kelson secret unset "+r.Name+" --project "+r.Project+
				" --env "+r.Environment+" <key>`; `kelson secret list` shows what the Secret holds")
	}
	return namespace, nil
}

// RemovedKeys is the sorted, de-duplicated key set an Unset removes. A key
// named twice is one removal, not an error: unlike a set — where a key supplied
// twice means two values and one of them is being silently dropped — a key
// named twice here means the same thing both times.
func (r UnsetRequest) RemovedKeys() []string {
	seen := make(map[string]bool, len(r.Keys))
	out := make([]string, 0, len(r.Keys))
	for _, k := range r.Keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Validate is [SetRequest.Validate] for a delete: everything decidable without
// a cluster, returning the target namespace.
func (r DeleteRequest) Validate() (string, error) {
	namespace, err := r.Resolve()
	if err != nil {
		return "", err
	}
	if err := checkName(namespace, r.Name); err != nil {
		return "", err
	}
	return namespace, nil
}

// Store writes and reads Secrets through a Kubernetes clientset.
//
// The clientset is injected rather than built here for the same reason every
// delivery adapter's is: the tests drive a fake and no test needs a cluster.
// internal/delivery/kube.Connect is the one place a kubeconfig becomes one.
type Store struct {
	clientset kubernetes.Interface
}

// New returns a Store over the given clientset.
func New(clientset kubernetes.Interface) *Store {
	return &Store{clientset: clientset}
}

// Set writes the named keys into the Secret, creating it if it does not exist,
// and returns the masked read-back.
//
// The order of the first two statements is the point of this method. Every
// value is registered with internal/redact before anything else happens —
// before validation, before the namespace is resolved, before any client call —
// so there is no window in which a value could reach an error message. Nothing
// after this line can print one, whichever plane writes the message.
func (s *Store) Set(ctx context.Context, req SetRequest) (Secret, error) {
	for _, v := range req.Values {
		redact.Register(v)
	}

	namespace, err := req.Validate()
	if err != nil {
		return Secret{}, err
	}

	data := make(map[string][]byte, len(req.Values))
	for k, v := range req.Values {
		data[k] = []byte(v)
	}
	if err := s.carryForward(ctx, namespace, req.Name, data); err != nil {
		return Secret{}, err
	}

	applied, err := s.clientset.CoreV1().Secrets(namespace).Apply(ctx,
		corev1ac.Secret(req.Name, namespace).
			WithLabels(labelsFor(req.Target)).
			WithType(corev1.SecretTypeOpaque).
			WithData(data),
		metav1.ApplyOptions{FieldManager: FieldManager, Force: true, DryRun: dryRunOptions(req.DryRun)})
	if err != nil {
		return Secret{}, applyError(namespace, req.Name, err)
	}
	return summarize(applied), nil
}

// carryForward adds the live Secret's other keys to data, so a set of one key
// does not prune the rest (see the package doc).
//
// It is also where the managed-secret rule is enforced on the write path: a
// Secret that exists without kelson's label is refused rather than adopted.
// Adopting it would relabel somebody else's object — a Secret an operator
// generated, a TLS pair cert-manager owns — and make every later listing and
// delete claim it as kelson's.
//
// The values it carries are registered with redact as they are learned, exactly
// like the ones the caller passed: they are values kelson now holds, and where
// they came from does not change what they are.
func (s *Store) carryForward(ctx context.Context, namespace, name string, data map[string][]byte) error {
	live, err := s.clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return newError(ErrReadFailed, resourceOf(namespace, name),
			"the existing Secret could not be read, so kelson cannot tell whether it manages it",
			"check that the credentials in use may get Secrets in this namespace; "+
				"the write is refused rather than attempted, because an apply that pruned keys kelson could not see "+
				"would lose credentials").withCause(err)
	}
	if !IsManaged(live) {
		return notManaged(namespace, name)
	}
	for k, v := range live.Data {
		if _, replacing := data[k]; replacing {
			continue
		}
		redact.Register(string(v))
		data[k] = v
	}
	// StringData is empty on anything a real API server returns — it folds into
	// Data on write — but a test double may not fold it, and a key is a key.
	for k, v := range live.StringData {
		if _, replacing := data[k]; replacing {
			continue
		}
		redact.Register(v)
		data[k] = []byte(v)
	}
	return nil
}

// Unset removes the named keys from a Secret kelson manages and returns the
// masked read-back of what is left.
//
// It is the complement of the merge (issue #269): [Store.Set] never prunes, so
// without this there is no way to express "this key should not exist" short of
// deleting the Secret and writing it again. Three rules make it safe to give a
// removal to the same surfaces that have the write.
//
// # A key that is not there is a refusal, not a no-op
//
// Every named key must be in the Secret or the whole call fails with
// [ErrKeyNotFound] and the missing keys named. Nothing here reports values, so
// a caller has no other way to notice that `kelson secret unset db pasword`
// removed nothing while they believed a credential was gone — and an unset that
// silently succeeded on a typo would make that belief permanent. The refusal is
// all-or-nothing for the same reason: a partial removal would leave the caller
// having to work out which half happened.
//
// # The last key leaves an empty Secret; it does not delete it
//
// This is the decision issue #269 left to the implementation. An unset that
// deleted the object when its last key went would make `kelson secret unset db
// url` a delete — without the confirmation `kelson secret delete` asks for,
// without the UID precondition it applies, and reachable from an agent through
// a tool the surface deliberately does not give it (internal/mcp/secret.go).
// Keeping the object bounds every removal to what the caller actually named:
// keys. What is left is an empty managed Secret, which is honest state — it
// lists with no keys, `set` refills it by merge, and `delete` removes it — and
// it is not a worse failure for a workload than a deleted one, because a
// `secretKeyRef` to a key that is not there fails a pod start either way.
//
// # It writes with Update, not Apply
//
// A server-side apply prunes only the fields kelson's own field manager owns,
// so a key some other writer had set would survive an apply that omitted it and
// this method would report a removal it did not make. An Update writes the
// object whole, and the ResourceVersion the read carried makes it a
// compare-and-swap: a Secret that changed between the read and the write is a
// conflict rather than a removal computed against a stale copy — the same
// window [Store.Delete]'s UID precondition closes.
func (s *Store) Unset(ctx context.Context, req UnsetRequest) (Secret, error) {
	namespace, err := req.Validate()
	if err != nil {
		return Secret{}, err
	}

	live, err := s.clientset.CoreV1().Secrets(namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Secret{}, secretMissing(namespace, req.Name)
		}
		return Secret{}, newError(ErrReadFailed, resourceOf(namespace, req.Name),
			"the Secret could not be read, so kelson cannot tell whether it manages it",
			"check that the credentials in use may get Secrets in this namespace; the removal is refused "+
				"rather than attempted, because a write computed against a Secret kelson could not read "+
				"would drop keys it never saw").withCause(err)
	}
	// Every value in the object is registered before anything else touches it,
	// for the reason [Store.Set]'s first statement gives: this method holds the
	// whole Secret in order to write the remainder back, and a value kelson
	// holds is a value kelson must not be able to print (issue #117).
	for _, v := range live.Data {
		redact.Register(string(v))
	}
	for _, v := range live.StringData {
		redact.Register(v)
	}
	if !IsManaged(live) {
		return Secret{}, notManaged(namespace, req.Name)
	}

	removing := req.RemovedKeys()
	held := summarize(live).Keys
	if missing := absent(removing, held); len(missing) > 0 {
		return Secret{}, newError(ErrKeyNotFound, resourceOf(namespace, req.Name),
			fmt.Sprintf("Secret %q in namespace %q does not hold %s", req.Name, namespace, quoteList(missing)),
			"nothing was removed: an unset is all-or-nothing, so a mistyped key cannot take a key you did "+
				"mean to keep. This Secret holds "+quoteList(held)+
				" — `kelson secret list` shows the same thing for every Secret in the environment")
	}

	next := live.DeepCopy()
	for _, k := range removing {
		delete(next.Data, k)
		delete(next.StringData, k)
	}
	updated, err := s.clientset.CoreV1().Secrets(namespace).Update(ctx, next,
		metav1.UpdateOptions{FieldManager: FieldManager, DryRun: dryRunOptions(req.DryRun)})
	if err != nil {
		if apierrors.IsConflict(err) {
			return Secret{}, newError(ErrWriteFailed, resourceOf(namespace, req.Name),
				"the Secret changed while the removal was being computed, so the write was rejected",
				"retry: the removal is recomputed against the Secret as it is now, which is what stops it "+
					"restoring a key somebody else removed in between").withCause(err)
		}
		if apierrors.IsNotFound(err) {
			return Secret{}, secretMissing(namespace, req.Name)
		}
		return Secret{}, newError(ErrWriteFailed, resourceOf(namespace, req.Name),
			"the API server refused the write",
			"retry, and check that the credentials in use may update Secrets in this namespace").withCause(err)
	}
	return summarize(updated), nil
}

// absent is the keys of want that are not in have. Both are sorted, so the
// answer is too and an error message reads the same on every run.
func absent(want, have []string) []string {
	set := make(map[string]bool, len(have))
	for _, k := range have {
		set[k] = true
	}
	var out []string
	for _, k := range want {
		if !set[k] {
			out = append(out, k)
		}
	}
	return out
}

// quoteList renders a key list for an error message. Keys are quoted because a
// Secret key may hold dots and dashes and an unquoted list of them is hard to
// read; "none" is spelled out because an empty list in a sentence reads as a
// missing word rather than as a fact.
func quoteList(keys []string) string {
	if len(keys) == 0 {
		return "no keys"
	}
	quoted := make([]string, 0, len(keys))
	for _, k := range keys {
		quoted = append(quoted, fmt.Sprintf("%q", k))
	}
	return strings.Join(quoted, ", ")
}

// List returns every Secret kelson manages in the target namespace, masked,
// sorted by name.
//
// The listing is a label query rather than a full list filtered client-side.
// That is the difference between kelson reading its own Secrets and kelson
// reading every Secret in the namespace: the second needs no extra RBAC than
// the first, but it does mean a service-account token or a TLS key transits
// kelson's process for no reason, and the narrower query is the one to ask for.
func (s *Store) List(ctx context.Context, t Target) ([]Secret, error) {
	namespace, err := t.Resolve()
	if err != nil {
		return nil, err
	}
	list, err := s.clientset.CoreV1().Secrets(namespace).List(ctx,
		metav1.ListOptions{LabelSelector: ManagedSelector()})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, namespaceMissing(namespace, "")
		}
		return nil, newError(ErrReadFailed, resourceOf(namespace, ""),
			fmt.Sprintf("the Secrets in namespace %q could not be listed", namespace),
			"check that the credentials in use may list Secrets in this namespace").withCause(err)
	}
	out := make([]Secret, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, summarize(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete removes a Secret kelson manages.
//
// It reads before it deletes, and refuses an unlabelled Secret. The read is not
// an optimisation: `kelson secret delete` takes a name, a namespace holds
// Secrets kelson never wrote, and a delete-by-name with no ownership check is
// one typo away from removing a service account's token. The UID precondition
// closes the window between the read and the delete, so a Secret replaced in
// between is not deleted on the strength of a check against the object it
// replaced.
func (s *Store) Delete(ctx context.Context, req DeleteRequest) error {
	namespace, err := req.Validate()
	if err != nil {
		return err
	}

	live, err := s.clientset.CoreV1().Secrets(namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return secretMissing(namespace, req.Name)
		}
		return newError(ErrReadFailed, resourceOf(namespace, req.Name),
			"the Secret could not be read, so kelson cannot tell whether it manages it",
			"check that the credentials in use may get Secrets in this namespace").withCause(err)
	}
	if !IsManaged(live) {
		return notManaged(namespace, req.Name)
	}

	uid := live.GetUID()
	err = s.clientset.CoreV1().Secrets(namespace).Delete(ctx, req.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
		DryRun:        dryRunOptions(req.DryRun),
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return secretMissing(namespace, req.Name)
		}
		return newError(ErrWriteFailed, resourceOf(namespace, req.Name),
			"the API server refused the delete",
			"retry, and check that the credentials in use may delete Secrets in this namespace").withCause(err)
	}
	return nil
}

// IsManaged reports whether a Secret carries kelson's managed-secret label. It
// is exported because the rule "kelson touches only what kelson manages" has to
// be checkable by anything that displays a Secret, not only by this package.
func IsManaged(s *corev1.Secret) bool {
	return s != nil && s.GetLabels()[LabelManaged] == "true"
}

// labelsFor is the provenance a written Secret carries.
func labelsFor(t Target) map[string]string {
	return map[string]string{
		labelManagedBy:   managedByKelson,
		labelProject:     t.Project,
		labelEnvironment: t.Environment,
		LabelManaged:     "true",
	}
}

// summarize masks a live Secret down to what may be displayed. It is the only
// function that reads a corev1.Secret's data, and it reads only the map's keys.
func summarize(s *corev1.Secret) Secret {
	keys := make([]string, 0, len(s.Data)+len(s.StringData))
	seen := make(map[string]bool, len(s.Data)+len(s.StringData))
	for k := range s.Data {
		keys = append(keys, k)
		seen[k] = true
	}
	for k := range s.StringData {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return Secret{
		Name:      s.GetName(),
		Namespace: s.GetNamespace(),
		Keys:      keys,
		CreatedAt: s.GetCreationTimestamp().Time,
	}
}

// checkName holds a Secret's name to what a `{secret: <name>, key: <key>}`
// reference can name, which is what the API server holds it to as well.
func checkName(namespace, name string) error {
	if name == "" {
		return newError(ErrInvalidName, resourceOf(namespace, ""),
			"a secret needs a name",
			"pass the Secret's name, e.g. `kelson secret set checkout-db --project checkout --env production url=…`")
	}
	if !model.ValidSecretName(name) {
		return newError(ErrInvalidName, resourceOf(namespace, name),
			fmt.Sprintf("%q is not a DNS-1123 label, so no Secret can be called that", name),
			"use lowercase letters, digits and dashes, e.g. checkout-db; it is the same rule a "+
				"{secret: <name>, key: <key>} reference is held to")
	}
	return nil
}

// applyError maps an apply failure onto this package's vocabulary.
//
// A NotFound from an apply is the namespace, never the Secret: an apply creates
// what is missing, so the only thing left that can be absent is what it would
// have been created in.
func applyError(namespace, name string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return namespaceMissing(namespace, name)
	case apierrors.IsConflict(err):
		return newError(ErrWriteFailed, resourceOf(namespace, name),
			"another field manager owns keys of this Secret and the apply conflicted",
			"the Secret was written by something other than kelson; either let that owner keep it, "+
				"or delete it and write it again with `kelson secret set`").withCause(err)
	default:
		return newError(ErrWriteFailed, resourceOf(namespace, name),
			"the API server refused the write",
			"retry, and check that the credentials in use may create and patch Secrets in this namespace").
			withCause(err)
	}
}

func namespaceMissing(namespace, name string) error {
	return newError(ErrNamespaceMissing, resourceOf(namespace, name),
		fmt.Sprintf("namespace %q does not exist", namespace),
		"deploy the environment first (`kelson deploy`), which creates its namespace, or pass --namespace "+
			"if the Environment's spec.namespace names a different one. kelson does not create the namespace here: "+
			"a Secret in a namespace no environment targets is one nothing will ever read")
}

// secretMissing is the answer for a named Secret that is not there. Both verbs
// that take a name — delete and unset — need it, and they need the same one: a
// name kelson cannot find is the same fact whichever verb asked.
func secretMissing(namespace, name string) error {
	return newError(ErrNotFound, resourceOf(namespace, name),
		fmt.Sprintf("no Secret named %q exists in namespace %q", name, namespace),
		"run `kelson secret list` for this environment to see what kelson manages here")
}

func notManaged(namespace, name string) error {
	return newError(ErrNotManaged, resourceOf(namespace, name),
		fmt.Sprintf("Secret %q in namespace %q exists but is not managed by kelson", name, namespace),
		"kelson writes, lists and deletes only Secrets it labelled "+ManagedSelector()+", so it will not "+
			"take over one it did not create. Use a different name, manage that Secret with the tool that made it, "+
			"or adopt it deliberately with `kubectl -n "+namespace+" label secret "+name+" "+ManagedSelector()+"`")
}

// dryRunOptions is the API server's dry-run request, which is what makes
// DRY_RUN_SERVER meaningful here: admission runs and the merge is computed
// against the live object, and nothing is persisted.
func dryRunOptions(dry bool) []string {
	if !dry {
		return nil
	}
	return []string{metav1.DryRunAll}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
