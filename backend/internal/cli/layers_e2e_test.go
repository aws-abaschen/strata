package cli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-abaschen/strata/backend/internal/cli"
	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/layers"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLayersEndToEnd(t *testing.T) {
	h := newHarness(t)
	const admin, lead, dev = "platform@example.com", "lead@example.com", "dev@example.com"

	// Org layer: CI is locked, AGENTS.md is append-only, the rest overridable.
	org := t.TempDir()
	h.ok(org, admin, "repo", "create", "org-rules", "--layer", "-d", "Company rules")
	h.ok(org, admin, "init", "--repo", "org-rules")
	write(t, org, map[string]string{
		"strata.layer.json": `{"rules":[{"path":"ci/**","mode":"locked"},{"path":"AGENTS.md","mode":"append"}]}`,
		"ci/pipeline.yml":   "steps: [secret-scan, test]\n",
		"AGENTS.md":         "Org: never commit secrets.",
		".editorconfig":     "indent_style = tab\n",
	})
	h.ok(org, admin, "push", "-m", "OPS-1 org baseline")

	// Team layer extends the org layer.
	team := t.TempDir()
	h.ok(team, lead, "repo", "create", "team-rules", "--layer", "--extends", "org-rules")
	h.ok(team, lead, "init", "--repo", "team-rules")
	write(t, team, map[string]string{"AGENTS.md": "Team: use the Money type for amounts.\n"})
	h.ok(team, lead, "push", "-m", "OPS-2 team rules")

	// An app extends the team layer; clone writes the inherited files.
	h.ok(t.TempDir(), dev, "repo", "create", "cards", "--extends", "team-rules")
	app := filepath.Join(t.TempDir(), "cards")
	out := h.ok(t.TempDir(), dev, "clone", "cards", app)
	if !strings.Contains(out, "3 files (1 locked") {
		t.Fatalf("clone output:\n%s", out)
	}
	if got := read(t, filepath.Join(app, "AGENTS.md")); got != "Org: never commit secrets.\nTeam: use the Money type for amounts.\n" {
		t.Fatalf("AGENTS.md = %q", got)
	}

	// Repo-level edits: append a line, override the editorconfig.
	agents := filepath.Join(app, "AGENTS.md")
	os.WriteFile(agents, []byte(read(t, agents)+"Repo: cards use PAN tokens only.\n"), 0o644)
	write(t, app, map[string]string{".editorconfig": "indent_style = space\n", "main.go": "package main\n"})
	h.ok(app, dev, "push", "-m", "CARD-1 first code")

	// The repo stores only its own contribution; a fresh clone merges again.
	d, _ := (&cli.Client{API: h.srv.URL, Token: dev}).Layers(context.Background(), "cards", "main", true)
	for _, f := range d.Files {
		if f.Path == "AGENTS.md" && (len(f.Parts) != 3 || f.Parts[2].Source != "cards") {
			t.Fatalf("AGENTS.md parts %+v", f.Parts)
		}
		if f.Path == ".editorconfig" && (len(f.Parts) != 1 || f.Parts[0].Source != "cards") {
			t.Fatalf(".editorconfig parts %+v", f.Parts)
		}
	}
	app2 := filepath.Join(t.TempDir(), "cards2")
	h.ok(t.TempDir(), dev, "clone", "cards", app2)
	if read(t, filepath.Join(app2, "AGENTS.md")) != read(t, agents) || read(t, filepath.Join(app2, ".editorconfig")) != "indent_style = space\n" {
		t.Fatal("second clone differs from the working copy that pushed")
	}
	if !strings.Contains(h.ok(app, dev, "status"), "clean") {
		t.Fatal("working copy should be clean after push")
	}

	// Editing a locked file is refused by the CLI...
	pipe := filepath.Join(app, "ci", "pipeline.yml")
	os.Chmod(pipe, 0o644)
	os.WriteFile(pipe, []byte("steps: [test]\n"), 0o644)
	if out, code := h.run(app, dev, "push", "-m", "CARD-2 skip scan"); code == 0 || !strings.Contains(out, "locked by layer org-rules") {
		t.Fatalf("locked edit should fail, got %d:\n%s", code, out)
	}
	h.ok(app, dev, "layers", "pull")
	if read(t, pipe) != "steps: [secret-scan, test]\n" {
		t.Fatal("pull should restore the locked file")
	}

	// ...and by the server, whatever the client does.
	c := &cli.Client{API: h.srv.URL, Token: dev}
	ctx := context.Background()
	bad := h.mem.Put("cards", []byte("steps: []"))
	ci, _, _ := core.Tree{Entries: []core.TreeEntry{{Name: "pipeline.yml", Kind: core.KindFile, Hash: bad}}}.Encode()
	ciHash := h.mem.Put("cards", ci)
	root, _, _ := core.Tree{Entries: []core.TreeEntry{{Name: "ci", Kind: core.KindDir, Hash: ciHash}}}.Encode()
	st, _, _ := cli.LoadState(app)
	_, err := c.CreateChange(ctx, "cards", cli.NewChange{Tree: h.mem.Put("cards", root), Parents: []core.Hash{st.Head}, Message: "sneaky", Ref: "main"})
	if ae, ok := err.(*cli.APIError); !ok || ae.Status != 403 || !strings.Contains(ae.Message, "ci/pipeline.yml") {
		t.Fatalf("want 403 policy, got %v", err)
	}

	// The org updates its rules; pulling keeps the repo's own line.
	write(t, org, map[string]string{"AGENTS.md": "Org: never commit secrets, ever."})
	h.ok(org, admin, "push", "-m", "OPS-3 stronger wording")
	h.ok(app, dev, "layers", "pull")
	if got := read(t, agents); got != "Org: never commit secrets, ever.\nTeam: use the Money type for amounts.\nRepo: cards use PAN tokens only.\n" {
		t.Fatalf("after org update AGENTS.md = %q", got)
	}

	// Fleet view and show.
	fleet := h.ok(org, admin, "layers", "fleet")
	if !strings.Contains(fleet, "team-rules (layer) via org-rules") || !strings.Contains(fleet, "  cards (code) via team-rules") {
		t.Fatalf("fleet:\n%s", fleet)
	}
	show := h.ok(app, dev, "layers", "show")
	if !strings.Contains(show, "locked") || !strings.Contains(show, "org-rules@main") {
		t.Fatalf("show:\n%s", show)
	}

	// Agents read their effective rules, rendered, in one call.
	req, _ := http.NewRequest("GET", h.srv.URL+"/repos/cards/layers?content=true", nil)
	req.Header.Set("authorization", "Bearer agent@example.com")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var eff layers.Effective
	json.NewDecoder(res.Body).Decode(&eff)
	res.Body.Close()
	for _, f := range eff.Files {
		if f.Path == "AGENTS.md" && (f.Content == nil || !strings.HasSuffix(*f.Content, "Repo: cards use PAN tokens only.\n")) {
			t.Fatalf("rendered AGENTS.md: %v", f.Content)
		}
	}

	// Guard rails: no cycles, and only Layers can be extended.
	if out, code := h.run(org, admin, "layers", "set", "team-rules"); code == 0 || !strings.Contains(out, "cycle") {
		t.Fatalf("cycle should be refused, got %d:\n%s", code, out)
	}
	if out, code := h.run(org, admin, "repo", "create", "other", "--extends", "cards"); code == 0 || !strings.Contains(out, "not a layer") {
		t.Fatalf("extending a code repo should be refused, got %d:\n%s", code, out)
	}
}
