package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dafrie/kelson/internal/build/registry"
)

// Pusher uploads artifacts over the OCI distribution API.
//
// # Why this is written and not imported
//
// Pushing an artifact is three requests — HEAD a blob, POST/PUT it if it is
// missing, PUT the manifest — plus the token dance registries answer a 401
// with. An OCI client library would bring a dependency tree measured in
// hundreds of packages onto a lint allow-list that today permits the standard
// library, cobra and yaml, for a protocol whose push half fits in this file.
// The build plane never needed one: an in-cluster build pushes from buildkit
// inside the pod, so this is kelson's first process that uploads to a registry
// itself.
//
// The credential vocabulary IS reused — registry.Credential and the
// dockerconfigjson parsing behind it — so there is one shape for "a registry
// credential" in this codebase, one place that resolves it from a Secret
// (internal/delivery/kube) and one place that knows it must never be printed
// (internal/redact).
type Pusher struct {
	// Client is the HTTP client. Nil means one with [DefaultRequestTimeout],
	// never http.DefaultClient — see [Pusher.client].
	Client *http.Client
	// Credential authenticates the push. The zero value is an anonymous push,
	// which is what a local registry with no auth takes.
	Credential registry.Credential
	// Insecure sends plain HTTP. It is implied for localhost and loopback
	// addresses, which is what makes a kind cluster's registry and an
	// httptest server work without a flag.
	Insecure bool

	// tokens caches the bearer token per scope for the life of one push, so
	// four requests do not each pay for a token exchange. The key is the scope
	// and not the host, because the same client both publishes and reads
	// (tags.go) and a token minted for "pull" would authorise neither the push
	// that reused it nor an honest error about why.
	mu     sync.Mutex
	tokens map[string]string
}

// Push uploads the artifact's blobs and then its manifest, and returns the
// digest-pinned reference the registry now serves.
//
// Blobs first, manifest last, is not a preference: a registry rejects a
// manifest whose layers it does not already hold, and doing it in this order
// means an interrupted push leaves unreferenced blobs (which registries garbage
// collect) rather than a manifest pointing at nothing (which an OCIRepository
// would fetch and fail on).
func (p *Pusher) Push(ctx context.Context, a Artifact) (string, error) {
	repo, err := parseRepository(a.Repository)
	if err != nil {
		return "", err
	}
	if a.Tag == "" {
		return "", fmt.Errorf("artifact: an artifact needs a tag to publish under")
	}
	for _, blob := range []struct {
		digest string
		body   []byte
	}{
		{a.ConfigDigest, a.Config},
		{a.LayerDigest, a.Layer},
	} {
		if err := p.pushBlob(ctx, repo, blob.digest, blob.body); err != nil {
			return "", err
		}
	}
	if err := p.pushManifest(ctx, repo, a); err != nil {
		return "", err
	}
	return a.Repository + "@" + a.Digest, nil
}

// RegistryHost is the registry an artifact repository lives on. Callers need it
// before they have an artifact — it is what a credential is looked up by, in a
// docker config or in a Secret — so it is exported rather than left inside the
// push.
func RegistryHost(repository string) (string, error) {
	t, err := parseRepository(repository)
	if err != nil {
		return "", err
	}
	return t.host, nil
}

// target is a parsed destination: which registry, which repository path, and
// what this conversation needs to be allowed to do there.
type target struct {
	host string
	path string
	// actions is the token scope requested when a registry challenges without
	// naming one of its own: "pull,push" for a publish, "pull" for the reads in
	// tags.go. Asking for push where only a read is happening is not free — a
	// credential with pull-only rights gets a token that authorises nothing,
	// and the 403 that follows says nothing about which half was missing.
	actions string
}

// The token scopes a target is authorised for. [parseRepository] defaults to
// the publish scope because publishing is what this client is mostly for; the
// read calls narrow it.
const (
	pushActions = "pull,push"
	pullActions = "pull"
)

// ociPrefix is how a repository is spelled where a scheme is written at all —
// `previews.artifacts.repository` in a spec, `url:` on an OCIRepository. It is
// accepted and stripped here so a caller may pass either spelling.
const ociPrefix = "oci://"

