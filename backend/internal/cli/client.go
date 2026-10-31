// Package cli is the Strata client: an API client, working-copy snapshots and
// the commands behind the strata binary.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/layers"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// Client calls the Strata API.
type Client struct {
	API   string
	Token string
	HTTP  *http.Client
}

// APIError is a non-2xx answer.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Message) }

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.API, "/")+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("authorization", "Bearer "+c.Token)
	}
	res, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(res.Body)
		json.Unmarshal(b, &e)
		msg := e.Error + e.Message
		if msg == "" {
			msg = strings.TrimSpace(string(b))
		}
		return &APIError{Status: res.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func esc(s string) string { return url.PathEscape(s) }

// CreateRepo creates a repository.
func (c *Client) CreateRepo(ctx context.Context, name, desc string) (core.Repo, error) {
	return c.CreateRepoWith(ctx, NewRepo{Name: name, Description: desc})
}

// NewRepo is the body of a create-repository call.
type NewRepo struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Kind        core.RepoKind  `json:"kind,omitempty"`
	Extends     *core.LayerRef `json:"extends,omitempty"`
}

// CreateRepoWith creates a repository with a kind or a Layer to extend.
func (c *Client) CreateRepoWith(ctx context.Context, in NewRepo) (core.Repo, error) {
	var r core.Repo
	err := c.do(ctx, http.MethodPost, "/repos", in, &r)
	return r, err
}

// GetRepo reads a repository.
func (c *Client) GetRepo(ctx context.Context, name string) (core.Repo, error) {
	var r core.Repo
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(name), nil, &r)
	return r, err
}

// SetExtends points a repository at a Layer, or detaches it (nil). It
// returns files in the repository's main that the new Layer locks.
func (c *Client) SetExtends(ctx context.Context, repo string, l *core.LayerRef) ([]layers.Violation, error) {
	var out struct{ Violations []layers.Violation }
	err := c.do(ctx, http.MethodPut, "/repos/"+esc(repo)+"/settings", map[string]any{"extends": l}, &out)
	return out.Violations, err
}

// Layers resolves a repository's configuration. own adds the repository's
// files at ref as the last level.
func (c *Client) Layers(ctx context.Context, repo, ref string, own bool) (layers.Effective, error) {
	var out layers.Effective
	q := "?ref=" + url.QueryEscape(ref)
	if !own {
		q = "?own=false"
	}
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(repo)+"/layers"+q, nil, &out)
	return out, err
}

// Subscriber is one repository inheriting from a Layer.
type Subscriber struct {
	Repo  core.Repo `json:"repo"`
	Via   string    `json:"via"`
	Depth int       `json:"depth"`
}

// Subscribers lists a Layer's fleet.
func (c *Client) Subscribers(ctx context.Context, layer string) ([]Subscriber, error) {
	var out struct{ Subscribers []Subscriber }
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(layer)+"/subscribers", nil, &out)
	return out.Subscribers, err
}

// ListRepos lists repositories.
func (c *Client) ListRepos(ctx context.Context) ([]core.Repo, error) {
	var out struct{ Repos []core.Repo }
	err := c.do(ctx, http.MethodGet, "/repos", nil, &out)
	return out.Repos, err
}

// ObjectSize is one entry of a negotiate request.
type ObjectSize struct {
	Hash core.Hash `json:"hash"`
	Size int64     `json:"size"`
}

// Negotiate returns uploads for the objects the server is missing.
func (c *Client) Negotiate(ctx context.Context, repo string, objs []ObjectSize) ([]store.Upload, error) {
	var out struct{ Uploads []store.Upload }
	err := c.do(ctx, http.MethodPost, "/repos/"+esc(repo)+"/objects/negotiate", map[string]any{"objects": objs}, &out)
	return out.Uploads, err
}

