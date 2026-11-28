package auth

import (
	"testing"
	"time"
)

func TestPasswordHashAndCheck(t *testing.T) {
	hash, err := HashPassword("moonwitness-secret-pass")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	if hash == "" || hash == "moonwitness-secret-pass" {
		t.Fatalf("expected non-empty bcrypt hash, got %q", hash)
	}
	if err := CheckPassword(hash, "moonwitness-secret-pass"); err != nil {
		t.Fatalf("CheckPassword rejected valid password: %v", err)
	}
	if err := CheckPassword(hash, "wrong-password"); err == nil {
		t.Fatal("expected CheckPassword to reject invalid password")
	}
}

func TestGenerateTokenLengths(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	if len(tok) != AccessTokenBytes*2 {
		t.Fatalf("expected %d hex chars, got %d", AccessTokenBytes*2, len(tok))
	}

	sessTok, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken failed: %v", err)
	}
	if len(sessTok) != SessionTokenBytes*2 {
		t.Fatalf("expected %d hex chars, got %d", SessionTokenBytes*2, len(sessTok))
	}
}

func TestCreateAccessTokenPair(t *testing.T) {
	pair, err := CreateAccessToken()
	if err != nil {
		t.Fatalf("CreateAccessToken failed: %v", err)
	}
	if pair.Prefix != TokenPrefix(pair.Token) || len(pair.Prefix) != TokenPrefixLen {
		t.Fatalf("unexpected access token prefix: %q", pair.Prefix)
	}
	if pair.Hash != TokenHash(pair.Token) {
		t.Fatal("access token hash mismatch")
	}
	if !pair.ExpiresAt.After(time.Now()) {
		t.Fatal("expected access token expiration in the future")
	}
}

func TestCreateSessionTokenPair(t *testing.T) {
	pair, err := CreateSessionToken()
	if err != nil {
		t.Fatalf("CreateSessionToken failed: %v", err)
	}
	if pair.Prefix != TokenPrefix(pair.Token) || pair.Hash != TokenHash(pair.Token) {
		t.Fatal("session token pair fields mismatch")
	}
}

func TestTokenPrefixAndComparison(t *testing.T) {
	if got := TokenPrefix("short"); got != "short" {
		t.Fatalf("expected short token unchanged, got %q", got)
	}
	if got := TokenPrefix("abcdefghijkl"); got != "abcdefgh" {
		t.Fatalf("expected 8-char prefix, got %q", got)
	}

	if !ConstantTimeCompare("abc123", "abc123") {
		t.Fatal("expected ConstantTimeCompare true for equal strings")
	}
	if ConstantTimeCompare("abc123", "abc124") {
		t.Fatal("expected ConstantTimeCompare false for different strings")
	}
	if !VerifyTokenHash("plain-token", "plain-token") {
		t.Fatal("expected VerifyTokenHash true for matching values")
	}
	if VerifyTokenHash("plain-token", "other-token") {
		t.Fatal("expected VerifyTokenHash false for mismatched values")
	}
}
