package githttp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/A-L-L-A-H/mw-lab/internal/ci"
	"github.com/A-L-L-A-H/mw-lab/internal/store"
	"github.com/labstack/echo/v4"
)

type Handler struct {
	manager      *Manager
	dbStore      *store.Store
	dispatcher   *WebhookDispatcher
	ciDispatcher *ci.Dispatcher
}

func NewHandler(manager *Manager, dbStore *store.Store, dispatcher *WebhookDispatcher) *Handler {
	return &Handler{
		manager:    manager,
		dbStore:    dbStore,
		dispatcher: dispatcher,
	}
}

func (h *Handler) SetCIDispatcher(ciDispatcher *ci.Dispatcher) {
	h.ciDispatcher = ciDispatcher
}

func (h *Handler) RegisterRoutes(echoApp *echo.Echo) {
	echoApp.GET("/:owner/:repo/info/refs", h.HandleInfoRefs)
	echoApp.POST("/:owner/:repo/git-upload-pack", h.HandleUploadPack)
	echoApp.POST("/:owner/:repo/git-receive-pack", h.HandleReceivePack)
}

func (h *Handler) HandleInfoRefs(c echo.Context) error {
	serviceName := strings.TrimSpace(c.QueryParam("service"))
	subcommand, ok := gitServiceSubcommand(serviceName)
	if !ok {
		return c.String(http.StatusForbidden, "unsupported git smart http service\n")
	}

	_, _, repoPath, err := h.resolveTargetRepo(c)
	if err != nil {
		return writeGitHTTPError(c, err)
	}

	rawRefs, err := runGit(c.Request().Context(), "", subcommand, "--stateless-rpc", "--advertise-refs", repoPath)
	if err != nil {
		return c.String(http.StatusInternalServerError, "git advertise-refs failed\n")
	}

	c.Response().Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", serviceName))
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Pragma", "no-cache")
	c.Response().WriteHeader(http.StatusOK)

	headerPacket := formatPktLine("# service="+serviceName+"\n") + "0000"
	_, _ = io.WriteString(c.Response().Writer, headerPacket)
	_, _ = c.Response().Writer.Write(rawRefs)
	return nil
}

func (h *Handler) HandleUploadPack(c echo.Context) error {
	_, _, repoPath, err := h.resolveTargetRepo(c)
	if err != nil {
		return writeGitHTTPError(c, err)
	}
	return h.streamStatelessRPC(c, "upload-pack", "application/x-git-upload-pack-result", repoPath)
}

func (h *Handler) HandleReceivePack(c echo.Context) error {
	owner, repoName, repoPath, err := h.resolveTargetRepo(c)
	if err != nil {
		return writeGitHTTPError(c, err)
	}

	ctx := c.Request().Context()
	beforeRefs, _ := h.manager.SnapshotRefs(ctx, repoPath)

	if err := h.streamStatelessRPC(c, "receive-pack", "application/x-git-receive-pack-result", repoPath); err != nil {
		return err
	}

	afterRefs, snapErr := h.manager.SnapshotRefs(context.Background(), repoPath)
	if snapErr != nil {
		return nil
	}
	h.emitPushEvents(owner, repoName, detectPusher(c.Request()), beforeRefs, afterRefs)
	return nil
}

func (h *Handler) streamStatelessRPC(c echo.Context, subcommand, contentType, repoPath string) error {
	bodyReader, closer, err := decodeRequestBody(c.Request())
	if err != nil {
		return c.String(http.StatusBadRequest, "invalid compressed request body\n")
	}
	defer closer()

	cmd := exec.CommandContext(c.Request().Context(), "git", subcommand, "--stateless-rpc", repoPath)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdin = bodyReader

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return c.String(http.StatusInternalServerError, "failed to attach git stdout\n")
	}
	if err := cmd.Start(); err != nil {
		return c.String(http.StatusInternalServerError, "failed to launch git service\n")
	}

	c.Response().Header().Set("Content-Type", contentType)
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().WriteHeader(http.StatusOK)

	_, copyErr := io.Copy(c.Response().Writer, stdoutPipe)
	waitErr := cmd.Wait()
	if copyErr != nil || waitErr != nil {
		return nil
	}
	return nil
}

