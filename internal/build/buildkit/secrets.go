package buildkit

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Build-time secrets go through BuildKit secret mounts, never build arguments
// and never image layers (ADR-0009, issue #117). This file is that mechanism.
//
// # Why a mount and not an argument
//
// A `--build-arg` is recorded in the image's own history: `docker history` and
// `crane config` print it back to anyone who can pull the image, and the
// generated Job carries it in `spec.template.spec.containers[].command`, which
// means `kubectl get job -o yaml` shows it too. A secret passed that way is
// public the moment the image is pushed. This is Coolify's documented gap and
// ADR-0009 names it explicitly.
//
// A BuildKit secret mount is the opposite: `RUN --mount=type=secret,id=<id>`
// exposes the file only for the duration of that one RUN instruction, on a
// tmpfs that is never committed to a layer.
//
// # References, never values
//
// [SecretMount] carries the *name of a Kubernetes Secret*, never a value. There
// is deliberately no field to pass a literal: kelson does not hold credentials
// (ADR-0009), the kubelet projects the Secret into the build pod, and kelson's
// own process never sees the bytes. That is why the acceptance test can assert
// the value appears in no rendered manifest and no log line without kelson
// having to remember to redact anything — it never had it.
//
// ADR-0009 does not yet name a spec field or a CLI flag for build secrets, and
// none is invented here. This is executor configuration in the sense ADR-0010
// means it — the same status as the push credential and the builder image — so
// the safe path exists for the reference model of #79 to be wired onto without
// a redesign.

// secretMountDir is where projected build secrets appear inside the build
// container. It is under /run, not the workspace, so a `COPY .` can never pick
// one up by accident.
const secretMountDir = "/run/kelson/secrets"

// secretIDRE is the character set BuildKit accepts for a secret id, and which
// is also safe to splice into the generated shell command and into a Kubernetes
// volume name.
var secretIDRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// SecretMount projects one key of a Kubernetes Secret into the build as a
// BuildKit secret, consumed in a Dockerfile as:
//
//	RUN --mount=type=secret,id=<ID> cat /run/secrets/<ID>
//
// Every field is a reference. There is no value field, by construction.
type SecretMount struct {
	// ID is the BuildKit secret id the Dockerfile names. Lowercase
	// alphanumerics, dots, dashes and underscores.
	ID string
	// SecretName is an existing Kubernetes Secret in Config.Namespace. kelson
	// does not create it and never reads it: the kubelet projects it.
	SecretName string
	// Key selects one key within that Secret. Empty means the key is named the
	// same as the ID, which is the convention that makes the common case
	// require no configuration at all.
	Key string
}

// key resolves the Secret key this mount reads.
func (m SecretMount) key() string {
	if m.Key != "" {
		return m.Key
	}
	return m.ID
}

// volumeName is the pod volume backing this mount. It is prefixed so it cannot
// collide with the workspace, state, tmp or push-credential volumes.
func (m SecretMount) volumeName() string {
	return "build-secret-" + m.ID
}

// mountPath is the directory the Secret key is projected into. One directory
// per mount, so the projected file is exactly one file and its name is the
// secret id.
func (m SecretMount) mountPath() string {
	return secretMountDir + "/" + m.ID
}

// filePath is what buildctl is pointed at with --secret src=.
func (m SecretMount) filePath() string {
	return m.mountPath() + "/" + m.ID
}

// sortedSecrets returns the configured mounts in id order. Determinism is the
// same requirement it is everywhere else in this package: the same Config and
// Request must render byte-identical Job manifests.
func sortedSecrets(mounts []SecretMount) []SecretMount {
	out := append([]SecretMount(nil), mounts...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// validateSecrets checks the mounts before anything is rendered: a malformed id
// would produce an invalid volume name or an unparseable buildctl flag, and a
// duplicate id would silently shadow one credential with another.
func validateSecrets(mounts []SecretMount) error {
	seen := map[string]bool{}
	for _, m := range mounts {
		switch {
		case m.ID == "":
			return fmt.Errorf("buildkit: a secret mount needs an ID (the id a Dockerfile's --mount=type=secret names)")
		case !secretIDRE.MatchString(m.ID):
			return fmt.Errorf("buildkit: secret id %q must be lowercase alphanumerics, dots, dashes or underscores", m.ID)
		case m.SecretName == "":
			return fmt.Errorf("buildkit: secret mount %q needs SecretName: kelson mounts an existing Kubernetes Secret, it never carries the value (ADR-0009)", m.ID)
		case seen[m.ID]:
			return fmt.Errorf("buildkit: secret id %q is mounted twice; ids are what a Dockerfile addresses, so one must win and neither should silently", m.ID)
		}
		seen[m.ID] = true
	}
	return nil
}

// secretFlags renders the buildctl --secret flags, in id order.
func secretFlags(b *strings.Builder, mounts []SecretMount) {
	for _, m := range sortedSecrets(mounts) {
		fmt.Fprintf(b, " --secret id=%s,src=%s", m.ID, m.filePath())
	}
}

// secretVolumes projects each mount as a read-only, 0400 Secret volume holding
// exactly the one key it names. Only the Secret's *name* enters the manifest.
func secretVolumes(mounts []SecretMount) []volume {
	sorted := sortedSecrets(mounts)
	out := make([]volume, 0, len(sorted))
	for _, m := range sorted {
		mode := pushSecretMode
		out = append(out, volume{
			Name: m.volumeName(),
			Secret: &secretVolume{
				SecretName:  m.SecretName,
				DefaultMode: &mode,
				Items:       []keyToPath{{Key: m.key(), Path: m.ID}},
			},
		})
	}
	return out
}

// secretVolumeMounts mounts each projected Secret read-only into the build
// container.
func secretVolumeMounts(mounts []SecretMount) []volumeMount {
	sorted := sortedSecrets(mounts)
	out := make([]volumeMount, 0, len(sorted))
	for _, m := range sorted {
		out = append(out, volumeMount{Name: m.volumeName(), MountPath: m.mountPath(), ReadOnly: true})
	}
	return out
}
