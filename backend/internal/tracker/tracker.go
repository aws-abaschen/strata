// Package tracker pushes Strata changes to external work trackers. It runs as
// a Lambda subscribed to the event bus, so the API never waits on a tracker
// and a tracker outage never blocks a push.
package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aws-abaschen/strata/backend/internal/api"
	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Linker records a change on one work item in an external system. Links must
// be idempotent: events are delivered at least once.
type Linker interface {
	Link(ctx context.Context, item core.WorkItem, ev api.ChangeEvent) error
}

// Tracker routes events to the linker of each work item's system.
type Tracker struct {
	Linkers map[string]Linker
	Log     *slog.Logger
}

// Handle processes one EventBridge event. A returned error makes Lambda retry
// the whole event, which is safe because links are idempotent.
func (t *Tracker) Handle(ctx context.Context, e events.EventBridgeEvent) error {
	var ev api.ChangeEvent
	if err := json.Unmarshal(e.Detail, &ev); err != nil {
		return fmt.Errorf("event %s: %w", e.ID, err)
	}
	var errs []error
	for _, w := range ev.Metadata.WorkItems {
		l, ok := t.Linkers[w.System]
		if !ok {
			continue
		}
		if err := l.Link(ctx, w, ev); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w, err))
		}
	}
	if len(errs) > 0 && t.Log != nil {
		t.Log.ErrorContext(ctx, "link failed", "event", e.ID, "err", errors.Join(errs...))
	}
	return errors.Join(errs...)
}

// JiraConfig is the JSON stored in Secrets Manager.
type JiraConfig struct {
	BaseURL  string `json:"baseUrl"` // https://your-org.atlassian.net
	Email    string `json:"email"`
	APIToken string `json:"apiToken"`
}

// Jira adds a remote link ("Strata change") to the issue. The link's
// globalId is the repo and revision, so a retry updates instead of duplicating.
type Jira struct {
	Config JiraConfig
	HTTP   *http.Client
	Log    *slog.Logger
}

// Link implements Linker.
func (j *Jira) Link(ctx context.Context, item core.WorkItem, ev api.ChangeEvent) error {
	title := fmt.Sprintf("%s %s: %s", ev.Repo, ev.Revision.Short(), ev.Subject)
	summary := "By " + ev.Author
	if ev.Via == "annotation" {
		summary = "Linked by annotation"
		if ev.Note != "" {
			summary += ": " + ev.Note
		}
	}
	link := ev.URL
	if link == "" {
		link = "strata://" + ev.Repo + "/" + string(ev.Revision)
	}
	body, _ := json.Marshal(map[string]any{
		"globalId":     "strata:" + ev.Repo + ":" + string(ev.Revision),
		"application":  map[string]string{"type": "io.strata", "name": "Strata"},
		"relationship": "changed by",
		"object":       map[string]any{"url": link, "title": title, "summary": summary},
	})
	u := strings.TrimRight(j.Config.BaseURL, "/") + "/rest/api/3/issue/" + url.PathEscape(item.Key) + "/remotelink"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(j.Config.Email, j.Config.APIToken)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json")
	c := j.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode/100 == 2:
		return nil
	case res.StatusCode == http.StatusNotFound:
		// The key looked like Jira but no such issue exists (or no access).
		// Retrying will not help, so log and move on.
		if j.Log != nil {
			j.Log.WarnContext(ctx, "jira issue not found", "key", item.Key, "repo", ev.Repo, "revision", ev.Revision)
		}
		return nil
	default:
		b, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		return fmt.Errorf("jira %s: %d %s", item.Key, res.StatusCode, b)
	}
}
