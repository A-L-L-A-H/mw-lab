package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/A-L-L-A-H/mw-lab/internal/auth"
	"github.com/A-L-L-A-H/mw-lab/internal/ci"
	"github.com/A-L-L-A-H/mw-lab/internal/githttp"
	"github.com/A-L-L-A-H/mw-lab/internal/lfs"
	"github.com/A-L-L-A-H/mw-lab/internal/store"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

//go:embed dashboard.html
var embeddedDashboardHTML string

type Config struct {
	Addr              string
	AuthToken         string
	RIDJWKSURL        string
	RIDIssuer         string
	RIDAudience       string
	RIDVerifier       *auth.RIDVerifier
	Version           string
	MissionURL        string
	CIHTTPClient      *http.Client
	CIRunStore        *ci.RunStore
	CIRunner          *ci.LocalRunner
	CIWebhookSecret   string
	CINtfyTopicURL    string
}

type Server struct {
	echoApp           *echo.Echo
	dbStore           *store.Store
	gitManager        *githttp.Manager
	gitHandler        *githttp.Handler
	dispatcher        *githttp.WebhookDispatcher
	ciDispatcher      *ci.Dispatcher
	ciRunStore        *ci.RunStore
	ciRunner          *ci.LocalRunner
	ciBadgesHandler   *ci.BadgesHandler
	ciWebhookHandler  *ci.WebhookTriggerHandler
	ciNtfyNotifier    *ci.NtfyNotifier
	lfsService        *lfs.Service
	ridVerifier       *auth.RIDVerifier
	cfg               Config
}

type webhookView struct {
	ID               int64  `json:"id"`
	RepoOwner        string `json:"repoOwner"`
	RepoName         string `json:"repoName"`
	TargetURL        string `json:"targetUrl"`
	SecretConfigured bool   `json:"secretConfigured"`
	Events           string `json:"events"`
	Active           bool   `json:"active"`
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
}

func New(cfg Config, dbStore *store.Store, gitManager *githttp.Manager, dispatcher *githttp.WebhookDispatcher, lfsService *lfs.Service) *Server {
	if strings.TrimSpace(cfg.Version) == "" {
		cfg.Version = "0.1.0"
	}
	echoApp := echo.New()
	echoApp.HideBanner = true
	echoApp.HidePort = true

	echoApp.Use(middleware.Recover())
	echoApp.Use(middleware.RequestID())
	echoApp.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions},
		AllowHeaders: []string{echo.HeaderOrigin, echo.HeaderContentType, echo.HeaderAccept, echo.HeaderAuthorization, "X-MW-Lab-Pusher"},
	}))

	missionURL := strings.TrimSpace(cfg.MissionURL)
	if missionURL == "" {
		missionURL = strings.TrimSpace(os.Getenv("MW_MISSION_URL"))
	}
	ciDispatcher := ci.NewDispatcher(missionURL, cfg.CIHTTPClient)

	ridVerifier := cfg.RIDVerifier
	if ridVerifier == nil && strings.TrimSpace(cfg.RIDJWKSURL) != "" {
		if createdVerifier, err := auth.NewRIDVerifier(auth.RIDConfig{
			JWKSURL:  cfg.RIDJWKSURL,
			Issuer:   cfg.RIDIssuer,
			Audience: cfg.RIDAudience,
		}); err == nil {
			ridVerifier = createdVerifier
		}
	}

	gitHandler := githttp.NewHandler(gitManager, dbStore, dispatcher)
	gitHandler.SetCIDispatcher(ciDispatcher)
	gitHandler.SetAuth(strings.TrimSpace(cfg.AuthToken), ridVerifier)

	ciRunStore := cfg.CIRunStore
	if ciRunStore == nil && dbStore != nil {
		if createdStore, err := ci.NewRunStore(dbStore.DB()); err == nil {
			ciRunStore = createdStore
		}
	}
	if ciRunStore == nil {
		ciRunStore, _ = ci.NewRunStore(nil)
	}

	ntfyTopicURL := strings.TrimSpace(cfg.CINtfyTopicURL)
	if ntfyTopicURL == "" {
		ntfyTopicURL = strings.TrimSpace(os.Getenv("MW_LAB_NTFY_URL"))
	}
	ciNtfyNotifier := ci.NewNtfyNotifier(ntfyTopicURL, cfg.CIHTTPClient)

	ciRunner := cfg.CIRunner
	if ciRunner == nil {
		ciRunner = ci.NewLocalRunner(ci.LocalRunnerConfig{
			Store: ciRunStore,
			OnFailure: func(ctx context.Context, run ci.RunRecord) {
				_ = ciNtfyNotifier.NotifyFailure(ctx, run)
			},
			AllowLocalClone: true,
		})
	}

	ciBadgesHandler := ci.NewBadgesHandler(ciRunStore)

	webhookSecret := strings.TrimSpace(cfg.CIWebhookSecret)
	if webhookSecret == "" {
		webhookSecret = strings.TrimSpace(os.Getenv("MW_LAB_CI_WEBHOOK_SECRET"))
	}
	ciWebhookHandler := ci.NewWebhookTriggerHandler(ci.WebhookTriggerConfig{
		Secret: webhookSecret,
		Runner: ciRunner,
		RepoResolver: func(owner, repo string) (string, bool) {
			if gitManager == nil {
				return "", false
			}
			barePath, err := gitManager.RepoPath(owner, repo)
			if err != nil {
				return "", false
			}
			if stat, err := os.Stat(barePath); err == nil && stat.IsDir() {
				return barePath, true
			}
			return "", false
		},
	})

	srv := &Server{
		echoApp:          echoApp,
		dbStore:          dbStore,
		gitManager:       gitManager,
		gitHandler:       gitHandler,
		dispatcher:       dispatcher,
		ciDispatcher:     ciDispatcher,
		ciRunStore:       ciRunStore,
		ciRunner:         ciRunner,
		ciBadgesHandler:  ciBadgesHandler,
		ciWebhookHandler: ciWebhookHandler,
		ciNtfyNotifier:   ciNtfyNotifier,
		lfsService:       lfsService,
		ridVerifier:      ridVerifier,
		cfg:              cfg,
	}
	srv.registerRoutes()
	return srv
}

