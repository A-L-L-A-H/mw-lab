package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	SessionTokenBytes = 32
	AccessTokenBytes  = 32
	TokenPrefixLen    = 8
	SessionTTL        = 30 * 24 * time.Hour // 30 days
)

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func CheckPassword(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

func GenerateToken() (string, error) {
	b := make([]byte, AccessTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func GenerateSessionToken() (string, error) {
	b := make([]byte, SessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func TokenPrefix(token string) string {
	if len(token) <= TokenPrefixLen {
		return token
	}
	return token[:TokenPrefixLen]
}

func TokenHash(token string) string {
	hash := hex.EncodeToString([]byte(token))
	return hash
}

func ConstantTimeCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

type TokenPair struct {
	Token     string
	Prefix    string
	Hash      string
	ExpiresAt time.Time
}

func CreateAccessToken() (TokenPair, error) {
	tok, err := GenerateToken()
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		Token:     tok,
		Prefix:    TokenPrefix(tok),
		Hash:      TokenHash(tok),
		ExpiresAt: time.Now().Add(SessionTTL),
	}, nil
}

func CreateSessionToken() (TokenPair, error) {
	tok, err := GenerateSessionToken()
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		Token:     tok,
		Prefix:    TokenPrefix(tok),
		Hash:      TokenHash(tok),
		ExpiresAt: time.Now().Add(SessionTTL),
	}, nil
}

func VerifyTokenHash(plain, hash string) bool {
	return ConstantTimeCompare(plain, hash)
}