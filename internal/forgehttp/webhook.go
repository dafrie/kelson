package forgehttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/forge"
	"github.com/dafrie/kelson/internal/forgeconn"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview/naming"
)

var errNoResolver = errors.New("forgehttp: a connection resolver is required")

// webhook handles POST /forge/github/webhook (ADR-0034 decisions 1 and 2).
//
// # Verification comes before parsing, and before anything else
//
// The body is read, then verified, then parsed, in that order and never
// another. A listener that parsed first would be branching on attacker-supplied
// structure before establishing the sender, and the fact that this one only
// ever *reads* four fields would be a property of today's code rather than of
// the flow.
//
// # Which secret verifies it: the multi-app case
//
// An instance may hold several GitHub connections — two organisations, a
// github.com app beside a GitHub Enterprise one — and a delivery carries
// nothing that says which app sent it. The app ID is in the payload, but the
// payload is exactly what is not yet trusted. So every github connection's
// webhook secret is tried, with [forge.WebhookSource.VerifySignature]'s
// constant-time compare, and the first that verifies wins.
//
// That is sound because the HMAC *is* the identity claim: a delivery that
// verifies against connection A's secret was signed by whoever holds A's
// secret, whatever the body says about itself. Two connections sharing one
// secret would make "first" arbitrary — but two apps never share a webhook
// secret, because GitHub generates one per app at creation (ADR-0033 decision
// 2), and a hand-made `generic` connection has no webhook secret at all and is
// skipped by [forge.For] returning an adapter with no WebhookSource.
//
// The cost is a bounded number of HMACs per delivery, which is nothing beside
// the JSON decode that follows it.
func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the delivery body could not be read"})
		return
	}

	verified, source, err := h.verify(r.Context(), r.Header, body)
	if err != nil {
		// Deliberately one answer for "no connection's secret matched", "this
		// instance holds no github connection" and "the signature header is
		// missing". They differ only in ways that would tell an unauthenticated
		// caller about this instance's configuration, and the action is the
		// same for all three.
		h.log.Warn("forge webhook refused", "reason", "signature did not verify against any connection")
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "the delivery did not verify against any git connection this instance holds",
		})
		return
	}

	event, err := source.ParseEvent(r.Header, body)
	if err != nil {
		if errors.Is(err, forge.ErrIgnoredEvent) {
			// An installation is subscribed to more than kelson acts on, and a
			// listener that called the forge working correctly a fault would be
			// noise in every delivery log.
			writeJSON(w, http.StatusAccepted, map[string]any{"ignored": true})
			return
		}
		h.log.Warn("forge webhook unparseable", "connection", verified.Name(), "error", err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the delivery could not be parsed"})
		return
	}

	switch event.Kind {
	case "ping":
		// GitHub's handshake. Answering it is the whole contract, and saying so
		// in the log is what turns "did the webhook URL work" into a fact.
		h.log.Info("forge webhook ping", "connection", verified.Name(), "repository", event.RepoFullName)
		writeJSON(w, http.StatusOK, map[string]any{"event": "ping", "connection": verified.Name()})

	case "pull_request":
		poked, missed := h.pokePreviews(r.Context(), verified, event)
		h.log.Info("forge webhook pull_request",
			"connection", verified.Name(), "repository", event.RepoFullName,
			"action", event.Action, "pr", event.PRNumber, "poked", len(poked), "failed", len(missed))
		writeJSON(w, http.StatusOK, map[string]any{
			"event": "pull_request", "action": event.Action, "poked": poked, "failed": missed,
		})

	case "push":
		// Deliberately nothing. `autoDeploy` — ADR-0034 decision 4, "an
		// environment with autoDeploy: true re-renders and republishes on a push
		// to the project's source ref" — is the consumer of this event, it is
		// opt-in, off by default, and not built. Acting on a push before that
		// field exists would deploy on every push to every environment, which is
		// the opposite of the posture ADR-0016 holds.
		h.log.Info("forge webhook push (no-op until autoDeploy, ADR-0034 decision 4)",
			"connection", verified.Name(), "repository", event.RepoFullName, "ref", event.Ref)
		writeJSON(w, http.StatusOK, map[string]any{"event": "push", "acted": false, "reason": "autoDeploy is not implemented (ADR-0034 decision 4)"})

	case "installation":
		h.recordInstallation(r.Context(), w, verified, event)

	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"ignored": true, "event": event.Kind})
	}
}

// verify finds the connection whose webhook secret signed this delivery.
func (h *Handler) verify(ctx context.Context, header http.Header, body []byte) (forgeconn.Resolution, forge.WebhookSource, error) {
	candidates, skipped, err := h.opts.Sources.ByProvider(ctx, "github")
	if err != nil {
		return forgeconn.Resolution{}, nil, err
	}
	if len(skipped) > 0 {
		// Named, because "the signature does not match" and "kelson could not
		// read that connection's Secret" are different problems with different
		// fixes, and the second one is invisible from the forge's side.
		h.log.Warn("forge webhook could not consider some connections",
			"connections", strings.Join(skipped, ","), "reason", "their auth Secret could not be read")
	}
	for _, c := range candidates {
		source, ok := c.Provider.(forge.WebhookSource)
		if !ok {
			continue
		}
		if err := source.VerifySignature(c.Conn, header, body); err == nil {
			return c, source, nil
		}
	}
	return forgeconn.Resolution{}, nil, forge.ErrBadSignature
}

