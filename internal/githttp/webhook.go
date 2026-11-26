package githttp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/A-L-L-A-H/mw-lab/internal/store"
)

type RepoSummary struct {
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	FullName string `json:"fullName"`
}

type PushPayload struct {
	Event      string       `json:"event"`
	DeliveryID string       `json:"deliveryId"`
	Repository RepoSummary  `json:"repository"`
	Ref        string       `json:"ref"`
	Before     string       `json:"before"`
	After      string       `json:"after"`
	Pusher     string       `json:"pusher"`
	Commits    []CommitInfo `json:"commits"`
	Timestamp  string       `json:"timestamp"`
}

type WebhookDispatcher struct {
	dbStore    *store.Store
	httpClient *http.Client
	inflight   sync.WaitGroup
}

func NewWebhookDispatcher(dbStore *store.Store, httpClient *http.Client) *WebhookDispatcher {
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &WebhookDispatcher{
		dbStore:    dbStore,
		httpClient: client,
	}
}

func (d *WebhookDispatcher) Wait() {
	d.inflight.Wait()
}

func (d *WebhookDispatcher) DispatchPushAsync(eventID int64, payload PushPayload) {
	if d == nil || d.dbStore == nil {
		return
	}
	d.inflight.Add(1)
	go func() {
		defer d.inflight.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		d.DispatchPush(ctx, eventID, payload)
	}()
}

func (d *WebhookDispatcher) DispatchPush(ctx context.Context, eventID int64, payload PushPayload) {
	if d == nil || d.dbStore == nil {
		return
	}
	if payload.Event == "" {
		payload.Event = "push"
	}
	if payload.DeliveryID == "" {
		payload.DeliveryID = generateDeliveryID()
	}
	if payload.Timestamp == "" {
		payload.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	hooks, err := d.dbStore.MatchWebhooks(ctx, payload.Repository.Owner, payload.Repository.Name, payload.Event)
	if err != nil {
		if eventID > 0 {
			_ = d.dbStore.UpdatePushEventStatus(ctx, eventID, "match_error")
		}
		return
	}
	if len(hooks) == 0 {
		if eventID > 0 {
			_ = d.dbStore.UpdatePushEventStatus(ctx, eventID, "no_webhooks")
		}
		return
	}

	encodedBody, err := json.Marshal(payload)
	if err != nil {
		if eventID > 0 {
			_ = d.dbStore.UpdatePushEventStatus(ctx, eventID, "marshal_error")
		}
		return
	}

	succeeded := 0
	lastFailure := ""
	for _, hook := range hooks {
		if deliverErr := d.sendSingle(ctx, hook, payload.Event, payload.DeliveryID, encodedBody); deliverErr != nil {
			lastFailure = deliverErr.Error()
			continue
		}
		succeeded++
	}

	if eventID > 0 {
		statusText := fmt.Sprintf("delivered:%d/%d", succeeded, len(hooks))
		if succeeded < len(hooks) && lastFailure != "" {
			statusText = fmt.Sprintf("partial:%d/%d (%s)", succeeded, len(hooks), truncateStatus(lastFailure, 120))
			if succeeded == 0 {
				statusText = fmt.Sprintf("failed:0/%d (%s)", len(hooks), truncateStatus(lastFailure, 120))
			}
		}
		_ = d.dbStore.UpdatePushEventStatus(ctx, eventID, statusText)
	}
}

func (d *WebhookDispatcher) sendSingle(ctx context.Context, hook store.WebhookRecord, eventName, deliveryID string, encodedBody []byte) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.TargetURL, bytes.NewReader(encodedBody))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "mw-lab-webhook/1.0")
	httpReq.Header.Set("X-MW-Lab-Event", eventName)
	httpReq.Header.Set("X-MW-Lab-Delivery", deliveryID)
	if strings.TrimSpace(hook.Secret) != "" {
		mac := hmac.New(sha256.New, []byte(hook.Secret))
		_, _ = mac.Write(encodedBody)
		httpReq.Header.Set("X-MW-Lab-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	httpResp, err := d.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, 4096))
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return fmt.Errorf("http %d", httpResp.StatusCode)
	}
	return nil
}

func generateDeliveryID() string {
	var rawBytes [16]byte
	if _, err := rand.Read(rawBytes[:]); err != nil {
		return fmt.Sprintf("dlv-%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(rawBytes[:])
}

func truncateStatus(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen]
}
