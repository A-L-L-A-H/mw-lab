package lfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/A-L-L-A-H/mw-lab/internal/githttp"
	"github.com/A-L-L-A-H/mw-lab/internal/store"
	"github.com/labstack/echo/v4"
)

const (
	LFSMediaType      = "application/vnd.git-lfs+json"
	defaultActionTTL  = 3600
	defaultMaxObjSize = 512 * 1024 * 1024
)

var oidPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type BatchRequest struct {
	Operation string       `json:"operation"`
	Transfers []string     `json:"transfers,omitempty"`
	Objects   []ObjectSpec `json:"objects"`
}

type ObjectSpec struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type BatchAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int               `json:"expires_in,omitempty"`
}

type BatchError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type BatchObjectResponse struct {
	OID           string                  `json:"oid"`
	Size          int64                   `json:"size"`
	Authenticated bool                    `json:"authenticated,omitempty"`
	Actions       map[string]*BatchAction `json:"actions,omitempty"`
	Error         *BatchError             `json:"error,omitempty"`
}

type BatchResponse struct {
	Transfer string                `json:"transfer"`
	Objects  []BatchObjectResponse `json:"objects"`
}

type Service struct {
	dbStore        *store.Store
	backend        BlobBackend
	publicBaseURL  string
	maxObjectBytes int64
}

func NewService(dbStore *store.Store, backend BlobBackend, publicBaseURL string, maxObjectBytes int64) (*Service, error) {
	if dbStore == nil {
		return nil, fmt.Errorf("store is required for lfs service")
	}
	if backend == nil {
		return nil, fmt.Errorf("blob backend is required for lfs service")
	}
	limitBytes := maxObjectBytes
	if limitBytes <= 0 {
		limitBytes = defaultMaxObjSize
	}
	return &Service{
		dbStore:        dbStore,
		backend:        backend,
		publicBaseURL:  strings.TrimRight(strings.TrimSpace(publicBaseURL), "/"),
		maxObjectBytes: limitBytes,
	}, nil
}

func (s *Service) BackendName() string {
	return s.backend.Name()
}

func (s *Service) RegisterRoutes(echoApp *echo.Echo) {
	echoApp.POST("/:owner/:repo/info/lfs/objects/batch", s.HandleBatch)
	echoApp.PUT("/lfs/objects/:oid", s.HandleUploadObject)
	echoApp.GET("/lfs/objects/:oid", s.HandleDownloadObject)
}

func ValidateOID(oid string) bool {
	return oidPattern.MatchString(oid)
}

func (s *Service) HandleBatch(c echo.Context) error {
	owner := c.Param("owner")
	rawRepo := c.Param("repo")
	if err := githttp.ValidateSlug(owner); err != nil {
		return writeLFSError(c, http.StatusBadRequest, "invalid repository owner")
	}
	repoName, err := githttp.NormalizeRepoName(rawRepo)
	if err != nil {
		return writeLFSError(c, http.StatusBadRequest, "invalid repository name")
	}

	var batchReq BatchRequest
	if err := json.NewDecoder(io.LimitReader(c.Request().Body, 2<<20)).Decode(&batchReq); err != nil {
		return writeLFSError(c, http.StatusUnprocessableEntity, "invalid git lfs batch request json")
	}
	operation := strings.ToLower(strings.TrimSpace(batchReq.Operation))
	if operation != "upload" && operation != "download" {
		return writeLFSError(c, http.StatusUnprocessableEntity, "operation must be 'upload' or 'download'")
	}

	baseURL := s.resolveBaseURL(c.Request())
	ctx := c.Request().Context()
	replyObjects := make([]BatchObjectResponse, 0, len(batchReq.Objects))

	for _, spec := range batchReq.Objects {
		if !ValidateOID(spec.OID) || spec.Size < 0 {
			replyObjects = append(replyObjects, BatchObjectResponse{
				OID:  spec.OID,
				Size: spec.Size,
				Error: &BatchError{
					Code:    http.StatusUnprocessableEntity,
					Message: "invalid oid or size",
				},
			})
			continue
		}

		objectHref := fmt.Sprintf("%s/lfs/objects/%s", baseURL, spec.OID)
		existing, lookupErr := s.dbStore.GetLFSObject(ctx, spec.OID)
		hasUploaded := false
		if lookupErr == nil && existing.Uploaded {
			existsOnBackend, existsErr := s.backend.Exists(ctx, spec.OID, existing.StorageRef)
			if existsErr == nil && existsOnBackend {
				hasUploaded = true
			}
		}

		if operation == "upload" {
			if hasUploaded && existing.Size == spec.Size {
				replyObjects = append(replyObjects, BatchObjectResponse{
					OID:           spec.OID,
					Size:          spec.Size,
					Authenticated: true,
				})
				continue
			}

			_, _ = s.dbStore.UpsertLFSObject(ctx, store.LFSObjectRecord{
				OID:       spec.OID,
				RepoOwner: owner,
				RepoName:  repoName,
				Size:      spec.Size,
				Backend:   s.backend.Name(),
				Uploaded:  false,
			})
			replyObjects = append(replyObjects, BatchObjectResponse{
				OID:           spec.OID,
				Size:          spec.Size,
				Authenticated: true,
				Actions: map[string]*BatchAction{
					"upload": {
						Href:      objectHref,
						Header:    map[string]string{"Content-Type": "application/octet-stream"},
						ExpiresIn: defaultActionTTL,
					},
				},
			})
			continue
		}

		// operation == "download"
		if !hasUploaded {
			replyObjects = append(replyObjects, BatchObjectResponse{
				OID:  spec.OID,
				Size: spec.Size,
				Error: &BatchError{
					Code:    http.StatusNotFound,
					Message: "Object does not exist on the server",
				},
			})
			continue
		}

		replyObjects = append(replyObjects, BatchObjectResponse{
			OID:           spec.OID,
			Size:          existing.Size,
			Authenticated: true,
			Actions: map[string]*BatchAction{
				"download": {
					Href:      objectHref,
					ExpiresIn: defaultActionTTL,
				},
			},
		})
	}

	return writeLFSJSON(c, http.StatusOK, BatchResponse{
		Transfer: "basic",
		Objects:  replyObjects,
	})
}

