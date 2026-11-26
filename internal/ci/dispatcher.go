package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const zeroCommitSHA = "0000000000000000000000000000000000000000"

var (
	ErrNotConfigured = errors.New("mission url is not configured")
	ErrInvalidInput  = errors.New("invalid ci dispatch input")
)

type StageTask struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Stage          string   `json:"stage"`
	DependsOn      []string `json:"depends_on"`
	CapabilityTags []string `json:"capability_tags"`
	Command        string   `json:"command"`
}

type MissionRequest struct {
	Title          string            `json:"title"`
	IdempotencyKey string            `json:"idempotency_key"`
	CapabilityTags []string          `json:"capability_tags"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Tasks          []StageTask       `json:"tasks"`
}

type DispatchInput struct {
	Owner  string
	Repo   string
	Ref    string
	SHA    string
	Pusher string
}

type DispatchResult struct {
	Mission         MissionRequest  `json:"mission"`
	StatusCode      int             `json:"statusCode"`
	MissionResponse json.RawMessage `json:"missionResponse,omitempty"`
}

type Dispatcher struct {
	baseURL    string
	httpClient *http.Client
	inflight   sync.WaitGroup
}

func NewDispatcher(baseURL string, httpClient *http.Client) *Dispatcher {
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return &Dispatcher{
		baseURL:    trimmed,
		httpClient: client,
	}
}

func ValidateMissionURL(rawURL string) error {
	trimmed := strings.TrimSpace(rawURL)
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("missionUrl must be a valid http or https URL")
	}
	return nil
}

func (d *Dispatcher) Enabled() bool {
	return d != nil && d.baseURL != ""
}

func (d *Dispatcher) BaseURL() string {
	if d == nil {
		return ""
	}
	return d.baseURL
}

func (d *Dispatcher) Wait() {
	if d == nil {
		return
	}
	d.inflight.Wait()
}

func BuildMission(input DispatchInput) (MissionRequest, error) {
	owner := strings.TrimSpace(input.Owner)
	repo := strings.TrimSpace(input.Repo)
	commitSHA := strings.TrimSpace(input.SHA)
	if owner == "" || repo == "" || commitSHA == "" || commitSHA == zeroCommitSHA {
		return MissionRequest{}, fmt.Errorf("%w: owner, repo, and non-zero sha are required", ErrInvalidInput)
	}

	refName := strings.TrimSpace(input.Ref)
	if refName == "" {
		refName = "refs/heads/main"
	}
	pusher := strings.TrimSpace(input.Pusher)
	if pusher == "" {
		pusher = "unknown"
	}

	shortSHA := commitSHA
	if len(shortSHA) > 12 {
		shortSHA = shortSHA[:12]
	}

	idempotencyKey := fmt.Sprintf("ci:%s/%s:%s", owner, repo, commitSHA)
	sharedTags := []string{"git", "ci"}

	return MissionRequest{
		Title:          fmt.Sprintf("CI %s/%s@%s", owner, repo, shortSHA),
		IdempotencyKey: idempotencyKey,
		CapabilityTags: sharedTags,
		Metadata: map[string]string{
			"owner":  owner,
			"repo":   repo,
			"ref":    refName,
			"sha":    commitSHA,
			"pusher": pusher,
		},
		Tasks: []StageTask{
			{
				ID:             "lint",
				Name:           "Lint",
				Stage:          "lint",
				DependsOn:      []string{},
				CapabilityTags: []string{"git", "ci"},
				Command:        "go vet ./...",
			},
			{
				ID:             "test",
				Name:           "Test",
				Stage:          "test",
				DependsOn:      []string{"lint"},
				CapabilityTags: []string{"git", "ci"},
				Command:        "go test -count=1 ./...",
			},
			{
				ID:             "build",
				Name:           "Build",
				Stage:          "build",
				DependsOn:      []string{"test"},
				CapabilityTags: []string{"git", "ci"},
				Command:        "go build ./...",
			},
		},
	}, nil
}

func (d *Dispatcher) Dispatch(ctx context.Context, input DispatchInput) (DispatchResult, error) {
	if !d.Enabled() {
		return DispatchResult{}, ErrNotConfigured
	}
	mission, err := BuildMission(input)
	if err != nil {
		return DispatchResult{}, err
	}

	encodedBody, err := json.Marshal(mission)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("marshal ci mission: %w", err)
	}

	endpoint := d.baseURL + "/api/missions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encodedBody))
	if err != nil {
		return DispatchResult{}, fmt.Errorf("create mission request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "mw-lab-ci/1.0")
	httpReq.Header.Set("Idempotency-Key", mission.IdempotencyKey)

	httpResp, err := d.httpClient.Do(httpReq)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("dispatch mission to %s: %w", endpoint, err)
	}
	defer httpResp.Body.Close()

	rawRespBody, readErr := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if readErr != nil {
		return DispatchResult{}, fmt.Errorf("read mission response: %w", readErr)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return DispatchResult{}, fmt.Errorf("mission endpoint returned http %d: %s", httpResp.StatusCode, strings.TrimSpace(string(rawRespBody)))
	}

	outcome := DispatchResult{
		Mission:    mission,
		StatusCode: httpResp.StatusCode,
	}
	trimmedResp := bytes.TrimSpace(rawRespBody)
	if len(trimmedResp) > 0 && json.Valid(trimmedResp) {
		outcome.MissionResponse = json.RawMessage(trimmedResp)
	}
	return outcome, nil
}

func (d *Dispatcher) DispatchAsync(input DispatchInput) {
	if !d.Enabled() {
		return
	}
	commitSHA := strings.TrimSpace(input.SHA)
	if commitSHA == "" || commitSHA == zeroCommitSHA {
		return
	}
	d.inflight.Add(1)
	go func() {
		defer d.inflight.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = d.Dispatch(ctx, input)
	}()
}
