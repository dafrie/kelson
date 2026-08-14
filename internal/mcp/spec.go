package mcp

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

const putSpecDescription = `Validate a kelson spec, and store it when you mean to.

MUTATES THE SERVER'S SPEC STORE when dry_run=false. The default is dry_run=true, which validates and renders every environment and stores nothing.

The spec is two document kinds: one Project document (components, images, shared environment variables) and one Environment document per environment (namespace, routing, delivery mode). Pass them verbatim as YAML — the server stores the documents you wrote, byte for byte.

A Project declares its deployables and its managed databases in one spec.components list. The kind is derived from the shape — port makes a service, schedule makes a cron job, neither makes a worker — or written explicitly as one of service, worker, cron, agent, postgres, valkey; postgres and valkey must be written, because there is nothing to derive them from. A field belonging to another kind (preset on a worker, port on a database, tools on anything but an agent) is a validation error, not a field that is ignored.

Validation errors come back with their machine-readable code (for example schema/unknown-field, image/unresolved, secret/literal), the field path, the line in your document, and the fix stated as an action. Branch on the code, not on the message text.

Preconditions for an update: pass the version returned by the last read or write of this project. A stale or missing version fails with store/version-conflict rather than overwriting a concurrent change — read the project again, reapply your edit, and retry with the new version.

Storing a spec deploys nothing. Call deploy afterwards.`

type putSpecInput struct {
	Project              string            `json:"project,omitempty" jsonschema:"the project this spec is for; checked against the Project document's metadata.name when set"`
	ProjectDocument      string            `json:"project_document" jsonschema:"the Project YAML document, verbatim"`
	EnvironmentDocuments map[string]string `json:"environment_documents,omitempty" jsonschema:"environment name to its Environment YAML document, verbatim"`
	DryRun               *bool             `json:"dry_run,omitempty" jsonschema:"true (default) validates and renders without storing; false stores the spec"`
	Version              string            `json:"version,omitempty" jsonschema:"the version from the last read or write; required to update an existing project"`
	Reason               string            `json:"reason,omitempty" jsonschema:"why you are changing the spec, in one sentence; recorded in the audit trail beside the action"`
}

func putSpecTool(c *clients) tool {
	def := mutatingTool("put_spec", "Validate or store a spec", putSpecDescription, false)
	return tool{
		def:  def,
		rpcs: []rpc{rpcPutSpec},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in putSpecInput) (*mcpsdk.CallToolResult, any, error) {
				return c.putSpec(ctx, in)
			})
		},
	}
}

// putSpec validates or stores the authored documents.
//
// An invalid spec is an answer here, not a failure: PutSpec reports validation
// findings in its response with the RPC succeeding, and this tool renders them
// verbatim. Only a failure of the store itself — a version conflict, a payload
// over budget — arrives as an error, carrying the same taxonomy in its details.
func (c *clients) putSpec(ctx context.Context, in putSpecInput) (*mcpsdk.CallToolResult, any, error) {
	if in.ProjectDocument == "" {
		return nil, nil, fmt.Errorf("put_spec needs project_document: the Project YAML document that names the project and its components")
	}
	dryRun := kelsonv1alpha1.DryRun_DRY_RUN_RENDER
	stores := in.DryRun != nil && !*in.DryRun
	if stores {
		dryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	}

	documents := &kelsonv1alpha1.SpecDocuments{
		Project:      []byte(in.ProjectDocument),
		Environments: map[string][]byte{},
	}
	for name, body := range in.EnvironmentDocuments {
		documents.Environments[name] = []byte(body)
	}

	res, err := c.spec.PutSpec(ctx, reasoned(connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents:      documents,
		Version:        in.Version,
		DryRun:         dryRun,
		IdempotencyKey: newIdempotencyKey(),
	}), in.Reason))
	if err != nil {
		return nil, nil, c.fail(rpcPutSpec, err)
	}

	var r report
	spec := res.Msg.GetSpec()
	errs := res.Msg.GetErrors()
	project := spec.GetProject()
	if project == "" {
		project = in.Project
	}

	switch {
	case len(errs) > 0:
		r.addf("put_spec %s: REJECTED with %d error(s). Nothing was stored.", orDash(project), len(errs))
	case stores:
		r.addf("put_spec %s: STORED.", orDash(project))
	default:
		r.addf("put_spec %s: VALID. Nothing was stored — set dry_run=false to store it.", orDash(project))
	}

	if len(errs) > 0 {
		shown, dropped := limit(errs, maxSpecErrors)
		r.section("ERRORS")
		for _, e := range shown {
			r.wireError("  ", e)
		}
		r.truncated(dropped, "errors")
		return text(&r)
	}

	r.section("SPEC")
	if version := spec.GetVersion(); version != "" {
		r.addf("  version       %s (pass it back on the next put_spec for this project)", version)
	}
	if environments := spec.GetEnvironments(); len(environments) > 0 {
		shown, dropped := limit(environments, maxEnvironments)
		r.addf("  environments  %s", joinCapped(shown, dropped))
	}
	if in.Project != "" && spec.GetProject() != "" && in.Project != spec.GetProject() {
		r.addf("  note: the project parameter was %q but the document declares %q; the document wins.", in.Project, spec.GetProject())
	}
	if stores {
		r.addf("  storing a spec deploys nothing: call deploy to apply it.")
	}
	return text(&r)
}
