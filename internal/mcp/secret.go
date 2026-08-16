package mcp

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

const setSecretDescription = `Write the credential a spec's secret reference points at, into the environment's namespace — or remove a key from it.

MUTATES THE CLUSTER when execute=true. The default is execute=false, which validates the request and writes nothing.

A kelson spec never contains a credential. An environment variable that needs one is written as a reference — env: { DATABASE_URL: { secret: <name>, key: <key> } } — and this tool writes the Secret that reference names. The two go together: a reference with no Secret behind it renders and deploys cleanly and then fails at pod start with CreateContainerConfigError.

Keys not named in this call are preserved, so rotating one credential leaves the others alone. Keys use Kubernetes' Secret alphabet: letters, digits, '-', '_' and '.'.

To remove a key — one written under the wrong name, or a credential that should no longer exist — pass remove_keys instead of values. Removal is the only way to take a key away, because a write never prunes what it does not name. A key that is not in the Secret is refused rather than ignored, so a mistyped removal fails loudly instead of reporting success. Removing the last key leaves an empty Secret; kelson has no tool that deletes a Secret, which stays a human's call.

One call does one of the two: pass values or remove_keys, never both.

Values travel one way. Nothing in kelson's API returns a secret value, this tool included: the answer reports the Secret's name, its namespace and its key names. Use diagnose_component to see what an environment already holds.

The Secret must be in an environment that has been deployed at least once, because that is what creates the namespace. kelson writes only Secrets it labels as its own and will not take over one created by kubectl or by an operator; that is reported as secret/not-managed rather than done. Under an environment whose secrets.backend is sops or externalSecrets the value belongs somewhere kelson does not write, and the call is refused naming the backend.

Cost: one server call.`

type setSecretInput struct {
	Project     string            `json:"project" jsonschema:"the stored project name, as reported by list_components"`
	Environment string            `json:"environment" jsonschema:"the environment whose namespace the Secret is written into, e.g. production"`
	Name        string            `json:"name" jsonschema:"the Secret's name — the same string a {secret: <name>, key: <key>} reference carries; a DNS-1123 label such as checkout-db"`
	Values      map[string]string `json:"values,omitempty" jsonschema:"the keys to write, as key to value. Keys already in the Secret and not named here are preserved"`
	RemoveKeys  []string          `json:"remove_keys,omitempty" jsonschema:"key names to remove from the Secret instead of writing values. Every one must already be there or the call is refused; removing the last key leaves the Secret empty rather than deleting it"`
	Execute     bool              `json:"execute,omitempty" jsonschema:"false (default) validates and writes nothing; true writes the Secret"`
	Namespace   string            `json:"namespace,omitempty" jsonschema:"override the derived <project>-<environment> namespace; needed only when the Environment sets spec.namespace"`
	// Never put the value here. The reason is recorded in the audit trail; the
	// values are not, and a reason that quoted one would be the leak this whole
	// surface is built to prevent (issue #117).
	Reason string `json:"reason,omitempty" jsonschema:"why you are writing this secret, in one sentence; recorded in the audit trail beside the action. Never include the value itself"`
}

// set_secret carries the removal too, rather than a tool beside it (issue
// #269).
//
// tools.go's count is a design decision — every tool costs selection accuracy
// for the ones already there (ADR-0008) — and the rule it states is the one
// that decided this: a task an agent has is a tool, and something an agent
// needs while doing another task extends an existing one. "Remove this key" is
// almost always the second half of a write that went in under the wrong name —
// a mistake made in this tool — so the correction belongs in the same call
// shape, and costs no selection accuracy at all.
//
// It stops short of DeleteSecret, which this surface still does not offer
// (secret_test.go's TestNoToolDeletesASecret). That is not an inconsistency:
// UnsetSecret cannot remove an object — the last key leaves an empty Secret —
// so what an agent gains here is bounded by the same decision the core made,
// and taking a Secret away stays a human's call with a confirmation in front
// of it.
func setSecretTool(c *clients) tool {
	// Destructive: overwriting a key changes what a running workload's next pod
	// reads, and there is no undo — kelson does not keep the previous value
	// (ADR-0009: kelson is not the store). That is the same class of change
	// `deploy` warns about, so it carries the same hint. Removing a key is the
	// same class again, for the same reason.
	def := mutatingTool("set_secret", "Set a secret", setSecretDescription, true)
	return tool{
		def:  def,
		rpcs: []rpc{rpcSetSecret, rpcUnsetSecret},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in setSecretInput) (*mcpsdk.CallToolResult, any, error) {
				return c.setSecret(ctx, in)
			})
		},
	}
}

