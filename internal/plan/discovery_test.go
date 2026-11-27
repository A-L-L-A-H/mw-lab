package plan

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func TestDiscoverStructuredAndMarkdown(t *testing.T) {
	root := t.TempDir()
	writeStructuredPlan(t, root, "structured", "T01")
	writeFile(t, filepath.Join(root, "legacy.md"), `# Legacy Plan

- **Target Branch:** `+"`dev`"+`

## Wave A — Collectors

#### - [x] D01 — Done item
#### - [ ] D02 — Pending item
`)
	summaries, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 plans, got %d", len(summaries))
	}
	if summaries[0].ID != "legacy" || summaries[0].TotalTasks != 2 || summaries[0].DoneTasks != 1 {
		t.Fatalf("unexpected legacy summary: %+v", summaries[0])
	}
	if summaries[1].ID != "structured" || summaries[1].TotalTasks != 1 || summaries[1].ProgressPct != 100 {
		t.Fatalf("unexpected structured summary: %+v", summaries[1])
	}
}

func TestDiscoverNestedAndSkipsWavePlaybooks(t *testing.T) {
	root := t.TempDir()
	nestedDir := filepath.Join(root, "mw-xv")
	writeStructuredPlan(t, nestedDir, "moved-bundle", "T01")
	// Simulate workspace-relative wave_file from before bundle moved into mw-xv/
	writeFile(t, filepath.Join(nestedDir, "moved-bundle", "manifest.json"), `{
  "plan_id": "moved-bundle",
  "title": "Moved Bundle",
  "repo": {"target_branch": "main"},
  "waves": [{"id": "W1", "title": "Wave 1", "wave_file": ".plan/moved-bundle/waves/W1.json", "depends_on": []}]
}`)
	writeFile(t, filepath.Join(nestedDir, "nested-plan.md"), `# Nested Plan

- **Target branch:** dev

### Wave 0 — Init
- [x] **I01** — Done nested
`)
	if err := os.MkdirAll(filepath.Join(nestedDir, "nested-plan"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(nestedDir, "nested-plan", "wave-0-init.md"), `# Wave 0
- [x] **I01** — Wave copy must not surface as separate plan
`)

	summaries, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 nested plans (skipping wave-0-init), got %d: %+v", len(summaries), summaries)
	}
	if summaries[0].ID != "moved-bundle" || summaries[0].TotalTasks != 1 {
		t.Fatalf("unexpected moved-bundle summary: %+v", summaries[0])
	}
	if summaries[1].ID != "nested-plan" || summaries[1].TargetBranch != "dev" || summaries[1].DoneTasks != 1 {
		t.Fatalf("unexpected nested-plan summary: %+v", summaries[1])
	}
}

func TestLoadRejectsTraversal(t *testing.T) {
	if _, err := Load(t.TempDir(), "../escape"); err == nil {
		t.Fatal("expected traversal plan id to be rejected")
	}
}

func TestDiscoverEmptyOrMissingRoot(t *testing.T) {
	if plans, err := Discover(""); err != nil || len(plans) != 0 {
		t.Fatalf("expected empty slice for empty root, got %+v err=%v", plans, err)
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if plans, err := Discover(missing); err != nil || len(plans) != 0 {
		t.Fatalf("expected empty slice for missing root, got %+v err=%v", plans, err)
	}
}

func TestLoadStructuredMergesMarkdownStatus(t *testing.T) {
	root := t.TempDir()
	writeStructuredPlan(t, root, "structured", "T01")
	writeFile(t, filepath.Join(root, "structured.md"), `#### - [ ] T01 — Marked pending in master
`)
	detail, err := Load(root, "structured")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(detail.Tasks) != 1 || detail.Tasks[0].Status != "backlog" {
		t.Fatalf("expected markdown status to override JSON status, got %+v", detail.Tasks)
	}
	if detail.DoneTasks != 0 || detail.ProgressPct != 0 {
		t.Fatalf("unexpected progress: %+v", detail.PlanSummary)
	}
}

func TestHandlerEndpoints(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sample.md"), `# Sample

- **Target Branch:** `+"`main`"+`

#### - [x] M01 — First
`)
	handler := NewHandler(root)
	app := echo.New()
	app.Use(middleware.Recover())
	app.GET("/api/plans", handler.HandleList)
	app.GET("/api/plans/:id", handler.HandleGet)

	listReq := httptest.NewRequest(http.MethodGet, "/api/plans", nil)
	listRec := httptest.NewRecorder()
	app.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), "sample") {
		t.Fatalf("unexpected list response: %d %s", listRec.Code, listRec.Body.String())
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/api/plans/sample", nil)
	detailRec := httptest.NewRecorder()
	app.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK || !strings.Contains(detailRec.Body.String(), "M01") {
		t.Fatalf("unexpected detail response: %d %s", detailRec.Code, detailRec.Body.String())
	}

	missingReq := httptest.NewRequest(http.MethodGet, "/api/plans/missing", nil)
	missingRec := httptest.NewRecorder()
	app.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing plan, got %d", missingRec.Code)
	}
}

func writeStructuredPlan(t *testing.T, root, id, code string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(filepath.Join(dir, "waves"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "manifest.json"), `{
  "plan_id": "`+id+`",
  "title": "Structured Plan",
  "repo": {"target_branch": "main"},
  "waves": [{"id": "W1", "title": "Wave 1", "wave_file": "waves/W1.json", "depends_on": []}]
}`)
	writeFile(t, filepath.Join(dir, "waves", "W1.json"), `{
  "wave_id": "W1",
  "title": "Wave 1",
  "tasks": [{
    "code": "`+code+`",
    "title": "A done task",
    "lifecycle": {"status": "done"},
    "governance": {"owner_files": ["a.go"]},
    "graph": {"depends_on": []}
  }]
}`)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
