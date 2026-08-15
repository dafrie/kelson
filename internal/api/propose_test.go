package api

import (
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/forge"
	"github.com/dafrie/kelson/internal/model"
)

// ProposeSpec served (#248, ADR-0033 decision 3). The forge is faked for the
// reason gitconnection_test.go fakes it: what these assert is the handler —
// what it refuses before reaching a forge, what it hands the adapter, and how
// each of the adapter's four sentinels becomes an answer a person can act on.
// internal/forge's own httptest suite is where the REST sequence is proven.

// --- the forge seam ----------------------------------------------------------

// proposingForge is a [forge.PRProposer] that records what it was asked for.
type proposingForge struct {
	fakeForge
	url string
	err error

	mu    sync.Mutex
	calls []proposedCall
}

type proposedCall struct {
	conn       forge.Conn
	repository string
	proposal   forge.Proposal
}

func (f *proposingForge) OpenPullRequest(_ context.Context, c forge.Conn, repoFullName string, p forge.Proposal) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, proposedCall{conn: c, repository: repoFullName, proposal: p})
	f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return f.url, nil
}

func (f *proposingForge) only(t *testing.T) proposedCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 {
		t.Fatalf("the forge was asked to open %d pull requests, want exactly one", len(f.calls))
	}
	return f.calls[0]
}

func (f *proposingForge) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newProposer() *proposingForge {
	return &proposingForge{
		fakeForge: fakeForge{name: "github"},
		url:       "https://github.test/acme/gitops/pull/7",
	}
}

// --- fixtures ----------------------------------------------------------------

const gitopsProject = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:3
  components:
    - {name: web, port: 8080}
    - {name: worker}
`

const gitopsEnvironment = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  namespace: shop-prod
`

// proposeOptions is a server wired for a proposal: a connection with material,
// a forge that proposes, and the policy spec in the store (so the environments
// an agent's policy is read from actually exist).
func proposeOptions(t *testing.T, p forge.Provider) Options {
	t.Helper()
	store := newFakeSpecStore()
	project, envs := policySpec()
	if _, err := store.Put(t.Context(), "shop",
		controlstore.Documents{Project: project, Environments: envs}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the spec: %v", err)
	}
	connections := newFakeConnections().
		with(appConnection("acme-github", "acme-github-app")).
		withSecret("acme-github", controlstore.AuthMaterial{PrivateKeyPEM: []byte("-----BEGIN PRIVATE KEY-----")})
	return Options{Specs: store, Connections: connections, Forges: forgeLookup(p)}
}

func proposeRequest() *kelsonv1alpha1.ProposeSpecRequest {
	return &kelsonv1alpha1.ProposeSpecRequest{
		Project:    "shop",
		Connection: "acme-github",
		Repository: "acme/gitops",
		BaseBranch: "main",
		Files: []*kelsonv1alpha1.ProposedFile{
			{Path: "clusters/prod/shop.yaml", Content: []byte(gitopsProject)},
		},
	}
}

// --- the happy path ----------------------------------------------------------