// setSecret composes the SetSecret RPC.
//
// The dry run is the default and it is the RENDER rung, not SERVER: a server
// dry run would send the values to the cluster to be validated and discarded,
// which is a strange thing to do by default with a credential. An agent that
// wants the cluster's opinion sets execute=true and gets the real thing.
func (c *clients) setSecret(ctx context.Context, in setSecretInput) (*mcpsdk.CallToolResult, any, error) {
	dryRun := kelsonv1alpha1.DryRun_DRY_RUN_RENDER
	if in.Execute {
		dryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	}
	if len(in.RemoveKeys) > 0 {
		// Writing and removing in one call would be two RPCs behind one answer,
		// and a failure between them would leave an agent unable to tell which
		// half happened. One call, one intent — and the audit record then says
		// which of the two it was rather than "both".
		if len(in.Values) > 0 {
			return nil, nil, fmt.Errorf("set_secret was given both values and remove_keys: a call either writes " +
				"keys or removes them. Send the write and the removal as two calls, so that a refusal names " +
				"which one it refused")
		}
		return c.unsetSecret(ctx, in, dryRun)
	}

	res, err := c.secrets.SetSecret(ctx, reasoned(connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
		Target: &kelsonv1alpha1.SecretTarget{
			Project:     in.Project,
			Environment: in.Environment,
			Namespace:   in.Namespace,
		},
		Name:   in.Name,
		Values: in.Values,
		DryRun: dryRun,
	}), in.Reason))
	if err != nil {
		// c.fail relays the server's structured error verbatim. The server
		// scrubs every free-text field through the process-wide known-value
		// registry before it sends one (#117), so a value cannot arrive here to
		// be re-rendered into a tool answer.
		return nil, nil, c.fail(rpcSetSecret, err)
	}
	msg := res.Msg

	var r report
	written := msg.GetWrittenKeys()
	if in.Execute {
		r.addf("set_secret %s in %s/%s: WRITTEN (%s). Values are not readable back — kelson does not store them.",
			msg.GetSecret().GetName(), in.Project, in.Environment, keyCount(written))
	} else {
		r.addf("set_secret %s in %s/%s: VALIDATED ONLY, nothing was written (%s). Set execute=true to write it.",
			in.Name, in.Project, in.Environment, keyCount(written))
	}

	r.section("SECRET")
	r.addf("  namespace   %s", orDash(msg.GetSecret().GetNamespace()))
	r.addf("  keys set    %s", joinOrDash(written))
	if kept := keptKeys(msg.GetSecret().GetKeys(), written); len(kept) > 0 {
		r.addf("  keys kept   %s (already there; this call preserved them)", strings.Join(kept, ", "))
	}

	r.section("NEXT")
	if !in.Execute {
		r.addf("  nothing exists yet. Call again with execute=true, then reference a key from the spec's env.")
		return text(&r)
	}
	if len(written) > 0 {
		r.addf("  reference it from a component's env in the spec:")
		r.addf("    env: { MY_VARIABLE: { secret: %s, key: %s } }", msg.GetSecret().GetName(), written[0])
	}
	r.addf("  a running workload keeps the value its pods already read; deploy or restart to pick up a change.")
	return text(&r)
}

// unsetSecret composes the UnsetSecret RPC (issue #269).
//
// The default is the same preview the write has, and it is worth as much here:
// whether the named keys are even in the Secret is the removal's precondition,
// so the preview answers "this is what I would remove" and the execute answers
// "these were there and are gone".
func (c *clients) unsetSecret(ctx context.Context, in setSecretInput, dryRun kelsonv1alpha1.DryRun) (*mcpsdk.CallToolResult, any, error) {
	res, err := c.secrets.UnsetSecret(ctx, reasoned(connect.NewRequest(&kelsonv1alpha1.UnsetSecretRequest{
		Target: &kelsonv1alpha1.SecretTarget{
			Project:     in.Project,
			Environment: in.Environment,
			Namespace:   in.Namespace,
		},
		Name:   in.Name,
		Keys:   in.RemoveKeys,
		DryRun: dryRun,
	}), in.Reason))
	if err != nil {
		return nil, nil, c.fail(rpcUnsetSecret, err)
	}
	msg := res.Msg

	var r report
	removed := msg.GetRemovedKeys()
	if in.Execute {
		r.addf("set_secret %s in %s/%s: REMOVED %s. The value is gone — kelson kept no copy to restore.",
			msg.GetSecret().GetName(), in.Project, in.Environment, keyCount(removed))
	} else {
		r.addf("set_secret %s in %s/%s: VALIDATED ONLY, nothing was removed (%s). Set execute=true to remove it.",
			in.Name, in.Project, in.Environment, keyCount(removed))
	}

	r.section("SECRET")
	r.addf("  namespace     %s", orDash(msg.GetSecret().GetNamespace()))
	r.addf("  keys removed  %s", joinOrDash(removed))
	if in.Execute {
		r.addf("  keys left     %s", joinOrDash(msg.GetSecret().GetKeys()))
	}

	r.section("NEXT")
	if !in.Execute {
		r.addf("  nothing changed yet. Call again with execute=true to remove %s.", joinOrDash(removed))
		return text(&r)
	}
	if len(msg.GetSecret().GetKeys()) == 0 {
		// An empty Secret is a state a user has to be told about: it still
		// exists, it still lists, and nothing on this surface removes it.
		r.addf("  the Secret is now empty and still exists. Removing the object itself is `kelson secret delete`,")
		r.addf("  which no tool here offers.")
	}
	r.addf("  a component whose env still references a removed key will fail its next pod start with")
	r.addf("  CreateContainerConfigError. Check the spec's references with diagnose_component.")
	return text(&r)
}

// keptKeys is what the Secret held that this call did not write.
func keptKeys(all, written []string) []string {
	set := make(map[string]bool, len(written))
	for _, k := range written {
		set[k] = true
	}
	var out []string
	for _, k := range all {
		if !set[k] {
			out = append(out, k)
		}
	}
	return out
}

func keyCount(keys []string) string {
	if len(keys) == 1 {
		return "1 key"
	}
	return fmt.Sprintf("%d keys", len(keys))
}

func joinOrDash(keys []string) string {
	if len(keys) == 0 {
		return "-"
	}
	return strings.Join(keys, ", ")
}
