package redact

import (
	"bytes"
	"strings"
	"testing"
)

func TestScrubberReplacesKnownValues(t *testing.T) {
	s := NewScrubber("hunter2-hunter2", "another-known-secret")
	got := s.Text("pushing as robot with hunter2-hunter2 and another-known-secret")
	if strings.Contains(got, "hunter2") || strings.Contains(got, "another-known-secret") {
		t.Fatalf("value survived scrubbing: %q", got)
	}
	if n := strings.Count(got, Sentinel); n != 2 {
		t.Fatalf("want 2 sentinels, got %d: %q", n, got)
	}
}

func TestScrubberDropsValuesTooShortToBeSafe(t *testing.T) {
	s := NewScrubber("abc", "")
	if !s.Empty() {
		t.Fatalf("a scrubber over sub-minimum values must be empty")
	}
	if got := s.Text("abcdef"); got != "abcdef" {
		t.Fatalf("an empty scrubber changed its input: %q", got)
	}
}

// A password and the base64 auth blob that embeds it are both registered; the
// longer must win so the shorter is not left visible inside a partial match.
func TestScrubberPrefersTheLongestMatch(t *testing.T) {
	short := "passw0rd-value"
	long := "robot:" + short + ":extra"
	s := NewScrubber(short, long)
	got := s.Text("auth=" + long)
	if got != "auth="+Sentinel {
		t.Fatalf("want the whole blob replaced once, got %q", got)
	}
}

func TestScrubberIsOrderIndependent(t *testing.T) {
	a := NewScrubber("first-known-value", "second-known-value")
	b := NewScrubber("second-known-value", "first-known-value")
	in := "second-known-value then first-known-value"
	if a.Text(in) != b.Text(in) {
		t.Fatalf("scrubbing depends on registration order: %q vs %q", a.Text(in), b.Text(in))
	}
}

func TestScrubWriterCatchesValuesSplitAcrossWrites(t *testing.T) {
	const secret = "split-across-two-chunks"
	var out bytes.Buffer
	w := NewScrubber(secret).Writer(&out)

	// Split the value in the middle: the transport chunks a build log at sizes
	// that have nothing to do with what is in it.
	if _, err := w.Write([]byte("logging in with split-across")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.Write([]byte("-two-chunks, done\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.(*ScrubWriter).Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got := out.String()
	if strings.Contains(got, secret) {
		t.Fatalf("a split value survived: %q", got)
	}
	if want := "logging in with " + Sentinel + ", done\n"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestScrubWriterReportsFullWritesSoIOCopyIsHappy(t *testing.T) {
	var out bytes.Buffer
	w := NewScrubber("a-known-secret-value").Writer(&out)
	p := []byte("short")
	n, err := w.Write(p)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(p) {
		t.Fatalf("Write reported %d of %d bytes; io.Copy treats that as a short write", n, len(p))
	}
	if err := w.(*ScrubWriter).Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if out.String() != "short" {
		t.Fatalf("withheld tail was never flushed: %q", out.String())
	}
}

func TestScrubberWithNoValuesDoesNotBuffer(t *testing.T) {
	var out bytes.Buffer
	w := Scrubber{}.Writer(&out)
	if w != &out { //nolint:staticcheck // identity is the assertion
		t.Fatalf("an empty scrubber must hand back the writer unchanged, or a log stream stalls")
	}
}

func TestRegisterMakesAValueUnprintableProcessWide(t *testing.T) {
	const value = "registered-process-wide-sentinel-4711"
	Register(value)
	if got := Scrub("cause: could not push with " + value); strings.Contains(got, value) {
		t.Fatalf("a registered value was printed: %q", got)
	}
	if Registered().Empty() {
		t.Fatal("Registered() reports empty after a successful Register")
	}
}

func TestRegisterIgnoresValuesTooShortToBeSafe(t *testing.T) {
	Register("tiny")
	if got := Scrub("tiny"); got != "tiny" {
		t.Fatalf("a sub-minimum value was registered anyway: %q", got)
	}
}
