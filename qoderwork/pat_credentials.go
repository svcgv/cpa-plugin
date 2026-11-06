package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"
)

// A separate lock per account prevents parallel requests from exchanging the
// same PAT repeatedly. The key is a digest, never the credential itself.
var patCredentialLocks sync.Map
var patCredentialCache sync.Map

func forcePATCredential(sa *storedAuth) error {
	if sa == nil || !strings.HasPrefix(sa.Auth.PersonalToken, "pt-") {
		return fmt.Errorf("账号需要 PAT")
	}
	key := fmt.Sprintf("%s:%x", sa.region(), sha256.Sum256([]byte(sa.Auth.PersonalToken)))
	mu, _ := patCredentialLocks.LoadOrStore(key, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if cached, ok := patCredentialCache.Load(key); ok {
		entry := cached.(cachedPATCredential)
		if entry.token.Token != sa.Auth.AccessToken && time.Until(entry.expiresAt) > 5*time.Minute {
			sa.Auth.AccessToken, sa.Auth.RefreshToken, sa.Auth.ExpiresAt = entry.token.Token, entry.token.RefreshToken, entry.expiresAt.Unix()
			return nil
		}
	}
	return exchangePATCredentialLocked(sa, key)
}

type cachedPATCredential struct {
	token     *jobTokenResponse
	expiresAt time.Time
}

func preparePATCredential(sa *storedAuth) error {
	if sa == nil || !strings.HasPrefix(sa.Auth.PersonalToken, "pt-") {
		return fmt.Errorf("账号需要补充对应区域的 PAT")
	}
	if strings.HasPrefix(sa.Auth.AccessToken, "jt-") && (sa.Auth.ExpiresAt == 0 || sa.Auth.ExpiresAt > time.Now().Add(5*time.Minute).Unix()) {
		return nil
	}
	key := fmt.Sprintf("%s:%x", sa.region(), sha256.Sum256([]byte(sa.Auth.PersonalToken)))
	mu, _ := patCredentialLocks.LoadOrStore(key, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if cached, ok := patCredentialCache.Load(key); ok {
		entry := cached.(cachedPATCredential)
		if time.Until(entry.expiresAt) > 5*time.Minute {
			sa.Auth.AccessToken, sa.Auth.RefreshToken = entry.token.Token, entry.token.RefreshToken
			sa.Auth.ExpiresAt = entry.expiresAt.Unix()
			return nil
		}
	}
	return exchangePATCredentialLocked(sa, key)
}

func exchangePATCredentialLocked(sa *storedAuth, key string) error {
	tok, err := exchangePATForJobTokenAt(sa.endpoints().OpenAPI, sa.Auth.PersonalToken)
	if err != nil {
		return fmt.Errorf("PAT 换取凭证失败: %w", err)
	}
	expires := time.Now().Add(24 * time.Hour)
	if tok.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Millisecond)
	}
	if parsed, err := time.Parse(time.RFC3339, tok.ExpiresAt); err == nil {
		expires = parsed
	}
	patCredentialCache.Store(key, cachedPATCredential{tok, expires})
	sa.Auth.AccessToken, sa.Auth.RefreshToken, sa.Auth.ExpiresAt = tok.Token, tok.RefreshToken, expires.Unix()
	return nil
}
