package git

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
)

// Actor distinguishes who caused a commit. Attribution is load-bearing: an
// agent's commit must never be attributable to a human (docs/architecture.md,
// agent principals). kelson therefore refuses to sign agent commits with a
// human identity — the human who asked for it is recorded in a trailer
// instead, so review still knows who wanted the change.
type Actor string

const (
	ActorHuman Actor = "human"
	ActorAgent Actor = "agent"
)

// Default agent attribution. Any agent commit is authored by this identity
// (optionally narrowed by an agent id) regardless of who is logged in.
const (
	DefaultAgentName   = "kelson agent"
	DefaultAgentEmail  = "agent@kelson.dev"
	DefaultCommitName  = "kelson"
	DefaultCommitEmail = "kelson@kelson.dev"
)

// Identity is the commit attribution. Name/Email are configurable for human
// authorship; agent authorship derives its identity from the kelson agent
// identity (env or config), never from a human's git config.
type Identity struct {
	// Name and Email author human commits.
	Name  string
	Email string

	// Actor is "human" or "agent"; empty means human.
	Actor Actor

	// AgentID identifies the agent principal (e.g. "ci-bot", "claude").
	// Only meaningful when Actor is ActorAgent.
	AgentID string

	// RequestedBy records the human who asked an agent to act, for audit. It
	// never becomes the commit author.
	RequestedBy string
}

// IdentityFromEnv reads attribution from the environment. getenv may be nil,
// in which case os.Getenv is used (injectable so tests need no ambient env).
//
//	KELSON_ACTOR         human | agent          (default human)
//	KELSON_AGENT_ID      agent principal id
//	KELSON_GIT_AUTHOR_NAME / KELSON_GIT_AUTHOR_EMAIL   human author
//	KELSON_REQUESTED_BY  human on whose behalf an agent acts
func IdentityFromEnv(getenv func(string) string) Identity {
	if getenv == nil {
		getenv = os.Getenv
	}
	id := Identity{
		Name:        getenv("KELSON_GIT_AUTHOR_NAME"),
		Email:       getenv("KELSON_GIT_AUTHOR_EMAIL"),
		Actor:       Actor(strings.ToLower(strings.TrimSpace(getenv("KELSON_ACTOR")))),
		AgentID:     strings.TrimSpace(getenv("KELSON_AGENT_ID")),
		RequestedBy: strings.TrimSpace(getenv("KELSON_REQUESTED_BY")),
	}
	if id.Actor != ActorAgent {
		id.Actor = ActorHuman
	}
	return id
}

// IsAgent reports whether this identity acts as an agent principal.
func (i Identity) IsAgent() bool { return i.Actor == ActorAgent }

// author returns the git author signature. Agent identities are forced onto
// the agent attribution so a human is never recorded as the author of an
// agent's commit.
func (i Identity) author(now time.Time) object.Signature {
	if i.IsAgent() {
		name := DefaultAgentName
		if i.AgentID != "" {
			name = DefaultAgentName + " " + i.AgentID
		}
		email := DefaultAgentEmail
		if i.AgentID != "" {
			email = i.AgentID + "@agents.kelson.dev"
		}
		return object.Signature{Name: name, Email: email, When: now}
	}
	name, email := i.Name, i.Email
	if name == "" {
		name = DefaultCommitName
	}
	if email == "" {
		email = DefaultCommitEmail
	}
	return object.Signature{Name: name, Email: email, When: now}
}

// committer is always kelson: the tool that wrote the commit, distinct from
// the actor that authored it.
func (i Identity) committer(now time.Time) object.Signature {
	return object.Signature{Name: DefaultCommitName, Email: DefaultCommitEmail, When: now}
}

// trailers returns the attribution trailers for this identity.
func (i Identity) trailers() map[string]string {
	t := map[string]string{TrailerActor: string(ActorHuman)}
	if i.IsAgent() {
		t[TrailerActor] = string(ActorAgent)
		if i.AgentID != "" {
			t[TrailerAgentID] = i.AgentID
		}
	}
	if i.RequestedBy != "" {
		t[TrailerRequestedBy] = i.RequestedBy
	}
	return t
}

// Commit-message trailer keys. They are the machine-readable half of the
// commit convention: history, provenance correlation (#37) and "who did this"
// are parsed from these, never from prose.
const (
	TrailerProject     = "Kelson-Project"
	TrailerEnvironment = "Kelson-Environment"
	TrailerSpecHash    = "Kelson-Spec-Hash"
	TrailerRevision    = "Kelson-Revision"
	TrailerActor       = "Kelson-Actor"
	TrailerAgentID     = "Kelson-Agent-Id"
	TrailerRequestedBy = "Kelson-Requested-By"
	TrailerRenderer    = "Kelson-Renderer-Version"
)

// Message is a kelson commit message: a human subject plus machine-readable
// trailers. Nothing downstream parses the subject.
type Message struct {
	Subject         string
	Project         string
	Environment     string
	SpecHash        string
	Revision        string
	RendererVersion string

	// Extra trailers, emitted after the standard ones in key order.
	Extra map[string]string
}

// String renders the commit message: subject, blank line, trailers.
func (m Message) String() string {
	var b strings.Builder
	subject := strings.TrimSpace(m.Subject)
	if subject == "" {
		subject = "kelson: update rendered manifests"
	}
	b.WriteString(subject)

	ordered := []struct{ k, v string }{
		{TrailerProject, m.Project},
		{TrailerEnvironment, m.Environment},
		{TrailerSpecHash, m.SpecHash},
		{TrailerRevision, m.Revision},
		{TrailerRenderer, m.RendererVersion},
	}
	trailers := make([]string, 0, len(ordered)+len(m.Extra))
	for _, kv := range ordered {
		if kv.v != "" {
			trailers = append(trailers, kv.k+": "+kv.v)
		}
	}
	keys := make([]string, 0, len(m.Extra))
	for k := range m.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := m.Extra[k]; v != "" {
			trailers = append(trailers, k+": "+v)
		}
	}
	if len(trailers) > 0 {
		b.WriteString("\n\n")
		b.WriteString(strings.Join(trailers, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// withIdentity returns a copy carrying the identity's attribution trailers.
func (m Message) withIdentity(id Identity) Message {
	out := m
	out.Extra = map[string]string{}
	for k, v := range m.Extra {
		out.Extra[k] = v
	}
	for k, v := range id.trailers() {
		out.Extra[k] = v
	}
	return out
}

// parseTrailers extracts "Key: value" trailers from a commit message body.
func parseTrailers(msg string) (subject string, trailers map[string]string) {
	trailers = map[string]string{}
	lines := strings.Split(msg, "\n")
	if len(lines) > 0 {
		subject = strings.TrimSpace(lines[0])
	}
	for _, ln := range lines[1:] {
		ln = strings.TrimSpace(ln)
		k, v, ok := strings.Cut(ln, ": ")
		if !ok || strings.ContainsAny(k, " \t") {
			continue
		}
		trailers[k] = strings.TrimSpace(v)
	}
	return subject, trailers
}

// expand substitutes {{key}} placeholders, so PR titles and bodies are
// configurable without a template engine.
func expand(tmpl string, vars map[string]string) string {
	out := tmpl
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = strings.ReplaceAll(out, fmt.Sprintf("{{%s}}", k), vars[k])
	}
	return out
}
