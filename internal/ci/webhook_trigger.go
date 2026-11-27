package ci

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"
)

const defaultWebhookMaxBodyBytes int64 = 2 << 20 // 2 MiB

type RepoResolver func(owner, repo string) (clonePath string, ok bool)

type WebhookTriggerConfig struct {
	Secret       string
	Runner       *LocalRunner
	RepoResolver RepoResolver
	MaxBodyBytes int64
}

type WebhookTriggerHandler struct {
	secret       string
	runner       *LocalRunner
	repoResolver RepoResolver
	maxBodyBytes int64
}

func NewWebhookTriggerHandler(cfg WebhookTriggerConfig) *WebhookTriggerHandler {
	maxBytes := cfg.MaxBodyBytes
	if maxBytes <= 0 {
		maxBytes = defaultWebhookMaxBodyBytes
	}
	return &WebhookTriggerHandler{
		secret:       strings.TrimSpace(cfg.Secret),
		runner:       cfg.Runner,
		repoResolver: cfg.RepoResolver,
		maxBodyBytes: maxBytes,
	}
}

func VerifyWebhookSignature(secret string, body []byte, signatureHeader string) error {
	trimmedSecret := strings.TrimSpace(secret)
	if trimmedSecret == "" {
		return errors.New("webhook secret is not configured")
	}
	trimmedSig := strings.TrimSpace(signatureHeader)
	if len(trimmedSig) <= len("sha256=") || !strings.EqualFold(trimmedSig[:7], "sha256=") {
		return errors.New("missing or malformed sha256 signature prefix")
	}
	providedHex := strings.TrimSpace(trimmedSig[7:])
	providedMAC, err := hex.DecodeString(providedHex)
	if err != nil || len(providedMAC) != sha256.Size {
		return errors.New("invalid hex signature")
	}

	mac := hmac.New(sha256.New, []byte(trimmedSecret))
	_, _ = mac.Write(body)
	expectedMAC := mac.Sum(nil)
	if !hmac.Equal(expectedMAC, providedMAC) {
		return errors.New("signature mismatch")
	}
	return nil
}

func (h *WebhookTriggerHandler) HandleWebhook(c echo.Context) error {
	if h == nil || h.secret == "" {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "ci webhook secret is not configured"})
	}
	if h.runner == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "ci runner is not configured"})
	}

	rawBody, err := io.ReadAll(io.LimitReader(c.Request().Body, h.maxBodyBytes+1))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
	}
	if int64(len(rawBody)) > h.maxBodyBytes {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "webhook payload too large"})
	}

	sigHeader := c.Request().Header.Get("X-Hub-Signature-256")
	if strings.TrimSpace(sigHeader) == "" {
		sigHeader = c.Request().Header.Get("X-MW-Lab-Signature-256")
	}
	if err := VerifyWebhookSignature(h.secret, rawBody, sigHeader); err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid webhook signature"})
	}

	eventName := firstNonEmpty(
		c.Request().Header.Get("X-GitHub-Event"),
		c.Request().Header.Get("X-Gitea-Event"),
		c.Request().Header.Get("X-MW-Lab-Event"),
	)
	if strings.EqualFold(eventName, "ping") {
		return c.JSON(http.StatusOK, map[string]any{"ok": true, "event": "ping"})
	}
	if eventName != "" && !strings.EqualFold(eventName, "push") {
		return c.JSON(http.StatusOK, map[string]any{"ignored": true, "event": eventName})
	}

	parsed, err := parseWebhookPayload(rawBody)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	if eventName == "" && parsed.Event != "" {
		if strings.EqualFold(parsed.Event, "ping") {
			return c.JSON(http.StatusOK, map[string]any{"ok": true, "event": "ping"})
		}
		if !strings.EqualFold(parsed.Event, "push") {
			return c.JSON(http.StatusOK, map[string]any{"ignored": true, "event": parsed.Event})
		}
	}

	if parsed.After == zeroCommitSHA {
		return c.JSON(http.StatusOK, map[string]any{"ignored": true, "reason": "ref deleted"})
	}
	if parsed.After != "" && !commitSHAPattern.MatchString(parsed.After) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid commit sha"})
	}

	if parsed.Owner != "" {
		if err := ValidateSlug(parsed.Owner); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid repository owner: %v", err)})
		}
	}
	if err := ValidateSlug(parsed.Repo); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid repository name: %v", err)})
	}

	cloneTarget := ""
	localMissing := false
	if h.repoResolver != nil {
		if resolvedPath, ok := h.repoResolver(parsed.Owner, parsed.Repo); ok && strings.TrimSpace(resolvedPath) != "" {
			cloneTarget = resolvedPath
		} else {
			localMissing = true
		}
	}
	if cloneTarget == "" && parsed.CloneURL != "" {
		if err := validateWebhookCloneURL(parsed.CloneURL); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		cloneTarget = parsed.CloneURL
	}
	if cloneTarget == "" {
		if localMissing {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "repository not found"})
		}
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "repository clone target could not be resolved"})
	}

	runRecord, err := h.runner.Enqueue(c.Request().Context(), RunRequest{
		Owner:    parsed.Owner,
		Repo:     parsed.Repo,
		Ref:      parsed.Ref,
		SHA:      parsed.After,
		Pusher:   parsed.Pusher,
		Source:   "webhook",
		CloneURL: cloneTarget,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusAccepted, map[string]any{
		"status": "queued",
		"run":    runRecord,
	})
}

