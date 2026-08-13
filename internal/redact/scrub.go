package redact

import (
	"io"
	"sort"
	"strings"
	"sync"
)

// MinScrubLength is the shortest literal the known-value scrubber will accept.
//
// A scrubber is a blunt substring replacement, so a short value is a liability:
// registering "abc" would blank that sequence out of every log line that
// happens to contain it, and a reader who cannot trust the output stops reading
// it. Anything shorter is dropped at registration rather than half-applied,
// because a credential that short is not protected by redaction anyway.
const MinScrubLength = 8

// maxRegistered bounds the process-wide set. It is a guard against a caller in
// a loop turning the scrubber into a memory leak and a linear scan over
// thousands of needles per log chunk; kelson resolves credentials one at a
// time and a real deployment is nowhere near this.
const maxRegistered = 256

// Scrubber replaces known literal secret values in arbitrary text.
//
// It is the mechanism for surfaces whose structure kelson does not own — a
// build log is the case that matters — where the only thing that can be said
// with certainty is "this exact string is a credential kelson resolved". It is
// never a content heuristic: a Scrubber with no values changes nothing, which
// is the honest behaviour when kelson holds no secret (which, per ADR-0009, is
// the normal case: kelson stores references).
//
// The zero Scrubber is valid and is a no-op.
type Scrubber struct {
	// values are sorted longest-first so an overlapping pair (a password and
	// the base64 auth blob that embeds it) redacts to one sentinel rather than
	// leaving the shorter half visible inside the longer.
	values []string
	// longest is the length of the longest value, which is what a streaming
	// scrubber must hold back to catch a value split across two writes.
	longest int
}

// NewScrubber returns a Scrubber over the given literal values. Empty values
// and values shorter than [MinScrubLength] are dropped, and duplicates are
// collapsed, so the result is deterministic for the same input set in any
// order.
func NewScrubber(values ...string) Scrubber {
	seen := map[string]bool{}
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) < MinScrubLength || seen[v] {
			continue
		}
		seen[v] = true
		kept = append(kept, v)
	}
	sort.Slice(kept, func(i, j int) bool {
		if len(kept[i]) != len(kept[j]) {
			return len(kept[i]) > len(kept[j])
		}
		return kept[i] < kept[j]
	})
	s := Scrubber{values: kept}
	if len(kept) > 0 {
		s.longest = len(kept[0])
	}
	return s
}

// Empty reports whether this Scrubber would change anything.
func (s Scrubber) Empty() bool { return len(s.values) == 0 }

// Text replaces every registered value in text with [Sentinel].
func (s Scrubber) Text(text string) string {
	if len(s.values) == 0 || text == "" {
		return text
	}
	for _, v := range s.values {
		text = strings.ReplaceAll(text, v, Sentinel)
	}
	return text
}

// Bytes is [Scrubber.Text] over a byte slice. The input is never mutated; an
// unchanged result is returned as the same slice.
func (s Scrubber) Bytes(b []byte) []byte {
	if len(s.values) == 0 || len(b) == 0 {
		return b
	}
	out := s.Text(string(b))
	if out == string(b) {
		return b
	}
	return []byte(out)
}

// Writer wraps w so everything written through it is scrubbed.
//
// It buffers, and it has to: a build log arrives in transport-sized chunks that
// have nothing to do with token boundaries, so a credential is as likely to
// straddle two writes as to sit inside one. The returned writer holds back the
// last len(longest value)-1 bytes until more arrives or [ScrubWriter.Flush] is
// called, which is the smallest window in which a straddling match can still
// complete.
//
// A Scrubber with no values returns w itself: there is nothing to catch, and
// buffering output nobody will change would delay a log stream for no reason.
func (s Scrubber) Writer(w io.Writer) io.Writer {
	if len(s.values) == 0 {
		return w
	}
	return &ScrubWriter{out: w, scrubber: s, hold: s.longest - 1}
}

// ScrubWriter is the buffering writer [Scrubber.Writer] returns. Callers that
// need the tail to reach the reader — the end of a build, the end of a stream —
// must call Flush.
type ScrubWriter struct {
	out      io.Writer
	scrubber Scrubber
	hold     int
	buf      []byte
}

// Write implements io.Writer. It always reports len(p) consumed even though it
// may emit less: io.Copy treats a short write as an error, and the withheld
// tail is owed to Flush, not lost.
func (w *ScrubWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.buf = append(w.buf, p...)
	// Scrub the whole buffer first: a value wholly inside it is replaced now,
	// and a value still being assembled cannot match yet and therefore lies
	// within the withheld tail by construction (it is at most `longest` bytes
	// and it runs past the end).
	w.buf = []byte(w.scrubber.Text(string(w.buf)))
	if len(w.buf) <= w.hold {
		return len(p), nil
	}
	cut := len(w.buf) - w.hold
	if _, err := w.out.Write(w.buf[:cut]); err != nil {
		return len(p), err
	}
	w.buf = append(w.buf[:0], w.buf[cut:]...)
	return len(p), nil
}

// Flush writes the withheld tail. It is safe to call more than once.
func (w *ScrubWriter) Flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	out := []byte(w.scrubber.Text(string(w.buf)))
	w.buf = w.buf[:0]
	_, err := w.out.Write(out)
	return err
}

// --- the process-wide set ---------------------------------------------------

// The registry below is process-wide state, which is deliberate and is the one
// place in kelson that is.
//
// The property this package exists for — "no secret value is ever written to a
// log, an event, an error or a diff" — is a statement about the process, not
// about one call path. Threading a scrubber from wherever a credential is
// resolved to every writer that might one day print it is exactly the
// convention-instead-of-property arrangement issue #117 was opened against: it
// holds until somebody adds an error message without the parameter. Registering
// the value once, where it is learned, makes it unprintable everywhere after
// that.
//
// It is append-only and never returns what it holds, so registration cannot be
// used to read a credential back out. Under ADR-0009 kelson resolves almost
// nothing — the spec carries references and the kubelet projects them — so in
// practice this set is empty and every Scrub is a no-op.
var registry struct {
	sync.RWMutex
	scrubber Scrubber
	values   []string
}

// Register adds literal secret values kelson has resolved to the process-wide
// scrub set, after which they are replaced by [Sentinel] on every surface wired
// through [Scrub] or [Registered]. Values shorter than [MinScrubLength] are
// ignored, and the set is capped at a size no real deployment approaches.
//
// Call it at the moment a value is learned, not at the moment it is printed.
func Register(values ...string) {
	registry.Lock()
	defer registry.Unlock()
	added := false
	for _, v := range values {
		if len(v) < MinScrubLength || len(registry.values) >= maxRegistered {
			continue
		}
		if contains(registry.values, v) {
			continue
		}
		registry.values = append(registry.values, v)
		added = true
	}
	if added {
		registry.scrubber = NewScrubber(registry.values...)
	}
}

// Registered returns a snapshot of the process-wide scrubber. A caller that
// scrubs a stream should take one snapshot and keep it, rather than re-reading
// the registry per chunk.
func Registered() Scrubber {
	registry.RLock()
	defer registry.RUnlock()
	return registry.scrubber
}

// Scrub replaces every registered value in text with [Sentinel]. It is the
// one-line form for error messages and other single strings.
func Scrub(text string) string {
	registry.RLock()
	s := registry.scrubber
	registry.RUnlock()
	return s.Text(text)
}

func contains(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}
