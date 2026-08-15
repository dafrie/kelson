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

// secretBackendSupported refuses a backend that is not one of the three.
//
// The backend selects the mechanism that puts a value where a reference points
// (ADR-0009), and since issue #81 all three have one. `cluster` means a
// Kubernetes Secret written out of band, which is exactly what a secretKeyRef
// addresses, so it needs nothing extra rendered. `externalSecrets` renders an
// ExternalSecret per referenced Secret, and the controller populates it
// (ADR-0020, internal/renderer/externalsecrets.go). `sops` means the Secret is
// in the delivery repository, encrypted, and kustomize-controller decrypts it
// on the way in — which the renderer expresses by writing `spec.decryption`
// onto every Kustomization it emits (ADR-0022, sopsDecryption below).
//
// The refusal is the renderer's for the reason ADR-0016's Helm gate is: it is
// decided from spec data alone, before anything is emitted, so the same
// document renders the same way against every cluster. Whether the *cluster*
// can serve the chosen backend is a different question with a different code
// (ErrExternalSecretsNotInstalled) and a ClusterProfile behind it.
func secretBackendSupported(resolved *model.Resolved) Errors {
	env := resolved.Environment
	switch env.Secrets.Backend {
	case "", model.SecretsCluster, model.SecretsExternalSecrets, model.SecretsSOPS:
		return nil
	}
	return Errors{{
		Code:        ErrSecretBackendUnsupported,
		Message:     "environment " + quoted(env.Name) + " selects unknown secret backend " + quoted(string(env.Secrets.Backend)),
		Remediation: "valid backends: cluster, externalSecrets, sops",
	}}
}

// The sops backend's delivery-mode gate — `render/sops-requires-flux`, the
// third instance of ADR-0016 decision 4's shape — is deleted (ADR-0028
// decision 8). It refused the backend outside flux mode because direct mode had
// no decryptor; the mode it refused does not exist.
//
// The mechanism it protected is intact and is now unconditional: the encrypted
// Secret ships inside the published artifact and kustomize-controller decrypts
// it through the `spec.decryption` block of the Kustomization kelson itself
// writes (ADR-0028 decision 7), which is why ADR-0022's "committed but never
// decrypted" failure mode is unreachable rather than merely refused.

// SOPSDecryptionBlock is the Kustomization stanza a sops environment needs,
// rendered at the given indentation:
//
//	decryption:
//	  provider: sops
//	  secretRef:
//	    name: sops-age
//
// The Secret it names holds the age *identity*. kelson writes the reference
// and never the Secret — creating it is the operator's documented step, and a
// kelson that could write it would be a kelson holding the key that opens
// every encrypted file in the repository (ADR-0022). The name comes from
// `secrets.ageKeySecret`, defaulted during resolution so nothing here has to
// decide what an empty one means.
//
// It is exported because a caller that only needs to *state* the requirement
// must get the identical text the renderer emits: `kelson secret set` prints
// this block, and a paraphrase that drifted from this function is exactly how
// "it was encrypted but never decrypted" happens.
func SOPSDecryptionBlock(ageKeySecret, indent string) string {
	if ageKeySecret == "" {
		ageKeySecret = model.DefaultAgeKeySecret
	}
	return indent + "decryption:\n" +
		indent + "  provider: " + sopsProvider + "\n" +
		indent + "  secretRef:\n" +
		indent + "    name: " + ageKeySecret + "\n"
}

// sopsProvider is kustomize-controller's only decryption provider. It is
// spelled out rather than inferred: the field is an enum of one today and
// writing it makes the manifest say what will happen to those files.
const sopsProvider = "sops"
