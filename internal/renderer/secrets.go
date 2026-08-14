package renderer

import (
	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/model"
)

// Secret references, and the property this file is responsible for (issue #82,
// ADR-0018).
//
// **Rendered output never contains a secret value, by construction.** Not
// because the renderer inspects what it emits and removes credentials — it has
// no way to know which bytes are one — but because there is no path from a spec
// to an inlined value:
//
//   - An env value that is a secret reference or a service binding becomes
//     `valueFrom.secretKeyRef`. Both forms carry a *name* and a *key*, and the
//     kubelet does the projection at pod start. kelson never reads the Secret.
//   - An env value that is a plain string becomes `value:`, and the model
//     refuses the credential-shaped ones before rendering is reached.
//   - The renderer emits no `kind: Secret` at all. There is no function here
//     that writes `data:` or `stringData:`, so there is no field a value could
//     be placed in even by a caller that wanted to.
//
// The boundary, stated plainly: kelson cannot stop a user writing a password as
// a plain string under a name no heuristic recognizes — `model.SecretShapedName`
// catches the common spellings and ADR-0009's honesty note says it is a
// heuristic. What is guaranteed is narrower and actually true: everything typed
// as a reference stays a reference all the way to the manifest. `spec.overlays`
// remains the documented escape hatch, and a Secret arriving that way is
// redacted in every display surface by `internal/redact` (issue #117).

// secretKeyRefNode is the one shape both reference forms render into. It is
// shared rather than written twice because they are the same mechanism: a
// binding is a reference whose Secret name kelson derives from the Project's
// own data component, and a `{secret, key}` reference is one the author names
// directly.
func secretKeyRefNode(name, key string) *yaml.Node {
	return mapNode("secretKeyRef", mapNode("name", name, "key", key))
}

// secretBackendSupported refuses a backend kelson cannot render.
//
// The backend selects the mechanism that puts a value where a reference points
// (ADR-0009). Two of the three have one: `cluster` means a Kubernetes Secret
// written out of band, which is exactly what a secretKeyRef addresses, so it
// needs nothing extra rendered; `externalSecrets` renders an ExternalSecret per
// referenced Secret, and the controller populates it (ADR-0020,
// internal/renderer/externalsecrets.go). `sops` has none — SOPS decryption in
// the delivery path is issue #81 — and rendering the cluster shape for it would
// produce manifests that apply cleanly and then fail at pod start, against a
// Secret nothing populates.
//
// The refusal is the renderer's for the reason ADR-0016's Helm gate is: it is
// decided from spec data alone, before anything is emitted, so the same
// document renders the same way against every cluster. Whether the *cluster*
// can serve the chosen backend is a different question with a different code
// (ErrExternalSecretsNotInstalled) and a ClusterProfile behind it.
func secretBackendSupported(resolved *model.Resolved) Errors {
	env := resolved.Environment
	switch env.Secrets.Backend {
	case "", model.SecretsCluster, model.SecretsExternalSecrets:
		return nil
	case model.SecretsSOPS:
		return Errors{{
			Code: ErrSecretBackendUnsupported,
			Message: "environment " + quoted(env.Name) + " selects secret backend " +
				quoted(string(model.SecretsSOPS)) + ", which kelson does not render yet",
			Remediation: "use backend: cluster, where a secret reference renders as a secretKeyRef against a " +
				"Secret in this namespace that you write out of band. SOPS-encrypted values in Git, decrypted " +
				"in-cluster, are issue #81 (milestone M8 · Secrets); the reference syntax in the spec does not " +
				"change when it lands (ADR-0018)",
		}}
	}
	return Errors{{
		Code:        ErrSecretBackendUnsupported,
		Message:     "environment " + quoted(env.Name) + " selects unknown secret backend " + quoted(string(env.Secrets.Backend)),
		Remediation: "valid backends: cluster, externalSecrets, sops — and sops does not render yet (issue #81)",
	}}
}
