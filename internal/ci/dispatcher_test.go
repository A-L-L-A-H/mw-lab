package ci

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

func TestBuildMissionDAGAndIdempotencyKey(t *testing.T) {
	commitSHA := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	mission, err := BuildMission(DispatchInput{
		Owner:  "moonwitness",
		Repo:   "mw-lab",
		Ref:    "refs/heads/main",
		SHA:    commitSHA,
		Pusher: "builder",
	})
	if err != nil {
		t.Fatalf("build mission: %v", err)
	}

	expectedKey := "ci:moonwitness/mw-lab:" + commitSHA
	if mission.IdempotencyKey != expectedKey {
		t.Fatalf("expected idempotency_key %q, got %q", expectedKey, mission.IdempotencyKey)
	}
	expectedTags := []string{"git", "ci"}
	if !reflect.DeepEqual(mission.CapabilityTags, expectedTags) {
		t.Fatalf("expected capability_tags %v, got %v", expectedTags, mission.CapabilityTags)
	}
	if len(mission.Tasks) != 3 {
		t.Fatalf("expected 3 DAG tasks, got %d", len(mission.Tasks))
	}

	lintTask := mission.Tasks[0]
	testTask := mission.Tasks[1]
	buildTask := mission.Tasks[2]

	if lintTask.ID != "lint" || len(lintTask.DependsOn) != 0 || !reflect.DeepEqual(lintTask.CapabilityTags, expectedTags) {
		t.Fatalf("unexpected lint task: %+v", lintTask)
	}
	if testTask.ID != "test" || !reflect.DeepEqual(testTask.DependsOn, []string{"lint"}) || !reflect.DeepEqual(testTask.CapabilityTags, expectedTags) {
		t.Fatalf("unexpected test task: %+v", testTask)
	}
	if buildTask.ID != "build" || !reflect.DeepEqual(buildTask.DependsOn, []string{"test"}) || !reflect.DeepEqual(buildTask.CapabilityTags, expectedTags) {
		t.Fatalf("unexpected build task: %+v", buildTask)
	}
}

func TestDispatcherPostsDAGToMissionEndpoint(t *testing.T) {
	var mu sync.Mutex
	var receivedPath string
	var receivedKeyHeader string
	var receivedMission MissionRequest

	mockMissionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var parsed MissionRequest
		if err := json.Unmarshal(rawBody, &parsed); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		receivedPath = r.URL.Path
		receivedKeyHeader = r.Header.Get("Idempotency-Key")
		receivedMission = parsed
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"mission-ci-001","status":"queued"}`))
	}))
	defer mockMissionSrv.Close()

	dispatcher := NewDispatcher(mockMissionSrv.URL, mockMissionSrv.Client())
	commitSHA := "deadbeef00112233445566778899aabbccddeeff"
	outcome, err := dispatcher.Dispatch(context.Background(), DispatchInput{
		Owner:  "alloy",
		Repo:   "forge",
		Ref:    "refs/heads/main",
		SHA:    commitSHA,
		Pusher: "ci-bot",
	})
	if err != nil {
		t.Fatalf("dispatch mission: %v", err)
	}

	mu.Lock()
	gotPath := receivedPath
	gotHeader := receivedKeyHeader
	gotMission := receivedMission
	mu.Unlock()

	expectedKey := "ci:alloy/forge:" + commitSHA
	if gotPath != "/api/missions" {
		t.Fatalf("expected POST /api/missions, got %q", gotPath)
	}
	if gotHeader != expectedKey || gotMission.IdempotencyKey != expectedKey {
		t.Fatalf("expected idempotency key %q, got header=%q body=%q", expectedKey, gotHeader, gotMission.IdempotencyKey)
	}
	if outcome.StatusCode != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", outcome.StatusCode)
	}
	if len(gotMission.Tasks) != 3 || gotMission.Tasks[0].ID != "lint" || gotMission.Tasks[1].ID != "test" || gotMission.Tasks[2].ID != "build" {
		t.Fatalf("unexpected DAG tasks: %+v", gotMission.Tasks)
	}
}

func TestDispatcherReturnsErrorWhenUnconfigured(t *testing.T) {
	dispatcher := NewDispatcher("", nil)
	_, err := dispatcher.Dispatch(context.Background(), DispatchInput{
		Owner: "alloy",
		Repo:  "forge",
		SHA:   "1234567890abcdef",
	})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}