func (s *Server) WaitCI() {
	if s.ciDispatcher != nil {
		s.ciDispatcher.Wait()
	}
}

func (s *Server) Echo() *echo.Echo {
	return s.echoApp
}

func (s *Server) Handler() http.Handler {
	return s.echoApp
}

func (s *Server) registerRoutes() {
	s.echoApp.GET("/", func(c echo.Context) error {
		return c.HTML(http.StatusOK, embeddedDashboardHTML)
	})

	s.echoApp.GET("/health", s.handleHealth)

	api := s.echoApp.Group("/api")
	if strings.TrimSpace(s.cfg.AuthToken) != "" || s.ridVerifier != nil {
		api.Use(s.bearerAuthMiddleware(strings.TrimSpace(s.cfg.AuthToken)))
	}

	api.GET("/repos", s.handleListRepos)
	api.POST("/repos", s.handleCreateRepo)
	api.GET("/repos/:owner/:name", s.handleGetRepo)
	api.PATCH("/repos/:owner/:name", s.handleUpdateRepo)
	api.DELETE("/repos/:owner/:name", s.handleDeleteRepo)
	api.POST("/repos/:owner/:name/mirror", s.handleMirrorRepo)
	api.POST("/repos/:owner/:name/trigger-ci", s.handleTriggerCI)
	api.GET("/repos/:owner/:name/branches", s.handleListBranches)
	api.GET("/repos/:owner/:name/commits", s.handleListCommits)
	api.GET("/repos/:owner/:name/tree", s.handleListTree)
	api.GET("/repos/:owner/:name/blob", s.handleReadBlob)
	api.GET("/repos/:owner/:name/events", s.handleListRepoEvents)

	api.GET("/webhooks", s.handleListWebhooks)
	api.POST("/webhooks", s.handleCreateWebhook)
	api.PATCH("/webhooks/:id", s.handleUpdateWebhook)
	api.DELETE("/webhooks/:id", s.handleDeleteWebhook)

	api.GET("/lfs/objects", s.handleListLFSObjects)
	api.GET("/events", s.handleListAllEvents)

	api.GET("/ci/runs", s.ciBadgesHandler.HandleListRuns)

	// Public SVG Badges & External Webhook Ingestion
	s.echoApp.GET("/api/ci/badges/:repo", s.ciBadgesHandler.HandleBadge)
	s.echoApp.POST("/api/ci/webhook", s.ciWebhookHandler.HandleWebhook)

	// Auth endpoints (public)
	s.echoApp.POST("/api/auth/signup", s.handleSignup)
	s.echoApp.POST("/api/auth/login", s.handleLogin)
	s.echoApp.POST("/api/auth/logout", s.handleLogout)
	s.echoApp.GET("/api/auth/me", s.handleMe)

	// Auth endpoints (protected)
	authGroup := s.echoApp.Group("/api/auth", s.bearerAuthMiddleware(strings.TrimSpace(s.cfg.AuthToken)))
	authGroup.POST("/tokens", s.handleCreateToken)
	authGroup.GET("/tokens", s.handleListTokens)
	authGroup.DELETE("/tokens/:id", s.handleDeleteToken)

	// Git Smart HTTP and Git LFS Batch API
	s.gitHandler.RegisterRoutes(s.echoApp)
	s.lfsService.RegisterRoutes(s.echoApp)
}

