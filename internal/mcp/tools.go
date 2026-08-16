package mcp

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
)

// rpc names one operation of the v1alpha1 schema.
//
// Every tool declares the RPCs it composes, and mcp_consistency_test.go asserts
// each pair against the generated service descriptors. That is ADR-0008's use
// of the Protobuf schema: a check that the surface maps onto real API
// operations, never the design of the surface. A tool that grew a capability of
// its own — reading a cluster directly, classifying health itself — would have
// nothing true to declare here, which is the point.
type rpc struct {
	service string
	method  string
}

func (r rpc) String() string { return r.service + "." + r.method }

var (
	rpcListSpecs = rpc{kelsonv1alpha1connect.SpecServiceName, "ListSpecs"}
	rpcGetSpec   = rpc{kelsonv1alpha1connect.SpecServiceName, "GetSpec"}
	rpcPutSpec   = rpc{kelsonv1alpha1connect.SpecServiceName, "PutSpec"}
	rpcStatus    = rpc{kelsonv1alpha1connect.DeployServiceName, "Status"}
	rpcHistory   = rpc{kelsonv1alpha1connect.DeployServiceName, "History"}
	rpcDeploy    = rpc{kelsonv1alpha1connect.DeployServiceName, "Deploy"}
	rpcRollback  = rpc{kelsonv1alpha1connect.DeployServiceName, "Rollback"}
	rpcPromote   = rpc{kelsonv1alpha1connect.DeployServiceName, "Promote"}
	rpcQueryLogs = rpc{kelsonv1alpha1connect.LogServiceName, "QueryLogs"}
	rpcWatch     = rpc{kelsonv1alpha1connect.EventServiceName, "Watch"}

	rpcGetProfile = rpc{kelsonv1alpha1connect.ProfileServiceName, "GetProfile"}

	// rpcExplain is the causal capability of issue #77. diagnose_component
	// composes it rather than deriving causes here: ADR-0023 put the
	// structured-cause machinery in the API layer precisely so this surface,
	// the CLI and the UI read one answer.
	rpcExplain = rpc{kelsonv1alpha1connect.ExplainServiceName, "Explain"}

	rpcSetSecret   = rpc{kelsonv1alpha1connect.SecretServiceName, "SetSecret"}
	rpcUnsetSecret = rpc{kelsonv1alpha1connect.SecretServiceName, "UnsetSecret"}
	rpcListSecrets = rpc{kelsonv1alpha1connect.SecretServiceName, "ListSecrets"}
)

// tool is one entry of the surface: the MCP definition a client sees, the RPCs
// it composes, and the binding of its typed handler.
type tool struct {
	def  *mcpsdk.Tool
	rpcs []rpc
	add  func(*mcpsdk.Server)
}

// surface is the whole agent-facing surface, in registration order.
//
// Nine tools, and the number is a design decision rather than a stopping point:
// every tool added costs selection accuracy for the ones already here
// (ADR-0008). Read-only tools come first, mutating ones after, and each says
// which it is in its own description as well as in its annotations — a model
// reads the prose.
//
// # Why #116 added one tool and not two
//
// Writing a secret is a task an agent has — "this app needs a Stripe key" — so
// set_secret is a tool. *Listing* secrets is not a task: it is something an
// agent needs to know while doing another one, which is precisely when
// ADR-0008's task-shape rule says to extend an existing tool rather than add a
// read tool beside it. It went into diagnose_component, because the question
// it answers is a diagnosis: a workload in CreateContainerConfigError is the
// failure a missing Secret or a missing key produces, and ADR-0018 records that
// kelson has nothing else that correlates a reference with the Secret it names.
//
// #269's key removal took the same rule the same way: it is `remove_keys` on
// set_secret rather than a tenth tool, because removing a key is what an agent
// does about a key it wrote under the wrong name — the second half of a task,
// not a task (secret.go's setSecretTool says the rest).
func surface(c *clients) []tool {
	return []tool{
		listComponentsTool(c),
		diagnoseComponentTool(c),
		logsWindowTool(c),
		deployTool(c),
		rollbackTool(c),
		promoteTool(c),
		putSpecTool(c),
		setSecretTool(c),
		waitForOutcomeTool(c),
	}
}

// text is the result shape of every tool: one block of prose. The SDK fills in
// the rest of the CallToolResult, and a handler that returns an error instead
// gets it packed into an error result the model can read and act on.
func text(r *report) (*mcpsdk.CallToolResult, any, error) {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: r.String()}},
	}, nil, nil
}

// readOnlyTool builds the definition of a tool that changes nothing.
func readOnlyTool(name, title, description string) *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcpsdk.ToolAnnotations{
			Title:        title,
			ReadOnlyHint: true,
		},
	}
}

// mutatingTool builds the definition of a tool that can change the cluster or
// the spec store. destructive marks the ones that can take a running workload
// away from what it is currently serving.
func mutatingTool(name, title, description string, destructive bool) *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        name,
		Description: description,
		Annotations: &mcpsdk.ToolAnnotations{
			Title:           title,
			ReadOnlyHint:    false,
			DestructiveHint: &destructive,
			// Every mutating tool carries an idempotency key, minted here when
			// the caller does not supply one, so a retry is the same operation
			// rather than a second one (issue #71).
			//
			// Every mutating tool also takes an optional `reason`, which rides
			// to the server as a header and lands in the action's audit record
			// (issue #78, ADR-0026 §4). It is how an agent states *why* it did
			// something, which is the half of "what did it actually do?" that
			// no amount of server-side observation can reconstruct.
			IdempotentHint: true,
		},
	}
}
