package lfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/A-L-L-A-H/mw-lab/internal/store"
	"github.com/labstack/echo/v4"
)

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func doBatchRequest(t *testing.T, httpClient *http.Client, url string, reqPayload BatchRequest) (*http.Response, BatchResponse) {
	t.Helper()
	encoded, err := json.Marshal(reqPayload)
	if err != nil {
		t.Fatalf("marshal batch request: %v", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("new batch request: %v", err)
	}
	httpReq.Header.Set("Content-Type", LFSMediaType)
	httpReq.Header.Set("Accept", LFSMediaType)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		t.Fatalf("send batch request: %v", err)
	}
	defer httpResp.Body.Close()

	var batchReply BatchResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&batchReply); err != nil {
		t.Fatalf("decode batch response: %v", err)
	}
	return httpResp, batchReply
}

func TestLFSBatchUploadDownloadLocalDisk(t *testing.T) {
	tempRoot := t.TempDir()
	labStore, err := store.Open(filepath.Join(tempRoot, "lab.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = labStore.Close() }()

	localBackend, err := NewLocalDiskBackend(filepath.Join(tempRoot, "lfs-blobs"))
	if err != nil {
		t.Fatalf("new local backend: %v", err)
	}
	svc, err := NewService(labStore, localBackend, "", 0)
	if err != nil {
		t.Fatalf("new lfs service: %v", err)
	}

	echoApp := echo.New()
	svc.RegisterRoutes(echoApp)
	lfsServer := httptest.NewServer(echoApp)
	defer lfsServer.Close()

	artifactBytes := []byte("moonwitness sovereign binary artifact payload v1")
	artifactOID := sha256Hex(artifactBytes)
	batchURL := lfsServer.URL + "/moonwitness/assets.git/info/lfs/objects/batch"

	// 1. Batch upload returns upload action href.
	uploadBatchResp, uploadReply := doBatchRequest(t, lfsServer.Client(), batchURL, BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectSpec{{OID: artifactOID, Size: int64(len(artifactBytes))}},
	})
	if uploadBatchResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on upload batch, got %d", uploadBatchResp.StatusCode)
	}
	if !strings.Contains(uploadBatchResp.Header.Get("Content-Type"), LFSMediaType) {
		t.Fatalf("expected %s Content-Type, got %q", LFSMediaType, uploadBatchResp.Header.Get("Content-Type"))
	}
	if len(uploadReply.Objects) != 1 || uploadReply.Objects[0].Actions["upload"] == nil {
		t.Fatalf("expected upload action in batch response: %+v", uploadReply)
	}
	uploadHref := uploadReply.Objects[0].Actions["upload"].Href

	// 2. Reject SHA-256 mismatch with 422.
	badReq, _ := http.NewRequest(http.MethodPut, uploadHref, bytes.NewReader([]byte("corrupted bytes")))
	badResp, err := lfsServer.Client().Do(badReq)
	if err != nil {
		t.Fatalf("put corrupted object: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on sha256 mismatch, got %d", badResp.StatusCode)
	}

	// 3. Valid PUT stores object on disk.
	putReq, _ := http.NewRequest(http.MethodPut, uploadHref, bytes.NewReader(artifactBytes))
	putResp, err := lfsServer.Client().Do(putReq)
	if err != nil {
		t.Fatalf("put valid object: %v", err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on valid PUT, got %d", putResp.StatusCode)
	}

	// 4. Second upload batch deduplicates (omits upload action).
	_, dedupReply := doBatchRequest(t, lfsServer.Client(), batchURL, BatchRequest{
		Operation: "upload",
		Objects:   []ObjectSpec{{OID: artifactOID, Size: int64(len(artifactBytes))}},
	})
	if len(dedupReply.Objects) != 1 || len(dedupReply.Objects[0].Actions) != 0 {
		t.Fatalf("expected deduplicated object with no upload actions, got %+v", dedupReply)
	}

	// 5. Download batch returns download href + missing OID returns 404 object error.
	missingOID := sha256Hex([]byte("does-not-exist"))
	_, dlBatchReply := doBatchRequest(t, lfsServer.Client(), batchURL, BatchRequest{
		Operation: "download",
		Objects: []ObjectSpec{
			{OID: artifactOID, Size: int64(len(artifactBytes))},
			{OID: missingOID, Size: 14},
		},
	})
	if len(dlBatchReply.Objects) != 2 || dlBatchReply.Objects[0].Actions["download"] == nil {
		t.Fatalf("expected download action for uploaded object: %+v", dlBatchReply)
	}
	if dlBatchReply.Objects[1].Error == nil || dlBatchReply.Objects[1].Error.Code != http.StatusNotFound {
		t.Fatalf("expected 404 object error for missing oid: %+v", dlBatchReply.Objects[1])
	}

	// 6. GET object streams exact bytes.
	dlResp, err := lfsServer.Client().Get(dlBatchReply.Objects[0].Actions["download"].Href)
	if err != nil {
		t.Fatalf("download lfs object: %v", err)
	}
	defer dlResp.Body.Close()
	downloadedBytes, _ := io.ReadAll(dlResp.Body)
	if dlResp.StatusCode != http.StatusOK || !bytes.Equal(downloadedBytes, artifactBytes) {
		t.Fatalf("download mismatch: status=%d len=%d", dlResp.StatusCode, len(downloadedBytes))
	}
	if dlResp.Header.Get("X-MW-Lab-LFS-Backend") != "local" {
		t.Fatalf("expected local backend header, got %q", dlResp.Header.Get("X-MW-Lab-LFS-Backend"))
	}
}

func TestLFSBatchWithMockMwEcho9Drive(t *testing.T) {
	tempRoot := t.TempDir()
	labStore, err := store.Open(filepath.Join(tempRoot, "lab-9drive.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = labStore.Close() }()

	// Mock mw-echo 9Drive HTTP server implementing /api/storage/upload and /api/storage/:id/download.
	var mu sync.Mutex
	storedBlobs := map[string][]byte{}
	recordedLogicalPaths := map[string]string{}
	nextCounter := 0
	const echoToken = "echo-9drive-bearer-token"

	mockEchoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+echoToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/storage/upload" {
			if err := r.ParseMultipartForm(16 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fileHandle, _, err := r.FormFile("file")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer fileHandle.Close()
			rawBytes, err := io.ReadAll(fileHandle)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			mu.Lock()
			nextCounter++
			remoteID := fmt.Sprintf("echo-9drive-%d", nextCounter)
			storedBlobs[remoteID] = rawBytes
			recordedLogicalPaths[remoteID] = r.FormValue("logicalPath")
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        remoteID,
				"project":   r.FormValue("project"),
				"sizeBytes": len(rawBytes),
				"sha256":    sha256Hex(rawBytes),
			})
			return
		}

		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/storage/") && strings.HasSuffix(r.URL.Path, "/download") {
			trimmed := strings.TrimPrefix(r.URL.Path, "/api/storage/")
			remoteID := strings.TrimSuffix(trimmed, "/download")
			mu.Lock()
			blobBytes, exists := storedBlobs[remoteID]
			mu.Unlock()
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(blobBytes)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockEchoServer.Close()

	echoBackend, err := NewEcho9DriveBackend(mockEchoServer.URL, echoToken, "mw-lab", mockEchoServer.Client())
	if err != nil {
		t.Fatalf("new echo 9drive backend: %v", err)
	}
	svc, err := NewService(labStore, echoBackend, "", 0)
	if err != nil {
		t.Fatalf("new lfs service: %v", err)
	}

	echoApp := echo.New()
	svc.RegisterRoutes(echoApp)
	lfsServer := httptest.NewServer(echoApp)
	defer lfsServer.Close()

	modelWeights := bytes.Repeat([]byte("9DRIVE-LFS-CHUNK-"), 512)
	weightsOID := sha256Hex(modelWeights)
	batchURL := lfsServer.URL + "/moonwitness/models.git/info/lfs/objects/batch"

	_, uploadReply := doBatchRequest(t, lfsServer.Client(), batchURL, BatchRequest{
		Operation: "upload",
		Objects:   []ObjectSpec{{OID: weightsOID, Size: int64(len(modelWeights))}},
	})
	if len(uploadReply.Objects) != 1 || uploadReply.Objects[0].Actions["upload"] == nil {
		t.Fatalf("expected upload action for 9drive backend: %+v", uploadReply)
	}

	putReq, _ := http.NewRequest(http.MethodPut, uploadReply.Objects[0].Actions["upload"].Href, bytes.NewReader(modelWeights))
	putResp, err := lfsServer.Client().Do(putReq)
	if err != nil {
		t.Fatalf("put to 9drive proxy: %v", err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from 9drive proxy upload, got %d", putResp.StatusCode)
	}

	storedMeta, err := labStore.GetLFSObject(context.Background(), weightsOID)
	if err != nil {
		t.Fatalf("get stored lfs metadata: %v", err)
	}
	if storedMeta.Backend != "mw-echo-9drive" || storedMeta.StorageRef != "echo-9drive-1" || !storedMeta.Uploaded {
		t.Fatalf("unexpected 9drive metadata row: %+v", storedMeta)
	}
	mu.Lock()
	logicalPath := recordedLogicalPaths["echo-9drive-1"]
	mu.Unlock()
	if !strings.HasPrefix(logicalPath, "lfs/"+weightsOID[:2]+"/"+weightsOID[2:4]+"/") {
		t.Fatalf("unexpected 9drive logicalPath: %q", logicalPath)
	}

	_, dlReply := doBatchRequest(t, lfsServer.Client(), batchURL, BatchRequest{
		Operation: "download",
		Objects:   []ObjectSpec{{OID: weightsOID, Size: int64(len(modelWeights))}},
	})
	if len(dlReply.Objects) != 1 || dlReply.Objects[0].Actions["download"] == nil {
		t.Fatalf("expected download action from 9drive backend: %+v", dlReply)
	}

	dlResp, err := lfsServer.Client().Get(dlReply.Objects[0].Actions["download"].Href)
	if err != nil {
		t.Fatalf("download via 9drive proxy: %v", err)
	}
	defer dlResp.Body.Close()
	streamedBytes, _ := io.ReadAll(dlResp.Body)
	if dlResp.StatusCode != http.StatusOK || !bytes.Equal(streamedBytes, modelWeights) {
		t.Fatalf("9drive proxy download mismatch: status=%d len=%d", dlResp.StatusCode, len(streamedBytes))
	}
	if dlResp.Header.Get("X-MW-Lab-LFS-Backend") != "mw-echo-9drive" {
		t.Fatalf("expected mw-echo-9drive backend header, got %q", dlResp.Header.Get("X-MW-Lab-LFS-Backend"))
	}
}