func (s *Server) CIRunner() *ci.LocalRunner {
	return s.ciRunner
}

func (s *Server) CIRunStore() *ci.RunStore {
	return s.ciRunStore
}

func (s *Server) bearerAuthMiddleware(expectedToken string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Check Authorization: Bearer header
			authHeader := strings.TrimSpace(c.Request().Header.Get("Authorization"))
			provided := strings.TrimPrefix(authHeader, "Bearer ")
			if provided != authHeader {
				provided = strings.TrimSpace(provided)
				if provided != "" {
					if expectedToken != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(expectedToken)) == 1 {
						c.Set("auth_method", "static_token")
						return next(c)
					}
					if s.ridVerifier != nil {
						claims, err := s.ridVerifier.Verify(c.Request().Context(), provided)
						if err == nil {
							c.Set("rid_subject", claims.Subject)
							c.Set("rid_issuer", claims.Issuer)
							c.Set("rid_scope", claims.Scope)
							c.Set("auth_method", "oidc")
							return next(c)
						}
					}
					// Check access token
					if s.dbStore != nil {
						hash := auth.TokenHash(provided)
						if tokenRec, err := s.dbStore.GetAccessTokenByHash(c.Request().Context(), hash); err == nil {
							c.Set("auth_method", "access_token")
							c.Set("auth_user_id", tokenRec.UserID)
							return next(c)
						}
					}
				}
			}

			// Check session cookie
			if cookie, err := c.Cookie("mw_lab_session"); err == nil && cookie.Value != "" {
				if s.dbStore != nil {
					if sessionRec, err := s.dbStore.GetSession(c.Request().Context(), cookie.Value); err == nil {
						if sessionRec.ExpiresAt > time.Now().UTC().Format(time.RFC3339Nano) {
							c.Set("auth_method", "session")
							c.Set("auth_user_id", sessionRec.UserID)
							return next(c)
						}
					}
				}
			}

			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		}
	}
}

func (s *Server) handleHealth(c echo.Context) error {
	metrics, err := s.dbStore.Metrics(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"status": "error", "error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"status":     "ok",
		"service":    "mw-lab",
		"version":    s.cfg.Version,
		"lfsBackend": s.lfsService.BackendName(),
		"metrics":    metrics,
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// --- Auth endpoints ---

func (s *Server) handleSignup(c echo.Context) error {
	if s.dbStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "user store not available"})
	}
	var body struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	body.Username = strings.TrimSpace(body.Username)
	body.Email = strings.TrimSpace(body.Email)
	if body.Username == "" || body.Email == "" || body.Password == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "username, email, password required"})
	}

	// First user becomes admin
	count, _ := s.dbStore.CountUsers(c.Request().Context())
	isAdmin := count == 0

	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "password hash failed"})
	}

	user, err := s.dbStore.CreateUser(c.Request().Context(), store.UserRecord{
		Username:     body.Username,
		Email:        body.Email,
		PasswordHash: hash,
		IsAdmin:      isAdmin,
	})
	if err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
	}

	// Create session
	tokenPair, _ := auth.CreateSessionToken()
	expiresAt := tokenPair.ExpiresAt.UTC().Format(time.RFC3339Nano)
	_ = s.dbStore.CreateSession(c.Request().Context(), store.SessionRecord{
		Token:     tokenPair.Token,
		UserID:    user.ID,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})

	c.SetCookie(&http.Cookie{
		Name:     "mw_lab_session",
		Value:    tokenPair.Token,
		Path:     "/",
		HttpOnly: true,
		Secure:   false, // set true behind HTTPS
		SameSite: http.SameSiteLaxMode,
		Expires:  tokenPair.ExpiresAt,
	})

	return c.JSON(http.StatusCreated, map[string]any{
		"user": map[string]any{"id": user.ID, "username": user.Username, "email": user.Email, "isAdmin": user.IsAdmin},
	})
}

