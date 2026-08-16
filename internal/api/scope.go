package api

import (
	"strings"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/controlstore"
)

// The RPC-to-scope table (issue #74, ADR-0024) — the #141 gate-table discipline
// applied to authorization.
//
// Every registered method of the kelson.v1alpha1 schema appears in [rpcScopes]
// exactly once, and TestEveryRegisteredMethodHasAScopeRow walks the generated
// descriptors to prove it. A method that is not in the table is refused to
// everyone, so adding an RPC and forgetting its row breaks that RPC loudly
// instead of shipping it wide open. That is the whole point: the failure mode of
// an authorization table is silence, and silence is what the gate table of #141
// was invented to make impossible.
//
// # A row says three things
//
// Which operation class the method belongs to (read, mutate, admin), how far it
// reaches (one target, the whole cluster, or every project at once), and — for
// the targeted ones — how to read the target out of the request.
//
// # Where the target is not knowable, the answer is no
//
// Some requests genuinely cannot say what they act on. `PutSpec` carries the
// documents and the project name is inside the YAML; any RPC taking an inline
// [kelsonv1alpha1.SpecRef] is the same. `ListSpecs` spans every project by
// definition, and answering it for a project-scoped credential would hand back
// other projects' specs. In all of those a *restricted* credential is refused —
// not filtered, not best-effort — because a scope that silently narrowed a
// response would be indistinguishable from one that did not apply. An
// unrestricted agent credential (no project and no environment named) still
// reaches them, since there is nothing left to check.
//
// # Log queries address a namespace, and that is a documented approximation
//
// LogService takes a namespace, not a (project, environment) pair, and the
// mapping between them belongs to the resolved spec — which this interceptor
// deliberately does not load. So a namespace is matched against the renderer's
// own convention, `<project>-<environment>`, and anything that does not match a
// pair the scope allows is refused. An environment that overrides
// `spec.namespace` therefore cannot be read by a scoped credential at all
// (fails closed), and a namespace that collides with the convention of an
// allowed pair would be allowed (the honest gap; ADR-0024 records it and the
// fix is to resolve the namespace through the spec store).

// reach says how far a method's effect extends, which decides what a scope has
// to be checked against.
type reach int

const (
	// reachTargeted acts on named (project, environment) pairs the request
	// carries.
	reachTargeted reach = iota
	// reachNamespace acts on a Kubernetes namespace the request names.
	reachNamespace
	// reachClusterWide names no project at all — a cluster profile is a
	// property of the cluster, and issuing an identity is a property of the
	// server. A scope has no project or environment to check, so only the
	// operation class applies.
	reachClusterWide
	// reachEveryProject spans every project the server holds. A restricted
	// credential cannot be served one.
	reachEveryProject
)

// scopeTarget is one (project, environment) a request acts on. An empty
// Environment means the request did not name one, which a credential restricted
// by environment cannot be given the benefit of the doubt on.
type scopeTarget struct {
	Project     string
	Environment string
}

func (t scopeTarget) String() string {
	if t.Environment == "" {
		return t.Project
	}
	return t.Project + "/" + t.Environment
}

// methodScope is one row of the table.
type methodScope struct {
	// Operation is the class the method belongs to.
	Operation controlstore.Operation
	// Reach is how far it goes.
	Reach reach
	// Targets reads the (project, environment) pairs out of a reachTargeted
	// request. The bool is false when the request shape cannot say — an inline
	// spec, or a message of the wrong type — and false is always a refusal for
	// a restricted credential.
	Targets func(msg any) ([]scopeTarget, bool)
	// Namespace reads the namespace out of a reachNamespace request.
	Namespace func(msg any) (string, bool)
}

