package ci

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestBadgesHandlerSVGRenderingAndValidation(t *testing.T) {
	runStore, err := NewRunStore(nil)
	if err != nil {
		t.Fatalf("new run store: %v", err)
	}

	ctx := context.Background()
	_, _ = runStore.Create(ctx, RunRecord{RepoOwner: "mw", RepoName: "pass-repo", Status: StatusPassed})
	_, _ = runStore.Create(ctx, RunRecord{RepoOwner: "mw", RepoName: "fail-repo", Status: StatusFailed})
	_, _ = runStore.Create(ctx, RunRecord{RepoOwner: "mw", RepoName: "busy-repo", Status: StatusRunning})

	handler := NewBadgesHandler(runStore)
	e := echo.New()
	e.GET("/api/ci/badges/:repo", handler.HandleBadge)

	cases := []struct {
		name      string
		path      string
		wantCode  int
		wantText  string
		wantColor string
	}{
		{name: "passed", path: "/api/ci/badges/pass-repo.svg", wantCode: http.StatusOK, wantText: "passing", wantColor: "#4c1"},
		{name: "failed", path: "/api/ci/badges/fail-repo.svg", wantCode: http.StatusOK, wantText: "failing", wantColor: "#e05d44"},
		{name: "running", path: "/api/ci/badges/busy-repo.svg", wantCode: http.StatusOK, wantText: "running", wantColor: "#dfb317"},
		{name: "unknown", path: "/api/ci/badges/missing-repo.svg", wantCode: http.StatusOK, wantText: "unknown", wantColor: "#9f9f9f"},
		{name: "owner-filter", path: "/api/ci/badges/pass-repo.svg?owner=mw", wantCode: http.StatusOK, wantText: "passing", wantColor: "#4c1"},
		{name: "missing-svg-suffix", path: "/api/ci/badges/pass-repo", wantCode: http.StatusNotFound},
		{name: "invalid-slug", path: "/api/ci/badges/-bad..repo.svg", wantCode: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("expected status %d, got %d (%s)", tc.wantCode, rec.Code, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK {
				ct := rec.Header().Get("Content-Type")
				if !strings.Contains(ct, "image/svg+xml") {
					t.Fatalf("expected image/svg+xml Content-Type, got %q", ct)
				}
				cc := rec.Header().Get("Cache-Control")
				if !strings.Contains(cc, "no-cache") {
					t.Fatalf("expected no-cache Cache-Control, got %q", cc)
				}
				body := rec.Body.String()
				if !strings.Contains(body, "<svg") || !strings.Contains(body, tc.wantText) || !strings.Contains(body, tc.wantColor) {
					t.Fatalf("unexpected svg body: %s", body)
				}
			}
		})
	}

	// Verify XML/SVG escaping prevents script injection.
	escaped := RenderBadgeSVG(`<script>`, `bad"status&`, `javascript:alert(1)`)
	if strings.Contains(escaped, "<script>") || strings.Contains(escaped, "javascript:") {
		t.Fatalf("expected XML escaping and safe color fallback, got %s", escaped)
	}
	if !strings.Contains(escaped, "&lt;script&gt;") || !strings.Contains(escaped, "#9f9f9f") {
		t.Fatalf("expected escaped text and fallback color, got %s", escaped)
	}
}

func TestBadgesHandlerListRunsJSONEnvelopeAndFiltering(t *testing.T) {
	runStore, _ := NewRunStore(nil)
	handler := NewBadgesHandler(runStore)
	e := echo.New()
	e.GET("/api/ci/runs", handler.HandleListRuns)

	// Empty store returns [] (never null) with count 0.
	emptyReq := httptest.NewRequest(http.MethodGet, "/api/ci/runs", nil)
	emptyRec := httptest.NewRecorder()
	e.ServeHTTP(emptyRec, emptyReq)
	if emptyRec.Code != http.StatusOK || !strings.Contains(emptyRec.Body.String(), `"runs":[]`) {
		t.Fatalf("expected empty runs slice, got status=%d body=%s", emptyRec.Code, emptyRec.Body.String())
	}

	ctx := context.Background()
	_, _ = runStore.Create(ctx, RunRecord{RepoOwner: "mw", RepoName: "repo-a", Status: StatusPassed})
	_, _ = runStore.Create(ctx, RunRecord{RepoOwner: "mw", RepoName: "repo-a", Status: StatusFailed})
	_, _ = runStore.Create(ctx, RunRecord{RepoOwner: "mw", RepoName: "repo-b", Status: StatusPassed})

	req := httptest.NewRequest(http.MethodGet, "/api/ci/runs?repo=repo-a&status=failed&limit=1", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var parsed struct {
		Runs  []RunRecord `json:"runs"`
		Count int         `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal runs: %v", err)
	}
	if parsed.Count != 1 || len(parsed.Runs) != 1 || parsed.Runs[0].RepoName != "repo-a" || parsed.Runs[0].Status != StatusFailed {
		t.Fatalf("unexpected filtered runs response: %+v", parsed)
	}
}

func TestNtfyNotifierSendsOnlyOnFailure(t *testing.T) {
	var mu sync.Mutex
	var requests []struct {
		Title    string
		Priority string
		Tags     string
		Body     string
	}

	ntfySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, struct {
			Title    string
			Priority string
			Tags     string
			Body     string
		}{
			Title:    r.Header.Get("Title"),
			Priority: r.Header.Get("Priority"),
			Tags:     r.Header.Get("Tags"),
			Body:     string(raw),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfySrv.Close()

	notifier := NewNtfyNotifier(ntfySrv.URL, ntfySrv.Client())
	if !notifier.Enabled() {
		t.Fatal("expected notifier to be enabled")
	}

	// Passed run should trigger zero HTTP calls.
	if err := notifier.NotifyFailure(context.Background(), RunRecord{
		ID:       "run-pass",
		FullName: "mw/app",
		Status:   StatusPassed,
	}); err != nil {
		t.Fatalf("NotifyFailure on passed run returned error: %v", err)
	}

	// Failed run should trigger one POST with expected headers and body.
	if err := notifier.NotifyFailure(context.Background(), RunRecord{
		ID:         "run-fail",
		FullName:   "mw/app",
		Ref:        "refs/heads/main",
		SHA:        "deadbeef1234",
		Status:     StatusFailed,
		DurationMs: 420,
		Summary:    "stage \"test\" exited with code 1",
	}); err != nil {
		t.Fatalf("NotifyFailure on failed run returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("expected exactly 1 ntfy POST, got %d", len(requests))
	}
	got := requests[0]
	if got.Title != "[mw-lab] CI failed: mw/app" || got.Priority != "high" || !strings.Contains(got.Tags, "ci") {
		t.Fatalf("unexpected ntfy headers: %+v", got)
	}
	if !strings.Contains(got.Body, "run-fail") || !strings.Contains(got.Body, "deadbeef1234") || !strings.Contains(got.Body, "exited with code 1") {
		t.Fatalf("unexpected ntfy body: %q", got.Body)
	}

	// Disabled notifier returns nil cleanly.
	disabled := NewNtfyNotifier("", nil)
	if disabled.Enabled() {
		t.Fatal("expected empty topicURL notifier to be disabled")
	}
	if err := disabled.NotifyFailure(context.Background(), RunRecord{Status: StatusFailed}); err != nil {
		t.Fatalf("disabled notifier should not error: %v", err)
	}
}