func (s *Server) handleLogin(c echo.Context) error {
	if s.dbStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "user store not available"})
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	body.Username = strings.TrimSpace(body.Username)

	user, err := s.dbStore.GetUserByUsername(c.Request().Context(), body.Username)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
	}
	if err := auth.CheckPassword(user.PasswordHash, body.Password); err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
	}

	tokenPair, _ := auth.CreateSessionToken()
	expiresAt := tokenPair.ExpiresAt.UTC().Format(time.RFC3339Nano)
	_ = s.dbStore.CreateSession(c.Request().Context(), store.SessionRecord{
		Token:     tokenPair.Token,
		UserID:    user.ID,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})

	c.SetCookie(&http.Cookie{
		Name:     "mw_lab_session",
		Value:    tokenPair.Token,
		Path:     "/",
		HttpOnly: true,
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
		Expires:  tokenPair.ExpiresAt,
	})

	return c.JSON(http.StatusOK, map[string]any{
		"user": map[string]any{"id": user.ID, "username": user.Username, "email": user.Email, "isAdmin": user.IsAdmin},
	})
}

func (s *Server) handleLogout(c echo.Context) error {
	if cookie, err := c.Cookie("mw_lab_session"); err == nil {
		if s.dbStore != nil {
			_ = s.dbStore.DeleteSession(c.Request().Context(), cookie.Value)
		}
		c.SetCookie(&http.Cookie{
			Name:     "mw_lab_session",
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			MaxAge:   -1,
		})
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "logged out"})
}

func (s *Server) handleMe(c echo.Context) error {
	userID := c.Get("auth_user_id")
	if userID == nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
	}
	user, err := s.dbStore.GetUserByID(c.Request().Context(), userID.(int64))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "user not found"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"user": map[string]any{"id": user.ID, "username": user.Username, "email": user.Email, "isAdmin": user.IsAdmin},
	})
}

func (s *Server) handleCreateToken(c echo.Context) error {
	userID := c.Get("auth_user_id")
	if userID == nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
	}
	var body struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if body.Name == "" {
		body.Name = "PAT"
	}
	if body.Scope == "" {
		body.Scope = "full"
	}

	tokenPair, err := auth.CreateAccessToken()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "token generation failed"})
	}

	expiresAt := tokenPair.ExpiresAt.UTC().Format(time.RFC3339Nano)
	_, err = s.dbStore.CreateAccessToken(c.Request().Context(), store.AccessTokenRecord{
		TokenHash:   tokenPair.Hash,
		TokenPrefix: tokenPair.Prefix,
		Name:        body.Name,
		UserID:      userID.(int64),
		Scope:       body.Scope,
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "token save failed"})
	}

	return c.JSON(http.StatusCreated, map[string]any{
		"token":     tokenPair.Token, // only shown once
		"prefix":    tokenPair.Prefix,
		"name":      body.Name,
		"scope":     body.Scope,
		"expiresAt": expiresAt,
	})
}

func (s *Server) handleListTokens(c echo.Context) error {
	userID := c.Get("auth_user_id")
	if userID == nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
	}
	tokens, err := s.dbStore.ListAccessTokens(c.Request().Context(), userID.(int64))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	// Don't return hashes
	var out []map[string]any
	for _, t := range tokens {
		out = append(out, map[string]any{
			"id":         t.ID,
			"prefix":     t.TokenPrefix,
			"name":       t.Name,
			"scope":      t.Scope,
			"lastUsedAt": t.LastUsedAt,
			"expiresAt":  t.ExpiresAt,
			"createdAt":  t.CreatedAt,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"tokens": out})
}

