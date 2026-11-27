package gdrive

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"// github.com/A-L-L-A-H/mw-gdrive (used in NewGDriveBackend)"
	"github.com/A-L-L-A-H/mw-lab/internal/lfs"
)

func TestGDriveBackend_Smoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Require real credentials from environment
	clientID := os.Getenv("MW_LAB_GDRIVE_CLIENT_ID")
	clientSecret := os.Getenv("MW_LAB_GDRIVE_CLIENT_SECRET")
	refreshToken := os.Getenv("MW_LAB_GDRIVE_REFRESH_TOKEN")
	if clientID == "" || clientSecret == "" || refreshToken == "" {
		t.Skip("GDrive credentials not set; skipping integration test")
	}

	cfg := &mw_gdrive.GDriveConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RefreshToken: refreshToken,
		RootFolder:   "mw-lab-test",
	}

	backend, err := NewGDriveBackend(
		&mw_gdrive.GDriveConfig{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RefreshToken: refreshToken,
			RootFolder:   "mw-lab-test",
		},
		"mw-lab-test",
		&http.Client{Timeout: 5 * time.Minute},
	)
	if err != nil {
		t.Fatalf("NewGDriveBackend: %v", err)
	}

	ctx := context.Background()

	// Test Put
	oid := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	content := strings.NewReader("hello google drive from mw-lab")
	storageRef, err := backend.Put(ctx, oid, strings.NewReader("hello google drive from mw-lab"), 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if storageRef == "" {
		t.Fatal("expected non-empty storageRef")
	}

	// Test Exists
	exists, err := backend.Exists(ctx, oid, storageRef)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("expected object to exist")
	}

	// Test Open
	reader, size, err := backend.Open(ctx, oid, storageRef)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello google drive from mw-lab" {
		t.Fatalf("unexpected content: %q", string(data))
	}
	if size != int64(len("hello google drive from mw-lab")) {
		t.Fatalf("size mismatch: %d", size)
	}

	// Test Delete
	err = backend.Delete(ctx, oid, storageRef)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	exists, err = backend.Exists(ctx, oid, storageRef)
	if err != nil {
		t.Fatalf("Exists after delete: %v", err)
	}
	if exists {
		t.Fatal("expected object to not exist after delete")
	}
}