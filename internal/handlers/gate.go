package handlers

import (
	"net/http"
	"time"

	"github.com/vinhpnguyen123/goalstream/internal/auth"
	"github.com/vinhpnguyen123/goalstream/internal/httpx"
)

type gateRequest struct {
	Code string `json:"code"`
}

// gate exchanges an invite code for the gs_invite cookie. The code itself is never logged.
func (s *Server) gate(w http.ResponseWriter, r *http.Request) {
	var req gateRequest
	if !decode(w, r, &req) {
		return
	}
	id, ok := auth.MatchInviteCode(s.Cfg.API.InviteCodes, req.Code)
	if !ok {
		httpx.Error(w, r, http.StatusUnauthorized, "gate_required", "that invite code is not valid")
		return
	}
	ttl := s.Cfg.API.InviteTTL
	http.SetCookie(w, &http.Cookie{
		Name:     auth.InviteCookie,
		Value:    auth.SignInvite(s.Cfg.API.InviteSecret, id, time.Now().Add(ttl)),
		Path:     "/",
		MaxAge:   int(ttl / time.Second),
		HttpOnly: true,
		Secure:   s.Cfg.API.CookieSecure,
		SameSite: http.SameSiteLaxMode, // never a Domain: host-only
	})
	w.WriteHeader(http.StatusNoContent)
}