func (s *Server) handleDeleteToken(c echo.Context) error {
	userID := c.Get("auth_user_id")
	if userID == nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
	}
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := s.dbStore.DeleteAccessToken(c.Request().Context(), id, userID.(int64)); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "deleted"})
}

func (s *Server) getCurrentUserID(c echo.Context) (int64, bool) {
	if v := c.Get("auth_user_id"); v != nil {
		if id, ok := v.(int64); ok {
			return id, true
		}
	}
	return 0, false
}

func (s *Server) isAdmin(c echo.Context) bool {
	if v := c.Get("auth_user_id"); v != nil {
		if user, err := s.dbStore.GetUserByID(c.Request().Context(), v.(int64)); err == nil {
			return user.IsAdmin
		}
	}
	return false
}

func (s *Server) checkRepoAccess(c echo.Context, owner, name, requiredRole string) (bool, error) {
	userID, ok := s.getCurrentUserID(c)
	if !ok {
		return false, nil
	}
	if s.isAdmin(c) {
		return true, nil
	}
	role, err := s.dbStore.GetRepoMemberRole(c.Request().Context(), owner, name, userID)
	if err != nil {
		return false, nil
	}
	return roleHasAccess(role, requiredRole), nil
}

func roleHasAccess(userRole, required string) bool {
	rank := map[string]int{"read": 1, "write": 2, "admin": 3}
	return rank[userRole] >= rank[required]
}

func (s *Server) handleListRepos(c echo.Context) error {
	repos, err := s.dbStore.ListRepos(c.Request().Context(), c.QueryParam("owner"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"repos": repos,
		"count": len(repos),
	})
}