func TestProposeSpecOpensThePullRequest(t *testing.T) {
	p := newProposer()
	c := serve(t, proposeOptions(t, p))

	req := proposeRequest()
	req.Title = "Bump shop to v3"
	req.Body = "Reviewed the render."
	req.Files = append(req.Files, &kelsonv1alpha1.ProposedFile{
		Path: "clusters/prod/shop-production.yaml", Content: []byte(gitopsEnvironment),
	})

	res, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ProposeSpec: %v", err)
	}
	if len(res.Msg.GetErrors()) != 0 {
		t.Fatalf("the documents were refused: %+v", res.Msg.GetErrors())
	}
	if res.Msg.GetUrl() != p.url {
		t.Errorf("url = %q, want the forge's %q", res.Msg.GetUrl(), p.url)
	}
	if res.Msg.GetBranch() == "" {
		t.Error("no branch was reported; a caller that proposed twice cannot tell the two apart")
	}

	call := p.only(t)
	if call.repository != "acme/gitops" {
		t.Errorf("repository = %q, want the one the request named", call.repository)
	}
	if call.proposal.BaseBranch != "main" {
		t.Errorf("base branch = %q, want main", call.proposal.BaseBranch)
	}
	if call.proposal.Title != "Bump shop to v3" {
		t.Errorf("title = %q, want the caller's", call.proposal.Title)
	}
	if len(call.proposal.Files) != 2 {
		t.Fatalf("the proposal carries %d files, want both", len(call.proposal.Files))
	}
	if string(call.proposal.Files["clusters/prod/shop.yaml"]) != gitopsProject {
		t.Error("the Project document did not reach the forge byte for byte")
	}
	// The connection's material is joined on, exactly as the browse path does
	// it — a proposal through a connection whose Secret was never opened would
	// authenticate as nobody.
	if len(call.conn.PrivateKeyPEM) == 0 || call.conn.AppID == 0 {
		t.Errorf("the forge was handed %v, want the connection's app material", call.conn)
	}

	// The body discloses what ProposedFile's schema says: each file is written
	// whole. A reviewer is the last person who can catch a file that held
	// something else, and only if they are told to look.
	if !strings.Contains(call.proposal.Body, "Reviewed the render.") {
		t.Error("the caller's body was dropped")
	}
	if !strings.Contains(call.proposal.Body, "written whole") {
		t.Errorf("the pull request body does not disclose that each file is replaced entirely:\n%s", call.proposal.Body)
	}
	for _, path := range []string{"clusters/prod/shop.yaml", "clusters/prod/shop-production.yaml"} {
		if !strings.Contains(call.proposal.Body, path) {
			t.Errorf("the body does not name %s", path)
		}
	}
}

func TestProposeSpecGeneratesAStableBranchForAnIdempotencyKey(t *testing.T) {
	p := newProposer()
	c := serve(t, proposeOptions(t, p))

	req := proposeRequest()
	req.IdempotencyKey = "one-edit"
	first, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ProposeSpec: %v", err)
	}
	second, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ProposeSpec (replay): %v", err)
	}
	if first.Msg.GetBranch() != second.Msg.GetBranch() {
		t.Errorf("a replayed key derived two branches (%q, %q); the forge's branch collision is what stops the "+
			"second proposal, and it can only fire if the name is stable",
			first.Msg.GetBranch(), second.Msg.GetBranch())
	}
	if !strings.HasPrefix(first.Msg.GetBranch(), "kelson/shop-") {
		t.Errorf("branch = %q, want it to name kelson and the project", first.Msg.GetBranch())
	}

	// Without a key the names must differ: two people editing one project in
	// the same minute must not collide on a branch neither of them named.
	req.IdempotencyKey = ""
	a, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ProposeSpec: %v", err)
	}
	b, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ProposeSpec: %v", err)
	}
	if a.Msg.GetBranch() == b.Msg.GetBranch() {
		t.Errorf("two keyless proposals derived the same branch %q", a.Msg.GetBranch())
	}
}

func TestProposeSpecDryRunTouchesNoForge(t *testing.T) {
	p := newProposer()
	c := serve(t, proposeOptions(t, p))

	req := proposeRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_RENDER
	res, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ProposeSpec: %v", err)
	}
	if !res.Msg.GetDryRun() || res.Msg.GetUrl() != "" {
		t.Errorf("a dry run answered %+v, want dry_run set and no URL", res.Msg)
	}
	if p.count() != 0 {
		t.Error("the dry run created a branch in somebody's repository")
	}
}

// --- refusals before the forge -----------------------------------------------

