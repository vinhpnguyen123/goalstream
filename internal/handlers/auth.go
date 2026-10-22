package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
	"github.com/vinhpnguyen123/goalstream/internal/middleware"
)

type registerRequest struct {
	Email       string  `json:"email"`
	Password    string  `json:"password"`
	DisplayName *string `json:"display_name"`
}

// register is POST /auth/register. It creates the account and does not log in.
// Registration sits behind the invite gate, so a 409 revealing a taken email is acceptable.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decode(w, r, &req) {
		return
	}
	email, ok := auth.NormalizeEmail(req.Email)
	if !ok {
		httpx.Error(w, r, http.StatusBadRequest, "validation_error", "email must contain @ and be at most 254 characters")
		return
	}
	if !auth.ValidPassword(req.Password) {
		httpx.Error(w, r, http.StatusBadRequest, "validation_error", "password must be 10 to 128 characters")
		return
	}
	name, ok := auth.NormalizeDisplayName(req.DisplayName)
	if !ok {
		httpx.Error(w, r, http.StatusBadRequest, "validation_error", "display_name must be at most 64 characters")
		return
	}
	u, err := auth.Register(r.Context(), s.Pool, email, req.Password, name)
	if errors.Is(err, auth.ErrEmailTaken) {
		httpx.Error(w, r, http.StatusConflict, "email_taken", "that email is already registered")
		return
	} else if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"user": u})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// login is POST /auth/login: an access token in the body, a refresh token (new family) in the
// gs_refresh cookie. A wrong password and an unknown email get the same response.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decode(w, r, &req) {
		return
	}
	email, _ := auth.NormalizeEmail(req.Email) // a malformed email simply matches no user
	// Per email, not per IP: a spoofed or shared IP (everyone behind the Vercel proxy) can
	// neither bypass nor exhaust it for everyone.
	if !middleware.Allow(w, r, s.Redis, middleware.RuleLoginEmail, email) {
		return
	}
	user, err := auth.Login(r.Context(), s.Pool, email, req.Password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		httpx.Error(w, r, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
		return
	} else if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	access, expiresIn, err := s.Signer.Issue(user, time.Now())
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	refresh, err := auth.IssueRefresh(r.Context(), s.Pool, user, uuid.NewString(), s.Cfg.API.RefreshTokenTTL)
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	s.setRefreshCookie(w, refresh, s.Cfg.API.RefreshTokenTTL)
	httpx.JSON(w, r, http.StatusOK, map[string]any{"access_token": access, "expires_in": expiresIn})
}

// setRefreshCookie writes gs_refresh (docs/API.md § Cookies); a negative ttl clears it. SameSite=Strict works because
// the browser only ever talks to one site: the frontend proxies /api/*. Path scopes it to the
// auth endpoints, so it never rides along on ordinary API calls. Never a Domain: host-only.
func (s *Server) setRefreshCookie(w http.ResponseWriter, value string, ttl time.Duration) {
	maxAge := int(ttl / time.Second)
	if ttl < 0 {
		maxAge = -1 // net/http: negative → "Max-Age=0", delete now (0 would mean a session cookie)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.RefreshCookie,
		Value:    value,
		Path:     "/api/v1/auth",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.Cfg.API.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

// refresh is POST /auth/refresh: cookie only, no body. Success rotates the cookie and returns a
// new access token; every refusal is 401 and clears the cookie (auth.Rotate decides).
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	ck, err := r.Cookie(auth.RefreshCookie)
	if err != nil {
		s.refuseRefresh(w, r)
		return
	}
	user, next, err := auth.Rotate(r.Context(), s.Pool, ck.Value, s.Cfg.API.RefreshTokenTTL, s.Cfg.API.RefreshReuseGrace)
	if errors.Is(err, auth.ErrRefreshInvalid) {
		s.refuseRefresh(w, r)
		return
	} else if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	access, expiresIn, err := s.Signer.Issue(user, time.Now())
	if err != nil {
		httpx.Internal(w, r, err)
		return
	}
	s.setRefreshCookie(w, next, s.Cfg.API.RefreshTokenTTL)
	httpx.JSON(w, r, http.StatusOK, map[string]any{"access_token": access, "expires_in": expiresIn})
}

func (s *Server) refuseRefresh(w http.ResponseWriter, r *http.Request) {
	s.setRefreshCookie(w, "", -1)
	httpx.Error(w, r, http.StatusUnauthorized, "unauthorized", "please sign in again")
}

// logout is POST /auth/logout: revoke the cookie's whole family and clear it. Always 204 —
// no cookie, or an unknown one, is already logged out.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(auth.RefreshCookie); err == nil {
		if err := auth.Logout(r.Context(), s.Pool, ck.Value); err != nil {
			httpx.Internal(w, r, err)
			return
		}
	}
	s.setRefreshCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