func (s *Service) HandleUploadObject(c echo.Context) error {
	oid := c.Param("oid")
	if !ValidateOID(oid) {
		return writeLFSError(c, http.StatusBadRequest, "invalid lfs oid")
	}

	spoolFile, err := os.CreateTemp("", "mw-lab-lfs-spool-*")
	if err != nil {
		return writeLFSError(c, http.StatusInternalServerError, "failed to allocate upload spool")
	}
	spoolPath := spoolFile.Name()
	defer func() {
		_ = spoolFile.Close()
		_ = os.Remove(spoolPath)
	}()

	hasher := sha256.New()
	limitedBody := io.LimitReader(c.Request().Body, s.maxObjectBytes+1)
	writtenBytes, err := io.Copy(io.MultiWriter(spoolFile, hasher), limitedBody)
	if err != nil {
		return writeLFSError(c, http.StatusBadRequest, "failed to read upload stream")
	}
	if writtenBytes > s.maxObjectBytes {
		return writeLFSError(c, http.StatusRequestEntityTooLarge, "lfs object exceeds configured limit")
	}

	computedOID := hex.EncodeToString(hasher.Sum(nil))
	if computedOID != oid {
		return writeLFSError(c, http.StatusUnprocessableEntity, fmt.Sprintf("sha256 mismatch: expected %s, got %s", oid, computedOID))
	}

	if _, err := spoolFile.Seek(0, io.SeekStart); err != nil {
		return writeLFSError(c, http.StatusInternalServerError, "failed to rewind spooled upload")
	}

	ctx := c.Request().Context()
	storageRef, err := s.backend.Put(ctx, oid, spoolFile, writtenBytes)
	if err != nil {
		return writeLFSError(c, http.StatusBadGateway, "lfs backend upload failed: "+err.Error())
	}

	owner := ""
	repoName := ""
	if existing, lookupErr := s.dbStore.GetLFSObject(ctx, oid); lookupErr == nil {
		owner = existing.RepoOwner
		repoName = existing.RepoName
	}

	savedObj, err := s.dbStore.UpsertLFSObject(ctx, store.LFSObjectRecord{
		OID:        oid,
		RepoOwner:  owner,
		RepoName:   repoName,
		Size:       writtenBytes,
		Backend:    s.backend.Name(),
		StorageRef: storageRef,
		Uploaded:   true,
	})
	if err != nil {
		return writeLFSError(c, http.StatusInternalServerError, "failed to persist lfs metadata")
	}

	return c.JSON(http.StatusOK, savedObj)
}

func (s *Service) HandleDownloadObject(c echo.Context) error {
	oid := c.Param("oid")
	if !ValidateOID(oid) {
		return writeLFSError(c, http.StatusBadRequest, "invalid lfs oid")
	}

	ctx := c.Request().Context()
	lfsRecord, err := s.dbStore.GetLFSObject(ctx, oid)
	if errors.Is(err, store.ErrNotFound) || !lfsRecord.Uploaded {
		return writeLFSError(c, http.StatusNotFound, "lfs object not found")
	}
	if err != nil {
		return writeLFSError(c, http.StatusInternalServerError, "failed to query lfs object")
	}

	stream, contentLength, err := s.backend.Open(ctx, oid, lfsRecord.StorageRef)
	if errors.Is(err, ErrObjectNotFound) {
		return writeLFSError(c, http.StatusNotFound, "lfs object missing from storage backend")
	}
	if err != nil {
		return writeLFSError(c, http.StatusBadGateway, "lfs backend download failed: "+err.Error())
	}
	defer stream.Close()

	c.Response().Header().Set("Content-Type", "application/octet-stream")
	c.Response().Header().Set("ETag", `"`+oid+`"`)
	c.Response().Header().Set("X-MW-Lab-LFS-Backend", lfsRecord.Backend)
	if contentLength >= 0 {
		c.Response().Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	} else if lfsRecord.Size >= 0 {
		c.Response().Header().Set("Content-Length", strconv.FormatInt(lfsRecord.Size, 10))
	}
	c.Response().WriteHeader(http.StatusOK)
	_, _ = io.Copy(c.Response().Writer, stream)
	return nil
}

func (s *Service) resolveBaseURL(httpReq *http.Request) string {
	if s.publicBaseURL != "" {
		return s.publicBaseURL
	}
	scheme := "http"
	if proto := strings.TrimSpace(httpReq.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	} else if httpReq.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + httpReq.Host
}

func writeLFSError(c echo.Context, statusCode int, message string) error {
	return writeLFSJSON(c, statusCode, map[string]string{"message": message})
}

func writeLFSJSON(c echo.Context, statusCode int, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return c.Blob(http.StatusInternalServerError, LFSMediaType, []byte(`{"message":"json encoding failed"}`))
	}
	return c.Blob(statusCode, LFSMediaType, encoded)
}