// pokePreviews stamps every ResourceSetInputProvider whose environment previews
// this repository (ADR-0034 decision 2).
//
// The two returns are the objects poked and the objects that could not be, both
// as `namespace/name`. Failures are reported and never fatal: a poke makes the
// poll instant, and losing one costs `previews.interval` of latency rather than
// correctness — so a 500 here would only make GitHub redeliver something that
// has already been handled as well as it can be.
func (h *Handler) pokePreviews(ctx context.Context, conn forgeconn.Resolution, event forge.Event) (poked, failed []string) {
	if h.opts.Specs == nil || h.opts.Previews == nil {
		return nil, nil
	}
	for _, target := range h.previewTargets(ctx, conn, event) {
		ref := target.namespace + "/" + target.name
		if err := h.opts.Previews.Poke(ctx, target.namespace, target.name); err != nil {
			h.log.Warn("could not poke a preview provider", "object", ref, "error", err.Error())
			failed = append(failed, ref)
			continue
		}
		poked = append(poked, ref)
	}
	sort.Strings(poked)
	sort.Strings(failed)
	return poked, failed
}

// previewTarget is one ResourceSetInputProvider to stamp.
type previewTarget struct{ namespace, name string }

// previewTargets finds the environments whose `previews.repo` is the repository
// this delivery is about.
//
// The name and namespace are derived rather than looked up, from the same
// functions the renderer writes them with (internal/preview/naming and
// model.DefaultNamespace): the object is created by applying kelson's own
// rendered manifests, so re-deriving its address is reading the contract rather
// than guessing at it. An environment whose previews have never been applied
// has no object, and the poke fails with NotFound — reported, not fatal.
func (h *Handler) previewTargets(ctx context.Context, conn forgeconn.Resolution, event forge.Event) []previewTarget {
	stored, err := h.opts.Specs.List(ctx)
	if err != nil {
		h.log.Warn("could not list projects for a webhook delivery", "error", err.Error())
		return nil
	}

	var out []previewTarget
	for _, s := range stored {
		for name, doc := range s.Documents.Environments {
			env, ok := decodeEnvironment(doc)
			if !ok || env.Spec.Previews == nil {
				continue
			}
			if !sameRepository(env.Spec.Previews.Repo, conn.Stored.Spec.EffectiveHost(), event) {
				continue
			}
			namespace := env.Spec.Namespace
			if namespace == "" {
				namespace = model.DefaultNamespace(s.Project, name)
			}
			out = append(out, previewTarget{namespace: namespace, name: naming.Lifecycle(s.Project, name)})
		}
	}
	return out
}

func decodeEnvironment(doc []byte) (*model.Environment, bool) {
	docs, errs := model.DecodeDocuments(doc)
	if len(errs) > 0 {
		return nil, false
	}
	for _, d := range docs {
		if env, ok := d.(*model.Environment); ok {
			return env, true
		}
	}
	return nil, false
}

// sameRepository reports whether an environment's `previews.repo` names the
// repository a delivery is about.
//
// Both the host and the path must agree. The path alone would match
// `acme/checkout` on gitlab.com against a GitHub delivery for the same path,
// which is a real collision for anyone mirroring — and the host is free to
// check because the delivery arrived through a connection that has one.
func sameRepository(previewsRepo, connectionHost string, event forge.Event) bool {
	wantHost, wantPath, ok := splitRepoURL(previewsRepo)
	if !ok {
		return false
	}
	if path := strings.ToLower(strings.Trim(event.RepoFullName, "/")); path != "" && path != wantPath {
		return false
	} else if path == "" {
		return false
	}
	// The connection's host is the authority on where the delivery came from;
	// the payload's own repository URL is only consulted when the connection
	// declared no host (which model.GitConnectionSpec.EffectiveHost only allows
	// for a provider that is not github).
	host := connectionHost
	if host == "" {
		host = event.RepoHTMLURL
	}
	eventHost, _, ok := splitRepoURL(host)
	return ok && eventHost == wantHost
}

// splitRepoURL reduces a repository or forge URL to its lowercased host and its
// path with no leading slash and no `.git` suffix.
func splitRepoURL(raw string) (host, path string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	return strings.ToLower(u.Host), strings.ToLower(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")), true
}

// recordInstallation writes the installation ID an `installation` event
// carries onto the connection whose secret verified it (ADR-0033 decision 2
// step 3).
//
// This is the one piece of state a delivery produces, and it is the one piece
// nothing else can learn: the manifest flow creates the app and the connection
// *before* anybody installs it, so `installationID: 0` is a real intermediate
// state and this event is how it ends. Everything about it is still
// replay-safe — the same event twice writes the same number — and a `deleted`
// action zeroes it back, because an app that has been uninstalled cannot mint
// and a connection that claims it can is a connection that fails at clone time
// instead of at its Ready condition.
func (h *Handler) recordInstallation(ctx context.Context, w http.ResponseWriter, conn forgeconn.Resolution, event forge.Event) {
	if h.opts.Connections == nil || conn.Bootstrap {
		writeJSON(w, http.StatusOK, map[string]any{"event": "installation", "recorded": false})
		return
	}
	id := event.InstallationID
	if event.Action == "deleted" || event.Action == "suspend" {
		id = 0
	}
	if _, err := h.opts.Connections.RecordInstallation(ctx, conn.Name(), id); err != nil {
		h.log.Warn("could not record an installation", "connection", conn.Name(), "error", err.Error())
		// A 500 asks GitHub to redeliver, which is what we want: this is the
		// one delivery whose loss is not merely latency.
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the installation could not be recorded"})
		return
	}
	h.log.Info("recorded a github app installation",
		"connection", conn.Name(), "action", event.Action, "installation", id)
	writeJSON(w, http.StatusOK, map[string]any{
		"event": "installation", "action": event.Action, "connection": conn.Name(), "installation": id,
	})
}

// Compile-time proof that the store satisfies the narrow seam above, so a
// change to either is a build failure here rather than a nil interface at
// startup.
var _ Connections = (*controlstore.GitConnectionStore)(nil)
