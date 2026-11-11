package pyclient

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxCookieAge = 10 * 365 * 24 * time.Hour

// cookieHeader arma la cabecera Cookie con las cookies permitidas y vigentes del usuario. Se omiten
// las de valor vacío o con caracteres que romperían la cabecera (nunca se inyecta nada raro).
func (c *Client) cookieHeader(cookies []Cookie) string {
	now := time.Now()
	parts := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		if !c.allowed[ck.Name] || ck.Value == "" || !validCookieValue(ck.Value) {
			continue
		}
		if ck.ExpiresAt != nil && !ck.ExpiresAt.After(now) {
			continue
		}
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	return strings.Join(parts, "; ")
}

func validCookieValue(v string) bool {
	for i := 0; i < len(v); i++ {
		b := v[i]
		if b <= 0x20 || b == 0x7f || b == ';' || b == ',' || b == '\\' || b == '"' {
			return false
		}
	}
	return true
}

// applySetCookies actualiza el almacén del usuario con los Set-Cookie de Python. Se analizan a mano
// porque net/http descarta los Max-Age decimales (Python emite "Max-Age=90.0"). Reglas: nombre fuera de
// la lista permitida, se ignora; valor vacío, Max-Age <= 0 o Expires pasado, se borra; si no, se guarda.
// Nunca se registran valores, solo nombres y cantidades.
func (c *Client) applySetCookies(userID string, headers []string) {
	if len(headers) == 0 {
		return
	}
	saved, deleted := 0, 0
	for _, h := range headers {
		ck, remove, ok := parseSetCookie(h, time.Now())
		if !ok || !c.allowed[ck.Name] {
			continue
		}
		if remove {
			if err := c.jar.Delete(userID, ck.Name); err != nil {
				slog.Error("python cookie delete failed", "user_id", userID, "cookie", ck.Name, "error", err)
				continue
			}
			deleted++
			continue
		}
		if err := c.jar.Save(userID, ck); err != nil {
			slog.Error("python cookie save failed", "user_id", userID, "cookie", ck.Name, "error", err)
			continue
		}
		saved++
	}
	if saved+deleted > 0 {
		slog.Info("python cookies updated", "user_id", userID, "saved", saved, "deleted", deleted)
	}
}

// parseSetCookie interpreta una cabecera Set-Cookie. ok=false si no tiene nombre válido.
func parseSetCookie(line string, now time.Time) (ck Cookie, remove, ok bool) {
	parts := strings.Split(line, ";")
	name, value, found := strings.Cut(strings.TrimSpace(parts[0]), "=")
	name = strings.TrimSpace(name)
	if !found || name == "" {
		return Cookie{}, false, false
	}
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	ck = Cookie{Name: name, Value: value, Path: "/"}
	var maxAge *float64
	var expires *time.Time
	for _, attr := range parts[1:] {
		k, v, _ := strings.Cut(strings.TrimSpace(attr), "=")
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "max-age":
			if f, err := strconv.ParseFloat(v, 64); err == nil && !math.IsNaN(f) {
				maxAge = &f
			}
		case "expires":
			if t, err := http.ParseTime(v); err == nil {
				expires = &t
			}
		case "path":
			if strings.HasPrefix(v, "/") {
				ck.Path = v
			}
		}
	}
	switch {
	case value == "":
		return ck, true, true
	case maxAge != nil: // Max-Age manda sobre Expires (RFC 6265)
		if *maxAge <= 0 {
			return ck, true, true
		}
		d := maxCookieAge
		if *maxAge < float64(maxCookieAge/time.Second) {
			d = time.Duration(*maxAge * float64(time.Second))
		}
		t := now.Add(d).UTC()
		ck.ExpiresAt = &t
	case expires != nil:
		if !expires.After(now) {
			return ck, true, true
		}
		t := expires.UTC()
		ck.ExpiresAt = &t
	}
	return ck, false, true
}
