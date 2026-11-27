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
	oid := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	exists, err := backend.Exists(ctx, oid, "")
	if err != nil || exists {
		t.Fatalf("expected Exists=false before Put, got %v err=%v", exists, err)
	}
	if _, _, err := backend.Open(ctx, oid, ""); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}
	if err := backend.Delete(ctx, oid, ""); err != nil {
		t.Fatalf("expected Delete on missing blob to return nil, got %v", err)
	}
}

func TestLocalDiskBackendRoundTrip(t *testing.T) {
	backend, _ := NewLocalDiskBackend(t.TempDir())
	ctx := context.Background()
	oid := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	ref, err := backend.Put(ctx, oid, strings.NewReader("payload"), 7)
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	exists, err := backend.Exists(ctx, oid, ref)
	if err != nil || !exists {
		t.Fatalf("expected Exists=true after Put, got %v err=%v", exists, err)
	}
	rc, size, err := backend.Open(ctx, oid, ref)
	if err != nil || size != 7 {
		t.Fatalf("Open failed: size=%d err=%v", size, err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(raw) != "payload" {
		t.Fatalf("unexpected read content: %q", string(raw))
	}
	if err := backend.Delete(ctx, oid, ref); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
}

func TestLocalDiskBackendRejectsTraversal(t *testing.T) {
	backend, _ := NewLocalDiskBackend(t.TempDir())
	ctx := context.Background()
	oid := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	if _, err := backend.Exists(ctx, oid, "../../escape"); err == nil {
		t.Fatal("expected error for path traversal storageRef")
	}
	if _, _, err := backend.Open(ctx, oid, "../../escape"); err == nil {
		t.Fatal("expected error for Open path traversal")
	}
	if err := backend.Delete(ctx, oid, "../../escape"); err == nil {
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
	if backend.Name() != "mw-echo-9drive" || backend.BaseURL() != "https://echo.local" || backend.project != "mw-lab" {
		t.Fatalf("unexpected backend fields: %+v", backend)
	}
}

func TestEcho9DriveBackendPutOpenExistsDelete(t *testing.T) {
	oid := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	uploadMode := "nested"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/storage/upload":
			switch uploadMode {
			case "nested":
				_, _ = w.Write([]byte(`{"object":{"id":"nested-obj-1"}}`))
			case "empty":
				_, _ = w.Write([]byte(`{}`))
			case "badjson":
				_, _ = w.Write([]byte(`not-json`))
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/storage/obj-ok/download":
			_, _ = w.Write([]byte("echo-bytes"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/storage/obj-missing/download":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/api/storage/obj-err/download":
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/storage/obj-ok":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/storage/obj-missing":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/storage/obj-err":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	backend, err := NewEcho9DriveBackend(srv.URL, "secret-token", "proj", srv.Client())
	if err != nil {
		t.Fatalf("NewEcho9DriveBackend failed: %v", err)
	}

	ref, err := backend.Put(context.Background(), oid, bytes.NewBufferString("abc"), 3)
	if err != nil || ref != "nested-obj-1" {
		t.Fatalf("expected nested-obj-1, got ref=%q err=%v", ref, err)
	}
	for _, mode := range []string{"empty", "badjson", "fail"} {
		uploadMode = mode
		if _, err := backend.Put(context.Background(), oid, bytes.NewBufferString("abc"), 3); !errors.Is(
			err, ErrBackendFailure,
		) {
			t.Fatalf("expected ErrBackendFailure for upload mode %s, got %v", mode, err)
		}
	}

	if _, _, err := backend.Open(context.Background(), oid, " "); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound for blank ref, got %v", err)
	}
	if _, _, err := backend.Open(context.Background(), oid, "obj-missing"); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound on 404, got %v", err)
	}
	if _, _, err := backend.Open(context.Background(), oid, "obj-err"); !errors.Is(err, ErrBackendFailure) {
		t.Fatalf("expected ErrBackendFailure on 500, got %v", err)
	}
	rc, _, err := backend.Open(context.Background(), oid, "obj-ok")
	if err != nil {
		t.Fatalf("Open obj-ok failed: %v", err)
	}
	gotBytes, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(gotBytes) != "echo-bytes" {
		t.Fatalf("unexpected download content: %q", string(gotBytes))
	}

	if ok, _ := backend.Exists(context.Background(), oid, " "); ok {
		t.Fatal("expected Exists=false for blank storageRef")
	}
	if ok, _ := backend.Exists(context.Background(), oid, "obj-ok"); !ok {
		t.Fatal("expected Exists=true for non-empty storageRef")
	}
	if err := backend.Delete(context.Background(), oid, ""); err != nil {
		t.Fatalf("expected nil on blank Delete, got %v", err)
	}
	if err := backend.Delete(context.Background(), oid, "obj-ok"); err != nil {
		t.Fatalf("expected nil on Delete 200, got %v", err)
	}
	if err := backend.Delete(context.Background(), oid, "obj-missing"); err != nil {
		t.Fatalf("expected nil on Delete 404, got %v", err)
	}
	if err := backend.Delete(context.Background(), oid, "obj-err"); !errors.Is(err, ErrBackendFailure) {
		t.Fatalf("expected ErrBackendFailure on Delete 500, got %v", err)
	}
}
