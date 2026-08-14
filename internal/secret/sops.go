package secret

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dafrie/kelson/internal/delivery/git"
	"github.com/dafrie/kelson/internal/redact"
	"github.com/dafrie/kelson/internal/sops"
)

// The `sops` backend's authoring path (issue #81, ADR-0022): the same three
// commands as the cluster backend, writing into the delivery repository
// instead of into the cluster.
//
// # Encrypt on write, and what "plaintext never touches the working tree"
// # actually rests on
//
// [SOPSStore.Set] holds a value for the length of one call, encrypts it in
// memory with internal/sops, and hands the *ciphertext* to the git writer. The
// git writer works on an in-memory billy filesystem (internal/delivery/git),
// so even the encrypted form never reaches the caller's disk, and the
// plaintext never exists anywhere but this process's heap. There is no
// temporary file, no `sops` subprocess to pipe a value to, and no code path
// between the caller and the AES-GCM seal that writes anything.
//
// # kelson has no private key, so `set` writes the whole Secret
//
// This is the one place the sops backend behaves differently from `cluster`,
// and it follows from the guarantee rather than from an implementation
// shortcut. Under `cluster`, Set merges: it reads the live Secret and carries
// the untouched keys forward. Here the untouched keys are ciphertext under a
// data key kelson cannot open, because opening it would need the age identity
// kelson deliberately never holds.
//
// So a `set` that would drop keys is **refused by name** rather than done
// quietly ([ErrSOPSPartialSet]). The keys already in the file are readable
// without any key — SOPS encrypts values, not structure — so the refusal can
// list exactly what would be lost and exactly what to pass. Writing a strict
// superset of the existing keys is the ordinary case and simply works.
//
// # Reading needs no key either
//
// [SOPSStore.List] reports each encrypted Secret's name and key names, and
// [SOPSStore.Drift] reports which files are wrapped for a recipient set other
// than the spec's. Both read only what SOPS leaves in the clear. As with the
// cluster backend, no type in this package has a field a value could be in.

// SOPSConfig configures the sops store for one environment.
type SOPSConfig struct {
	// Writer commits into the environment's delivery repository. Its
	// Target.Path is the directory the environment's Flux Kustomization
	// reconciles; encrypted Secrets go into git.SecretsDir beneath it.
	Writer *git.Writer

	// Recipients are the age public keys from `secrets.ageRecipients`. At
	// least one is required — there is nothing to default an encryption key
	// to, which is why the model makes the field required rather than
	// optional (ADR-0022).
	Recipients []string

	// Now stamps the encrypted file's `lastmodified`. Nil means time.Now.
	Now func() time.Time
}

// SOPSStore writes, lists and deletes SOPS-encrypted Secrets in a delivery
// repository. It satisfies the same three-method shape the cluster [Store]
// does, so `kelson secret` selects a backend and not a code path.
type SOPSStore struct {
	cfg SOPSConfig
}

