package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// SpecHash is the content identity of a resolved spec: a sha256 over its
// canonical JSON.
//
// # What it is for
//
// It is the second half of the artifact tag, `<generation>-<spec-hash-short>`
// (ADR-0028 decision 2). The generation says *which* revision this is; the hash
// says *what* it is, which is what makes a tag content-identified as well as
// sequence-identified: two tags with the same hash suffix are the same input,
// and a rollback target is recognisable without fetching it.
//
// # Why it hashes the resolved spec and not the document
//
// Because the resolved spec is what renders. Two documents that differ only in
// comments, key order or an override that resolves to the value it was already
// overriding produce the same manifests, and a hash over the authored bytes
// would claim they were different revisions. Resolution has already applied
// P1–P6 and every default, so what is hashed is exactly the input the renderer
// consumes.
//
// # Why the standard library is enough
//
// encoding/json is deterministic where it matters: struct fields marshal in
// declaration order, and map keys are sorted. So "canonical JSON" here needs no
// canonicaliser — it needs the marshaller not to be given anything whose
// encoding is unordered, and internal/model has no such type. This mirrors the
// per-component hash the renderer already stamps as `kelson.dev/spec-hash`
// (internal/renderer/renderer.go), which is the same technique at a smaller
// scope: the renderer's is per resource so a sibling edit does not churn every
// annotation, and this one is per environment because the artifact is one
// object.
//
// HTML escaping is off and there is no trailing newline, so the bytes that are
// hashed are the bytes the encoder produced and nothing else.
func SpecHash(r *Resolved) (string, error) {
	if r == nil {
		return "", fmt.Errorf("model: a spec hash needs a resolved spec")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return "", fmt.Errorf("model: hashing the resolved spec: %w", err)
	}
	sum := sha256.Sum256(bytes.TrimRight(buf.Bytes(), "\n"))
	return hex.EncodeToString(sum[:]), nil
}

// ShortHashLength is how much of a [SpecHash] the artifact tag carries. Eight
// hex characters is 32 bits, which is plenty to recognise a revision by eye and
// far too little to rely on as an identity — which is why the tag also carries
// the generation, and why the full hash is what `status.history[]` records.
const ShortHashLength = 8

// ShortHash is the leading [ShortHashLength] characters of a spec hash: the
// suffix of an artifact tag. A hash shorter than that is returned whole rather
// than panicking, because a truncation bug must not become a crash in a
// reconcile loop.
func ShortHash(hash string) string {
	if len(hash) <= ShortHashLength {
		return hash
	}
	return hash[:ShortHashLength]
}
