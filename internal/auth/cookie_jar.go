package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

// Cookie jar: las cookies que el servicio Python deja en el navegador (tokens de Brightspace) se
// guardan aquí, por usuario y cifradas en reposo, para que el cliente Go → Python las reenvíe.

var (
	ErrJarDisabled = errors.New("el almacén de cookies no está habilitado (falta COOKIE_JAR_KEY)")
	ErrJarCorrupt = errors.New("cookie del almacén ilegible: clave distinta o dato alterado")
)

// JarCookie es una cookie guardada para un usuario.
type JarCookie struct {
	Name string
	Value string
	Path string
	ExpiresAt *time.Time
}

// SetCookieJarKey activa el almacén con una clave AES de 16, 24 o 32 bytes (se exigen 32 en config).
func (s *Store) SetCookieJarKey(key []byte) error {
	if _, err := aes.NewCipher(key); err != nil {
		return err
	}
	s.jarKey = append([]byte(nil), key...)
	return nil
}

func (s *Store) jarGCM() (cipher.AEAD, error) {
	if len(s.jarKey) == 0 {
		return nil, ErrJarDisabled
	}
	block, err := aes.NewCipher(s.jarKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SaveJarCookie guarda (o reemplaza) una cookie del usuario. El nombre y el usuario van como datos
// asociados del cifrado: una fila copiada a otro usuario o nombre no descifra.
func (s *Store) SaveJarCookie(userID string, c JarCookie) error {
	gcm, err := s.jarGCM()
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(c.Value), []byte(userID+"\x00"+c.Name))
	path := c.Path
	if path == "" {
		path = "/"
	}
	_, err = s.db.Exec(`INSERT INTO python_cookie_jar (user_id, name, value, path, expires_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, name) DO UPDATE SET value = excluded.value, path = excluded.path,
			expires_at = excluded.expires_at, updated_at = excluded.updated_at`,
		userID, c.Name, base64.StdEncoding.EncodeToString(sealed), path, fmtTimePtr(c.ExpiresAt), fmtTime(time.Now().UTC()))
	return err
}

// LoadJar devuelve las cookies vigentes del usuario, descifradas. Las vencidas se omiten.
func (s *Store) LoadJar(userID string) ([]JarCookie, error) {
	gcm, err := s.jarGCM()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT name, value, path, expires_at FROM python_cookie_jar WHERE user_id = ? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JarCookie{}
	now := time.Now()
	for rows.Next() {
		var c JarCookie
		var raw string
		var exp sql.NullString
		if err := rows.Scan(&c.Name, &raw, &c.Path, &exp); err != nil {
			return nil, err
		}
		c.ExpiresAt = timePtr(exp)
		if c.ExpiresAt != nil && c.ExpiresAt.Before(now) {
			continue
		}
		sealed, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(sealed) < gcm.NonceSize() {
			return nil, ErrJarCorrupt
		}
		plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte(userID+"\x00"+c.Name))
		if err != nil {
			return nil, ErrJarCorrupt
		}
		c.Value = string(plain)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteJarCookie borra una cookie del usuario (Python la dio por vencida o la renovó vacía).
func (s *Store) DeleteJarCookie(userID, name string) error {
	_, err := s.db.Exec(`DELETE FROM python_cookie_jar WHERE user_id = ? AND name = ?`, userID, name)
	return err
}

// DeleteJar borra todas las cookies guardadas del usuario (logout, revocación).
func (s *Store) DeleteJar(userID string) error {
	_, err := s.db.Exec(`DELETE FROM python_cookie_jar WHERE user_id = ?`, userID)
	return err
}

// JarEnabled indica si hay clave de cifrado configurada.
func (s *Store) JarEnabled() bool { return len(s.jarKey) > 0 }
