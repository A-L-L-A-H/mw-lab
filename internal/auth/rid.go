package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultJWKSCacheTTL = 5 * time.Minute
	maxJWKSBodyBytes    = 1 << 20
)

var (
	ErrInvalidToken      = errors.New("invalid jwt token")
	ErrUnsupportedAlg    = errors.New("unsupported jwt algorithm: only RS256 is permitted")
	ErrIDTokenRejected   = errors.New("oidc id tokens are rejected; access token required")
	ErrInvalidIssuer     = errors.New("jwt issuer mismatch")
	ErrInvalidAudience   = errors.New("jwt audience mismatch")
	ErrTokenExpired      = errors.New("jwt token expired")
	ErrTokenNotYetValid  = errors.New("jwt token not yet valid")
	ErrUnknownKeyID      = errors.New("jwt signing key not found in jwks")
	ErrSignatureMismatch = errors.New("jwt signature verification failed")
)

type RIDConfig struct {
	JWKSURL    string
	Issuer     string
	Audience   string
	CacheTTL   time.Duration
	HTTPClient *http.Client
	Now        func() time.Time
}

type RIDClaims struct {
	Subject   string
	Issuer    string
	Audience  []string
	Scope     string
	ClientID  string
	ExpiresAt int64
	NotBefore int64
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

type rawJWTClaims struct {
	Sub      string          `json:"sub"`
	Iss      string          `json:"iss"`
	Aud      json.RawMessage `json:"aud"`
	TokenUse string          `json:"token_use"`
	Scope    string          `json:"scope"`
	ClientID string          `json:"client_id"`
	Exp      *float64        `json:"exp"`
	Nbf      *float64        `json:"nbf"`
	AtHash   string          `json:"at_hash"`
	CHash    string          `json:"c_hash"`
}

type jwksDocument struct {
	Keys []jwkEntry `json:"keys"`
}

type jwkEntry struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type RIDVerifier struct {
	jwksURL    string
	issuer     string
	audience   string
	cacheTTL   time.Duration
	httpClient *http.Client
	nowFn      func() time.Time

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func NewRIDVerifier(cfg RIDConfig) (*RIDVerifier, error) {
	jwksURL := strings.TrimSpace(cfg.JWKSURL)
	if jwksURL == "" {
		return nil, errors.New("mw-rid jwks url is required")
	}
	cacheTTL := cfg.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = defaultJWKSCacheTTL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &RIDVerifier{
		jwksURL:    jwksURL,
		issuer:     strings.TrimSpace(cfg.Issuer),
		audience:   strings.TrimSpace(cfg.Audience),
		cacheTTL:   cacheTTL,
		httpClient: client,
		nowFn:      nowFn,
		keys:       make(map[string]*rsa.PublicKey),
	}, nil
}

func (v *RIDVerifier) Verify(ctx context.Context, rawToken string) (*RIDClaims, error) {
	trimmed := strings.TrimSpace(rawToken)
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, ErrInvalidToken
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header decode", ErrInvalidToken)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("%w: header json", ErrInvalidToken)
	}
	if header.Alg != "RS256" {
		return nil, ErrUnsupportedAlg
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload decode", ErrInvalidToken)
	}
	var rawClaims rawJWTClaims
	if err := json.Unmarshal(payloadBytes, &rawClaims); err != nil {
		return nil, fmt.Errorf("%w: payload json", ErrInvalidToken)
	}

	if strings.EqualFold(strings.TrimSpace(rawClaims.TokenUse), "id") ||
		strings.TrimSpace(rawClaims.AtHash) != "" ||
		strings.TrimSpace(rawClaims.CHash) != "" {
		return nil, ErrIDTokenRejected
	}

	if strings.TrimSpace(rawClaims.Sub) == "" {
		return nil, fmt.Errorf("%w: missing sub", ErrInvalidToken)
	}

	if v.issuer != "" && rawClaims.Iss != v.issuer {
		return nil, ErrInvalidIssuer
	}

	audiences, err := parseAudienceClaim(rawClaims.Aud)
	if err != nil {
		return nil, fmt.Errorf("%w: aud claim", ErrInvalidToken)
	}
	if v.audience != "" && !containsString(audiences, v.audience) {
		return nil, ErrInvalidAudience
	}

	if rawClaims.Exp == nil {
		return nil, fmt.Errorf("%w: missing exp", ErrInvalidToken)
	}
	nowUnix := v.nowFn().UTC().Unix()
	expUnix := int64(*rawClaims.Exp)
	if nowUnix >= expUnix {
		return nil, ErrTokenExpired
	}
	var nbfUnix int64
	if rawClaims.Nbf != nil {
		nbfUnix = int64(*rawClaims.Nbf)
		if nowUnix < nbfUnix {
			return nil, ErrTokenNotYetValid
		}
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature decode", ErrInvalidToken)
	}

	pubKey, err := v.resolveKey(ctx, header.Kid)
	if err != nil {
		return nil, err
	}

	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, digest[:], sigBytes); err != nil {
		return nil, ErrSignatureMismatch
	}

	return &RIDClaims{
		Subject:   rawClaims.Sub,
		Issuer:    rawClaims.Iss,
		Audience:  audiences,
		Scope:     rawClaims.Scope,
		ClientID:  rawClaims.ClientID,
		ExpiresAt: expUnix,
		NotBefore: nbfUnix,
	}, nil
}

