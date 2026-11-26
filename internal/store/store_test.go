package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestStoreCRUDAndMetrics(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mw-lab-test.db")
	labStore, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = labStore.Close() }()

	createdRepo, err := labStore.CreateRepo(ctx, RepoRecord{
		Owner:         "moonwitness",
		Name:          "forge-core",
		Description:   "Sovereign forge repo",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if createdRepo.ID <= 0 || createdRepo.Owner != "moonwitness" || createdRepo.Name != "forge-core" {
		t.Fatalf("unexpected repo record: %+v", createdRepo)
	}

	if _, dupErr := labStore.CreateRepo(ctx, RepoRecord{Owner: "moonwitness", Name: "forge-core"}); !errors.Is(dupErr, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists on duplicate repo, got %v", dupErr)
	}

	updatedRepo, err := labStore.UpdateRepo(ctx, "moonwitness", "forge-core", RepoRecord{
		Description:   "Updated sovereign forge",
		DefaultBranch: "trunk",
		Private:       true,
		MirrorURL:     "https://github.com/A-L-L-A-H/mw-lab.git",
	})
	if err != nil {
		t.Fatalf("update repo: %v", err)
	}
	if !updatedRepo.Private || updatedRepo.DefaultBranch != "trunk" {
		t.Fatalf("unexpected updated repo: %+v", updatedRepo)
	}

	mirrorRec, err := labStore.UpdateMirrorStatus(ctx, "moonwitness", "forge-core", "https://github.com/A-L-L-A-H/mw-lab.git", "synced", "")
	if err != nil {
		t.Fatalf("update mirror status: %v", err)
	}
	if mirrorRec.LastStatus != "synced" || mirrorRec.LastSyncAt == "" {
		t.Fatalf("unexpected mirror record: %+v", mirrorRec)
	}

	savedObj, err := labStore.UpsertLFSObject(ctx, LFSObjectRecord{
		OID:        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		RepoOwner:  "moonwitness",
		RepoName:   "forge-core",
		Size:       128,
		Backend:    "local",
		StorageRef: "e3/b0/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Uploaded:   true,
	})
	if err != nil {
		t.Fatalf("upsert lfs object: %v", err)
	}
	if !savedObj.Uploaded || savedObj.Size != 128 {
		t.Fatalf("unexpected lfs object: %+v", savedObj)
	}

	globalHook, err := labStore.CreateWebhook(ctx, WebhookRecord{
		RepoOwner: "*",
		RepoName:  "*",
		TargetURL: "http://127.0.0.1:8080/api/hooks/git",
		Secret:    "top-secret",
		Events:    "push",
		Active:    true,
	})
	if err != nil {
		t.Fatalf("create global webhook: %v", err)
	}
	_, err = labStore.CreateWebhook(ctx, WebhookRecord{
		RepoOwner: "other",
		RepoName:  "unrelated",
		TargetURL: "http://127.0.0.1:8080/api/hooks/other",
		Events:    "push",
		Active:    true,
	})
	if err != nil {
		t.Fatalf("create scoped webhook: %v", err)
	}

	matchedHooks, err := labStore.MatchWebhooks(ctx, "moonwitness", "forge-core", "push")
	if err != nil {
		t.Fatalf("match webhooks: %v", err)
	}
	if len(matchedHooks) != 1 || matchedHooks[0].ID != globalHook.ID {
		t.Fatalf("expected 1 matching global webhook, got %+v", matchedHooks)
	}

	eventRec, err := labStore.RecordPushEvent(ctx, PushEventRecord{
		RepoOwner:      "moonwitness",
		RepoName:       "forge-core",
		RefName:        "refs/heads/main",
		BeforeSHA:      "0000000000000000000000000000000000000000",
		AfterSHA:       "1111111111111111111111111111111111111111",
		Pusher:         "tester",
		DeliveryStatus: "queued",
	})
	if err != nil {
		t.Fatalf("record push event: %v", err)
	}
	if err := labStore.UpdatePushEventStatus(ctx, eventRec.ID, "delivered:1/1"); err != nil {
		t.Fatalf("update push event status: %v", err)
	}

	metrics, err := labStore.Metrics(ctx)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if metrics.RepoCount != 1 || metrics.WebhookCount != 2 || metrics.LFSObjectCount != 1 || metrics.PushEventCount != 1 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}

	if err := labStore.DeleteWebhook(ctx, globalHook.ID); err != nil {
		t.Fatalf("delete webhook: %v", err)
	}
	if err := labStore.DeleteRepo(ctx, "moonwitness", "forge-core"); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	if _, getErr := labStore.GetRepo(ctx, "moonwitness", "forge-core"); !errors.Is(getErr, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", getErr)
	}
}
