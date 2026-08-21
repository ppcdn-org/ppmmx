package admin

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
)

const adminSessionCookie = "mmx_admin_session"

type authManager struct {
	store    *Store
	username string
	mu       sync.RWMutex
	sessions map[string]struct{}
}

func newAuthManager(store *Store, log logger.Writer) (*authManager, error) {
	if store == nil {
		return nil, fmt.Errorf("admin authentication requires a persistent store")
	}
	username, passwordHash := store.AdminCredentials()
	if username == "" || passwordHash == "" {
		username = strings.TrimSpace(conf.DotenvValue("MMXADMIN_USERNAME"))
		password := conf.DotenvValue("MMXADMIN_PASSWORD")
		if username == "" {
			username = "admin"
		}
		if password == "" {
			generated, err := generateRandomPassword()
			if err != nil {
				return nil, err
			}
			password = generated
			if log != nil {
				log.Log(logger.Info, "[admin] MMXADMIN_PASSWORD not set, generated random admin password: %s", password)
			}
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		if err := store.SetAdminCredentials(username, string(hash)); err != nil {
			return nil, err
		}
	}
	return &authManager{store: store, username: username, sessions: make(map[string]struct{})}, nil
}

func generateRandomPassword() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (a *authManager) authenticated(c *gin.Context) bool {
	token, err := c.Cookie(adminSessionCookie)
	if err != nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.sessions[token]
	return ok
}

func (a *authManager) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !a.authenticated(c) {
			respErr(c, http.StatusUnauthorized, "authentication required", "")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (a *authManager) login(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respErr(c, http.StatusBadRequest, "invalid request", "")
		return
	}
	username, hash := a.store.AdminCredentials()
	if req.Username != username || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		respErr(c, http.StatusUnauthorized, "invalid username or password", "")
		return
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		respErr(c, http.StatusInternalServerError, "unable to create session", "")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	a.mu.Lock()
	a.sessions[token] = struct{}{}
	a.mu.Unlock()
	http.SetCookie(c.Writer, &http.Cookie{Name: adminSessionCookie, Value: token, Path: "/admin", HttpOnly: true, Secure: c.Request.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 60 * 60})
	respOK(c, gin.H{"username": username})
}

func (a *authManager) me(c *gin.Context) {
	respOK(c, gin.H{"username": a.username})
}

func (a *authManager) logout(c *gin.Context) {
	if token, err := c.Cookie(adminSessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, token)
		a.mu.Unlock()
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: adminSessionCookie, Path: "/admin", HttpOnly: true, Secure: c.Request.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	respOK(c, gin.H{})
}

func (a *authManager) changePassword(c *gin.Context) {
	var req struct {
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.NewPassword) < 8 {
		respErr(c, http.StatusBadRequest, "new password must contain at least 8 characters", "")
		return
	}
	username, _ := a.store.AdminCredentials()
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil || a.store.SetAdminCredentials(username, string(newHash)) != nil {
		respErr(c, http.StatusInternalServerError, "unable to update password", "")
		return
	}
	a.mu.Lock()
	a.sessions = make(map[string]struct{})
	a.mu.Unlock()
	http.SetCookie(c.Writer, &http.Cookie{Name: adminSessionCookie, Path: "/admin", HttpOnly: true, Secure: c.Request.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	respOK(c, gin.H{})
}