func (h *Handler) emitPushEvents(owner, repoName, pusher string, beforeRefs, afterRefs map[string]string) {
	refSet := make(map[string]struct{}, len(beforeRefs)+len(afterRefs))
	for refName := range beforeRefs {
		refSet[refName] = struct{}{}
	}
	for refName := range afterRefs {
		refSet[refName] = struct{}{}
	}
	sortedRefs := make([]string, 0, len(refSet))
	for refName := range refSet {
		sortedRefs = append(sortedRefs, refName)
	}
	sort.Strings(sortedRefs)

	bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, refName := range sortedRefs {
		beforeSHA := beforeRefs[refName]
		if beforeSHA == "" {
			beforeSHA = ZeroSHA
		}
		afterSHA := afterRefs[refName]
		if afterSHA == "" {
			afterSHA = ZeroSHA
		}
		if beforeSHA == afterSHA {
			continue
		}

		var recentCommits []CommitInfo
		if afterSHA != ZeroSHA {
			loadedCommits, commitErr := h.manager.ListCommits(bgCtx, owner, repoName, afterSHA, 10)
			if commitErr == nil {
				recentCommits = loadedCommits
			}
		}

		var eventID int64
		if h.dbStore != nil {
			savedEvent, saveErr := h.dbStore.RecordPushEvent(bgCtx, store.PushEventRecord{
				RepoOwner:      owner,
				RepoName:       repoName,
				RefName:        refName,
				BeforeSHA:      beforeSHA,
				AfterSHA:       afterSHA,
				Pusher:         pusher,
				DeliveryStatus: "queued",
			})
			if saveErr == nil {
				eventID = savedEvent.ID
			}
		}

		if h.dispatcher != nil {
			h.dispatcher.DispatchPushAsync(eventID, PushPayload{
				Event:      "push",
				DeliveryID: generateDeliveryID(),
				Repository: RepoSummary{
					Owner:    owner,
					Name:     repoName,
					FullName: owner + "/" + repoName,
				},
				Ref:       refName,
				Before:    beforeSHA,
				After:     afterSHA,
				Pusher:    pusher,
				Commits:   recentCommits,
				Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			})
		}

		if h.ciDispatcher != nil && h.ciDispatcher.Enabled() && afterSHA != ZeroSHA {
			h.ciDispatcher.DispatchAsync(ci.DispatchInput{
				Owner:  owner,
				Repo:   repoName,
				Ref:    refName,
				SHA:    afterSHA,
				Pusher: pusher,
			})
		}
	}
}

func (h *Handler) resolveTargetRepo(c echo.Context) (string, string, string, error) {
	owner := c.Param("owner")
	rawRepo := c.Param("repo")
	if err := ValidateSlug(owner); err != nil {
		return "", "", "", err
	}
	cleanRepo, err := NormalizeRepoName(rawRepo)
	if err != nil {
		return "", "", "", err
	}
	repoPath, err := h.manager.requireRepoPath(owner, cleanRepo)
	if err != nil {
		return "", "", "", err
	}
	return owner, cleanRepo, repoPath, nil
}

func gitServiceSubcommand(serviceName string) (string, bool) {
	switch serviceName {
	case "git-upload-pack":
		return "upload-pack", true
	case "git-receive-pack":
		return "receive-pack", true
	default:
		return "", false
	}
}

func formatPktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

func decodeRequestBody(httpReq *http.Request) (io.Reader, func(), error) {
	if strings.EqualFold(strings.TrimSpace(httpReq.Header.Get("Content-Encoding")), "gzip") {
		gzReader, err := gzip.NewReader(httpReq.Body)
		if err != nil {
			return nil, func() {}, err
		}
		return gzReader, func() { _ = gzReader.Close() }, nil
	}
	return httpReq.Body, func() {}, nil
}

func detectPusher(httpReq *http.Request) string {
	if customPusher := strings.TrimSpace(httpReq.Header.Get("X-MW-Lab-Pusher")); customPusher != "" {
		return customPusher
	}
	if basicUser, _, hasBasic := httpReq.BasicAuth(); hasBasic && strings.TrimSpace(basicUser) != "" {
		return strings.TrimSpace(basicUser)
	}
	return "anonymous"
}

func writeGitHTTPError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, ErrInvalidSlug), errors.Is(err, ErrPathEscape):
		return c.String(http.StatusBadRequest, err.Error()+"\n")
	case errors.Is(err, ErrRepoNotFound):
		return c.String(http.StatusNotFound, "repository not found\n")
	default:
		return c.String(http.StatusInternalServerError, err.Error()+"\n")
	}
}