type parsedWebhookPayload struct {
	Event    string
	Owner    string
	Repo     string
	Ref      string
	After    string
	Pusher   string
	CloneURL string
}

type flexOwner struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

func (f *flexOwner) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var str string
		if err := json.Unmarshal(trimmed, &str); err != nil {
			return err
		}
		f.Login = str
		f.Name = str
		return nil
	}
	var obj struct {
		Login    string `json:"login"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return err
	}
	f.Login = firstNonEmpty(obj.Login, obj.Username, obj.Name)
	f.Name = firstNonEmpty(obj.Name, obj.Login, obj.Username)
	return nil
}

func parseWebhookPayload(rawBody []byte) (parsedWebhookPayload, error) {
	if len(bytes.TrimSpace(rawBody)) == 0 {
		return parsedWebhookPayload{}, errors.New("empty webhook payload")
	}

	var envelope struct {
		Event      string `json:"event"`
		Ref        string `json:"ref"`
		After      string `json:"after"`
		HeadCommit struct {
			ID string `json:"id"`
		} `json:"head_commit"`
		Pusher     flexOwner `json:"pusher"`
		Repository struct {
			Owner       flexOwner `json:"owner"`
			Name        string    `json:"name"`
			FullName    string    `json:"full_name"`
			FullNameCam string    `json:"fullName"`
			CloneURL    string    `json:"clone_url"`
			CloneURLCam string    `json:"cloneUrl"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return parsedWebhookPayload{}, errors.New("invalid webhook json payload")
	}

	owner := strings.TrimSpace(envelope.Repository.Owner.Login)
	repo := strings.TrimSpace(envelope.Repository.Name)
	fullName := firstNonEmpty(envelope.Repository.FullName, envelope.Repository.FullNameCam)
	if (owner == "" || repo == "") && fullName != "" {
		parts := strings.SplitN(fullName, "/", 2)
		if len(parts) == 2 {
			if owner == "" {
				owner = strings.TrimSpace(parts[0])
			}
			if repo == "" {
				repo = strings.TrimSpace(parts[1])
			}
		} else if repo == "" {
			repo = strings.TrimSpace(parts[0])
		}
	}
	repo = strings.TrimSuffix(repo, ".git")

	ref := strings.TrimSpace(envelope.Ref)
	if ref == "" {
		ref = "refs/heads/main"
	}
	after := firstNonEmpty(envelope.After, envelope.HeadCommit.ID)
	pusher := firstNonEmpty(envelope.Pusher.Login, envelope.Pusher.Name)
	cloneURL := firstNonEmpty(envelope.Repository.CloneURL, envelope.Repository.CloneURLCam)

	return parsedWebhookPayload{
		Event:    strings.TrimSpace(envelope.Event),
		Owner:    owner,
		Repo:     repo,
		Ref:      ref,
		After:    after,
		Pusher:   pusher,
		CloneURL: cloneURL,
	}, nil
}

func validateWebhookCloneURL(rawURL string) error {
	trimmed := strings.TrimSpace(rawURL)
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("repository clone_url must be a valid http or https URL")
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
