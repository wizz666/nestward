package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/wizz666/nestward/internal/auth"
	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/publish"
)

const sessionCookie = "nestward_session"

// public paths need no login.
func public(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case !strings.HasPrefix(p, "/api/"):
		return true // the page itself, its script and styles
	case p == "/api/v1/i18n", strings.HasPrefix(p, "/api/v1/auth/"):
		return true
	case p == "/api/v1/share/summary":
		return true // checks its own read-only key
	}
	return false
}

// guard enforces login and blocks cross-site requests. With no Auth
// manager (tests only) everything is open.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := r.Method != http.MethodGet && r.Method != http.MethodHead
		if write && !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		if s.Auth != nil && !public(r) {
			c, err := r.Cookie(sessionCookie)
			if err != nil || !s.Auth.Valid(r.Context(), c.Value) {
				need, _ := s.Auth.NeedsSetup(r.Context())
				key := "login_required"
				if need {
					key = "setup_required"
				}
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": key})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func setSession(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge: int(auth.SessionLife / time.Second),
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return false
	}
	return true
}

// authError answers with a translation key; unknown errors stay generic.
func (s *Server) authError(w http.ResponseWriter, err error) {
	known := []error{auth.ErrSetupDone, auth.ErrBadSetupCode, auth.ErrBadLogin, auth.ErrBadTOTP,
		auth.ErrTooMany, auth.ErrNoPending}
	for _, k := range known {
		if errors.Is(err, k) {
			code := http.StatusBadRequest
			if errors.Is(err, auth.ErrTooMany) {
				code = http.StatusTooManyRequests
			}
			writeJSON(w, code, map[string]string{"error": k.Error()})
			return
		}
	}
	switch err.Error() {
	case "password_too_short", "password_is_username", "bad_username":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.logger().Error("auth", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
}

func (s *Server) registerAuth(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/auth/state", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{"enabled": s.Auth != nil, "setup_required": false, "logged_in": s.Auth == nil}
		if s.Auth != nil {
			need, _ := s.Auth.NeedsSetup(r.Context())
			out["setup_required"] = need
			if c, err := r.Cookie(sessionCookie); err == nil && s.Auth.Valid(r.Context(), c.Value) {
				out["logged_in"] = true
				out["username"] = s.Auth.Username(r.Context())
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
	if s.Auth == nil {
		return
	}
	mux.HandleFunc("POST /api/v1/auth/setup/start", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code     string `json:"code"`
			Username string `json:"username"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		secret, err := s.Auth.SetupStart(r.Context(), in.Code, clientIP(r))
		if err != nil {
			s.authError(w, err)
			return
		}
		name := strings.TrimSpace(in.Username)
		if name == "" {
			name = "admin"
		}
		uri := auth.OTPAuthURI(secret, name)
		qr, _ := auth.QRDataURI(uri)
		writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "otpauth": uri, "qr": qr})
	})
	mux.HandleFunc("POST /api/v1/auth/setup/finish", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Code, Username, Password, TOTP string }
		if !readJSON(w, r, &in) {
			return
		}
		tok, err := s.Auth.SetupFinish(r.Context(), in.Code, in.Username, in.Password, in.TOTP, clientIP(r))
		if err != nil {
			s.authError(w, err)
			return
		}
		setSession(w, r, tok)
		s.logger().Info("admin account created", "user", strings.TrimSpace(in.Username))
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Username, Password, TOTP string }
		if !readJSON(w, r, &in) {
			return
		}
		tok, err := s.Auth.Login(r.Context(), in.Username, in.Password, in.TOTP, clientIP(r))
		if err != nil {
			if errors.Is(err, auth.ErrBadLogin) || errors.Is(err, auth.ErrBadTOTP) {
				s.logger().Warn("failed login", "ip", clientIP(r))
			}
			s.authError(w, err)
			return
		}
		setSession(w, r, tok)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			_ = s.Auth.Logout(r.Context(), c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// settingsFromUI are the settings the web page may change.
var settingsFromUI = map[string]bool{
	config.CloudflareToken: true, config.CloudflareAccount: true, config.PublishEmails: true,
	config.LANAddress: true, config.NtfyURL: true, config.NtfyTopic: true, config.NtfyToken: true,
	config.AlertsLang: true, config.HeartbeatURL: true, config.TailscaleAuthKey: true, config.TailscaleAPIKey: true,
	config.ExposureHosts: true,
}

func (s *Server) registerPublish(mux *http.ServeMux) {
	if s.Settings != nil {
		mux.HandleFunc("GET /api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
			list, err := s.Settings.List(r.Context())
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
				return
			}
			writeJSON(w, http.StatusOK, list)
		})
		mux.HandleFunc("POST /api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
			var in struct{ Name, Value string }
			if !readJSON(w, r, &in) {
				return
			}
			if !settingsFromUI[in.Name] {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "setting_not_allowed"})
				return
			}
			if err := s.Settings.Set(r.Context(), in.Name, in.Value); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_value", "detail": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
		})
	}
	if s.Publish == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/publish/options", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Publish.Options(r.Context()))
	})
	mux.HandleFunc("GET /api/v1/publications", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Publish.List(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
			return
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST /api/v1/publications", func(w http.ResponseWriter, r *http.Request) {
		var req publish.Request
		if !readJSON(w, r, &req) {
			return
		}
		p, err := s.Publish.Publish(r.Context(), req)
		if err != nil {
			s.publishError(w, err, p)
			return
		}
		s.logger().Info("published", "host", p.Hostname, "target", p.Target, "login", p.Access)
		writeJSON(w, http.StatusOK, p)
	})
	mux.HandleFunc("DELETE /api/v1/publications/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Publish.Undo(r.Context(), r.PathValue("id"))
		if err != nil {
			s.publishError(w, err, p)
			return
		}
		s.logger().Info("unpublished", "host", p.Hostname)
		writeJSON(w, http.StatusOK, p)
	})
	mux.HandleFunc("POST /api/v1/publications/{id}/check", func(w http.ResponseWriter, r *http.Request) {
		ch, err := s.Publish.CheckOutside(r.Context(), r.PathValue("id"))
		if err != nil {
			s.publishError(w, err, publish.Publication{})
			return
		}
		writeJSON(w, http.StatusOK, ch)
	})
}

func (s *Server) publishError(w http.ResponseWriter, err error, p publish.Publication) {
	known := []error{publish.ErrNotConfigured, publish.ErrTailscaleNotConfigured, publish.ErrBadName, publish.ErrBadTarget, publish.ErrLoopback,
		publish.ErrNameTaken, publish.ErrNoZone, publish.ErrNoTunnel, publish.ErrLocalTunnel,
		publish.ErrLoginRequired, publish.ErrNoEmails, publish.ErrVideoWarning, publish.ErrAlreadyUndone}
	for _, k := range known {
		if errors.Is(err, k) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": k.Error()})
			return
		}
	}
	if errors.Is(err, publish.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "publish_not_found"})
		return
	}
	// Cloudflare or network problem: say what happened, show the steps.
	s.logger().Warn("publish failed", "err", err)
	out := map[string]any{"error": "publish_failed", "detail": err.Error()}
	if p.ID != "" {
		out["publication"] = p
	}
	writeJSON(w, http.StatusBadGateway, out)
}
