package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TicketTTL es lo que dura un ticket de sesión: el servicio Python lo pide y redirige al navegador
// de inmediato, así que 60 s sobran.
const TicketTTL = 60 * time.Second

var ErrInvalidTicket = errors.New("el ticket de sesión no es válido, venció o ya se usó")

// ErrTicketExpired es el caso de un ticket auténtico que venció sin usarse. Cumple errors.Is(ErrInvalidTicket).
var ErrTicketExpired = fmt.Errorf("%w: venció", ErrInvalidTicket)

// SetTicketTTL cambia la vida de los tickets (pruebas).
func (s *Store) SetTicketTTL(d time.Duration) { s.ticketTTL = d }

// IssueSessionTicket emite un ticket de un solo uso para el usuario externo. Devuelve el ticket en
// claro (hex de 32 bytes); en la base solo queda su hash.
func (s *Store) IssueSessionTicket(externalUserID string) (string, time.Duration, error) {
	var userID string
	err := s.db.QueryRow(`SELECT id FROM users WHERE external_id = ?`, externalUserID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrUnknownUser
	}
	if err != nil {
		return "", 0, err
	}
	token, err := randomHex(32)
	if err != nil {
		return "", 0, err
	}
	now := time.Now().UTC()
	// Limpieza de tickets vencidos: no es crítica, por eso se ignora su error.
	_, _ = s.db.Exec(`DELETE FROM session_tickets WHERE expires_at < ?`, fmtTime(now))
	if _, err := s.db.Exec(`INSERT INTO session_tickets (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(token), userID, fmtTime(now), fmtTime(now.Add(s.ticketTTL))); err != nil {
		return "", 0, err
	}
	return token, s.ticketTTL, nil
}

// ConsumeSessionTicket valida y quema el ticket (un solo uso) y devuelve el usuario dueño.
// Un ticket inexistente, vencido o ya usado da ErrInvalidTicket.
func (s *Store) ConsumeSessionTicket(token string) (*User, error) {
	if token == "" || len(token) > 256 {
		return nil, ErrInvalidTicket
	}
	now := time.Now().UTC()
	var userID string
	// El UPDATE condicional es atómico: dos peticiones con el mismo ticket no pueden ganar las dos.
	err := s.db.QueryRow(`UPDATE session_tickets SET used_at = ?
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ? RETURNING user_id`,
		fmtTime(now), hashToken(token), fmtTime(now)).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		var expired bool
		if s.db.QueryRow(`SELECT 1 FROM session_tickets WHERE token_hash = ? AND used_at IS NULL AND expires_at <= ?`,
			hashToken(token), fmtTime(now)).Scan(&expired) == nil {
			return nil, ErrTicketExpired
		}
		return nil, ErrInvalidTicket
	}
	if err != nil {
		return nil, err
	}
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users u WHERE u.id = ?`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidTicket
	}
	return u, err
}

// RevokeExternalSessions cierra todas las sesiones (y tickets pendientes) de un usuario externo.
// Devuelve cuántas sesiones borró.
func (s *Store) RevokeExternalSessions(externalUserID string) (int64, error) {
	var userID string
	err := s.db.QueryRow(`SELECT id FROM users WHERE external_id = ?`, externalUserID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrUnknownUser
	}
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM python_cookie_jar WHERE user_id = ?`, userID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM session_tickets WHERE user_id = ?`, userID); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
