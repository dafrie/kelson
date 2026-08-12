package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dafrie/kelson/internal/delivery"
)

// ClientConfig configures the Argo CD REST client.
type ClientConfig struct {
	// BaseURL is the Argo CD API server, e.g. https://argocd.example.com or
	// http://argocd-server.argocd.svc from inside the cluster.
	BaseURL string
	// Token is an Argo CD API token (bearer). It needs "applications, get" and
	// "applications, sync" on the target Application; see the package doc.
	Token string
	// Namespace is the namespace the Applications live in. It is only needed
	// for apps-in-any-namespace installs, where it becomes the appNamespace
	// query/body parameter; leave empty for the usual single-namespace install.
	Namespace string
	// Prune lets the triggered sync delete resources kelson no longer renders.
	// It is off by default: the git writer already pruned the files, and
	// deleting live resources is a decision an operator makes once, in the
	// Application's sync policy, not something kelson turns on implicitly.
	Prune bool
	// HTTP is optional; tests inject an httptest client.
	HTTP interface {
		Do(*http.Request) (*http.Response, error)
	}
}

// Client talks to the Argo CD REST API. It is both the ApplicationReader and
// the Syncer: Argo exposes everything kelson needs over HTTP, so this adapter
// needs no Kubernetes client and no kubeconfig.
type Client struct {
	cfg ClientConfig
}

// NewClient validates the configuration and returns a Client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, delivery.ApplyFailed("argocd/config", "server",
			"no Argo CD API server is configured",
			"set the Argo CD base URL (e.g. https://argocd.example.com)")
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	return &Client{cfg: cfg}, nil
}

// applicationJSON is the subset of the Argo Application API kelson reads. Only
// these fields are parsed: everything else about the Application is Argo's
// business, and depending on more of it would make the adapter brittle across
// Argo versions.
type applicationJSON struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Project    string       `json:"project"`
		Source     *sourceJSON  `json:"source"`
		Sources    []sourceJSON `json:"sources"`
		SyncPolicy struct {
			Automated *struct {
				Prune    bool `json:"prune"`
				SelfHeal bool `json:"selfHeal"`
			} `json:"automated"`
		} `json:"syncPolicy"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"health"`
		OperationState struct {
			Phase      string `json:"phase"`
			Message    string `json:"message"`
			SyncResult struct {
				Revision string `json:"revision"`
			} `json:"syncResult"`
			Operation struct {
				Sync struct {
					Revision string `json:"revision"`
				} `json:"sync"`
			} `json:"operation"`
		} `json:"operationState"`
		Conditions []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

type sourceJSON struct {
	RepoURL        string `json:"repoURL"`
	Path           string `json:"path"`
	TargetRevision string `json:"targetRevision"`
}

// Application implements ApplicationReader.
func (c *Client) Application(ctx context.Context, name string) (Application, bool, error) {
	u := c.cfg.BaseURL + "/api/v1/applications/" + url.PathEscape(name)
	if c.cfg.Namespace != "" {
		u += "?appNamespace=" + url.QueryEscape(c.cfg.Namespace)
	}
	resp, body, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Application{}, false, readErr(name, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Application{}, false, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// Argo answers 403 both for "no such app" and for "not allowed to see
		// it" when RBAC hides existence; the remediation covers both.
		return Application{}, false, authErr("read", name, resp.StatusCode, body)
	case resp.StatusCode >= 300:
		return Application{}, false, readErr(name, fmt.Errorf("argo CD returned HTTP %d: %s", resp.StatusCode, snippet(body)))
	}

	var raw applicationJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return Application{}, false, readErr(name, err)
	}
	return raw.toApplication(), true, nil
}