func (v *RIDVerifier) resolveKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if key, ok := v.lookupCachedKey(kid); ok {
		return key, nil
	}
	if err := v.refreshJWKS(ctx); err != nil {
		return nil, err
	}
	if key, ok := v.lookupCachedKey(kid); ok {
		return key, nil
	}
	return nil, ErrUnknownKeyID
}

func (v *RIDVerifier) lookupCachedKey(kid string) (*rsa.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.fetchedAt.IsZero() || v.nowFn().Sub(v.fetchedAt) >= v.cacheTTL {
		return nil, false
	}
	if kid != "" {
		key, ok := v.keys[kid]
		return key, ok
	}
	if len(v.keys) == 1 {
		for _, singleKey := range v.keys {
			return singleKey, true
		}
	}
	return nil, false
}

func (v *RIDVerifier) refreshJWKS(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("build jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks endpoint returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBodyBytes))
	if err != nil {
		return fmt.Errorf("read jwks body: %w", err)
	}

	var doc jwksDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("decode jwks json: %w", err)
	}

	parsedKeys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for idx, entry := range doc.Keys {
		if entry.Kty != "RSA" || entry.N == "" || entry.E == "" {
			continue
		}
		if entry.Use != "" && entry.Use != "sig" {
			continue
		}
		pubKey, parseErr := parseRSAPublicKey(entry.N, entry.E)
		if parseErr != nil {
			continue
		}
		keyID := entry.Kid
		if keyID == "" {
			keyID = fmt.Sprintf("_idx_%d", idx)
		}
		parsedKeys[keyID] = pubKey
	}

	v.mu.Lock()
	v.keys = parsedKeys
	v.fetchedAt = v.nowFn()
	v.mu.Unlock()
	return nil
}

func parseRSAPublicKey(modulusB64, exponentB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(modulusB64)
	if err != nil || len(nBytes) == 0 {
		return nil, errors.New("invalid rsa modulus")
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(exponentB64)
	if err != nil || len(eBytes) == 0 {
		return nil, errors.New("invalid rsa exponent")
	}
	nVal := new(big.Int).SetBytes(nBytes)
	eVal := new(big.Int).SetBytes(eBytes)
	if !eVal.IsInt64() || eVal.Int64() <= 1 {
		return nil, errors.New("invalid rsa exponent value")
	}
	return &rsa.PublicKey{
		N: nVal,
		E: int(eVal.Int64()),
	}, nil
}

func parseAudienceClaim(raw json.RawMessage) ([]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err == nil {
		return multiple, nil
	}
	return nil, errors.New("unsupported aud format")
}

func containsString(values []string, target string) bool {
	for _, candidate := range values {
		if candidate == target {
			return true
		}
	}
	return false
}