// parseRepository splits a repository reference into a host and a repository
// path, refusing anything that carries a tag or a digest — the tag is the
// publisher's to choose (the head commit for a preview, the generation and spec
// hash for the spine), and a pinned repository would silently publish every
// revision over the same artifact.
func parseRepository(repository string) (target, error) {
	ref := strings.TrimPrefix(strings.TrimSpace(repository), ociPrefix)
	if ref == "" {
		return target{}, Error{
			Reason:      ReasonNoRepository,
			Message:     "no artifact repository to publish to",
			Remediation: "name one: spec.previews.artifacts.repository for a preview, --registry for the controller",
		}
	}
	parsed, err := registry.Parse(ref)
	if err != nil {
		return target{}, Error{
			Reason:      ReasonRepositoryInvalid,
			Message:     err.Error(),
			Remediation: "an artifact repository is a registry host and a path, e.g. ghcr.io/acme/checkout-previews",
		}
	}
	if parsed.Tag != "" || parsed.Digest != "" {
		return target{}, Error{
			Reason:      ReasonRepositoryInvalid,
			Message:     "artifact repository " + quoted(repository) + " carries a tag or a digest",
			Remediation: "drop it: the tag is the publisher's to choose (ADR-0017 decision 2, ADR-0028 decision 2)",
		}
	}
	path := parsed.Repository
	if parsed.Namespace != "" {
		path = parsed.Namespace + "/" + parsed.Repository
	}
	return target{host: parsed.Registry, path: path, actions: pushActions}, nil
}

// scheme is https everywhere except a loopback registry, matching the
// convention every registry client follows: a developer's local registry is
// reachable and a remote one over plaintext is a mistake, not a configuration.
func (p *Pusher) scheme(host string) string {
	if p.Insecure || isLoopback(host) {
		return "http"
	}
	return "https"
}

func isLoopback(host string) bool {
	name := host
	if h, _, ok := strings.Cut(host, ":"); ok {
		name = h
	}
	switch name {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// scopeKey identifies a cached bearer token: the repository it was minted for
// and the actions it authorises.
func (t target) scopeKey() string { return t.host + "/" + t.path + ":" + t.actions }

func (p *Pusher) endpoint(t target, suffix string) string {
	return p.scheme(t.host) + "://" + t.host + "/v2/" + t.path + suffix
}

// pushBlob uploads one blob unless the registry already has it. The HEAD is
// what makes a republish cheap: an unchanged render produces the same digests,
// so the second publish of the same commit uploads nothing at all.
func (p *Pusher) pushBlob(ctx context.Context, t target, digest string, body []byte) error {
	head, err := p.do(ctx, t, http.MethodHead, p.endpoint(t, "/blobs/"+digest), nil, "")
	if err != nil {
		return err
	}
	closeBody(head)
	if head.StatusCode == http.StatusOK {
		return nil
	}

	start, err := p.do(ctx, t, http.MethodPost, p.endpoint(t, "/blobs/uploads/"), nil, "")
	if err != nil {
		return err
	}
	if start.StatusCode != http.StatusAccepted && start.StatusCode != http.StatusCreated {
		return responseError(start, "starting an upload to "+t.host+"/"+t.path)
	}
	location := start.Header.Get("Location")
	closeBody(start)
	if location == "" {
		return fmt.Errorf("artifact: %s accepted an upload without saying where to send it (no Location header)", t.host)
	}

	upload, err := p.uploadURL(t, location, digest)
	if err != nil {
		return err
	}
	done, err := p.do(ctx, t, http.MethodPut, upload, body, "application/octet-stream")
	if err != nil {
		return err
	}
	defer closeBody(done)
	if done.StatusCode != http.StatusCreated && done.StatusCode != http.StatusOK {
		return responseError(done, "uploading a blob to "+t.host+"/"+t.path)
	}
	return nil
}

// uploadURL resolves the Location a registry handed back — it may be absolute
// or relative — and appends the digest query the PUT is completed with.
func (p *Pusher) uploadURL(t target, location, digest string) (string, error) {
	base, err := url.Parse(p.endpoint(t, "/blobs/uploads/"))
	if err != nil {
		return "", fmt.Errorf("artifact: %w", err)
	}
	loc, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("artifact: %s returned an unusable upload location: %w", t.host, err)
	}
	resolved := base.ResolveReference(loc)
	q := resolved.Query()
	q.Set("digest", digest)
	resolved.RawQuery = q.Encode()
	return resolved.String(), nil
}

func (p *Pusher) pushManifest(ctx context.Context, t target, a Artifact) error {
	res, err := p.do(ctx, t, http.MethodPut, p.endpoint(t, "/manifests/"+a.Tag), a.Manifest, ManifestMediaType)
	if err != nil {
		return err
	}
	defer closeBody(res)
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return responseError(res, "publishing the artifact to "+t.host+"/"+t.path+":"+a.Tag)
	}
	return nil
}

