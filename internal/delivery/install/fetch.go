package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dafrie/kelson/internal/delivery"
)

// Fetcher retrieves the bytes at a pinned upstream URL.
//
// It is an interface for the same reason direct.Mapper and uninstall.Catalog
// are: the production implementation needs the network, and none of the tests
// that assert what an install plans, refuses or applies should. A fake Fetcher
// returns a fixture manifest and the whole plan-and-apply path is exercised
// with no socket opened.
//
// Implementations return the bytes verbatim. Digest verification is the
// installer's, not the fetcher's — a Fetcher that verified its own downloads
// would let a fake silently skip the check that makes the pin mean anything.
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// maxManifestBytes bounds a fetch. The largest pinned manifest today is
// cert-manager's at roughly one megabyte; 32 MiB leaves several years of growth
// and still refuses to buffer a redirect that landed on something enormous.
const maxManifestBytes = 32 << 20

// HTTPFetcher is the production Fetcher.
type HTTPFetcher struct {
	// Client overrides the HTTP client. Nil uses one with the timeout below.
	Client *http.Client
	// Timeout bounds the whole fetch. Zero uses defaultFetchTimeout.
	Timeout time.Duration
}

const defaultFetchTimeout = 2 * time.Minute

var _ Fetcher = HTTPFetcher{}

// Fetch downloads a pinned manifest.
//
// Failures are returned plainly, as causes. The one shape every fetch failure
// takes for a caller is [unreachable], applied by the installer, so a fake
// Fetcher's error reaches the user with the same URL and the same "nothing was
// applied" promise as a real DNS failure.
func (f HTTPFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	client := f.Client
	if client == nil {
		timeout := f.Timeout
		if timeout == 0 {
			timeout = defaultFetchTimeout
		}
		client = &http.Client{Timeout: timeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the response body: %w", err)
	}
	if len(body) > maxManifestBytes {
		return nil, fmt.Errorf("the response exceeds the %d-byte limit for an install manifest", maxManifestBytes)
	}
	return body, nil
}

// unreachable is the one shape every fetch failure takes on its way out.
//
// A half-installed component — the CRDs applied and the controller not, or the
// controller applied and its RBAC not — is worse than an absent one, because it
// looks installed to everything that only checks the API group. So a fetch
// failure stops the whole plan before anything is applied, and says so.
func unreachable(url, cause string) error {
	return delivery.ApplyFailed(url, "",
		"the pinned install manifest could not be fetched: "+cause,
		"kelson installs by referencing upstream releases and vendors nothing, so this needs network access to "+
			"that URL. Nothing was applied. On an air-gapped cluster, mirror the manifest and apply it yourself — "+
			"kelson will then detect the component and adopt it")
}

// verifyDigest checks fetched bytes against the pin.
//
// The digest is what makes "reference upstream, do not vendor" safe rather than
// merely convenient: without it, kelson would apply whatever bytes a URL served
// today into a cluster with cluster-admin-shaped RBAC. A mismatch is never a
// warning and never a prompt — the pin is a claim about exact bytes, and bytes
// that are not those bytes are not the thing this repository reviewed.
func verifyDigest(c Component, body []byte) error {
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got == c.SHA256 {
		return nil
	}
	return delivery.ApplyFailed(c.ManifestURL, "",
		fmt.Sprintf("the fetched manifest does not match the pinned digest for %s %s: got %s, pinned %s",
			c.Title, c.Version, got, c.SHA256),
		"nothing was applied. Either the release was re-published, something is rewriting the response, or the "+
			"pin in internal/delivery/install/pins.go is stale. Verify with `curl -fsSL "+c.ManifestURL+
			" | sha256sum` before changing anything")
}
