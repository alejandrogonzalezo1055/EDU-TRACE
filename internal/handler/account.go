package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/marlon/edu-trace/internal/auth"
	"github.com/marlon/edu-trace/internal/model"
)

func (h *AuthHandler) Consent(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	at, err := h.store.RecordConsent(user.ID)
	if err != nil {
		writeStoreErr(w, err, "registrar tu autorización")
		return
	}
	user.ConsentAt, user.ConsentVersion = &at, auth.PrivacyVersion
	slog.Info("privacy consent recorded", "user_id", user.ID, "version", auth.PrivacyVersion)
	writeJSON(w, http.StatusOK, toPublic(user))
}

func (h *AuthHandler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if n := utf8.RuneCountInString(name); n < 2 || n > 80 {
		writeJSON(w, http.StatusBadRequest, model.ErrorResponse{Error: "el nombre debe tener entre 2 y 80 caracteres"})
		return
	}
	user := auth.UserFrom(r.Context())
	if err := h.store.UpdateName(user.ID, name); err != nil {
		writeStoreErr(w, err, "actualizar tu nombre")
		return
	}
	user.Name = name
	writeJSON(w, http.StatusOK, toPublic(user))
}

func (h *AuthHandler) Export(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	data, err := h.store.ExportData(user)
	if err != nil {
		writeStoreErr(w, err, "exportar tus datos")
		return
	}
	slog.Info("personal data exported", "user_id", user.ID)
	w.Header().Set("Content-Disposition", `attachment; filename="edutrace-mis-datos.json"`)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, data)
}

func (h *AuthHandler) DeleteAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ip := clientIP(r)
	if h.limiter.Blocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, model.ErrorResponse{Error: "demasiados intentos, espera unos minutos"})
		return
	}
	user := auth.UserFrom(r.Context())
	err := h.store.DeleteAccount(user.ID, req.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		h.limiter.Fail(ip)
		writeJSON(w, http.StatusForbidden, model.ErrorResponse{Error: "la contraseña no es correcta"})
		return
	case errors.Is(err, auth.ErrNoLocalPassword):
		writeJSON(w, http.StatusConflict, model.ErrorResponse{Error: err.Error()})
		return
	case errors.Is(err, auth.ErrOwnsCourses):
		writeJSON(w, http.StatusConflict, model.ErrorResponse{Error: err.Error()})
		return
	case err != nil:
		writeStoreErr(w, err, "eliminar tu cuenta")
		return
	}
	slog.Info("account deleted", "user_id", user.ID, "role", user.Role)
	h.Logout(w, r)
}
