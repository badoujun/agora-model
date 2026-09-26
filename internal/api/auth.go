package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SessionCookieName 是 Web UI 会话 Cookie 名。
const SessionCookieName = "agoramodel_session"

// SessionTTL 是会话有效期。
const SessionTTL = 12 * time.Hour

// Authenticator 管理 Web UI 的登录会话。
//
// 访问控制策略（DESIGN FR-3.5）：
//   - 本地回环访问且未设置管理密码时，视为已授权（便于本机使用）；
//   - 设置了管理密码（或监听非回环）时，/api/* 必须携带有效会话。
type Authenticator struct {
	password string
	mu       sync.Mutex
	sessions map[string]time.Time // token -> 过期时间
	now      func() time.Time
}

// NewAuthenticator 创建会话管理器；password 为空表示未启用登录。
func NewAuthenticator(password string) *Authenticator {
	return &Authenticator{
		password: password,
		sessions: make(map[string]time.Time),
		now:      time.Now,
	}
}

// LoginEnabled 报告是否需要登录。
func (a *Authenticator) LoginEnabled() bool { return strings.TrimSpace(a.password) != "" }

// Login 校验密码并返回会话令牌与过期时间；密码错误返回 ok=false。
//
// 比对使用常量时间比较，避免计时侧信道。
func (a *Authenticator) Login(password string) (token string, expires time.Time, ok bool) {
	if !a.LoginEnabled() {
		return "", time.Time{}, false
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(password)), []byte(a.password)) != 1 {
		return "", time.Time{}, false
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, false
	}
	token = hex.EncodeToString(raw)
	expires = a.now().Add(SessionTTL)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[token] = expires
	a.pruneLocked()
	return token, expires, true
}

// Logout 使指定会话失效。
func (a *Authenticator) Logout(token string) {
	if token == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

// Valid 报告令牌是否有效。
func (a *Authenticator) Valid(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	expires, ok := a.sessions[token]
	if !ok {
		return false
	}
	if a.now().After(expires) {
		delete(a.sessions, token)
		return false
	}
	return true
}

// pruneLocked 清理过期会话（调用方需持锁）。
func (a *Authenticator) pruneLocked() {
	now := a.now()
	for token, expires := range a.sessions {
		if now.After(expires) {
			delete(a.sessions, token)
		}
	}
}

// authorized 判断请求是否已获授权。
//
// loopback 为 true 且未启用登录时直接放行（本地使用场景）。
func (a *Authenticator) authorized(r *http.Request, loopback bool) bool {
	if !a.LoginEnabled() {
		return loopback
	}
	if cookie, err := r.Cookie(SessionCookieName); err == nil && a.Valid(cookie.Value) {
		return true
	}
	// 便于脚本与 CLI 使用：也接受 Authorization: Bearer <session-token>
	if token := bearerToken(r); token != "" && a.Valid(token) {
		return true
	}
	return false
}

// sessionToken 从请求中取出会话令牌（Cookie 优先）。
func sessionToken(r *http.Request) string {
	if cookie, err := r.Cookie(SessionCookieName); err == nil {
		return cookie.Value
	}
	return bearerToken(r)
}

func bearerToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "bearer "
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return strings.TrimSpace(auth[len(prefix):])
	}
	return ""
}

// isLoopbackRemote 判断请求是否来自本机回环地址。
func isLoopbackRemote(r *http.Request) bool {
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		host = host[:idx]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}
