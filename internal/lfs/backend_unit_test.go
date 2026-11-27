package lfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const unitTestOID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestLocalDiskBackendValidation(t *testing.T) {
	if _, err := NewLocalDiskBackend("   "); err == nil {
		t.Fatal("expected error for blank local rootDir")
	}

	backend, err := NewLocalDiskBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalDiskBackend failed: %v", err)
	}
	if backend.Name() != "local" {
		t.Fatalf("expected backend name 'local', got %q", backend.Name())
	}
}

func TestLocalDiskBackendMissingObject(t *testing.T) {
	backend, _ := NewLocalDiskBackend(t.TempDir())
	ctx := context.Background()

	exists, err := backend.Exists(ctx, unitTestOID, "")
	if err != nil || exists {
		t.Fatalf("expected Exists=false before Put, got %v err=%v", exists, err)
	}
	if _, _, err := backend.Open(ctx, unitTestOID, ""); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
	if err := backend.Delete(ctx, unitTestOID, ""); err != nil {
		t.Fatalf("expected Delete on missing blob to return nil, got %v", err)
	}
}

func TestLocalDiskBackendRoundTrip(t *testing.T) {
	backend, _ := NewLocalDiskBackend(t.TempDir())
	ctx := context.Background()

	ref, err := backend.Put(ctx, unitTestOID, strings.NewReader("payload"), 7)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	exists, err := backend.Exists(ctx, unitTestOID, ref)
	if err != nil || !exists {
		t.Fatalf("expected Exists=true after Put, got %v err=%v", exists, err)
	}
	rc, size, err := backend.Open(ctx, unitTestOID, ref)
	if err != nil || size != 7 {
		t.Fatalf("Open failed: size=%d err=%v", size, err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(raw) != "payload" {
		t.Fatalf("unexpected read content: %q", string(raw))
	}
	if err := backend.Delete(ctx, unitTestOID, ref); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
}

func TestLocalDiskBackendRejectsTraversal(t *testing.T) {
	backend, _ := NewLocalDiskBackend(t.TempDir())
	ctx := context.Background()

	if _, err := backend.Exists(ctx, unitTestOID, "../../escape"); err == nil {
		t.Fatal("expected error for path traversal storageRef")
	}
	if _, _, err := backend.Open(ctx, unitTestOID, "../../escape"); err == nil {
		t.Fatal("expected error for Open path traversal")
	}
	if err := backend.Delete(ctx, unitTestOID, "../../escape"); err == nil {
		t.Fatal("expected error for Delete path traversal")
	}
}

func TestEcho9DriveBackendValidation(t *testing.T) {
	if _, err := NewEcho9DriveBackend("", "", "", nil); err == nil {
		t.Fatal("expected error on empty baseURL")
	}
	if _, err := NewEcho9DriveBackend("ftp://invalid-scheme", "", "", nil); err == nil {
		t.Fatal("expected error on non-http scheme")
	}
	if _, err := NewEcho9DriveBackend("https:///no-host", "", "", nil); err == nil {
		t.Fatal("expected error on empty host")
	}
	backend, err := NewEcho9DriveBackend("https://echo.local/", "tok", "", nil)
	if err != nil {
		t.Fatalf("valid NewEcho9DriveBackend failed: %v", err)
	}
	if backend.Name() != "mw-echo-9drive" || backend.BaseURL() != "https://echo.local" ||
		backend.project != "mw-lab" {
		t.Fatalf("unexpected backend fields: %+v", backend)
	}
}

type echoStub struct {
	mode string
	srv  *httptest.Server
}

func newEchoStub(t *testing.T) *echoStub {
	t.Helper()
	stub := &echoStub{mode: "nested"}
	stub.srv = httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *echoStub) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/storage/upload":
		s.handleUpload(w)
	case "/api/storage/obj-ok/download":
		_, _ = w.Write([]byte("echo-bytes"))
	case "/api/storage/obj-missing/download":
		w.WriteHeader(http.StatusNotFound)
	case "/api/storage/obj-err/download":
		w.WriteHeader(http.StatusInternalServerError)
	case "/api/storage/obj-ok":
		w.WriteHeader(http.StatusOK)
	case "/api/storage/obj-err":
		w.WriteHeader(http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *echoStub) handleUpload(w http.ResponseWriter) {
	switch s.mode {
	case "nested":
		_, _ = w.Write([]byte(`{"object":{"id":"nested-obj-1"}}`))
	case "empty":
		_, _ = w.Write([]byte(`{}`))
	case "badjson":
		_, _ = w.Write([]byte(`not-json`))
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func newUnitEchoBackend(t *testing.T nip, stub *echoStub) *Echo9DriveBackend {
	t.Helper()
	backend, err := NewEcho9DriveBackend(stub.srv.URL, "secret-token", "proj", stub.srv.Client())
	if err != nil {
		t.Fatalf("NewEcho9DriveBackend failed: %v", err)
	}
	return backend
}

func TestEcho9DriveBackendPut(t *testing.T) {
	stub := newEchoStub(t)
	backend := newUnitEchoBackend(t, stub)

	ref, err := backend.Put(context.Background(), unitTestOID, bytes.NewBufferString("abc"), 3)
	if err != nil || ref != "nested-obj-1" {
		t.Fatalf("expected nested-obj-1, got ref=%q err=%v", ref, err)
	}
	for _, mode := range []string{"empty", "badjson", "fail"} {
		stub.mode = mode
		_, err := backend.Put(context.Background(), unitTestOID, bytes.NewBufferString("abc"), 3)
		if !errors.Is(err, ErrBackendFailure) {
			t.Fatalf("expected ErrBackendFailure for upload mode %s, got %v", mode, err)
		}
	}
}

func TestEcho9DriveBackendOpen(t *testing.T) {
	stub := newEchoStub(t)
	backend := newUnitEchoBackend(t, stub)
	ctx := context.Background()

	if _, _, err := backend.Open(ctx, unitTestOID, " "); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound for blank ref, got %v", err)
	}
	if _, _, err := backend.Open(ctx, unitTestOID, "obj-missing"); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound on 404, got %v", err)
	}
	if _, _, err := backend.Open(ctx, unitTestOID, "obj-err"); !errors.Is(err, ErrBackendFailure) {
		t.Fatalf("expected ErrBackendFailure on 500, got %v", err)
	}
	rc, _, err := backend.Open(ctx, unitTestOID, "obj-ok")
	if err != nil {
		t.Fatalf("Open obj-ok failed: %v", err)
	}
	gotBytes, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(gotBytes) != "echo-bytes" {
		t.Fatalf("unexpected download content: %q", string(gotBytes))
	}
}

func TestEcho9DriveBackendExistsAndDelete(t *testing.T) {
	stub := newEchoStub(t)
	backend := newUnitEchoBackend(t, stub)
	ctx := context.Background()

	if ok, _ := backend.Exists(ctx, unitTestOID, " "); ok {
		t.Fatal("expected Exists=false for blank storageRef")
	}
	if ok, _ := backend.Exists(ctx, unitTestOID, "obj-ok"); !ok {
		t.Fatal("expected Exists=true for non-empty storageRef")
	}
	if err := backend.Delete(ctx, unitTestOID, ""); err != nil {
		t.Fatalf("expected nil on blank Delete, got %v", err)
	}
	if err := backend.Delete(ctx, unitTestOID, "obj-ok"); err != nil {
		t.Fatalf("expected nil on Delete 200, got %v", err)
	}
	if err := backend.Delete(ctx, unitTestOID, "obj-missing"); err != nil {
		t.Fatalf("expected nil on Delete 404, got %v", err)
	}
	if err := backend.Delete(ctx, unitTestOID, "obj-err"); !errors.Is(err, ErrBackendFailure) {
		t.Fatalf("expected ErrBackendFailure on Delete 500, got %v", err)
	}
}
