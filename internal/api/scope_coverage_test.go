package api

import (
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/dafrie/kelson/internal/controlstore"
)

// The scope table's coverage harness (issue #74), modelled on the field
// coverage harness of issue #141 in internal/model.
//
// The failure mode of an authorization table is silence: a method nobody
// mapped is a method nobody decided the rules for, and the natural
// implementation — a map lookup with a permissive default — ships it wide open.
// So the table is checked against the registered schema rather than against a
// list somebody maintains by hand, and the interceptor refuses anything the
// table does not cover.
//
// A new RPC therefore fails twice, loudly: this test names it, and the RPC
// itself is refused until a row exists.

// registeredMethods walks the generated descriptors for every kelson.v1alpha1
// service and returns the ConnectRPC procedure of every method. It reads the
// same registry the generated handlers register into, so it cannot fall behind
// the schema.
func registeredMethods(t *testing.T) []string {
	t.Helper()
	var out []string
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Package() != "kelson.v1alpha1" {
			return true
		}
		services := fd.Services()
		for i := range services.Len() {
			service := services.Get(i)
			methods := service.Methods()
			for j := range methods.Len() {
				out = append(out, "/"+string(service.FullName())+"/"+string(methods.Get(j).Name()))
			}
		}
		return true
	})
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("no kelson.v1alpha1 services are registered: the harness is not looking at the real schema")
	}
	return out
}

// TestEveryRegisteredMethodHasAScopeRow is the gate. An RPC added to the schema
// without a scope row fails here by name.
func TestEveryRegisteredMethodHasAScopeRow(t *testing.T) {
	for _, procedure := range registeredMethods(t) {
		if _, ok := rpcScopes[procedure]; !ok {
			t.Errorf(`%s is served but has no row in rpcScopes (issue #74).

Add one to internal/api/scope.go naming the operation class it belongs to (read,
mutate or admin) and how far it reaches. Until then the interceptor refuses it to
every caller, which is the fail-closed half of this table's contract — an
unmapped method must never default to allowed.`, procedure)
		}
	}
}

// TestScopeTableHasNoStaleRows is the other direction: a row for a method that
// no longer exists is a rule nothing enforces, and it would quietly rot.
func TestScopeTableHasNoStaleRows(t *testing.T) {
	registered := map[string]bool{}
	for _, procedure := range registeredMethods(t) {
		registered[procedure] = true
	}
	for procedure := range rpcScopes {
		if !registered[procedure] {
			t.Errorf("rpcScopes has a row for %s, which the schema does not serve: delete the row", procedure)
		}
	}
}

// TestEveryScopeRowIsWellFormed pins the invariants a row must satisfy for the
// interceptor to be able to act on it. Each is a mistake that would otherwise
// surface as an RPC that quietly allowed too much.
func TestEveryScopeRowIsWellFormed(t *testing.T) {
	for procedure, row := range rpcScopes {
		switch row.Operation {
		case controlstore.OpRead, controlstore.OpMutate, controlstore.OpAdmin:
		default:
			t.Errorf("%s has operation class %q, which is not one an identity can be granted or refused",
				procedure, row.Operation)
		}

		switch row.Reach {
		case reachTargeted:
			if row.Targets == nil {
				t.Errorf("%s reaches a target but has no Targets extractor, so a scoped credential could not be checked against it", procedure)
				continue
			}
			// Fail closed on a message of the wrong type. An extractor that
			// returned ok for something it could not read would authorize
			// against a zero target.
			if _, ok := row.Targets(struct{}{}); ok {
				t.Errorf("%s's Targets extractor accepted a message of the wrong type", procedure)
			}
		case reachNamespace:
			if row.Namespace == nil {
				t.Errorf("%s reaches a namespace but has no Namespace extractor", procedure)
				continue
			}
			if _, ok := row.Namespace(struct{}{}); ok {
				t.Errorf("%s's Namespace extractor accepted a message of the wrong type", procedure)
			}
		case reachClusterWide, reachEveryProject:
			if row.Targets != nil || row.Namespace != nil {
				t.Errorf("%s reaches no target but carries an extractor, which nothing reads", procedure)
			}
		default:
			t.Errorf("%s has an unknown reach %d", procedure, row.Reach)
		}

		if row.Operation == controlstore.OpAdmin && !adminService(procedure) {
			t.Errorf("%s is classed admin but belongs to none of the administrative services (%s): admin is "+
				"refused to every agent credential, so classing an ordinary RPC that way locks agents out of it "+
				"entirely. If this service really is administrative, add it to adminServices and say why in its ADR",
				procedure, strings.Join(adminServices, ", "))
		}
	}
}

// adminServices is the closed list of services whose methods may be classed
// admin. It is a list rather than a predicate over the procedure string so that
// filing a *new* service under admin is a deliberate edit here with a reason
// beside it, and not something that happens because a name happened to match.
//
//   - AgentService: an agent that could mint an agent could mint one wider than
//     itself, which would make every scope advisory (ADR-0024 §5).
//   - AuditService: an agent that could read the audit trail could read what its
//     reviewer will see, and the trail spans every project, so there is no
//     scoped version of the answer that would be safe to serve (ADR-0026 §5).
//   - InstallService: installing a platform component writes cluster-scoped
//     RBAC, CRDs and webhook configurations — wider than any (project,
//     environment) scope can bound, so there is no scoped version of the grant
//     (ADR-0021; the CLI's confirmation gate makes the same argument).
//     ListComponents stays a read; Plan and Install are the admin pair.
var adminServices = []string{"AgentService", "AuditService", "InstallService"}

func adminService(procedure string) bool {
	for _, service := range adminServices {
		if strings.Contains(procedure, service) {
			return true
		}
	}
	return false
}

// TestMutatingMethodsAreClassedAsMutations is a spot check with teeth: the
// classes are what an operator reasons about when they hand out a credential,
// so a deploy filed under `read` would make `--allow read` a lie.
func TestMutatingMethodsAreClassedAsMutations(t *testing.T) {
	mutating := []string{"PutSpec", "DeleteSpec", "Deploy", "Rollback", "Promote", "Build", "SetSecret", "DeleteSecret"}
	reading := []string{"GetSpec", "ListSpecs", "Render", "Diff", "GetProfile", "Status", "History",
		"QueryLogs", "FollowLogs", "Watch", "ListSecrets", "ListPreviews", "Explain"}

	for procedure, row := range rpcScopes {
		method := procedure[strings.LastIndex(procedure, "/")+1:]
		switch {
		case namesMethod(mutating, method) && row.Operation != controlstore.OpMutate:
			t.Errorf("%s changes state but is classed %q", procedure, row.Operation)
		case namesMethod(reading, method) && row.Operation != controlstore.OpRead:
			t.Errorf("%s only reads but is classed %q", procedure, row.Operation)
		}
	}
}

func namesMethod(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