// NewSOPS returns a store over the given delivery repository.
func NewSOPS(cfg SOPSConfig) (*SOPSStore, error) {
	if cfg.Writer == nil {
		return nil, newError(ErrSOPSNoTarget, "",
			"the sops backend writes into the environment's delivery repository and none is configured",
			"set delivery.git.repo on the Environment; backend sops requires delivery.mode: flux, which "+
				"requires a git target (ADR-0022)")
	}
	if len(cfg.Recipients) == 0 {
		return nil, newError(ErrSOPSNoRecipients, "",
			"the sops backend encrypts to age recipients and the environment lists none",
			"generate a key with `age-keygen -o age.key`, keep the AGE-SECRET-KEY line out of Git, and add "+
				"the public half to the Environment: secrets: { backend: sops, ageRecipients: [age1…] }")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &SOPSStore{cfg: cfg}, nil
}

// Set encrypts the keys and commits the result.
//
// The order of the first statement is the same as the cluster store's and for
// the same reason: every value is registered with internal/redact before
// anything else can happen to it, so no later failure — a validation refusal,
// a git conflict, a push rejection — can put one in a message.
func (s *SOPSStore) Set(ctx context.Context, req SetRequest) (Secret, error) {
	for _, v := range req.Values {
		redact.Register(v)
	}

	namespace, err := req.Validate()
	if err != nil {
		return Secret{}, err
	}

	session, err := s.cfg.Writer.Open(ctx, s.message("set "+req.Name, req.Target))
	if err != nil {
		return Secret{}, s.gitError(namespace, req.Name, "reading the delivery repository", err)
	}
	if err := s.refuseKeyLoss(session, namespace, req); err != nil {
		return Secret{}, err
	}

	data := make(map[string][]byte, len(req.Values))
	for k, v := range req.Values {
		data[k] = []byte(v)
	}
	encrypted, err := sops.EncryptSecret(sops.Secret{
		Name:      req.Name,
		Namespace: namespace,
		Labels:    labelsFor(req.Target),
		Data:      data,
	}, sops.Options{Recipients: s.cfg.Recipients, Now: s.cfg.Now()})
	if err != nil {
		// The encryption failure this reaches in practice is a recipient the
		// model's spelling check let through and age's parser did not.
		return Secret{}, newError(ErrSOPSEncryptFailed, resourceOf(namespace, req.Name),
			"the Secret could not be encrypted",
			"check secrets.ageRecipients: each entry must be an age public key as printed by `age-keygen`").
			withCause(err)
	}

	written := Secret{Name: req.Name, Namespace: namespace, Keys: req.Keys()}
	if req.DryRun {
		// Nothing is opened, nothing is committed. The encryption above still
		// ran, so a dry run answers the question it is asked — would this
		// write succeed — rather than answering a cheaper one.
		return written, nil
	}
	if err := s.put(ctx, session, git.PutRequest{
		Files: []git.File{{Path: git.SecretPath(req.Name), Data: encrypted}},
	}, namespace, req.Name); err != nil {
		return Secret{}, err
	}
	return written, nil
}

// refuseKeyLoss is the guarantee this backend cannot merge, stated as a
// refusal. See the file comment: the keys already in the file are ciphertext
// under a data key kelson has no identity for, so a write that names fewer
// keys than the file holds would silently drop credentials.
func (s *SOPSStore) refuseKeyLoss(session *git.Session, namespace string, req SetRequest) error {
	existing, err := s.inspect(session, req.Name)
	if err != nil || existing == nil {
		return err
	}
	var lost []string
	for _, key := range existing.Keys {
		if _, kept := req.Values[key]; !kept {
			lost = append(lost, key)
		}
	}
	if len(lost) == 0 {
		return nil
	}
	sort.Strings(lost)
	return newError(ErrSOPSPartialSet, resourceOf(namespace, req.Name),
		fmt.Sprintf("this write names %s and the encrypted Secret also holds %s, which it would drop",
			quoteList(req.Keys()), quoteList(lost)),
		"pass every key the Secret should have — under backend sops a set writes the whole file, because "+
			"kelson would need the age identity to carry the other keys forward and never holds one. "+
			"To drop them deliberately, `kelson secret delete "+req.Name+"` first (ADR-0022)")
}

// List reports the encrypted Secrets in the delivery repository, masked.
//
// There is no age column. A Secret's age here would be its file's last commit
// date, which is a fact about the repository rather than about the credential,
// and reporting a commit date under a heading that means "how old is this
// value" everywhere else would be worse than reporting nothing.
func (s *SOPSStore) List(ctx context.Context, t Target) ([]Secret, error) {
	namespace, err := t.Resolve()
	if err != nil {
		return nil, err
	}
	session, err := s.cfg.Writer.Open(ctx, s.message("list", t))
	if err != nil {
		return nil, s.gitError(namespace, "", "reading the delivery repository", err)
	}
	paths, err := session.SecretFiles()
	if err != nil {
		return nil, s.gitError(namespace, "", "listing the encrypted Secrets", err)
	}
	out := make([]Secret, 0, len(paths))
	for _, p := range paths {
		name, _ := git.SecretName(p)
		f, err := s.inspect(session, name)
		if err != nil {
			return nil, err
		}
		if f == nil {
			continue
		}
		out = append(out, Secret{Name: name, Namespace: f.Namespace, Keys: f.Keys})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete removes an encrypted Secret from the delivery repository.
//
// There is no managed-secret check here and there does not need to be: the
// cluster backend's rule exists because a namespace holds Secrets kelson never
// wrote, and `<path>/secrets/<name>.enc.yaml` is a location kelson defines.
// What the file *is* is checked instead — a path there that is not a SOPS
// document is reported rather than deleted.
func (s *SOPSStore) Delete(ctx context.Context, req DeleteRequest) error {
	namespace, err := req.Validate()
	if err != nil {
		return err
	}
	session, err := s.cfg.Writer.Open(ctx, s.message("delete "+req.Name, req.Target))
	if err != nil {
		return s.gitError(namespace, req.Name, "reading the delivery repository", err)
	}
	f, err := s.inspect(session, req.Name)
	if err != nil {
		return err
	}
	if f == nil {
		return newError(ErrNotFound, resourceOf(namespace, req.Name),
			fmt.Sprintf("no encrypted Secret named %q is committed at %s", req.Name,
				s.repoPath(git.SecretPath(req.Name))),
			"run `kelson secret list` for this environment to see what is there")
	}
	if req.DryRun {
		return nil
	}
	return s.put(ctx, session, git.PutRequest{Remove: []string{git.SecretPath(req.Name)}}, namespace, req.Name)
}

// SOPSDrift is one encrypted Secret whose recipients are not the spec's.
//
// It is reported rather than fixed, because fixing it means either the values
// (which kelson does not have) or the age identity (which kelson must not
// have). What kelson can do is say precisely which files are stale and why,
// and that is the part a human doing a key rotation actually gets wrong.
type SOPSDrift struct {
	// Name is the Secret's, and Path is where its file lives in the
	// repository — a rotation is often driven from a checkout, and
	// `sops updatekeys` takes a path.
	Name string
	Path string
	// Keys are the Secret's key names, so `kelson secret set` can be
	// reconstructed from the report without a second lookup.
	Keys []string
	// Recipients are what the *file* is wrapped for, sorted. Comparing them
	// with the spec's is the whole of the drift.
	Recipients []string
	// Missing are spec recipients the file is not wrapped for: whoever holds
	// those identities cannot read this Secret. Extra are recipients the file
	// carries and the spec no longer lists: whoever holds those identities
	// still can.
	Missing []string
	Extra   []string
}

// Drift reports every encrypted Secret whose recipient set differs from the
// environment's `secrets.ageRecipients`.
//
// This is what `kelson secret rotate` runs. It needs no key material: SOPS
// stores each recipient in the clear beside the data key it wrapped, which is
// exactly so that a reader can answer "who can open this" without being one of
// them.
func (s *SOPSStore) Drift(ctx context.Context, t Target) ([]SOPSDrift, error) {
	namespace, err := t.Resolve()
	if err != nil {
		return nil, err
	}
	session, err := s.cfg.Writer.Open(ctx, s.message("rotate", t))
	if err != nil {
		return nil, s.gitError(namespace, "", "reading the delivery repository", err)
	}
	paths, err := session.SecretFiles()
	if err != nil {
		return nil, s.gitError(namespace, "", "listing the encrypted Secrets", err)
	}

	want := make([]string, len(s.cfg.Recipients))
	for i, r := range s.cfg.Recipients {
		want[i] = strings.TrimSpace(r)
	}
	var out []SOPSDrift
	for _, p := range paths {
		name, _ := git.SecretName(p)
		f, err := s.inspect(session, name)
		if err != nil {
			return nil, err
		}
		if f == nil || f.EncryptedTo(want) {
			continue
		}
		recipients := append([]string(nil), f.Recipients...)
		sort.Strings(recipients)
		out = append(out, SOPSDrift{
			Name:       name,
			Path:       s.repoPath(p),
			Keys:       f.Keys,
			Recipients: recipients,
			Missing:    missingFrom(recipients, want),
			Extra:      missingFrom(want, recipients),
		})
	}
	return out, nil
}

// Recipients is the spec's recipient set, so a caller reporting drift can
// print what the files are being compared against without holding the config
// twice.
func (s *SOPSStore) Recipients() []string { return append([]string(nil), s.cfg.Recipients...) }

// RepoPath is where an encrypted Secret lives in the repository, for a message
// that has to name a file a human will `git checkout` and `sops updatekeys`.
func (s *SOPSStore) RepoPath(name string) string { return s.repoPath(git.SecretPath(name)) }

// inspect reads one encrypted Secret's public half, or nil when there is no
// file. A file that exists and is not a SOPS document is an error: it is
// almost certainly a plaintext Secret somebody committed by hand, which is the
// exact failure this backend exists to prevent, and treating it as absent
// would have `set` overwrite it and `rotate` ignore it.
func (s *SOPSStore) inspect(session *git.Session, name string) (*sops.File, error) {
	body, err := session.ReadFile(git.SecretPath(name))
	if err != nil {
		return nil, nil //nolint:nilerr // an unreadable path here means "not committed"
	}
	f, err := sops.Inspect(body)
	if err != nil {
		return nil, newError(ErrSOPSNotEncrypted, resourceOf("", name),
			fmt.Sprintf("%s exists but is not a SOPS-encrypted document", s.repoPath(git.SecretPath(name))),
			"kelson will not overwrite or report on a file it did not encrypt. If this is a plaintext Secret, "+
				"treat its contents as compromised — it is in the repository's history — then remove the file and "+
				"write it again with `kelson secret set`").withCause(err)
	}
	return &f, nil
}

// put commits an additive change, mapping the git plane's refusals onto this
// package's vocabulary. The conflict case gets its own message because it is
// the one a user hits: two people setting secrets at once, or a deploy landing
// between this session's read and its push.
func (s *SOPSStore) put(ctx context.Context, session *git.Session, req git.PutRequest, namespace, name string) error {
	if err := session.Put(req.Files, req.Remove); err != nil {
		return s.gitError(namespace, name, "staging the encrypted Secret", err)
	}
	if _, err := session.Commit(ctx); err != nil {
		return s.gitError(namespace, name, "committing the encrypted Secret", err)
	}
	return nil
}

func (s *SOPSStore) gitError(namespace, name, doing string, err error) error {
	return newError(ErrSOPSWriteFailed, resourceOf(namespace, name),
		"failed while "+doing,
		"check the delivery repository URL, the branch, and the credential in KELSON_GIT_TOKEN. "+
			"kelson never force-pushes: a conflict means the branch moved while this command was running, "+
			"and re-running it re-reads the branch").withCause(err)
}

// message is the commit this store writes. It carries the same trailers a
// deploy commit does, so `kelson history` and a reviewer see one vocabulary,
// and the subject names the Secret rather than its keys — a commit subject is
// the most-copied line in a repository and a key name is a hint about what a
// credential is for.
func (s *SOPSStore) message(subject string, t Target) git.Message {
	return git.Message{
		Subject:     "kelson: secret " + subject,
		Project:     t.Project,
		Environment: t.Environment,
	}
}

func (s *SOPSStore) repoPath(rel string) string {
	if base := s.cfg.Writer.Target().Path; base != "" {
		return base + "/" + rel
	}
	return rel
}

// missingFrom returns the entries of want that are not in have, sorted.
func missingFrom(have, want []string) []string {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	var out []string
	for _, w := range want {
		if !set[w] {
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}

func quoteList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, i := range items {
		quoted = append(quoted, `"`+i+`"`)
	}
	return strings.Join(quoted, ", ")
}