func TestProposeSpecRefusesMalformedRequests(t *testing.T) {
	p := newProposer()
	c := serve(t, proposeOptions(t, p))

	tests := []struct {
		name string
		req  func(*kelsonv1alpha1.ProposeSpecRequest)
		want string
	}{
		{"no project", func(r *kelsonv1alpha1.ProposeSpecRequest) { r.Project = "" }, "project"},
		{"no repository", func(r *kelsonv1alpha1.ProposeSpecRequest) { r.Repository = "" }, "owner/name"},
		{"no files", func(r *kelsonv1alpha1.ProposeSpecRequest) { r.Files = nil }, "at least one file"},
		{"no connection", func(r *kelsonv1alpha1.ProposeSpecRequest) { r.Connection = "" }, "connection"},
		{
			"an empty file",
			func(r *kelsonv1alpha1.ProposeSpecRequest) {
				r.Files = []*kelsonv1alpha1.ProposedFile{{Path: "a.yaml"}}
			},
			"deletion dressed up as an edit",
		},
		{
			"one path twice",
			func(r *kelsonv1alpha1.ProposeSpecRequest) {
				r.Files = append(r.Files, &kelsonv1alpha1.ProposedFile{
					Path: "clusters/prod/shop.yaml", Content: []byte(gitopsProject),
				})
			},
			"twice",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := proposeRequest()
			tc.req(req)
			_, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
			if err == nil {
				t.Fatal("ProposeSpec accepted a request it cannot honour")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if p.count() != 0 {
		t.Errorf("%d pull requests were opened for requests that should never have reached a forge", p.count())
	}
}

// TestProposeSpecRefusesWhatItWouldNotStore is the rule the handler's doc
// comment states: kelson does not ask a human to review a document it would
// have refused. The findings come back inline, exactly as PutSpec's do.
func TestProposeSpecRefusesWhatItWouldNotStore(t *testing.T) {
	p := newProposer()
	c := serve(t, proposeOptions(t, p))

	req := proposeRequest()
	req.Files = []*kelsonv1alpha1.ProposedFile{{
		Path:    "clusters/prod/shop.yaml",
		Content: []byte("apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata: {name: shop}\nspec:\n  components: [{name: WEB, port: 8080}]\n"),
	}}
	res, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ProposeSpec: an invalid document is an answer, not a transport failure: %v", err)
	}
	if len(res.Msg.GetErrors()) == 0 {
		t.Fatal("a component named WEB was proposed without a finding")
	}
	if res.Msg.GetUrl() != "" || p.count() != 0 {
		t.Error("a pull request was opened for a document kelson would have refused to store")
	}
}

func TestProposeSpecValidatesAnEnvironmentAgainstTheStoredProject(t *testing.T) {
	p := newProposer()
	c := serve(t, proposeOptions(t, p))

	req := proposeRequest()
	// An image pin for a component the stored Project does not declare. Nothing
	// in this document alone is wrong; it is wrong against the project it
	// resolves against, which is why the stored one is the context.
	req.Files = []*kelsonv1alpha1.ProposedFile{{
		Path: "clusters/prod/shop-production.yaml",
		Content: []byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  components:
    - {name: nonexistent, image: ghcr.io/acme/nope:1}
`),
	}}
	res, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ProposeSpec: %v", err)
	}
	if len(res.Msg.GetErrors()) == 0 {
		t.Fatalf("an environment overriding a component the project does not declare was proposed without a finding")
	}
	if p.count() != 0 {
		t.Error("a pull request was opened for a document that does not resolve")
	}
}

func TestProposeSpecRefusesADocumentForAnotherProject(t *testing.T) {
	p := newProposer()
	c := serve(t, proposeOptions(t, p))

	req := proposeRequest()
	req.Project = "warehouse"
	res, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
	if err == nil && len(res.Msg.GetErrors()) == 0 {
		t.Fatal("a Project document naming shop was proposed under project warehouse")
	}
	if p.count() != 0 {
		t.Error("the forge was asked to open a pull request for a mismatched project")
	}
}

// --- the capability gate ------------------------------------------------------

func TestProposeSpecRefusesAConnectionThatCannotPropose(t *testing.T) {
	// A provider with the mandatory method and nothing else — the `generic`
	// token connection's shape.
	c := serve(t, proposeOptions(t, fakeForge{name: "generic"}))

	_, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(proposeRequest()))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("error code = %s, want unimplemented: no request makes a bare git host grow a pull-request API",
			connect.CodeOf(err))
	}
	if !hasCode(detailCodes(err), ErrConnectionCapabilityUnsupported) {
		t.Errorf("the refusal does not carry %s: %v", ErrConnectionCapabilityUnsupported, detailCodes(err))
	}
	if !strings.Contains(err.Error(), "commit it yourself") {
		t.Errorf("the refusal does not point at the export, which is the path that always works: %v", err)
	}
}

func TestProposeSpecDryRunLearnsTheCapabilityWithoutWriting(t *testing.T) {
	c := serve(t, proposeOptions(t, fakeForge{name: "generic"}))

	req := proposeRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_RENDER
	_, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(req))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("the rehearsal did not learn that this connection cannot propose: %v", err)
	}
}

// --- the forge's four answers -------------------------------------------------

func TestProposeSpecExplainsAForgeRefusal(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "write not permitted",
			err:  forge.ErrWriteNotPermitted,
			want: []string{"contents: write", "/settings/apps", "/settings/installations", "commit the change yourself"},
		},
		{
			name: "not installed",
			err:  forge.ErrNotInstalled,
			want: []string{"acme/gitops", "/settings/installations"},
		},
		{
			name: "credential rejected",
			err:  forge.ErrAuthFailed,
			want: []string{"rejected", "Secret"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newProposer()
			p.err = tc.err
			c := serve(t, proposeOptions(t, p))

			_, err := c.spec.ProposeSpec(t.Context(), connect.NewRequest(proposeRequest()))
			if connect.CodeOf(err) != connect.CodePermissionDenied {
				t.Fatalf("error code = %s, want permission-denied: no rewrite of the request fixes any of these",
					connect.CodeOf(err))
			}
			if !hasCode(detailCodes(err), ErrConnectionWriteNotPermitted) {
				t.Errorf("the refusal does not carry %s: %v", ErrConnectionWriteNotPermitted, detailCodes(err))
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q:\n%v", want, err)
				}
			}
		})
	}
}

// --- agent policy -------------------------------------------------------------

// TestProposeOnlyDoesNotRefuseAProposal is the asymmetry [Server.guardProposal]
// argues, asserted rather than described.
//
// The shared gate table cannot see it: policyDrivers is keyed by *operation*,
// ProposeSpec shares `spec-write` with PutSpec, and TestProposeOnlyRefusesEveryMutation
// therefore drives PutSpec for that word and never reaches this route. So this
// pair of tests is what stands behind the exemption — a change that made
// propose-only refuse proposals, or made `forbid` stop refusing them, fails
// here and nowhere else.
func TestProposeOnlyDoesNotRefuseAProposal(t *testing.T) {
	p := newProposer()
	g := newGatedServer(t, proposeOptions(t, p))
	agent := g.as(g.mint(t, "proposebot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	res, err := agent.spec.ProposeSpec(t.Context(), connect.NewRequest(proposeRequest()))
	if err != nil {
		t.Fatalf("an agent was refused a proposal in a project holding a propose-only environment: %v", err)
	}
	if res.Msg.GetUrl() == "" {
		t.Error("the proposal was accepted and nothing was opened")
	}

	// And PutSpec is still refused through the same server, which is the half
	// that must not have moved: exempting the *word* would have exempted it.
	_, err = agent.spec.PutSpec(t.Context(), connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: &kelsonv1alpha1.SpecDocuments{
			Project:      []byte(gitopsProject),
			Environments: map[string][]byte{"production": []byte(gitopsEnvironment)},
		},
		Force: true,
	}))
	if !hasCode(detailCodes(err), ErrPolicyProposeOnly) {
		t.Fatalf("PutSpec is no longer refused by propose-only (%v); the exemption leaked from the RPC to the operation",
			detailCodes(err))
	}
}

func TestForbidSpecWriteRefusesAProposal(t *testing.T) {
	p := newProposer()
	opts := proposeOptions(t, p)
	store := newFakeSpecStore()
	if _, err := store.Put(t.Context(), "shop", controlstore.Documents{
		Project: []byte(gitopsProject),
		Environments: map[string][]byte{"production": []byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  policy:
    forbid: [spec-write]
`)},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the spec: %v", err)
	}
	opts.Specs = store

	g := newGatedServer(t, opts)
	agent := g.as(g.mint(t, "proposebot", controlstore.Scope{
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	_, err := agent.spec.ProposeSpec(t.Context(), connect.NewRequest(proposeRequest()))
	if !hasCode(detailCodes(err), ErrPolicyForbidden) {
		t.Fatalf("`forbid: [spec-write]` did not refuse a proposal (%v). A route that let an agent change the "+
			"documents by pull request instead would make the rule advisory", detailCodes(err))
	}
	if p.count() != 0 {
		t.Error("the forge was asked to open a pull request the policy forbids")
	}
}

func TestProposeSpecIsScopedByProject(t *testing.T) {
	p := newProposer()
	g := newGatedServer(t, proposeOptions(t, p))
	// A credential restricted to another project. ProposeSpec carries its
	// project as a field, so this is checked rather than refused wholesale the
	// way PutSpec's unknowable target is.
	agent := g.as(g.mint(t, "warehousebot", controlstore.Scope{
		Projects:   []string{"warehouse"},
		Operations: []controlstore.Operation{controlstore.OpMutate},
	}))

	_, err := agent.spec.ProposeSpec(t.Context(), connect.NewRequest(proposeRequest()))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("a credential scoped to another project proposed against shop: %v", err)
	}
	if p.count() != 0 {
		t.Error("the forge was reached by an out-of-scope credential")
	}
}

// --- the read side ------------------------------------------------------------

// TestGetSpecReportsGitOpsOwnership is detection on the wire: the store's
// answer, projected without reinterpretation.
func TestGetSpecReportsGitOpsOwnership(t *testing.T) {
	store := newFakeSpecStore()
	store.gitops = []controlstore.GitOpsOwner{
		{Document: controlstore.DocumentProject, Kustomization: "apps", Namespace: "flux-system"},
		{Document: "production", Kustomization: "apps", Namespace: "flux-system"},
	}
	if _, err := store.Put(t.Context(), "shop", controlstore.Documents{
		Project:      []byte(gitopsProject),
		Environments: map[string][]byte{"production": []byte(gitopsEnvironment)},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the spec: %v", err)
	}
	c := serve(t, Options{Specs: store})

	res, err := c.spec.GetSpec(t.Context(), connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{Project: "shop"}))
	if err != nil {
		t.Fatalf("GetSpec: %v", err)
	}
	got := res.Msg.GetSpec().GetGitops()
	if len(got) != 2 {
		t.Fatalf("gitops = %+v, want both documents", got)
	}
	if got[0].GetDocument() != controlstore.DocumentProject || got[0].GetKustomization() != "apps" ||
		got[0].GetNamespace() != "flux-system" {
		t.Errorf("gitops[0] = %+v, want the project owned by flux-system/apps", got[0])
	}

	// And on the listing, which omits the documents: a project page has to be
	// able to say what the edit page is about to refuse.
	list, err := c.spec.ListSpecs(t.Context(), connect.NewRequest(&kelsonv1alpha1.ListSpecsRequest{}))
	if err != nil {
		t.Fatalf("ListSpecs: %v", err)
	}
	if len(list.Msg.GetSpecs()) != 1 || len(list.Msg.GetSpecs()[0].GetGitops()) != 2 {
		t.Errorf("ListSpecs answered %+v, want the ownership without the documents", list.Msg.GetSpecs())
	}
	if list.Msg.GetSpecs()[0].GetDocuments() != nil {
		t.Error("ListSpecs returned documents; the schema says it does not")
	}
}

func TestGetSpecReportsNoOwnershipForAnOrdinaryProject(t *testing.T) {
	store := newFakeSpecStore()
	if _, err := store.Put(t.Context(), "shop", controlstore.Documents{
		Project:      []byte(gitopsProject),
		Environments: map[string][]byte{"production": []byte(gitopsEnvironment)},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the spec: %v", err)
	}
	c := serve(t, Options{Specs: store})

	res, err := c.spec.GetSpec(t.Context(), connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{Project: "shop"}))
	if err != nil {
		t.Fatalf("GetSpec: %v", err)
	}
	if len(res.Msg.GetSpec().GetGitops()) != 0 {
		t.Errorf("gitops = %+v, want empty: kelson's own store is the only writer here", res.Msg.GetSpec().GetGitops())
	}
}

var _ = model.AgentOpSpecWrite
