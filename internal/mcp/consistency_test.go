package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/reflect/protoreflect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// The consistency check of ADR-0008: the Protobuf schema is not the design of
// this surface, but it is the proof that the surface maps onto it. Every tool
// declares the RPCs it composes, and every declared pair must name a real
// service and method in the generated descriptors.
//
// It is the cheap half of capability parity. The expensive half — that a tool
// calls nothing it did not declare — is asserted at runtime by
// [harness.assertComposed] on every tool call in this package's tests.

// schemaFiles is every file of the v1alpha1 schema. A service added to the
// schema and not listed here would make the check weaker rather than fail it,
// so the list is the whole set rather than the four services the surface uses.
func schemaFiles() []protoreflect.FileDescriptor {
	return []protoreflect.FileDescriptor{
		kelsonv1alpha1.File_kelson_v1alpha1_common_proto,
		kelsonv1alpha1.File_kelson_v1alpha1_spec_proto,
		kelsonv1alpha1.File_kelson_v1alpha1_render_proto,
		kelsonv1alpha1.File_kelson_v1alpha1_profile_proto,
		kelsonv1alpha1.File_kelson_v1alpha1_deploy_proto,
		kelsonv1alpha1.File_kelson_v1alpha1_logs_proto,
		kelsonv1alpha1.File_kelson_v1alpha1_events_proto,
	}
}

func schemaOperations(t *testing.T) map[string]protoreflect.MethodDescriptor {
	t.Helper()
	operations := map[string]protoreflect.MethodDescriptor{}
	for _, file := range schemaFiles() {
		services := file.Services()
		for i := range services.Len() {
			service := services.Get(i)
			methods := service.Methods()
			for j := range methods.Len() {
				method := methods.Get(j)
				operations[string(service.FullName())+"."+string(method.Name())] = method
			}
		}
	}
	if len(operations) == 0 {
		t.Fatal("no service descriptors were found; the generated code is not what this check reads")
	}
	return operations
}

// TestEveryToolComposesRealAPIOperations is issue #73's acceptance criterion:
// no tool exposes a capability the API does not have, asserted against the
// schema rather than by reading the code.
func TestEveryToolComposesRealAPIOperations(t *testing.T) {
	operations := schemaOperations(t)

	for _, tool := range surface(&clients{}) {
		if len(tool.rpcs) == 0 {
			t.Errorf("tool %q declares no RPCs: a tool that composes nothing is a capability of its own", tool.def.Name)
		}
		for _, r := range tool.rpcs {
			if _, ok := operations[r.String()]; !ok {
				t.Errorf("tool %q declares %s, which is not a service and method of the v1alpha1 schema", tool.def.Name, r)
			}
		}
	}
}

// TestNoToolComposesAnUnboundedStream: FollowLogs is a real API operation and
// deliberately has no tool. The API streams; tools return windows (ADR-0008
// §2), and the check that stops that rule eroding is this one.
func TestNoToolComposesAnUnboundedStream(t *testing.T) {
	for _, tool := range surface(&clients{}) {
		for _, r := range tool.rpcs {
			if r.method == "FollowLogs" {
				t.Errorf("tool %q composes %s: an unbounded stream cannot be a tool result", tool.def.Name, r)
			}
		}
	}
}

// TestSurfaceIsSmallAndDescribed: the tool count is a design decision, and the
// descriptions are documentation for a model — every one states whether it
// mutates, so the answer is in the prose and not only in the annotations.
func TestSurfaceIsSmallAndDescribed(t *testing.T) {
	tools := surface(&clients{})
	if len(tools) != 7 {
		t.Errorf("the surface has %d tools, want 7: adding one is a deliberate design change (ADR-0008), not a detail", len(tools))
	}

	seen := map[string]bool{}
	for _, tool := range tools {
		name := tool.def.Name
		if seen[name] {
			t.Errorf("tool %q is registered twice", name)
		}
		seen[name] = true

		if len(tool.def.Description) < 100 {
			t.Errorf("tool %q has no model-facing description", name)
		}
		if tool.def.Annotations == nil {
			t.Fatalf("tool %q carries no annotations", name)
		}
		readOnly := tool.def.Annotations.ReadOnlyHint
		saysReadOnly := strings.Contains(tool.def.Description, "READ-ONLY")
		saysMutates := strings.Contains(tool.def.Description, "MUTATES")
		if readOnly != saysReadOnly || readOnly == saysMutates {
			t.Errorf("tool %q: readOnlyHint=%t disagrees with its description", name, readOnly)
		}
	}
}

// TestMutatingToolsExposeADryRun: every mutation an agent can make must be
// previewable, universally rather than selectively (epic #8, ADR-0008 §3). The
// schemas are read as the client sees them, after the SDK inferred them.
func TestMutatingToolsExposeADryRun(t *testing.T) {
	h := start(t, &fakeServer{})
	listed, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	declared := map[string]*mcpsdk.Tool{}
	for _, tool := range surface(&clients{}) {
		declared[tool.def.Name] = tool.def
	}
	if len(listed.Tools) != len(declared) {
		t.Errorf("the server offers %d tools, the surface table has %d", len(listed.Tools), len(declared))
	}

	for _, tool := range listed.Tools {
		if _, ok := declared[tool.Name]; !ok {
			t.Errorf("the server offers %q, which is not in the surface table", tool.Name)
			continue
		}
		if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
			continue
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling %s's input schema: %v", tool.Name, err)
		}
		if !strings.Contains(string(schema), "dry_run") && !strings.Contains(string(schema), "execute") {
			t.Errorf("mutating tool %q exposes no dry-run parameter: %s", tool.Name, schema)
		}
	}
}
