package auth

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func mustGenerateRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return key
}

func rsaPublicToJWK(kid string, pub *rsa.PublicKey) jwkEntry {
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	return jwkEntry{
		Kty: "RSA",
		Kid: kid,
		Alg: "RS256",
		Use: "sig",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(eBytes),
	}
}

func signRS256Token(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	headerJSON, err := json.Marshal(map[string]any{
		"alg": "RS256",
		"typ": "at+jwt",
		"kid": kid,
	})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	headerPart := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadPart := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerPart + "." + payloadPart
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign rs256: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func signHS256Token(t *testing.T, secret string, claims map[string]any) string {
	t.Helper()
	headerJSON, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT"})
	payloadJSON, _ := json.Marshal(claims)
	headerPart := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadPart := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerPart + "." + payloadPart
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestRIDVerifier_VerifyAndADR001Enforcement(t *testing.T) {
	keyV1 := mustGenerateRSAKey(t)
	keyV2 := mustGenerateRSAKey(t)

	var mu sync.RWMutex
	activeKeys := []jwkEntry{rsaPublicToJWK("kid-1", &keyV1.PublicKey)}
	fetchCount := 0

	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetchCount++
		snapshot := append([]jwkEntry(nil), activeKeys...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwksDocument{Keys: snapshot})
	}))
	defer jwksServer.Close()

	fixedNow := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	verifier, err := NewRIDVerifier(RIDConfig{
		JWKSURL:    jwksServer.URL,
		Issuer:     "https://id.a-l-l-a-h.com",
		Audience:   "urn:moonwitness:api",
		CacheTTL:   5 * time.Minute,
		HTTPClient: jwksServer.Client(),
		Now:        func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}

	validClaims := map[string]any{
		"sub":       "usr-42",
		"iss":       "https://id.a-l-l-a-h.com",
		"aud":       "urn:moonwitness:api",
		"scope":     "openid api:read",
		"client_id": "mw-lab",
		"exp":       fixedNow.Add(15 * time.Minute).Unix(),
		"nbf":       fixedNow.Add(-1 * time.Minute).Unix(),
	}

	validToken := signRS256Token(t, keyV1, "kid-1", validClaims)
	verified, err := verifier.Verify(context.Background(), validToken)
	if err != nil {
		t.Fatalf("expected valid token to pass, got: %v", err)
	}
	if verified.Subject != "usr-42" || verified.Issuer != "https://id.a-l-l-a-h.com" || verified.Scope != "openid api:read" {
		t.Fatalf("unexpected verified claims: %+v", verified)
	}

	// Second verify should hit cache without refetching JWKS.
	if _, err := verifier.Verify(context.Background(), validToken); err != nil {
		t.Fatalf("expected cached verify to pass, got: %v", err)
	}
	mu.RLock()
	if fetchCount != 1 {
		t.Fatalf("expected 1 jwks fetch due to cache, got %d", fetchCount)
	}
	mu.RUnlock()

	// Reject HS256 forged token.
	hsToken := signHS256Token(t, "forged-secret", validClaims)
	if _, err := verifier.Verify(context.Background(), hsToken); !errors.Is(err, ErrUnsupportedAlg) {
		t.Fatalf("expected ErrUnsupportedAlg for HS256, got: %v", err)
	}

	// Reject alg=none token.
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	nonePayload, _ := json.Marshal(validClaims)
	noneToken := noneHeader + "." + base64.RawURLEncoding.EncodeToString(nonePayload) + ".sig"
	if _, err := verifier.Verify(context.Background(), noneToken); !errors.Is(err, ErrUnsupportedAlg) {
		t.Fatalf("expected ErrUnsupportedAlg for none, got: %v", err)
	}

	// Reject token_use == "id".
	idClaims := map[string]any{
		"sub":       "usr-42",
		"iss":       "https://id.a-l-l-a-h.com",
		"aud":       "urn:moonwitness:api",
		"token_use": "id",
		"exp":       fixedNow.Add(15 * time.Minute).Unix(),
	}
	idToken := signRS256Token(t, keyV1, "kid-1", idClaims)
	if _, err := verifier.Verify(context.Background(), idToken); !errors.Is(err, ErrIDTokenRejected) {
		t.Fatalf("expected ErrIDTokenRejected, got: %v", err)
	}

	// Reject wrong issuer.
	wrongIssClaims := map[string]any{
		"sub": "usr-42",
		"iss": "https://evil.example.com",
		"aud": "urn:moonwitness:api",
		"exp": fixedNow.Add(15 * time.Minute).Unix(),
	}
	if _, err := verifier.Verify(context.Background(), signRS256Token(t, keyV1, "kid-1", wrongIssClaims)); !errors.Is(err, ErrInvalidIssuer) {
		t.Fatalf("expected ErrInvalidIssuer, got: %v", err)
	}

	// Reject wrong audience.
	wrongAudClaims := map[string]any{
		"sub": "usr-42",
		"iss": "https://id.a-l-l-a-h.com",
		"aud": "urn:other:api",
		"exp": fixedNow.Add(15 * time.Minute).Unix(),
	}
	if _, err := verifier.Verify(context.Background(), signRS256Token(t, keyV1, "kid-1", wrongAudClaims)); !errors.Is(err, ErrInvalidAudience) {
		t.Fatalf("expected ErrInvalidAudience, got: %v", err)
	}

	// Reject expired token.
	expiredClaims := map[string]any{
		"sub": "usr-42",
		"iss": "https://id.a-l-l-a-h.com",
		"aud": "urn:moonwitness:api",
		"exp": fixedNow.Add(-1 * time.Second).Unix(),
	}
	if _, err := verifier.Verify(context.Background(), signRS256Token(t, keyV1, "kid-1", expiredClaims)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got: %v", err)
	}

	// Rotate JWKS key and verify refresh on unknown kid.
	mu.Lock()
	activeKeys = append(activeKeys, rsaPublicToJWK("kid-2", &keyV2.PublicKey))
	mu.Unlock()

	rotatedToken := signRS256Token(t, keyV2, "kid-2", validClaims)
	if _, err := verifier.Verify(context.Background(), rotatedToken); err != nil {
		t.Fatalf("expected rotated kid-2 token to succeed after jwks refresh, got: %v", err)
	}
	mu.RLock()
	if fetchCount != 2 {
		t.Fatalf("expected 2 jwks fetches after key rotation, got %d", fetchCount)
	}
	mu.RUnlock()
}