func (s *Server) handleCreateRepo(c echo.Context) error {
	var body struct {
		Owner         string `json:"owner"`
		Name          string `json:"name"`
		Description   string `json:"description"`
		DefaultBranch string `json:"defaultBranch"`
		Private       bool   `json:"private"`
		MirrorURL     string `json:"mirrorUrl"`
		SyncNow       bool   `json:"syncNow"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid repository json"})
	}
	owner := strings.TrimSpace(body.Owner)
	if err := githttp.ValidateSlug(owner); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	repoName, err := githttp.NormalizeRepoName(strings.TrimSpace(body.Name))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	defaultBranch := strings.TrimSpace(body.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	if err := githttp.ValidateRefName(defaultBranch); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	mirrorURL := strings.TrimSpace(body.MirrorURL)
	if mirrorURL != "" {
		if err := s.gitManager.ValidateUpstreamURL(mirrorURL); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}

	ctx := c.Request().Context()
	savedRepo, err := s.dbStore.CreateRepo(ctx, store.RepoRecord{
		Owner:         owner,
		Name:          repoName,
		Description:   strings.TrimSpace(body.Description),
		DefaultBranch: defaultBranch,
		Private:       body.Private,
		MirrorURL:     mirrorURL,
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		return c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	if _, initErr := s.gitManager.InitBareRepo(ctx, owner, repoName, defaultBranch); initErr != nil {
		_ = s.dbStore.DeleteRepo(ctx, owner, repoName)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": initErr.Error()})
	}

	var syncOutcome *githttp.MirrorSyncResult
	if mirrorURL != "" {
		_, _ = s.dbStore.UpsertMirror(ctx, store.MirrorRecord{
			RepoOwner:   owner,
			RepoName:    repoName,
			UpstreamURL: mirrorURL,
			Direction:   "pull",
			LastStatus:  "configured",
		})
		if body.SyncNow {
			outcome, syncErr := s.gitManager.SyncMirror(ctx, owner, repoName, mirrorURL)
			if syncErr != nil {
				return c.JSON(http.StatusBadGateway, map[string]any{
					"repo":  savedRepo,
					"error": syncErr.Error(),
				})
			}
			syncOutcome = outcome
			if refreshed, getErr := s.dbStore.GetRepo(ctx, owner, repoName); getErr == nil {
				savedRepo = refreshed
			}
		}
	}

	reply := map[string]any{"repo": savedRepo}
	if syncOutcome != nil {
		reply["mirrorSync"] = syncOutcome
	}
	return c.JSON(http.StatusCreated, reply)
}

func (s *Server) handleGetRepo(c echo.Context) error {
	owner, repoName, err := parseRepoParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	ctx := c.Request().Context()
	repoRecord, err := s.dbStore.GetRepo(ctx, owner, repoName)
	if errors.Is(err, store.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "repository not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	branches, _ := s.gitManager.ListBranches(ctx, owner, repoName)
	mirrorRecord, mirrorErr := s.dbStore.GetMirror(ctx, owner, repoName)
	reply := map[string]any{
		"repo":     repoRecord,
		"branches": branches,
	}
	if mirrorErr == nil {
		reply["mirror"] = mirrorRecord
	}
	return c.JSON(http.StatusOK, reply)
}

func (s *Server) handleUpdateRepo(c echo.Context) error {
	owner, repoName, err := parseRepoParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	var body struct {
		Description   string `json:"description"`
		DefaultBranch string `json:"defaultBranch"`
		Private       bool   `json:"private"`
		MirrorURL     string `json:"mirrorUrl"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid json"})
	}
	if strings.TrimSpace(body.DefaultBranch) != "" {
		if err := githttp.ValidateRefName(body.DefaultBranch); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}
	mirrorURL := strings.TrimSpace(body.MirrorURL)
	if mirrorURL != "" {
		if err := s.gitManager.ValidateUpstreamURL(mirrorURL); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}

	ctx := c.Request().Context()
	updatedRepo, err := s.dbStore.UpdateRepo(ctx, owner, repoName, store.RepoRecord{
		Description:   strings.TrimSpace(body.Description),
		DefaultBranch: strings.TrimSpace(body.DefaultBranch),
		Private:       body.Private,
		MirrorURL:     mirrorURL,
	})
	if errors.Is(err, store.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "repository not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if mirrorURL != "" {
		_, _ = s.dbStore.UpsertMirror(ctx, store.MirrorRecord{
			RepoOwner:   owner,
			RepoName:    repoName,
			UpstreamURL: mirrorURL,
			Direction:   "pull",
			LastStatus:  "configured",
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"repo": updatedRepo})
}

func (s *Server) handleDeleteRepo(c echo.Context) error {
	owner, repoName, err := parseRepoParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	ctx := c.Request().Context()
	if err := s.dbStore.DeleteRepo(ctx, owner, repoName); errors.Is(err, store.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "repository not found"})
	} else if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if err := s.gitManager.DeleteBareRepo(owner, repoName); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleMirrorRepo(c echo.Context) error {
	owner, repoName, err := parseRepoParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	ctx := c.Request().Context()
	repoRecord, err := s.dbStore.GetRepo(ctx, owner, repoName)
	if errors.Is(err, store.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "repository not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	var body struct {
		UpstreamURL string `json:"upstreamUrl"`
	}
	_ = c.Bind(&body)

	upstreamURL := strings.TrimSpace(body.UpstreamURL)
	if upstreamURL == "" {
		upstreamURL = strings.TrimSpace(repoRecord.MirrorURL)
	}
	if upstreamURL == "" {
		if mirrorRecord, mirrorErr := s.dbStore.GetMirror(ctx, owner, repoName); mirrorErr == nil {
			upstreamURL = strings.TrimSpace(mirrorRecord.UpstreamURL)
		}
	}
	if upstreamURL == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "upstreamUrl is required for mirror sync"})
	}

	outcome, err := s.gitManager.SyncMirror(ctx, owner, repoName, upstreamURL)
	if errors.Is(err, githttp.ErrInvalidUpstream) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, outcome)
}