// do performs one request, authenticating it and retrying once against the
// challenge a registry answers an unauthenticated request with.
//
// The body is a []byte rather than an io.Reader precisely because of that
// retry: a request that has to be replayed cannot have consumed its body.
// Artifacts are manifest sets, so they are small enough that this costs
// nothing worth optimising.
func (p *Pusher) do(ctx context.Context, t target, method, endpoint string, body []byte, contentType string) (*http.Response, error) {
	res, err := p.attempt(ctx, t, method, endpoint, body, contentType, false)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusUnauthorized {
		return res, nil
	}
	challenge := res.Header.Get("WWW-Authenticate")
	closeBody(res)
	if err := p.authorize(ctx, t, challenge); err != nil {
		return nil, err
	}
	return p.attempt(ctx, t, method, endpoint, body, contentType, true)
}

func (p *Pusher) attempt(ctx context.Context, t target, method, endpoint string, body []byte, contentType string, authorized bool) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("artifact: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", ManifestMediaType)
	p.authenticate(req, t, authorized)

	res, err := p.client().Do(req)
	if err != nil {
		// A transport error may quote the URL but never a header, so the
		// credential cannot be in here — and internal/redact is the net under
		// it either way.
		return nil, &UnreachableError{Doing: method + " " + redactQuery(endpoint), Err: err}
	}
	return res, nil
}

// authenticate attaches whatever credential this push has. A bearer token from
// a previous challenge wins; otherwise basic auth is offered up front, which is
// what a registry with no token service expects.
func (p *Pusher) authenticate(req *http.Request, t target, authorized bool) {
	p.mu.Lock()
	token := p.tokens[t.scopeKey()]
	p.mu.Unlock()
	if authorized && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		return
	}
	if p.Credential.Username != "" {
		req.SetBasicAuth(p.Credential.Username, p.Credential.Password)
	}
}

// authorize answers a registry's challenge.
//
// Bearer is the token flow the big registries use: the challenge names a realm
// and a scope, the credential is exchanged there for a token scoped to this
// repository, and the token authenticates the retry. Basic needs no exchange —
// the retry simply has to actually carry the header.
func (p *Pusher) authorize(ctx context.Context, t target, challenge string) error {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "bearer":
		realm := params["realm"]
		if realm == "" {
			return fmt.Errorf("artifact: %s asked for a bearer token but named no realm to get one from", t.host)
		}
		token, err := p.fetchToken(ctx, t, realm, params)
		if err != nil {
			return err
		}
		p.mu.Lock()
		if p.tokens == nil {
			p.tokens = map[string]string{}
		}
		p.tokens[t.scopeKey()] = token
		p.mu.Unlock()
		return nil
	case "basic", "":
		if p.Credential.Username == "" {
			return &DeniedError{
				StatusCode: http.StatusUnauthorized,
				Status:     "401 Unauthorized",
				Doing:      "publishing to " + t.host + "/" + t.path,
				Detail:     "this push carries no credential; " + credentialHint,
			}
		}
		return nil
	default:
		return fmt.Errorf("artifact: %s asked for %q authentication, which kelson does not speak", t.host, scheme)
	}
}

// credentialHint is the one sentence every auth refusal ends with, so every way
// to supply a credential is always named together — the CLI's two, and the
// controller's mounted docker config (ADR-0028 decision 2).
const credentialHint = "log the runner in to the registry (docker login writes the config this reads), pass --registry-secret to resolve a dockerconfigjson Secret from the cluster, or mount one at the controller's --registry-config path"

