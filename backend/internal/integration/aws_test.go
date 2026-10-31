//go:build aws

// Package integration runs the real CLI and API against a deployed stack's
// S3 buckets and DynamoDB table (the in-memory store accepts things AWS
// rejects). Run with:
//
//	AWS_PROFILE=admin STRATA_DATA_BUCKET=... STRATA_AUDIT_BUCKET=... STRATA_TABLE=... \
//	  go test -tags aws -count=1 -v ./internal/integration/
//
// Everything is created under a unique prefix. Audit records written to the
// Object Lock bucket stay locked for the bucket's retention period.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/aws-abaschen/strata/backend/internal/api"
	"github.com/aws-abaschen/strata/backend/internal/cli"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

func env(t *testing.T, k string) string {
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("%s not set", k)
	}
	return v
}

type harness struct {
	t   *testing.T
	srv *httptest.Server
}

func (h *harness) run(dir, user string, args ...string) (string, int) {
	var out bytes.Buffer
	code := cli.Run(context.Background(), args, cli.Env{Dir: dir, Out: &out, Getenv: func(k string) string {
		switch k {
		case "STRATA_API":
			return h.srv.URL
		case "STRATA_TOKEN":
			return user
		}
		return ""
	}})
	return out.String(), code
}

func (h *harness) ok(dir, user string, args ...string) string {
	h.t.Helper()
	out, code := h.run(dir, user, args...)
	if code != 0 {
		h.t.Fatalf("strata %v: exit %d\n%s", args, code, out)
	}
	return out
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeployedStack(t *testing.T) {
	dataBucket, auditBucket, table := env(t, "STRATA_DATA_BUCKET"), env(t, "STRATA_AUDIT_BUCKET"), env(t, "STRATA_TABLE")
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("eu-west-1"))
	if err != nil {
		t.Fatal(err)
	}
	s3c := s3.NewFromConfig(cfg, func(o *s3.Options) { o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired })
	a := &api.API{
		Objects:       &store.S3{Client: s3c, Presign: s3.NewPresignClient(s3c), Bucket: dataBucket},
		Meta:          &store.Dynamo{Client: dynamodb.NewFromConfig(cfg), Table: table},
		PolicyRepo:    fmt.Sprintf("it-nopolicy-%d", time.Now().UnixNano()), // no policy repo: bootstrap mode
		OpenBootstrap: true,
	}
	handler := a.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.Header.Get("authorization"), "Bearer ")
		handler.ServeHTTP(w, r.WithContext(api.WithPrincipal(r.Context(), api.ParseDevToken(id))))
	}))
	defer srv.Close()
	h := &harness{t: t, srv: srv}

	id := fmt.Sprintf("it%d", time.Now().Unix())
	layer, app := id+"-rules", id+"-app"
	const ana, bo = "it-ana@example.com", "it-bo@example.com"
	ticket := fmt.Sprintf("ITTEST-%d", time.Now().Unix()%900000+100000) // Jira keys allow at most 7 digits

	// Layer repository with a locked file.
	lay := t.TempDir()
	h.ok(lay, ana, "repo", "create", layer, "--layer")
	h.ok(lay, ana, "init", "--repo", layer)
	write(t, lay, map[string]string{
		"strata.layer.json": `{"rules":[{"path":"ci/**","mode":"locked"},{"path":"AGENTS.md","mode":"append"}]}`,
		"ci/pipeline.yml":   "steps: [scan]\n",
		"AGENTS.md":         "org rule\n",
	})
	h.ok(lay, ana, "push", "-m", ticket+" layer baseline", "--label", "it:test")

	// App extends it: clone merges the Layer, push stores only its own part.
	h.ok(t.TempDir(), ana, "repo", "create", app, "--extends", layer)
	a1 := filepath.Join(t.TempDir(), "a1")
	out := h.ok(t.TempDir(), ana, "clone", app, a1)
	if !strings.Contains(out, "2 locked") && !strings.Contains(out, "(2 locked") {
		t.Logf("clone output: %s", out)
	}
	write(t, a1, map[string]string{"main.go": "package main\n", "AGENTS.md": "org rule\napp rule\n"})
	h.ok(a1, ana, "push", "-m", ticket+" app skeleton", "--label", "it:test", "--attr", "approver=bo")

	// Second user: stale push, pull, push.
	a2 := filepath.Join(t.TempDir(), "a2")
	h.ok(t.TempDir(), bo, "clone", app, a2)
	write(t, a2, map[string]string{"other.go": "package main\n"})
	h.ok(a2, bo, "push", "-m", ticket+" bo adds a file")
	write(t, a1, map[string]string{"main.go": "package main\n// v2\n"})
	if _, code := h.run(a1, ana, "push", "-m", ticket+" stale"); code == 0 {
		t.Fatal("stale push must be rejected")
	}
	h.ok(a1, ana, "pull")
	h.ok(a1, ana, "push", "-m", ticket+" ana after pull", "--amend")

	// Locked file: server-side rejection through the API.
	os.Chmod(filepath.Join(a1, "ci", "pipeline.yml"), 0o644)
	write(t, a1, map[string]string{"ci/pipeline.yml": "steps: []\n"})
	if out, code := h.run(a1, ana, "push", "-m", ticket+" skip scan"); code == 0 {
		t.Fatalf("locked file edit must fail:\n%s", out)
	}
	h.ok(a1, ana, "layers", "pull")

	// Settings path: detach and re-attach (the DynamoDB UpdateRepo transaction).
	h.ok(a1, ana, "layers", "unset")
	h.ok(a1, ana, "layers", "set", layer)

	// Work tracking and search.
	h.ok(a1, bo, "annotate", "--work-item", "SEC-"+ticket[7:], "--note", "reviewed")
	if !strings.Contains(h.ok(t.TempDir(), bo, "find", "--work-item", ticket), app) {
		t.Fatal("find by work item should list the app")
	}
	if !strings.Contains(h.ok(t.TempDir(), bo, "find", "--label", "it:test"), app) {
		t.Fatal("find by label should list the app")
	}
	if !strings.Contains(h.ok(lay, ana, "layers", "fleet"), app) {
		t.Fatal("fleet should list the app")
	}

	// Ledger: chain verifies on the client...
	if out := h.ok(a1, ana, "audit", "--verify"); !strings.Contains(out, "verified") {
		t.Fatalf("audit: %s", out)
	}
	// ...and the stream Lambda archived record 1 under Object Lock.
	key := fmt.Sprintf("audit/%s/%012d.json", app, 1)
	deadline := time.Now().Add(90 * time.Second)
	for {
		_, err := s3c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &auditBucket, Key: &key})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ledger Lambda did not archive %s within 90s: %v", key, err)
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("ok: repos %s and %s", layer, app)
}