// rpcScopes is the table. One row per registered method, no exceptions and no
// default.
var rpcScopes = map[string]methodScope{
	// SpecService. PutSpec is the interesting one: the request carries
	// documents and no project name, so the project it writes is inside the
	// YAML. A restricted credential is refused rather than served by parsing
	// the spec here — the interceptor would then have to agree with the
	// resolver about what a project name is, in a second place.
	kelsonv1alpha1connect.SpecServicePutSpecProcedure: {
		Operation: controlstore.OpMutate,
		Reach:     reachTargeted,
		Targets:   func(any) ([]scopeTarget, bool) { return nil, false },
	},
	kelsonv1alpha1connect.SpecServiceGetSpecProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.GetSpecRequest)
			if !ok {
				return nil, false
			}
			return project(req.GetProject())
		},
	},
	kelsonv1alpha1connect.SpecServiceListSpecsProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachEveryProject,
	},
	kelsonv1alpha1connect.SpecServiceDeleteSpecProcedure: {
		Operation: controlstore.OpMutate,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.DeleteSpecRequest)
			if !ok {
				return nil, false
			}
			return project(req.GetProject())
		},
	},
	// ProposeSpec is `mutate` although it writes nothing kelson owns: it
	// creates a branch and a pull request in the user's repository, through a
	// credential kelson holds. A read scope must not reach it — "this
	// credential may look at the spec" is not "this credential may push to the
	// repository the spec came from" — and mutate is the class that says so.
	//
	// Unlike PutSpec it carries its project as a field rather than inside the
	// YAML, so a restricted credential is checked rather than refused: the
	// paragraph above about unknowable targets is a statement about request
	// shapes, and this request has the field.
	kelsonv1alpha1connect.SpecServiceProposeSpecProcedure: {
		Operation: controlstore.OpMutate,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.ProposeSpecRequest)
			if !ok {
				return nil, false
			}
			return project(req.GetProject())
		},
	},

	// GetEffectiveConfig is a read of the project and the environment it names.
	// It says strictly less than GetSpec — the merged answer rather than the
	// documents it was merged from — so it takes the ordinary read row, and it
	// names both halves of the pair rather than only the project, because the
	// answer is a property of the pair.
	kelsonv1alpha1connect.SpecServiceGetEffectiveConfigProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.GetEffectiveConfigRequest)
			if !ok || req.GetProject() == "" {
				return nil, false
			}
			return []scopeTarget{{Project: req.GetProject(), Environment: req.GetEnvironment()}}, true
		},
	},

	// RenderService. Rendering touches no cluster, but it reads a stored spec
	// and returns it as manifests, so it is a read of the project it names.
	kelsonv1alpha1connect.RenderServiceRenderProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.RenderRequest)
			if !ok {
				return nil, false
			}
			return specRef(req.GetSpec(), req.GetEnvironment())
		},
	},
	kelsonv1alpha1connect.RenderServiceDiffProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.DiffRequest)
			if !ok {
				return nil, false
			}
			return specRef(req.GetSpec(), req.GetEnvironment())
		},
	},

	// ProfileService reports what the cluster can do. There is no project in
	// the question and none in the answer.
	kelsonv1alpha1connect.ProfileServiceGetProfileProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachClusterWide,
	},

	// DeployService: the acceptance criterion of #74 lives here. A credential
	// scoped to `development` reaching Deploy with `environment: production` is
	// refused by the Targets extractor below and the scope check that consumes
	// it, server-side, before the handler runs.
	kelsonv1alpha1connect.DeployServiceDeployProcedure: {
		Operation: controlstore.OpMutate,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.DeployRequest)
			if !ok {
				return nil, false
			}
			return specRef(req.GetSpec(), req.GetEnvironment())
		},
	},
	kelsonv1alpha1connect.DeployServiceStatusProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.StatusRequest)
			if !ok {
				return nil, false
			}
			return specRef(req.GetSpec(), req.GetEnvironment())
		},
	},
	kelsonv1alpha1connect.DeployServiceRollbackProcedure: {
		Operation: controlstore.OpMutate,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.RollbackRequest)
			if !ok {
				return nil, false
			}
			return specRef(req.GetSpec(), req.GetEnvironment())
		},
	},
	kelsonv1alpha1connect.DeployServiceHistoryProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.HistoryRequest)
			if !ok {
				return nil, false
			}
			return specRef(req.GetSpec(), req.GetEnvironment())
		},
	},
	// Promote touches two environments and both are checked: promoting *from*
	// production is a read of production, and a credential that may not see it
	// may not launder it into an environment it does own.
	kelsonv1alpha1connect.DeployServicePromoteProcedure: {
		Operation: controlstore.OpMutate,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.PromoteRequest)
			if !ok || req.GetProject() == "" {
				return nil, false
			}
			return []scopeTarget{
				{Project: req.GetProject(), Environment: req.GetFromEnvironment()},
				{Project: req.GetProject(), Environment: req.GetToEnvironment()},
			}, true
		},
	},

	// LogService addresses a namespace; see this file's header for what that
	// costs a scoped credential.
	kelsonv1alpha1connect.LogServiceQueryLogsProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachNamespace,
		Namespace: func(msg any) (string, bool) {
			req, ok := msg.(*kelsonv1alpha1.QueryLogsRequest)
			if !ok {
				return "", false
			}
			return namespaceOf(req.GetSelector())
		},
	},
	kelsonv1alpha1connect.LogServiceFollowLogsProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachNamespace,
		Namespace: func(msg any) (string, bool) {
			req, ok := msg.(*kelsonv1alpha1.FollowLogsRequest)
			if !ok {
				return "", false
			}
			return namespaceOf(req.GetSelector())
		},
	},

	// EventService. An empty scope list means "every project the server has",
	// which a restricted credential cannot be served: the stream would carry
	// events from projects it may not see.
	kelsonv1alpha1connect.EventServiceWatchProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.WatchRequest)
			if !ok || len(req.GetScopes()) == 0 {
				return nil, false
			}
			out := make([]scopeTarget, 0, len(req.GetScopes()))
			for _, s := range req.GetScopes() {
				if s.GetProject() == "" {
					return nil, false
				}
				out = append(out, scopeTarget{Project: s.GetProject(), Environment: s.GetEnvironment()})
			}
			return out, true
		},
	},

	// BuildService pushes an image and is therefore a mutation, even though it
	// deploys nothing: it spends the server's push credential.
	kelsonv1alpha1connect.BuildServiceBuildProcedure: {
		Operation: controlstore.OpMutate,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.BuildRequest)
			if !ok {
				return nil, false
			}
			return specRef(req.GetSpec(), req.GetEnvironment())
		},
	},

	// ReportBuild is the CI hand-off (ADR-0034 decision 3), and decision 6 makes
	// the reporter a principal: it is called by an agent identity minted for the
	// pipeline, so it must be reachable by a scoped credential rather than
	// administrative. It is a mutation — it triggers a server-side render and
	// publish — and it names its project directly, so a credential scoped to one
	// project cannot report a build for another. Like every project-only target,
	// a credential restricted by *environment* is refused, because the report
	// names no environment and the server decides which ones it feeds.
	kelsonv1alpha1connect.BuildServiceReportBuildProcedure: {
		Operation: controlstore.OpMutate,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.ReportBuildRequest)
			if !ok {
				return nil, false
			}
			return project(req.GetProject())
		},
	},

	// GitConnectionService (ADR-0033). A connection is instance-wide
	// configuration in kelson-system: there is no project in the question and
	// none in the answer, so every row here is cluster-wide and only the
	// operation class applies.
	//
	// The reads are ordinary reads — references and provider-reported facts,
	// never material (ADR-0009, ADR-0033 decision 1) — and ADR-0033 decision 6
	// grants *use* by visibility, so any credential that may read at all may
	// see which forges this instance can pull from. TestConnection is filed with
	// them: it spends the credential on an outbound call but changes no state,
	// and its answer is the same reachable/account/count triple the status
	// subresource already carries.
	//
	// The two listings are filed with them for the same reason, and the reason
	// is worth stating because they look heavier than they are. Browsing spends
	// the credential exactly as TestConnection does and writes nothing at all —
	// not even the status subresource a probe records — so it is the *lighter*
	// of the two operations, and what it returns is what the forge would show
	// the credential's owner. Classifying either as a mutation would put a
	// read-only picker behind an admin credential, which is the failure mode
	// ADR-0024 §3 warns about from the other direction.
	kelsonv1alpha1connect.GitConnectionServiceListConnectionsProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachClusterWide,
	},
	kelsonv1alpha1connect.GitConnectionServiceGetConnectionProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachClusterWide,
	},
	kelsonv1alpha1connect.GitConnectionServiceTestConnectionProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachClusterWide,
	},
	kelsonv1alpha1connect.GitConnectionServiceListConnectionRepositoriesProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachClusterWide,
	},
	kelsonv1alpha1connect.GitConnectionServiceListConnectionBranchesProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachClusterWide,
	},
	// Creating and deleting are administrative, which is ADR-0033 decision 6
	// read literally: use is granted by visibility, but *mutation belongs to the
	// owner and to instance admins*. Until #231 there is no owner to be, so the
	// only honest enforcement of that sentence is the admin class — and the
	// alternative is worse than theoretical: a connection is not scoped to a
	// project, so OpMutate plus cluster-wide reach would let a credential
	// minted for one project delete the connection every other project builds
	// through, and create one pointing anywhere. That is wider than any (project,
	// environment) scope can bound, which is InstallService's argument for the
	// same class, unchanged (ADR-0021, ADR-0024 §3).
	kelsonv1alpha1connect.GitConnectionServiceCreateConnectionProcedure: {
		Operation: controlstore.OpAdmin,
		Reach:     reachClusterWide,
	},
	kelsonv1alpha1connect.GitConnectionServiceDeleteConnectionProcedure: {
		Operation: controlstore.OpAdmin,
		Reach:     reachClusterWide,
	},

	// SecretService. Listing is a read of names and keys — no RPC in the
	// schema can return a value (ADR-0009) — and writing is a mutation of the
	// environment's namespace.
	kelsonv1alpha1connect.SecretServiceSetSecretProcedure: {
		Operation: controlstore.OpMutate,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.SetSecretRequest)
			if !ok {
				return nil, false
			}
			return secretTargetScope(req.GetTarget())
		},
	},
	kelsonv1alpha1connect.SecretServiceListSecretsProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.ListSecretsRequest)
			if !ok {
				return nil, false
			}
			return secretTargetScope(req.GetTarget())
		},
	},
	kelsonv1alpha1connect.SecretServiceDeleteSecretProcedure: {
		Operation: controlstore.OpMutate,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.DeleteSecretRequest)
			if !ok {
				return nil, false
			}
			return secretTargetScope(req.GetTarget())
		},
	},

	// ExplainService answers "why is this environment in the state it is in"
	// (issue #77). It reads the same live state Status does and renders the same
	// spec, so it is a read of the project and environment it names.
	kelsonv1alpha1connect.ExplainServiceExplainProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.ExplainRequest)
			if !ok {
				return nil, false
			}
			return specRef(req.GetSpec(), req.GetEnvironment())
		},
	},

	// PreviewService reads which pull-request previews are running.
	kelsonv1alpha1connect.PreviewServiceListPreviewsProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachTargeted,
		Targets: func(msg any) ([]scopeTarget, bool) {
			req, ok := msg.(*kelsonv1alpha1.ListPreviewsRequest)
			if !ok {
				return nil, false
			}
			return specRef(req.GetSpec(), req.GetEnvironment())
		},
	},

	// AgentService is administrative in full. controlstore.OpAdmin cannot be
	// granted to an agent identity at issuance, so these three rows are what
	// make "an agent may not mint an agent" a server-side fact rather than a
	// convention (ADR-0024 §5).
	kelsonv1alpha1connect.AgentServiceCreateAgentProcedure: {
		Operation: controlstore.OpAdmin,
		Reach:     reachClusterWide,
	},
	kelsonv1alpha1connect.AgentServiceListAgentsProcedure: {
		Operation: controlstore.OpAdmin,
		Reach:     reachClusterWide,
	},
	kelsonv1alpha1connect.AgentServiceRevokeAgentProcedure: {
		Operation: controlstore.OpAdmin,
		Reach:     reachClusterWide,
	},

	// InstallService (issue #60, ADR-0021). ListComponents is a read of the
	// cluster's platform layer, cluster-wide like GetProfile. PlanInstall and
	// Install are administrative: an install writes cluster-scoped RBAC, CRDs
	// and webhook configurations, and ADR-0024 §3's argument for AgentService
	// applies unchanged — nothing that wide belongs to an agent credential.
	// PlanInstall applies nothing, but it is the confirmation step's evidence,
	// and a class of caller that may not perform the install has no business
	// rehearsing it either.
	kelsonv1alpha1connect.InstallServiceListComponentsProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachClusterWide,
	},
	kelsonv1alpha1connect.InstallServicePlanInstallProcedure: {
		Operation: controlstore.OpAdmin,
		Reach:     reachClusterWide,
	},
	kelsonv1alpha1connect.InstallServiceInstallProcedure: {
		Operation: controlstore.OpAdmin,
		Reach:     reachClusterWide,
	},

	// NodeService reports the cluster's machines. No project in the question,
	// none in the answer — the same shape as GetProfile.
	kelsonv1alpha1connect.NodeServiceGetNodesProcedure: {
		Operation: controlstore.OpRead,
		Reach:     reachClusterWide,
	},

	// AuditService is administrative for the same reason AgentService is, with
	// a different argument (issue #78, ADR-0026 §5). An agent that could read
	// the audit trail could read what the reviewer will see and plan around it,
	// and it could read every other principal's actions besides its own — the
	// trail spans every project, so there is no scoped version of the answer
	// that would be safe to serve. The default is therefore no, and the way an
	// agent legitimately explains itself is the reason field it supplies on the
	// way in, not a read on the way out.
	kelsonv1alpha1connect.AuditServiceQueryAuditProcedure: {
		Operation: controlstore.OpAdmin,
		Reach:     reachClusterWide,
	},
}