// quoted is the house spelling for a value echoed back inside a message.
func quoted(s string) string { return `"` + s + `"` }

func (p *Pusher) fetchToken(ctx context.Context, t target, realm string, params map[string]string) (string, error) {
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("artifact: %s named an unusable token realm: %w", t.host, err)
	}
	q := u.Query()
	if service := params["service"]; service != "" {
		q.Set("service", service)
	}
	// The scope the challenge asked for is honoured when present; otherwise the
	// minimum this push needs is requested explicitly, because a token with no
	// scope authorises nothing and the resulting 403 says nothing useful.
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + t.path + ":" + t.actions
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("artifact: %w", err)
	}
	if p.Credential.Username != "" {
		req.SetBasicAuth(p.Credential.Username, p.Credential.Password)
	}
	res, err := p.client().Do(req)
	if err != nil {
		return "", &UnreachableError{Doing: "requesting a token from " + redactQuery(realm), Err: err}
	}
	defer closeBody(res)
	if res.StatusCode != http.StatusOK {
		return "", &DeniedError{
			StatusCode: res.StatusCode,
			Status:     res.Status,
			Doing:      "requesting a push token for " + t.path + " from " + u.Host,
			Detail:     credentialHint,
		}
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, tokenLimit)).Decode(&payload); err != nil {
		return "", fmt.Errorf("artifact: %s returned a token response kelson could not read: %w", u.Host, err)
	}
	if payload.Token != "" {
		return payload.Token, nil
	}
	if payload.AccessToken != "" {
		return payload.AccessToken, nil
	}
	return "", fmt.Errorf("artifact: %s returned a token response with no token in it", u.Host)
}

// tokenLimit bounds a token response. It is generous for a JWT and small
// enough that a misconfigured endpoint returning a web page is an error rather
// than a memory problem.
const tokenLimit = 1 << 20

// errorLimit bounds how much of a registry's error body is quoted back.
const errorLimit = 4096

// DefaultRequestTimeout bounds one request of a push — a blob HEAD, a blob PUT,
// the manifest PUT, a token exchange — for a Pusher that was given no client of
// its own.
//
// The default it replaces was http.DefaultClient, which has no timeout at all:
// a registry that completes its handshake and then answers nothing holds the
// caller forever, and both callers are single-threaded about it. In the
// controller that is one reconcile worker out of a small pool, so one black-hole
// registry stops the cluster's deployments; in the CLI it is a `kelson preview
// publish` that never returns and never says why. A minute is generous for a
// manifest set measured in kilobytes and short enough that the failure is
// reported while somebody is still watching.
const DefaultRequestTimeout = time.Minute

// defaultClient is shared so a Pusher per push does not build a connection pool
// per push. It carries no credential — that is [Pusher.Credential] — so sharing
// it leaks nothing between callers.
var defaultClient = &http.Client{Timeout: DefaultRequestTimeout}

func (p *Pusher) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return defaultClient
}

// parseChallenge splits a WWW-Authenticate header into its scheme and its
// key="value" parameters.
func parseChallenge(header string) (string, map[string]string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", nil
	}
	scheme, rest, _ := strings.Cut(header, " ")
	params := map[string]string{}
	for _, part := range splitParams(rest) {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return scheme, params
}

// splitParams splits on commas that are not inside a quoted value: a scope
// commonly contains one ("pull,push").
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			quoted = !quoted
			cur.WriteByte(c)
		case c == ',' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// responseError quotes the registry's own error, which is the only party that
// knows why it said no. The body is bounded and the URL is never included with
// its query string, because an upload location can carry a signed token.
func responseError(res *http.Response, doing string) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, errorLimit))
	return &DeniedError{
		StatusCode: res.StatusCode,
		Status:     res.Status,
		Doing:      doing,
		Detail:     strings.TrimSpace(string(body)),
	}
}

// redactQuery drops a URL's query string before it reaches an error message.
// Registries hand back upload locations carrying signed tokens, and an error
// that quoted one would put a credential-equivalent in a CI log.
func redactQuery(raw string) string {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i]
	}
	return raw
}

func closeBody(res *http.Response) {
	if res != nil && res.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, errorLimit))
		_ = res.Body.Close()
	}
}
