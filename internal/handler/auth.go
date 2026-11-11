package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/marlon/edu-trace/internal/auth"
	"github.com/marlon/edu-trace/internal/config"
	"github.com/marlon/edu-trace/internal/model"
)

// AuthHandler agrupa la apertura de sesión por ticket, logout, sesión actual y datos de la cuenta.
type AuthHandler struct {
	store *auth.Store
	cfg *config.Config
	limiter *auth.LoginLimiter
}

func NewAuthHandler(store *auth.Store, cfg *config.Config) *AuthHandler {
	return &AuthHandler{
		store: store,
		cfg: cfg,
		limiter: auth.NewLoginLimiter(10, 15*time.Minute),
	}
}

type publicUser struct {
	ID string `json:"id"`
	Name string `json:"name"`
	Email string `json:"email"`
	Role auth.Role `json:"role"`
	Activity auth.Activity `json:"activity"`
	HasRecovery bool `json:"has_recovery"`
	ConsentAt *time.Time `json:"consent_at,omitempty"`
	ConsentOutdated bool `json:"consent_outdated,omitempty"`
}

func toPublic(u *auth.User) publicUser {
	p := publicUser{ID: u.ID, Name: u.Name, Email: u.Email, Role: u.Role, Activity: u.Activity, HasRecovery: u.RecoveryHash != ""}
	if u.ConsentCurrent() {
		p.ConsentAt = u.ConsentAt
	} else if u.ConsentAt != nil {
		p.ConsentOutdated = true
	}
	return p
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// decodeJSON lee un cuerpo JSON pequeño (formularios). Para cuerpos grandes usa decodeJSONLimit.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeJSONLimit(w, r, v, 16*1024)
}

func decodeJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, model.ErrorResponse{Error: "cuerpo de la petición inválido"})
		return false
	}
	return true
}

// SessionByTicket maneja GET /api/auth/session?ticket=: canjea el ticket de un solo uso que pidió el
// servicio Python, abre la sesión de Go (cookie propia edutrace_session) y redirige al frontend.
// Ticket inválido, vencido o ya usado: 401 y se manda al navegador a /login.
func (h *AuthHandler) SessionByTicket(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if h.limiter.Blocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, model.ErrorResponse{Error: "demasiados intentos, espera unos minutos"})
		return
	}
	u, err := h.store.ConsumeSessionTicket(r.URL.Query().Get("ticket"))
	if errors.Is(err, auth.ErrInvalidTicket) {
		h.limiter.Fail(ip)
		reason := "invalid_ticket"
		if errors.Is(err, auth.ErrTicketExpired) {
			reason = "session_expired"
		}
		h.redirectToLogin(w, reason)
		return
	}
	if err != nil {
		slog.Error("consume session ticket failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, model.ErrorResponse{Error: "no se pudo iniciar sesión"})
		return
	}
	token, expires, err := h.store.CreateSession(u.ID)
	if err != nil {
		slog.Error("create session failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, model.ErrorResponse{Error: "no se pudo iniciar sesión"})
		return
	}
	h.setSessionCookie(w, token, expires)
	h.captureJar(r, u.ID)
	slog.Info("session started from ticket", "user_id", u.ID)
	target := h.cfg.PublicURL
	if target == "" {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// captureJar guarda en el almacén cifrado las cookies que Python dejó en el navegador (el navegador
// las manda también a Go porque las cookies de un mismo host no distinguen puerto).
func (h *AuthHandler) captureJar(r *http.Request, userID string) {
	if !h.store.JarEnabled() {
		return
	}
	saved := 0
	for _, name := range h.cfg.PythonCookieNames {
		c, err := r.Cookie(name)
		if err != nil || c.Value == "" {
			continue
		}
		if err := h.store.SaveJarCookie(userID, auth.JarCookie{Name: name, Value: c.Value, Path: "/"}); err != nil {
			slog.Error("save cookie jar failed", "user_id", userID, "cookie", name, "error", err)
			continue
		}
		saved++
	}
	if saved > 0 {
		slog.Info("python cookies stored", "user_id", userID, "count", saved)
		// Con cookies nuevas, lo que esperaba una sesión de Brightspace puede enviarse ya.
		if err := h.store.WakeD2LFor(userID); err != nil {
			slog.Error("wake brightspace queue failed", "user_id", userID, "error", err)
		}
	}
}

// redirectToLogin responde 401 e indica el destino por Location, Refresh y meta refresh (un 401 no
// hace que el navegador siga Location por sí solo). reason va como /login?error=<reason>, que el
// frontend traduce a un mensaje (invalid_ticket, session_expired).
func (h *AuthHandler) redirectToLogin(w http.ResponseWriter, reason string) {
	login := h.cfg.PublicURL + "/login?error=" + url.QueryEscape(reason)
	w.Header().Set("Location", login)
	w.Header().Set("Refresh", "0; url="+login)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	esc := html.EscapeString(login)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%[1]s"><title>Sesión no iniciada</title><p>El enlace de acceso no es válido o venció. <a href="%[1]s">Volver a iniciar sesión</a></p>`, esc)
}

// setSessionCookie: HttpOnly, SameSite=Lax y Secure (forzado en config cuando Go sirve HTTPS).
func (h *AuthHandler) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: auth.CookieName,
		Value: token,
		Path: "/",
		Expires: expires,
		HttpOnly: true,
		Secure: h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Logout maneja POST /api/auth/logout.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		if u, ok := h.store.UserForSession(c.Value); ok {
			if err := h.store.DeleteJar(u.ID); err != nil {
				slog.Error("delete cookie jar failed", "user_id", u.ID, "error", err)
			}
		}
		if err := h.store.DeleteSession(c.Value); err != nil {
			slog.Error("delete session failed", "error", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.CookieName,
		Value: "",
		Path: "/",
		MaxAge: -1,
		HttpOnly: true,
		Secure: h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// Me maneja GET /api/auth/me (requiere sesión).
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, toPublic(auth.UserFrom(r.Context())))
}
