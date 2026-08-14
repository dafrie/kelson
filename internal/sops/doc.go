// Package sops writes SOPS-encrypted Kubernetes Secret manifests with age
// recipients (issue #81, [ADR-0022]). It encrypts and it never decrypts.
//
// # What it is for
//
// The `sops` secret backend keeps a credential in the delivery repository,
// encrypted, and lets Flux's kustomize-controller decrypt it in the cluster.
// kelson's half of that is one operation: turn a set of key/value pairs into
// the bytes of an encrypted Secret manifest, in memory, so that
// `kelson secret set` can commit them without a plaintext file ever existing.
// [EncryptSecret] is that operation and it is the whole of the write surface.
//
// # Why the format is implemented here rather than imported
//
// sops is a Go program with a usable library API, and using it was the first
// choice. Measured against `github.com/getsops/sops/v3 v3.13.3`, importing so
// much as the root package pulls in **98 additional modules** — the AWS SDK,
// the Azure SDK, Google Cloud KMS, HashiCorp Vault's API client, the MongoDB
// driver, OpenTelemetry and gRPC — because `sops.Metadata` names every key
// provider the tool supports and the root package is not separable from them.
// (`github.com/getsops/sops/v3/age` alone is 12, but it cannot build a tree,
// and `stores/yaml` is 119.) kelson's entire dependency list is 16 direct
// modules; the `sops` binary built from that graph is 74 MB. That is not a
// dependency a CLI takes on to encrypt a two-key Secret.
//
// The alternative rejected second is shelling out to a `sops` binary. It is
// honest and it is what ADR-0022 records as the fallback, but it makes a
// second executable a runtime dependency of `kelson secret set`, and the
// plaintext then has to reach that process — through a pipe or, worse, a file.
// "Plaintext never touches the working tree" is the acceptance criterion of
// issue #81, and the strongest way to keep it is for the plaintext never to
// leave kelson's address space.
//
// So kelson implements the file format directly over `filippo.io/age` (the
// reference age implementation, BSD-3-Clause, three modules) and the standard
// library's AES-GCM and SHA-512. What is implemented is deliberately narrow:
// one document shape (a Kubernetes Secret), one key type (age X25519
// recipients), one `encrypted_regex`, and encryption only.
//
// # The format, and where each rule comes from
//
// Verified against getsops/sops v3.13.3, whose behaviour these rules restate:
//
//   - Every leaf value on a path where any element matches the file's
//     `encrypted_regex` becomes
//     `ENC[AES256_GCM,data:…,iv:…,tag:…,type:str]` — AES-256-GCM under the
//     file's data key, with a **32-byte** nonce (sops uses
//     `cipher.NewGCMWithNonceSize(…, 32)`, not the 12-byte default) and the
//     value's path joined with `:` and a trailing `:` as additional
//     authenticated data. `data:url:` for `data.url`.
//   - The MAC is the uppercase hex SHA-512 of every leaf value's bytes,
//     concatenated **in document order**, encrypted the same way with the
//     file's `lastmodified` timestamp (RFC 3339) as additional data. It covers
//     unencrypted leaves too: `mac_only_encrypted` is off, which is sops's own
//     default.
//   - The data key is 32 random bytes, wrapped once per recipient with age and
//     ASCII-armoured, exactly as `sops/age.MasterKey.Encrypt` does.
//
// The document-order rule is why this package emits the manifest itself
// instead of accepting an arbitrary tree: the MAC is computed over the same
// traversal the emitter writes, so the bytes and the MAC cannot disagree.
//
// # kelson holds no private key, ever
//
// There is no `Decrypt`. The age identity that opens these files belongs in a
// Kubernetes Secret the operator creates, read by kustomize-controller and by
// nothing of kelson's — [EncryptSecret] needs only the public `age1…`
// recipients, which live in the Environment spec in the clear because that is
// what they are. [Inspect] exists so `kelson secret list` and
// `kelson secret rotate` can report a file's key *names* and recipients, and
// it reads only what SOPS leaves in plaintext: the mapping keys and the
// recipient list. Neither needs a key and neither can produce a value.
//
// # Plaintext lifetime
//
// A caller hands [EncryptSecret] the values and gets ciphertext back. Nothing
// in this package writes a file, and the plaintext exists only as the
// `Data` map the caller already held and as the intermediate byte slices of
// one AES-GCM seal. Callers register every value with `internal/redact` before
// calling (internal/secret does), so a value cannot reach an error message on
// any path out of here either.
//
// [ADR-0022]: https://github.com/dafrie/kelson/blob/main/docs/adr/0022-sops-age.md
package sops