func (s *Server) handleTriggerCI(c echo.Context) error {
	owner, repoName, err := parseRepoParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	ctx := c.Request().Context()
	repoRecord, err := s.dbStore.GetRepo(ctx, owner, repoName)
	if errors.Is(err, store.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "repository not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	var body struct {
		Ref        string `json:"ref"`
		SHA        string `json:"sha"`
		Pusher     string `json:"pusher"`
		MissionURL string `json:"missionUrl"`
	}
	_ = c.Bind(&body)

	targetDispatcher := s.ciDispatcher
	if overrideURL := strings.TrimSpace(body.MissionURL); overrideURL != "" {
		if err := ci.ValidateMissionURL(overrideURL); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		targetDispatcher = ci.NewDispatcher(overrideURL, s.cfg.CIHTTPClient)
	} else if targetDispatcher == nil || !targetDispatcher.Enabled() {
		if envMissionURL := strings.TrimSpace(os.Getenv("MW_MISSION_URL")); envMissionURL != "" {
			if err := ci.ValidateMissionURL(envMissionURL); err != nil {
				return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
			targetDispatcher = ci.NewDispatcher(envMissionURL, s.cfg.CIHTTPClient)
		}
	}
	if targetDispatcher == nil || !targetDispatcher.Enabled() {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "missionUrl or MW_MISSION_URL is not configured"})
	}

	refName := strings.TrimSpace(body.Ref)
	if refName == "" {
		defaultBranch := strings.TrimSpace(repoRecord.DefaultBranch)
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		refName = "refs/heads/" + defaultBranch
	}

	commitSHA := strings.TrimSpace(body.SHA)
	if commitSHA == "" {
		commits, commitErr := s.gitManager.ListCommits(ctx, owner, repoName, refName, 1)
		if commitErr == nil && len(commits) > 0 {
			commitSHA = commits[0].SHA
		}
	}
	if commitSHA == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sha is required when repository has no commits"})
	}

	pusher := strings.TrimSpace(body.Pusher)
	if pusher == "" {
		pusher = "manual"
	}

	dispatchOutcome, err := targetDispatcher.Dispatch(ctx, ci.DispatchInput{
		Owner:  owner,
		Repo:   repoName,
		Ref:    refName,
		SHA:    commitSHA,
		Pusher: pusher,
	})
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusAccepted, dispatchOutcome)
}

func (s *Server) handleListBranches(c echo.Context) error {
	owner, repoName, err := parseRepoParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	branches, err := s.gitManager.ListBranches(c.Request().Context(), owner, repoName)
	if err != nil {
		return mapGitInspectError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"branches": branches,
		"count":    len(branches),
	})
}

func (s *Server) handleListCommits(c echo.Context) error {
	owner, repoName, err := parseRepoParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	commits, err := s.gitManager.ListCommits(c.Request().Context(), owner, repoName, c.QueryParam("ref"), limit)
	if err != nil {
		return mapGitInspectError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"commits": commits,
		"count":   len(commits),
	})
}

func (s *Server) handleListTree(c echo.Context) error {
	owner, repoName, err := parseRepoParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	subPath := c.QueryParam("path")
	entries, err := s.gitManager.ListTree(c.Request().Context(), owner, repoName, c.QueryParam("ref"), subPath)
	if err != nil {
		return mapGitInspectError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"path":    subPath,
		"entries": entries,
		"count":   len(entries),
	})
}

func (s *Server) handleReadBlob(c echo.Context) error {
	owner, repoName, err := parseRepoParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	filePath := c.QueryParam("path")
	blobDoc, err := s.gitManager.ReadBlob(c.Request().Context(), owner, repoName, c.QueryParam("ref"), filePath, 0)
	if err != nil {
		return mapGitInspectError(c, err)
	}
	return c.JSON(http.StatusOK, blobDoc)
}

func (s *Server) handleListRepoEvents(c echo.Context) error {
	owner, repoName, err := parseRepoParams(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	events, err := s.dbStore.ListPushEvents(c.Request().Context(), owner, repoName, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"events": events,
		"count":  len(events),
	})
}

func (s *Server) handleListAllEvents(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	events, err := s.dbStore.ListPushEvents(c.Request().Context(), "", "", limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"events": events,
		"count":  len(events),
	})
}

