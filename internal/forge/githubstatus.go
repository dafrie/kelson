package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The [StatusReporter] half: the write-back ADR-0034 decision 5 describes — a
// commit status per publish, and *one* pull-request comment per preview,
// edited in place. The comment being upserted rather than appended is the
// whole point: a comment per push turns a busy pull request into a wall of
// kelson, which is the behaviour every reviewer has learned to mute.

// statusStates is the vocabulary GitHub accepts. Anything else is refused here
// rather than sent, because the API's rejection ("Validation Failed") names
// neither the field nor the value.
var statusStates = map[string]bool{"pending": true, "success": true, "failure": true, "error": true}

// defaultStatusContext is the check name a status appears under when the
// caller does not choose one. It matters that it is stable: GitHub keys
// statuses by context, so a changing one leaves a graveyard of stale checks on
// every commit instead of replacing the previous answer.
const defaultStatusContext = "kelson"

// ReportStatus implements [StatusReporter].
func (g *gitHubProvider) ReportStatus(ctx context.Context, c Conn, repoFullName, sha string, s Status) error {
	if !statusStates[s.State] {
		return fmt.Errorf("forge/github: %q is not a commit status state (pending, success, failure, error)", s.State)
	}
	if strings.TrimSpace(sha) == "" {
		return fmt.Errorf("forge/github: a commit status needs the commit it is about")
	}
	base, err := apiBase(c.Host)
	if err != nil {
		return err
	}
	repo, err := repoPath(base, repoFullName)
	if err != nil {
		return err
	}
	authorization, err := g.authorization(ctx, c)
	if err != nil {
		return err
	}

	check := s.Context
	if check == "" {
		check = defaultStatusContext
	}
	body := struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description,omitempty"`
		TargetURL   string `json:"target_url,omitempty"`
	}{State: s.State, Context: check, Description: s.Description, TargetURL: s.TargetURL}

	u := repo + "/statuses/" + url.PathEscape(strings.TrimSpace(sha))
	if _, _, err := g.send(ctx, http.MethodPost, u, authorization, body); err != nil {
		return notInstalled(err)
	}
	return nil
}

// UpsertPRComment implements [StatusReporter].
//
// The marker is how the comment is found again: GitHub has no "my comment on
// this pull request" lookup, so the identity has to live in the body. Callers
// pass an HTML comment (invisible when rendered), and the body written here
// always contains it — a body that lost its marker would be a comment this
// call could never find again, and the next publish would post a second one.
func (g *gitHubProvider) UpsertPRComment(ctx context.Context, c Conn, repoFullName string, pr int, marker, body string) error {
	if strings.TrimSpace(marker) == "" {
		return fmt.Errorf("forge/github: upserting a comment needs a marker to find it by")
	}
	if pr <= 0 {
		return fmt.Errorf("forge/github: %d is not a pull request number", pr)
	}
	base, err := apiBase(c.Host)
	if err != nil {
		return err
	}
	repo, err := repoPath(base, repoFullName)
	if err != nil {
		return err
	}
	authorization, err := g.authorization(ctx, c)
	if err != nil {
		return err
	}

	if !strings.Contains(body, marker) {
		body = marker + "\n" + body
	}
	payload := struct {
		Body string `json:"body"`
	}{Body: body}

	// Pull request comments are issue comments: the same thread, the same
	// endpoint, and the reason the path below says issues for a pull request.
	existing, err := g.findComment(ctx, repo, authorization, pr, marker)
	if err != nil {
		return err
	}
	if existing != 0 {
		u := repo + "/issues/comments/" + strconv.FormatInt(existing, 10)
		if _, _, err := g.send(ctx, http.MethodPatch, u, authorization, payload); err != nil {
			return notInstalled(err)
		}
		return nil
	}

	u := repo + "/issues/" + strconv.Itoa(pr) + "/comments"
	if _, _, err := g.send(ctx, http.MethodPost, u, authorization, payload); err != nil {
		return notInstalled(err)
	}
	return nil
}

// findComment returns the id of the first comment carrying the marker, or 0.
// First rather than last: if a bug ever produced two, editing the oldest keeps
// the conversation's history in the place readers already scrolled past.
func (g *gitHubProvider) findComment(ctx context.Context, repo, authorization string, pr int, marker string) (int64, error) {
	var found int64
	u := repo + "/issues/" + strconv.Itoa(pr) + "/comments?per_page=" + strconv.Itoa(perPage)
	err := g.paginate(ctx, u, authorization, func(data []byte) error {
		var page []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return fmt.Errorf("forge/github: decoding the comments on pull request %d: %w", pr, err)
		}
		for _, comment := range page {
			if strings.Contains(comment.Body, marker) {
				found = comment.ID
				return errStopPaging
			}
		}
		return nil
	})
	if err != nil {
		return 0, notInstalled(err)
	}
	return found, nil
}
