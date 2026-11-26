package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/A-L-L-A-H/mw-lab/internal/githttp"
	"github.com/A-L-L-A-H/mw-lab/internal/lfs"
	"github.com/A-L-L-A-H/mw-lab/internal/server"
	"github.com/A-L-L-A-H/mw-lab/internal/store"
)

const appVersion = "0.1.0"

func main() {
	addr := envOrDefault("MW_LAB_ADDR", ":8090")
	baseDir := envOrDefault("MW_LAB_DATA_DIR", "./data")
	dbPath := envOrDefault("MW_LAB_DB_PATH", filepath.Join(baseDir, "mw-lab.db"))
	reposDir := envOrDefault("MW_LAB_REPOS_DIR", filepath.Join(baseDir, "repos"))
	lfsDir := envOrDefault("MW_LAB_LFS_DIR", filepath.Join(baseDir, "lfs"))
	publicBaseURL := strings.TrimSpace(os.Getenv("MW_LAB_PUBLIC_URL"))
	authToken := strings.TrimSpace(os.Getenv("MW_LAB_TOKEN"))
	echoBaseURL := strings.TrimSpace(os.Getenv("MW_ECHO_URL"))
	echoToken := strings.TrimSpace(os.Getenv("MW_ECHO_TOKEN"))
	echoProject := envOrDefault("MW_ECHO_PROJECT", "mw-lab")
	allowFileMirror := envBool("MW_LAB_ALLOW_FILE_MIRROR", false)

	labStore, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer func() { _ = labStore.Close() }()

	gitManager, err := githttp.NewManager(reposDir, labStore, allowFileMirror)
	if err != nil {
		log.Fatalf("init git manager: %v", err)
	}

	var lfsBackend lfs.BlobBackend
	if echoBaseURL != "" {
		lfsBackend, err = lfs.NewEcho9DriveBackend(echoBaseURL, echoToken, echoProject, nil)
		if err != nil {
			log.Fatalf("init mw-echo 9drive lfs backend: %v", err)
		}
		log.Printf("mw-lab LFS backend: mw-echo 9Drive (%s)", echoBaseURL)
	} else {
		lfsBackend, err = lfs.NewLocalDiskBackend(lfsDir)
		if err != nil {
			log.Fatalf("init local disk lfs backend: %v", err)
		}
		log.Printf("mw-lab LFS backend: local disk (%s)", lfsDir)
	}

	lfsService, err := lfs.NewService(labStore, lfsBackend, publicBaseURL, 0)
	if err != nil {
		log.Fatalf("init lfs service: %v", err)
	}

	dispatcher := githttp.NewWebhookDispatcher(labStore, nil)

	if defaultHookURL := strings.TrimSpace(os.Getenv("MW_LAB_DEFAULT_WEBHOOK_URL")); defaultHookURL != "" {
		seedDefaultWebhook(labStore, defaultHookURL, strings.TrimSpace(os.Getenv("MW_LAB_DEFAULT_WEBHOOK_SECRET")))
	}

	labServer := server.New(server.Config{
		Addr:      addr,
		AuthToken: authToken,
		Version:   appVersion,
	}, labStore, gitManager, dispatcher, lfsService)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           labServer.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errChan := make(chan error, 1)
	go func() {
		log.Printf("mw-lab listening on %s (repos=%s)", addr, reposDir)
		errChan <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Printf("mw-lab shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		dispatcher.Wait()
	case serveErr := <-errChan:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Fatalf("http server failed: %v", serveErr)
		}
	}
}

func seedDefaultWebhook(labStore *store.Store, targetURL, secret string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	existingHooks, err := labStore.ListWebhooks(ctx, "", "")
	if err != nil {
		return
	}
	for _, existing := range existingHooks {
		if existing.TargetURL == targetURL {
			return
		}
	}
	_, _ = labStore.CreateWebhook(ctx, store.WebhookRecord{
		RepoOwner: "*",
		RepoName:  "*",
		TargetURL: targetURL,
		Secret:    secret,
		Events:    "push",
		Active:    true,
	})
}

func envOrDefault(key, fallback string) string {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return fallback
	}
	return val
}

func envBool(key string, fallback bool) bool {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(val)
	if err != nil {
		return fallback
	}
	return parsed
}
