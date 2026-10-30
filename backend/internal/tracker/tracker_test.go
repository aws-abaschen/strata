package tracker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/aws-abaschen/strata/backend/internal/api"
	"github.com/aws-abaschen/strata/backend/internal/core"
)

func TestJiraLinksEveryJiraItem(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "bot@example.com" || pass != "tok" {
			w.WriteHeader(401)
			return
		}
		paths = append(paths, r.URL.Path)
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		if strings.Contains(r.URL.Path, "GONE-1") {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(201)
	}))
	defer srv.Close()

	tr := &Tracker{Linkers: map[string]Linker{"jira": &Jira{Config: JiraConfig{BaseURL: srv.URL, Email: "bot@example.com", APIToken: "tok"}}}}
	rev := core.HashBytes([]byte("x"))
	detail, _ := json.Marshal(api.ChangeEvent{
		ChangeSummary: core.ChangeSummary{Repo: "payments", Revision: rev, Subject: "PAY-42 fix", Author: "ana", Via: "change"},
		Metadata: core.Metadata{WorkItems: []core.WorkItem{
			{System: "jira", Key: "PAY-42"}, {System: "jira", Key: "GONE-1"}, {System: "servicenow", Key: "CHG1"},
		}},
		URL: "https://strata.example.com/repos/payments/changes/" + string(rev),
	})
	if err := tr.Handle(context.Background(), events.EventBridgeEvent{ID: "e1", Detail: detail}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/rest/api/3/issue/PAY-42/remotelink" {
		t.Fatalf("paths %v", paths)
	}
	if bodies[0]["globalId"] != "strata:payments:"+string(rev) {
		t.Fatalf("globalId %v", bodies[0]["globalId"])
	}
}

func TestJiraErrorIsRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	tr := &Tracker{Linkers: map[string]Linker{"jira": &Jira{Config: JiraConfig{BaseURL: srv.URL}}}}
	detail, _ := json.Marshal(api.ChangeEvent{Metadata: core.Metadata{WorkItems: []core.WorkItem{{System: "jira", Key: "PAY-1"}}}})
	if err := tr.Handle(context.Background(), events.EventBridgeEvent{Detail: detail}); err == nil {
		t.Fatal("a 503 must return an error so Lambda retries")
	}
}
