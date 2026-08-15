package artifact

import (
	"net/http"
	"testing"
	"time"
)

// The one test in this package that is not black-box, because what it asserts
// is not observable from outside: which client a Pusher that was given none
// falls back to. Proving it through the wire would mean waiting out the
// timeout, which is exactly the thing the fallback exists to bound.

// TestTheDefaultClientIsBounded: the fallback used to be http.DefaultClient,
// which has no timeout at all. A registry that completes its handshake and then
// answers nothing therefore held the caller forever — one reconcile worker of a
// small pool in the controller, and a `kelson preview publish` that never
// returned and never said why on the CLI.
func TestTheDefaultClientIsBounded(t *testing.T) {
	got := (&Pusher{}).client()
	if got == http.DefaultClient {
		t.Fatal("a Pusher with no client falls back to http.DefaultClient, which never times out")
	}
	if got.Timeout <= 0 {
		t.Fatalf("the default client's timeout is %s, want a bound on one request", got.Timeout)
	}
	if got.Timeout != DefaultRequestTimeout {
		t.Errorf("the default client's timeout is %s, want DefaultRequestTimeout (%s)",
			got.Timeout, DefaultRequestTimeout)
	}

	// A caller that brought its own client still gets exactly that one: the
	// controller and the CLI may both want their own transport.
	mine := &http.Client{Timeout: time.Second}
	if (&Pusher{Client: mine}).client() != mine {
		t.Error("a Pusher ignored the client it was given")
	}
}
