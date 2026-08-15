package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The read half of the distribution API: what a repository holds, and what one
// tag resolves to.
//
// # Why the reads live on [Pusher]
//
// They are the same conversation with the same registry: the same host, the
// same credential, the same 401-challenge-and-token dance, and — for a
// controller that has just published — the same process. A second type with its
// own client would be a second credential story to configure, a second place to
// remember that a token is scoped, and a second thing to get wrong about a
// registry that speaks plain HTTP. So the type keeps the name of what it is
// mostly for and grows the two reads kelson actually needs.
//
// # Why kelson needs to read a registry at all
//
// `Environment.status.history[]` is a bounded mirror — ADR-0028 decision 4
// fixes it at twenty entries — and that ADR is explicit that the mirror "is not
// the record; the record is the registry". A revision that has aged out of the
// mirror is still in the registry, immutably, and a rollback to it is a pointer
// move like any other (issue #241). These two calls are how kelson asks the
// record: [Pusher.Tags] for what exists, [Pusher.Resolve] for whether one
// revision does and which bytes it names.
//
// What they do *not* recover is everything the mirror held besides the tag.
// When a revision was published, which images it ran and how that deployment
// ended are observations of a cluster, and a registry never saw any of them.
// Callers say so rather than presenting an empty field as a fact.

// TagPageSize is how many tags one page of a listing asks for, and MaxTagPages
// is how many pages [Pusher.Tags] walks before it gives up.
//
// The product is the bound on a listing, and it is generous on purpose: two
// thousand tags is two thousand publishes of one environment. A repository past
// that is not truncated silently — [Pusher.Tags] refuses instead — because a
// tag list comes back in the registry's own lexical order, in which "10-…"
// sorts before "9-…", so a truncated walk is an arbitrary subset rather than
// the newest anything. A subset presented as a history would be a list that
// quietly forgets revisions, which is the exact failure this path exists to fix.
const (
	TagPageSize = 200
	MaxTagPages = 10
)

// tagListLimit bounds a tag-list response body. It is large enough for
// TagPageSize tags with room to spare and small enough that an endpoint
// answering with something other than a tag list is an error rather than a
// memory problem.
const tagListLimit = 1 << 20

// Tags lists the tags a repository holds.
//
// The order is the registry's own — the distribution spec says lexical, and
// registries vary — so a caller that wants the newest revision first sorts by
// the tag grammar it owns rather than trusting this slice's order.
//
// A repository the registry does not know is an empty list and not an error: an
// environment that has never published has no artifacts, and that is an answer
// rather than a failure. Every other refusal is the registry's own and comes
// back as [DeniedError] or [UnreachableError], so a caller can tell "there is
// nothing there" from "kelson was not allowed to look".
func (p *Pusher) Tags(ctx context.Context, repository string) ([]string, error) {
	repo, err := parseRepository(repository)
	if err != nil {
		return nil, err
	}
	repo.actions = pullActions

	listing := p.endpoint(repo, "/tags/list")
	next := listing + "?n=" + strconv.Itoa(TagPageSize)
	var tags []string
	for page := 0; next != ""; page++ {
		if page >= MaxTagPages {
			return nil, Error{
				Reason: ReasonTagListTooLarge,
				Message: fmt.Sprintf("%s/%s holds more than %d tags, which is more than kelson lists in one query",
					repo.host, repo.path, MaxTagPages*TagPageSize),
				Remediation: "read them with the registry's own client (`crane ls`, `flux list artifacts`): kelson " +
					"refuses a partial tag list rather than presenting one as complete",
			}
		}
		page, link, err := p.tagPage(ctx, repo, next)
		if err != nil {
			return nil, err
		}
		tags = append(tags, page...)
		next = nextPage(listing, link)
	}
	return tags, nil
}

// tagPage reads one page of a listing: the tags it holds and the Link header
// that says whether another page follows.
func (p *Pusher) tagPage(ctx context.Context, repo target, endpoint string) ([]string, string, error) {
	res, err := p.do(ctx, repo, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return nil, "", err
	}
	defer closeBody(res)
	// A repository the registry has never been told the name of, which is what
	// an environment that has published nothing looks like from here.
	if res.StatusCode == http.StatusNotFound {
		return nil, "", nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, "", responseError(res, "listing the tags of "+repo.host+"/"+repo.path)
	}
	var payload struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, tagListLimit)).Decode(&payload); err != nil {
		return nil, "", Error{
			Reason:      ReasonTagListUnreadable,
			Message:     repo.host + " returned a tag list kelson could not read: " + err.Error(),
			Remediation: "something other than a registry is answering /v2/<name>/tags/list; check what sits in front of it",
		}
	}
	return payload.Tags, res.Header.Get("Link"), nil
}

// Resolve reports what one tag names: the manifest digest the registry serves
// it as, and whether the registry holds it at all.
//
// It is one HEAD, which is why a rollback verifies its target this way rather
// than by listing: the answer is exact, it costs one request whatever the
// repository holds, and it arrives with the digest — so a revision that has
// aged out of the bounded mirror can still be pinned to its bytes and not only
// to the tag pointing at them. ADR-0028 decision 2 writes a tag once and never
// rewrites it, so the two agree forever; the digest is what proves it.
//
// A registry that answers 404 has no such tag: found is false and the error is
// nil, because "there is no revision 3-a1b2c3d4" is an answer. A registry that
// serves the tag without a `Docker-Content-Digest` header is found with an empty
// digest, which is a tag-only pin — the same shape as a mirror entry recorded
// before digests were.
func (p *Pusher) Resolve(ctx context.Context, repository, tag string) (digest string, found bool, err error) {
	repo, err := parseRepository(repository)
	if err != nil {
		return "", false, err
	}
	repo.actions = pullActions
	if strings.TrimSpace(tag) == "" {
		return "", false, Error{
			Reason:      ReasonRepositoryInvalid,
			Message:     "no tag to resolve in " + repo.host + "/" + repo.path,
			Remediation: "name the revision to look up, e.g. 7-a1b2c3d4",
		}
	}

	res, err := p.do(ctx, repo, http.MethodHead, p.endpoint(repo, "/manifests/"+tag), nil, "")
	if err != nil {
		return "", false, err
	}
	defer closeBody(res)
	switch {
	case res.StatusCode == http.StatusNotFound:
		return "", false, nil
	case res.StatusCode != http.StatusOK:
		return "", false, responseError(res, "resolving "+repo.host+"/"+repo.path+":"+tag)
	}
	return strings.TrimSpace(res.Header.Get("Docker-Content-Digest")), true, nil
}

// nextPage reads the Link header a paginating registry sends and resolves it
// against the listing endpoint, which is what makes a relative
// `</v2/x/tags/list?last=…>` — the spelling registries that paginate use — a URL
// this client can follow. An absent or unusable header ends the walk: there is
// no page to ask for, which is the ordinary end of a listing.
func nextPage(listing, link string) string {
	for _, part := range strings.Split(link, ",") {
		value, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.Contains(strings.ToLower(params), `rel="next"`) {
			continue
		}
		ref, err := url.Parse(strings.Trim(strings.TrimSpace(value), "<>"))
		if err != nil || ref.String() == "" {
			return ""
		}
		root, err := url.Parse(listing)
		if err != nil {
			return ""
		}
		return root.ResolveReference(ref).String()
	}
	return ""
}
