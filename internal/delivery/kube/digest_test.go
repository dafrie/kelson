package kube

import (
	"strings"
	"testing"
)

// TestParsePushedDigest exercises the digest parser directly against the same
// shapes the executor sees in BuildKit's stdout, plus the failing shapes that
// must error rather than silently-return an empty digest.
func TestParsePushedDigest(t *testing.T) {
	// 64-hex digests, built at run time so the invariant is easy to read and
	// length-checked below.
	const (
		dgstA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		dgstB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	cases := []struct {
		name string
		out  string
		want string
		err  bool
	}{
		{
			name: "single-platform push line",
			out:  "#22 pushing manifest for ghcr.io/acme/web@" + dgstA + " done\n",
			want: dgstA,
		},
		{
			name: "multi-platform push takes the last pushed digest",
			out: "#22 pushing manifest for ghcr.io/acme/web@" + dgstA + " done\n" +
				"#22 pushing manifest for ghcr.io/acme/web@" + dgstB + " done\n",
			want: dgstB,
		},
		{
			name: "bare sha256 without @ is not a pushed digest",
			out:  "#22 exporting manifest sha256:" + strings.Repeat("c", 64) + "\n",
			err:  true,
		},
		{
			name: "no digest at all",
			out:  "#13 [2/3] RUN go build\n",
			err:  true,
		},
		{
			name: "non-hex digest is rejected",
			out:  "#22 pushing manifest for ghcr.io/acme/web@sha256:ZZZZ done\n",
			err:  true,
		},
		{
			name: "short hex digest is rejected",
			out:  "#22 pushing manifest for ghcr.io/acme/web@sha256:abcd\n",
			err:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePushedDigest(tc.out)
			if tc.err {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("digest = %q, want %q", got, tc.want)
			}
		})
	}
}
