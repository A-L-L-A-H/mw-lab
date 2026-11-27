package ci

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

var hexColorRegex = regexp.MustCompile(`^#[0-9a-fA-F]{3,6}$`)

type BadgesHandler struct {
	store *RunStore
}

func NewBadgesHandler(store *RunStore) *BadgesHandler {
	return &BadgesHandler{store: store}
}

func BadgeStatusForRun(runStatus string) (statusText string, colorHex string) {
	switch strings.ToLower(strings.TrimSpace(runStatus)) {
	case StatusPassed:
		return "passing", "#4c1"
	case StatusFailed:
		return "failing", "#e05d44"
	case StatusRunning, StatusQueued:
		return "running", "#dfb317"
	case StatusCancelled:
		return "cancelled", "#9f9f9f"
	default:
		return "unknown", "#9f9f9f"
	}
}

func RenderBadgeSVG(label, statusText, colorHex string) string {
	cleanLabel := strings.TrimSpace(label)
	if cleanLabel == "" {
		cleanLabel = "ci"
	}
	cleanStatus := strings.TrimSpace(statusText)
	if cleanStatus == "" {
		cleanStatus = "unknown"
	}
	cleanColor := strings.TrimSpace(colorHex)
	if !hexColorRegex.MatchString(cleanColor) {
		cleanColor = "#9f9f9f"
	}

	labelWidth := len(cleanLabel)*7 + 14
	if labelWidth < 34 {
		labelWidth = 34
	}
	statusWidth := len(cleanStatus)*7 + 14
	if statusWidth < 52 {
		statusWidth = 52
	}
	totalWidth := labelWidth + statusWidth

	labelX := float64(labelWidth) / 2.0
	statusX := float64(labelWidth) + float64(statusWidth)/2.0

	escapedLabel := html.EscapeString(cleanLabel)
	escapedStatus := html.EscapeString(cleanStatus)

	return fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">`+
			`<title>%s: %s</title>`+
			`<linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>`+
			`<clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>`+
			`<g clip-path="url(#r)">`+
			`<rect width="%d" height="20" fill="#555"/>`+
			`<rect x="%d" width="%d" height="20" fill="%s"/>`+
			`<rect width="%d" height="20" fill="url(#s)"/>`+
			`</g>`+
			`<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" text-rendering="geometricPrecision" font-size="11">`+
			`<text x="%.1f" y="14">%s</text>`+
			`<text x="%.1f" y="14">%s</text>`+
			`</g>`+
			`</svg>`,
		totalWidth, escapedLabel, escapedStatus,
		escapedLabel, escapedStatus,
		totalWidth,
		labelWidth,
		labelWidth, statusWidth, cleanColor,
		totalWidth,
		labelX, escapedLabel,
		statusX, escapedStatus,
	)
}

func (h *BadgesHandler) HandleBadge(c echo.Context) error {
	if h == nil || h.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "ci run store is not configured"})
	}

	rawParam := strings.TrimSpace(c.Param("repo"))
	if !strings.HasSuffix(rawParam, ".svg") {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "badge path must end with .svg"})
	}
	repoName := strings.TrimSuffix(rawParam, ".svg")
	if err := ValidateSlug(repoName); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	owner := strings.TrimSpace(c.QueryParam("owner"))
	if owner != "" {
		if err := ValidateSlug(owner); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}

	statusText, colorHex := BadgeStatusForRun("")
	latest, err := h.store.LatestByRepo(c.Request().Context(), owner, repoName)
	if err == nil {
		statusText, colorHex = BadgeStatusForRun(latest.Status)
	} else if !errors.Is(err, ErrRunNotFound) {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	svg := RenderBadgeSVG("ci", statusText, colorHex)
	c.Response().Header().Set(echo.HeaderContentType, "image/svg+xml; charset=utf-8")
	c.Response().Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	c.Response().Header().Set("Pragma", "no-cache")
	return c.String(http.StatusOK, svg)
}

func (h *BadgesHandler) HandleListRuns(c echo.Context) error {
	if h == nil || h.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "ci run store is not configured"})
	}

	limit, _ := strconv.Atoi(strings.TrimSpace(c.QueryParam("limit")))
	runs, err := h.store.List(c.Request().Context(), RunFilter{
		Owner:  strings.TrimSpace(c.QueryParam("owner")),
		Repo:   strings.TrimSpace(c.QueryParam("repo")),
		Status: strings.TrimSpace(c.QueryParam("status")),
		Limit:  limit,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if runs == nil {
		runs = []RunRecord{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"runs":  runs,
		"count": len(runs),
	})
}

type NtfyNotifier struct {
	topicURL   string
	httpClient *http.Client
}

func NewNtfyNotifier(topicURL string, httpClient *http.Client) *NtfyNotifier {
	trimmed := strings.TrimRight(strings.TrimSpace(topicURL), "/")
	if trimmed == "" {
		return &NtfyNotifier{}
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return &NtfyNotifier{}
	}
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &NtfyNotifier{
		topicURL:   trimmed,
		httpClient: client,
	}
}

func (n *NtfyNotifier) Enabled() bool {
	return n != nil && n.topicURL != ""
}

func (n *NtfyNotifier) TopicURL() string {
	if n == nil {
		return ""
	}
	return n.topicURL
}

func (n *NtfyNotifier) NotifyFailure(ctx context.Context, run RunRecord) error {
	if !n.Enabled() || run.Status != StatusFailed {
		return nil
	}

	displayRepo := strings.TrimSpace(run.FullName)
	if displayRepo == "" {
		displayRepo = strings.TrimSpace(run.RepoName)
	}
	if displayRepo == "" {
		displayRepo = "unknown"
	}

	body := fmt.Sprintf(
		"CI run %s failed for %s\nRef: %s\nSHA: %s\nDuration: %dms\nSummary: %s",
		run.ID,
		displayRepo,
		run.Ref,
		run.SHA,
		run.DurationMs,
		run.Summary,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.topicURL, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("create ntfy request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("User-Agent", "mw-lab-ci/1.0")
	req.Header.Set("Title", fmt.Sprintf("[mw-lab] CI failed: %s", displayRepo))
	req.Header.Set("Priority", "high")
	req.Header.Set("Tags", "rotating_light,ci")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send ntfy alert: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned http %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