// specRef turns a stored-spec reference into a target. An inline spec has no
// project name in the request, which is the "not knowable" answer.
func specRef(ref *kelsonv1alpha1.SpecRef, environment string) ([]scopeTarget, bool) {
	name := ref.GetProject()
	if name == "" {
		return nil, false
	}
	return []scopeTarget{{Project: name, Environment: environment}}, true
}

// project is the target of a request that names a project and no environment.
func project(name string) ([]scopeTarget, bool) {
	if name == "" {
		return nil, false
	}
	return []scopeTarget{{Project: name}}, true
}

func secretTargetScope(t *kelsonv1alpha1.SecretTarget) ([]scopeTarget, bool) {
	if t.GetProject() == "" {
		return nil, false
	}
	return []scopeTarget{{Project: t.GetProject(), Environment: t.GetEnvironment()}}, true
}

func namespaceOf(selector *kelsonv1alpha1.LogSelector) (string, bool) {
	namespace := strings.TrimSpace(selector.GetNamespace())
	if namespace == "" {
		return "", false
	}
	return namespace, true
}

// namespaceTargets reads a namespace as the renderer writes one:
// `<project>-<environment>`. Both halves may contain '-', so every split is
// offered and the scope check accepts the request if any of them is allowed.
// A namespace with no '-' yields nothing, which is a refusal.
func namespaceTargets(namespace string) []scopeTarget {
	var out []scopeTarget
	for i := 1; i < len(namespace)-1; i++ {
		if namespace[i] != '-' {
			continue
		}
		out = append(out, scopeTarget{Project: namespace[:i], Environment: namespace[i+1:]})
	}
	return out
}

// scopeFor returns the row governing a procedure. The bool is false for a
// method with no row, which the interceptor turns into a refusal for every
// caller — the fail-closed half of the table's contract.
func scopeFor(procedure string) (methodScope, bool) {
	row, ok := rpcScopes[procedure]
	return row, ok
}

// describeScope renders a scope for an error message, in the vocabulary the
// issuance flags use.
func describeScope(s controlstore.Scope) string {
	parts := make([]string, 0, 3)
	parts = append(parts, "projects="+listOrAny(s.Projects))
	parts = append(parts, "environments="+listOrAny(s.Environments))
	ops := make([]string, 0, len(s.Operations))
	for _, op := range s.Operations {
		ops = append(ops, string(op))
	}
	parts = append(parts, "operations="+listOrAny(ops))
	return strings.Join(parts, " ")
}

func listOrAny(values []string) string {
	if len(values) == 0 {
		return "(any)"
	}
	return strings.Join(values, ",")
}
