package forge

import (
	"errors"
	"strings"
	"testing"
)

// The generic adapter is the other end of the capability range, so its tests
// are about what it does *not* do as much as what it does: it hands back the
// stored token and nothing about it rotates, expires or reaches a network.
func TestGenericMintCloneCredential(t *testing.T) {
	tests := []struct {
		name         string
		conn         Conn
		wantUser     string
		wantPassword string
		wantErr      bool
	}{
		{
			name:         "the stored token, under the gitref default username",
			conn:         Conn{Provider: nameGeneric, Host: "https://git.acme.example", Token: "glpat-secret"},
			wantUser:     defaultTokenUsername,
			wantPassword: "glpat-secret",
		},
		{
			name:         "a forge that cares which user the token belongs to",
			conn:         Conn{Provider: nameGeneric, Token: "glpat-secret", Username: "acme-bot"},
			wantUser:     "acme-bot",
			wantPassword: "glpat-secret",
		},
		{
			name:    "no token is an auth failure, not an empty credential",
			conn:    Conn{Provider: nameGeneric, Host: "https://git.acme.example"},
			wantErr: true,
		},
		{
			// App material on a generic connection is not a credential this
			// adapter can use: there is no forge API behind it to mint against.
			name:    "app material without a token",
			conn:    Conn{Provider: nameGeneric, AppID: 1, InstallationID: 2, PrivateKeyPEM: []byte("x")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := genericProvider{}.MintCloneCredential(t.Context(), tt.conn, "https://git.acme.example/acme/checkout.git")
			if tt.wantErr {
				if !errors.Is(err, ErrAuthFailed) {
					t.Fatalf("error = %v, want ErrAuthFailed", err)
				}
				if !strings.Contains(err.Error(), "acme/checkout") {
					t.Errorf("error = %v, want it to name the repository", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("MintCloneCredential: %v", err)
			}
			if got.Username != tt.wantUser {
				t.Errorf("username = %q, want %q", got.Username, tt.wantUser)
			}
			if got.Password != tt.wantPassword {
				t.Errorf("password = %q, want the stored token", got.Password)
			}
			if !got.ExpiresAt.IsZero() {
				t.Error("a stored token is non-expiring; a caller reads ExpiresAt to decide whether to re-mint")
			}
		})
	}
}

// The clone URL only ever reaches an error message, and it reaches it reduced
// to the name a user recognises.
func TestRepoName(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "https clone URL", url: "https://github.com/acme/checkout.git", want: "acme/checkout"},
		{name: "no .git suffix", url: "https://github.com/acme/checkout", want: "acme/checkout"},
		{name: "a self-hosted subpath", url: "https://git.acme.example/team/group/repo.git", want: "team/group/repo"},
		{name: "already a name", url: "acme/checkout", want: "acme/checkout"},
		{name: "empty", url: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repoName(tt.url); got != tt.want {
				t.Errorf("repoName(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}