func (raw applicationJSON) toApplication() Application {
	app := Application{
		Name:           raw.Metadata.Name,
		Namespace:      raw.Metadata.Namespace,
		Project:        raw.Spec.Project,
		SyncStatus:     SyncStatus(raw.Status.Sync.Status),
		SyncedRevision: raw.Status.Sync.Revision,
		Health:         HealthStatus(raw.Status.Health.Status),
		HealthMessage:  raw.Status.Health.Message,
		Operation: Operation{
			Phase:    OperationPhase(raw.Status.OperationState.Phase),
			Message:  raw.Status.OperationState.Message,
			Revision: raw.Status.OperationState.SyncResult.Revision,
		},
	}
	if app.Operation.Revision == "" {
		app.Operation.Revision = raw.Status.OperationState.Operation.Sync.Revision
	}
	// Multi-source Applications keep spec.sources instead of spec.source; the
	// first source is the one a kelson delivery target maps to.
	src := raw.Spec.Source
	if src == nil && len(raw.Spec.Sources) > 0 {
		src = &raw.Spec.Sources[0]
	}
	if src != nil {
		app.RepoURL, app.Path, app.TargetRevision = src.RepoURL, src.Path, src.TargetRevision
	}
	if auto := raw.Spec.SyncPolicy.Automated; auto != nil {
		app.AutoSync, app.SelfHeal = true, auto.SelfHeal
	}
	for _, cond := range raw.Status.Conditions {
		app.Conditions = append(app.Conditions, Condition{Type: cond.Type, Message: cond.Message})
	}
	if app.SyncStatus == "" {
		app.SyncStatus = SyncUnknown
	}
	if app.Health == "" {
		app.Health = HealthUnknown
	}
	return app
}

// syncRequest is Argo's ApplicationSyncRequest. Passing the committed revision
// explicitly is what makes the trigger deterministic: Argo syncs the sha kelson
// just wrote, fetching it if the repo-server has not seen it yet, rather than
// whatever the branch happens to point at by the time the request lands.
type syncRequest struct {
	Name         string `json:"name"`
	Revision     string `json:"revision,omitempty"`
	Prune        bool   `json:"prune"`
	DryRun       bool   `json:"dryRun"`
	AppNamespace string `json:"appNamespace,omitempty"`
}

// Sync implements Syncer.
func (c *Client) Sync(ctx context.Context, app Application, revision string) error {
	payload, err := json.Marshal(syncRequest{
		Name:         app.Name,
		Revision:     revision,
		Prune:        c.cfg.Prune,
		AppNamespace: c.cfg.Namespace,
	})
	if err != nil {
		return syncErr(app, err)
	}
	u := c.cfg.BaseURL + "/api/v1/applications/" + url.PathEscape(app.Name) + "/sync"
	resp, body, err := c.do(ctx, http.MethodPost, u, payload)
	if err != nil {
		return syncErr(app, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return authErr("sync", app.Name, resp.StatusCode, body)
	case resp.StatusCode >= 300:
		return syncErr(app, fmt.Errorf("argo CD returned HTTP %d: %s", resp.StatusCode, snippet(body)))
	}
	return nil
}

// do performs one API call and reads the (bounded) response body. The body is
// read here because every caller needs it for the error message.
func (c *Client) do(ctx context.Context, method, u string, payload []byte) (*http.Response, []byte, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kelson")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}

	client := c.cfg.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

func authErr(op, name string, status int, body []byte) error {
	e := delivery.ApplyFailed("argocd/api", "token",
		fmt.Sprintf("Argo CD refused to %s Application %q (HTTP %d)", op, name, status),
		"check the kelson API token and its Argo CD RBAC policy; it needs at least "+
			"`p, role:kelson, applications, get, <project>/<app>, allow` and "+
			"`p, role:kelson, applications, sync, <project>/<app>, allow`")
	e.Cause = snippet(body)
	return e
}

func readErr(name string, err error) error {
	e := delivery.ApplyFailed("argocd/status", "",
		fmt.Sprintf("could not read Argo CD Application %q", name),
		"check the Argo CD API server URL, network reachability and the API token")
	e.Cause = err.Error()
	return e
}

// snippet trims an API error body down to something an error message can carry.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	// Argo's gRPC gateway wraps errors as {"error":"...","code":7}.
	var wrapped struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil {
		if wrapped.Error != "" {
			s = wrapped.Error
		} else if wrapped.Message != "" {
			s = wrapped.Message
		}
	}
	if len(s) > 512 {
		s = s[:512] + "…"
	}
	return s
}

var (
	_ ApplicationReader = (*Client)(nil)
	_ Syncer            = (*Client)(nil)
)