// Upload sends one object to its presigned URL. A 412 means another client
// stored the same content first, which is fine.
func (c *Client) Upload(ctx context.Context, u store.Upload, body io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, u.Method, u.URL, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	for k, v := range u.Headers {
		if strings.EqualFold(k, "content-length") {
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 && res.StatusCode != http.StatusPreconditionFailed {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("upload %s: %d %s", u.Hash.Short(), res.StatusCode, b)
	}
	return nil
}

// Download fetches an object's bytes.
func (c *Client) Download(ctx context.Context, repo string, h core.Hash) (io.ReadCloser, error) {
	var out struct{ URL string }
	if err := c.do(ctx, http.MethodGet, "/repos/"+esc(repo)+"/objects/"+string(h), nil, &out); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, out.URL, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("download %s: %d", h.Short(), res.StatusCode)
	}
	return res.Body, nil
}

// NewChange is the body of a create-change call.
type NewChange struct {
	ChangeID    string        `json:"changeId,omitempty"`
	Tree        core.Hash     `json:"tree"`
	Parents     []core.Hash   `json:"parents"`
	Message     string        `json:"message"`
	Metadata    core.Metadata `json:"metadata"`
	Ref         string        `json:"ref,omitempty"`
	ExpectedRef *core.Hash    `json:"expectedRef,omitempty"`
}

// CreateChange writes a change.
func (c *Client) CreateChange(ctx context.Context, repo string, in NewChange) (core.Change, error) {
	var out core.Change
	err := c.do(ctx, http.MethodPost, "/repos/"+esc(repo)+"/changes", in, &out)
	return out, err
}

// ChangeDetail is a change with its annotations.
type ChangeDetail struct {
	Change            core.Change       `json:"change"`
	Annotations       []core.Annotation `json:"annotations"`
	EffectiveMetadata core.Metadata     `json:"effectiveMetadata"`
	LatestRevision    core.Hash         `json:"latestRevision"`
}

// GetChange reads a change.
func (c *Client) GetChange(ctx context.Context, repo string, rev core.Hash) (ChangeDetail, error) {
	var out ChangeDetail
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(repo)+"/changes/"+string(rev), nil, &out)
	return out, err
}

// GetRef reads one ref, by listing (refs may contain slashes).
func (c *Client) GetRef(ctx context.Context, repo, name string) (core.Ref, bool, error) {
	var out struct{ Refs []core.Ref }
	if err := c.do(ctx, http.MethodGet, "/repos/"+esc(repo)+"/refs", nil, &out); err != nil {
		return core.Ref{}, false, err
	}
	for _, r := range out.Refs {
		if r.Name == name {
			return r, true, nil
		}
	}
	return core.Ref{}, false, nil
}

// Log follows first parents from a ref.
func (c *Client) Log(ctx context.Context, repo, ref string, n int) ([]core.Change, error) {
	var out struct{ Changes []core.Change }
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/log?ref=%s&limit=%d", esc(repo), url.QueryEscape(ref), n), nil, &out)
	return out.Changes, err
}

// Annotate adds metadata to a change.
func (c *Client) Annotate(ctx context.Context, repo string, rev core.Hash, note string, md core.Metadata) (core.Annotation, error) {
	var out core.Annotation
	err := c.do(ctx, http.MethodPost, "/repos/"+esc(repo)+"/changes/"+string(rev)+"/annotations", map[string]any{"note": note, "metadata": md}, &out)
	return out, err
}

// FindWorkItem lists changes linked to a work item, across repositories.
func (c *Client) FindWorkItem(ctx context.Context, w core.WorkItem) ([]core.ChangeSummary, error) {
	var out struct{ Changes []core.ChangeSummary }
	err := c.do(ctx, http.MethodGet, "/work-items/"+esc(w.System)+"/"+esc(w.Key), nil, &out)
	return out.Changes, err
}

// FindLabel lists changes with a label, across repositories.
func (c *Client) FindLabel(ctx context.Context, label string) ([]core.ChangeSummary, error) {
	var out struct{ Changes []core.ChangeSummary }
	err := c.do(ctx, http.MethodGet, "/labels/"+label, nil, &out)
	return out.Changes, err
}

// AuditPage is one page of a repository's ledger.
type AuditPage struct {
	Records  []core.AuditRecord `json:"records"`
	Verified bool               `json:"verified"`
	Error    string             `json:"error"`
	Next     *int64             `json:"next"`
}

// Audit returns every ledger record of a repository, in order.
func (c *Client) Audit(ctx context.Context, repo string) ([]core.AuditRecord, error) {
	var all []core.AuditRecord
	var after int64
	for {
		var p AuditPage
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/audit?after=%d", esc(repo), after), nil, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Records...)
		if p.Next == nil {
			return all, nil
		}
		after = *p.Next
	}
}