func (s *Server) handleListWebhooks(c echo.Context) error {
	records, err := s.dbStore.ListWebhooks(c.Request().Context(), c.QueryParam("owner"), c.QueryParam("repo"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	views := make([]webhookView, 0, len(records))
	for _, record := range records {
		views = append(views, toWebhookView(record))
	}
	return c.JSON(http.StatusOK, map[string]any{
		"webhooks": views,
		"count":    len(views),
	})
}

func (s *Server) handleCreateWebhook(c echo.Context) error {
	var body struct {
		RepoOwner string `json:"repoOwner"`
		RepoName  string `json:"repoName"`
		TargetURL string `json:"targetUrl"`
		Secret    string `json:"secret"`
		Events    string `json:"events"`
		Active    *bool  `json:"active"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid webhook json"})
	}
	if err := validateWebhookTargetURL(body.TargetURL); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	repoOwner := strings.TrimSpace(body.RepoOwner)
	if repoOwner == "" {
		repoOwner = "*"
	} else if repoOwner != "*" {
		if err := githttp.ValidateSlug(repoOwner); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}
	repoName := strings.TrimSpace(body.RepoName)
	if repoName == "" {
		repoName = "*"
	} else if repoName != "*" {
		cleanName, err := githttp.NormalizeRepoName(repoName)
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		repoName = cleanName
	}

	isActive := true
	if body.Active != nil {
		isActive = *body.Active
	}
	created, err := s.dbStore.CreateWebhook(c.Request().Context(), store.WebhookRecord{
		RepoOwner: repoOwner,
		RepoName:  repoName,
		TargetURL: strings.TrimSpace(body.TargetURL),
		Secret:    body.Secret,
		Events:    strings.TrimSpace(body.Events),
		Active:    isActive,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, map[string]any{"webhook": toWebhookView(created)})
}

func (s *Server) handleUpdateWebhook(c echo.Context) error {
	hookID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || hookID <= 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid webhook id"})
	}
	var body struct {
		RepoOwner string `json:"repoOwner"`
		RepoName  string `json:"repoName"`
		TargetURL string `json:"targetUrl"`
		Secret    string `json:"secret"`
		Events    string `json:"events"`
		Active    *bool  `json:"active"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid webhook json"})
	}
	if strings.TrimSpace(body.TargetURL) != "" {
		if err := validateWebhookTargetURL(body.TargetURL); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}

	ctx := c.Request().Context()
	existing, err := s.dbStore.GetWebhook(ctx, hookID)
	if errors.Is(err, store.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "webhook not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	nextActive := existing.Active
	if body.Active != nil {
		nextActive = *body.Active
	}
	updated, err := s.dbStore.UpdateWebhook(ctx, hookID, store.WebhookRecord{
		RepoOwner: body.RepoOwner,
		RepoName:  body.RepoName,
		TargetURL: body.TargetURL,
		Secret:    body.Secret,
		Events:    body.Events,
		Active:    nextActive,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{"webhook": toWebhookView(updated)})
}

func (s *Server) handleDeleteWebhook(c echo.Context) error {
	hookID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || hookID <= 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid webhook id"})
	}
	if err := s.dbStore.DeleteWebhook(c.Request().Context(), hookID); errors.Is(err, store.ErrNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "webhook not found"})
	} else if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleListLFSObjects(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	objects, err := s.dbStore.ListLFSObjects(c.Request().Context(), c.QueryParam("owner"), c.QueryParam("repo"), limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"backend": s.lfsService.BackendName(),
		"objects": objects,
		"count":   len(objects),
	})
}

func parseRepoParams(c echo.Context) (string, string, error) {
	owner := strings.TrimSpace(c.Param("owner"))
	if err := githttp.ValidateSlug(owner); err != nil {
		return "", "", err
	}
	repoName, err := githttp.NormalizeRepoName(strings.TrimSpace(c.Param("name")))
	if err != nil {
		return "", "", err
	}
	return owner, repoName, nil
}

func mapGitInspectError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, githttp.ErrRepoNotFound):
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, githttp.ErrInvalidSlug),
		errors.Is(err, githttp.ErrInvalidRef),
		errors.Is(err, githttp.ErrInvalidPath),
		errors.Is(err, githttp.ErrPathEscape):
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, githttp.ErrBlobTooLarge):
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
	default:
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func validateWebhookTargetURL(rawURL string) error {
	trimmed := strings.TrimSpace(rawURL)
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("targetUrl must be a valid http or https URL")
	}
	return nil
}

func toWebhookView(record store.WebhookRecord) webhookView {
	return webhookView{
		ID:               record.ID,
		RepoOwner:        record.RepoOwner,
		RepoName:         record.RepoName,
		TargetURL:        record.TargetURL,
		SecretConfigured: strings.TrimSpace(record.Secret) != "",
		Events:           record.Events,
		Active:           record.Active,
		CreatedAt:        record.CreatedAt,
		UpdatedAt:        record.UpdatedAt,
	}
}
