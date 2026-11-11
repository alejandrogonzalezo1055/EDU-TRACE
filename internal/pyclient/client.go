// Package pyclient es el cliente HTTPS de Go hacia el servicio Python (Brightspace), contrato §5.
//
// Python guarda los tokens de Brightspace en cookies HttpOnly/Secure del navegador. Go las captura
// por usuario al abrir sesión (python_cookie_jar) y este cliente las reenvía en la cabecera Cookie
// de cada llamada hecha en nombre de ese usuario, y actualiza el almacén con los Set-Cookie de la
// respuesta. Los valores de las cookies y la clave nunca se escriben en registros.
package pyclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTimeout rige health y cualquier llamada corta; SubmitTimeout, la subida a Brightspace.
	DefaultTimeout = 10 * time.Second
	SubmitTimeout = 30 * time.Second
	maxBody = 1 << 20
	// GradeScale es el máximo de la nota de EduTrace (0 a 5).
	GradeScale = 5.0
)

// Códigos de error de la tabla §5.1 del contrato.
const (
	CodeInvalidToken = "token_invalido"
	CodeOutOfWindow = "fuera_de_ventana"
	CodeNoFiles = "carpeta_no_acepta"
	CodeTooLarge = "muy_grande"
	CodeRateLimit = "limite_tasa"
	CodeDown = "brightspace_caido"
	CodeNotTeacher = "no_es_docente"
	CodeConfig = "configuracion"
)

// Cookie es una cookie guardada para un usuario.
type Cookie struct {
	Name string
	Value string
	Path string
	ExpiresAt *time.Time
}

// Jar es el almacén de cookies por usuario (lo implementa el Store de Go).
type Jar interface {
	Load(userID string) ([]Cookie, error)
	Save(userID string, c Cookie) error
	Delete(userID, name string) error
}

// Config configura el cliente.
type Config struct {
	BaseURL string
	APIKey string
	CAFile string // PEM opcional (p. ej. la CA de mkcert) que se suma al almacén del sistema
	AllowHTTP bool // solo desarrollo: permite http:// (las cookies de usuario nunca viajan por http)
	CookieNames []string // únicas cookies que se envían y se aceptan
	Timeout time.Duration
	SubmitTimeout time.Duration
}

// Client habla con el servicio Python. Es seguro para uso concurrente.
type Client struct {
	base *url.URL
	key string
	http *http.Client
	jar Jar
	allowed map[string]bool
	timeout, submitTimeout time.Duration

	mu sync.Mutex
	userLocks map[string]*sync.Mutex
}

// Error es un fallo de la llamada a Python, ya traducido a los códigos del contrato.
type Error struct {
	Code string
	Status int
	Message string
	RetryAfter time.Duration // solo limite_tasa
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Message
}

// Retryable indica si conviene reintentar (límite de tasa, caída de Brightspace o de Python).
// token_invalido, fuera_de_ventana, carpeta_no_acepta, muy_grande, no_es_docente y cualquier otro
// rechazo 4xx son definitivos: repetir la misma llamada daría lo mismo.
func (e *Error) Retryable() bool {
	switch e.Code {
	case CodeRateLimit, CodeDown:
		return true
	case CodeInvalidToken, CodeOutOfWindow, CodeNoFiles, CodeTooLarge, CodeNotTeacher, CodeConfig:
		return false
	}
	return e.Status == 0 || e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

// IsInvalidToken indica que el usuario debe volver a iniciar sesión.
func IsInvalidToken(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Code == CodeInvalidToken
}

// New valida la configuración y arma el cliente. Rechaza http:// salvo AllowHTTP y nunca desactiva
// la verificación del certificado.
func New(cfg Config, jar Jar) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("PYTHON_BASE_URL inválida: debe ser una URL como https://localhost:8000")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !cfg.AllowHTTP {
			return nil, errors.New("PYTHON_BASE_URL debe ser https:// (para desarrollo, PYTHON_ALLOW_HTTP=true permite http://)")
		}
	default:
		return nil, fmt.Errorf("PYTHON_BASE_URL con esquema %q no permitido: usa https://", u.Scheme)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("PYTHON_BASE_URL no debe llevar usuario, parámetros ni fragmento")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if cfg.APIKey == "" {
		return nil, errors.New("falta PYTHON_API_KEY")
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("PYTHON_CA_FILE: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("PYTHON_CA_FILE no contiene ningún certificado PEM válido")
		}
	}
	transport := &http.Transport{
		// Sin proxy de entorno: las cookies y la clave solo salen hacia el host configurado.
		Proxy: nil,
		DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns: 4,
		IdleConnTimeout: 60 * time.Second,
	}
	c := &Client{
		base: u,
		key: cfg.APIKey,
		jar: jar,
		allowed: map[string]bool{},
		timeout: cfg.Timeout,
		submitTimeout: cfg.SubmitTimeout,
		userLocks: map[string]*sync.Mutex{},
		http: &http.Client{
			Transport: transport,
			// Nunca se sigue una redirección: reenviaría la clave y las cookies a otro destino.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.submitTimeout <= 0 {
		c.submitTimeout = SubmitTimeout
	}
	for _, n := range cfg.CookieNames {
		c.allowed[n] = true
	}
	return c, nil
}

// HasCookies indica si el usuario tiene guardada al menos una cookie utilizable de Python.
func (c *Client) HasCookies(userID string) bool {
	if c.jar == nil {
		return false
	}
	cookies, err := c.jar.Load(userID)
	return err == nil && c.cookieHeader(cookies) != ""
}

// BaseURL devuelve la base configurada (sin credenciales; útil para registros).
func (c *Client) BaseURL() string { return c.base.String() }

// SubmitRequest es el cuerpo de POST /internal/v1/dropbox/submit (§5.1).
type SubmitRequest struct {
	StudentExternalID string `json:"student_external_id"`
	CourseExternalID string `json:"course_external_id"`
	AssignmentExternalID string `json:"assignment_external_id"`
	Filename string `json:"filename"`
	Content string `json:"content"`
	Comment string `json:"comment"`
	GoSubmissionID string `json:"go_submission_id"`
}

// SubmitResult es la respuesta 200 de submit.
type SubmitResult struct {
	Status string `json:"status"`
	ExternalID string `json:"external_id"`
}

// FeedbackRequest es el cuerpo de POST /internal/v1/dropbox/feedback (§5.2). Score va null si el
// docente no puso nota (is_graded=false).
type FeedbackRequest struct {
	TeacherExternalID string `json:"teacher_external_id"`
	StudentExternalID string `json:"student_external_id"`
	CourseExternalID string `json:"course_external_id"`
	AssignmentExternalID string `json:"assignment_external_id"`
	Score *float64 `json:"score"`
	ScoreDenominator float64 `json:"score_denominator"`
	Feedback string `json:"feedback"`
	IsGraded bool `json:"is_graded"`
	GoReviewID string `json:"go_review_id"`
}

// HealthResult es la respuesta de GET /internal/v1/health de Python.
type HealthResult struct {
	Status string `json:"status"`
	Brightspace *bool `json:"brightspace,omitempty"`
}

// Score convierte la nota de EduTrace (0 a 5) a la escala de la carpeta: nota ÷ 5,0 × denominador,
// redondeada a 2 decimales. Un denominador no positivo se toma como 100.
func Score(grade, denominator float64) float64 {
	if denominator <= 0 || math.IsNaN(denominator) || math.IsInf(denominator, 0) {
		denominator = 100
	}
	return math.Round(grade/GradeScale*denominator*100) / 100
}

// Submit envía una entrega oficial con las cookies de userID (el estudiante).
func (c *Client) Submit(ctx context.Context, userID string, req SubmitRequest) (*SubmitResult, error) {
	var out SubmitResult
	if err := c.do(ctx, http.MethodPost, "/internal/v1/dropbox/submit", userID, req, &out, c.submitTimeout); err != nil {
		return nil, err
	}
	if out.Status != "" && out.Status != "enviada" {
		return nil, &Error{Code: CodeDown, Status: http.StatusOK, Message: "Python respondió un estado inesperado en submit"}
	}
	return &out, nil
}

// Feedback publica nota y texto con las cookies de userID (el docente). Reemplaza lo publicado antes.
func (c *Client) Feedback(ctx context.Context, userID string, req FeedbackRequest) error {
	var out struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodPost, "/internal/v1/dropbox/feedback", userID, req, &out, c.submitTimeout); err != nil {
		return err
	}
	if out.Status != "" && out.Status != "publicada" {
		return &Error{Code: CodeDown, Status: http.StatusOK, Message: "Python respondió un estado inesperado en feedback"}
	}
	return nil
}

// Health consulta GET /internal/v1/health (sin cookies: no actúa en nombre de nadie).
func (c *Client) Health(ctx context.Context) (*HealthResult, error) {
	var out HealthResult
	if err := c.do(ctx, http.MethodGet, "/internal/v1/health", "", nil, &out, c.timeout); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) lockUser(userID string) func() {
	c.mu.Lock()
	l := c.userLocks[userID]
	if l == nil {
		l = &sync.Mutex{}
		c.userLocks[userID] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// do hace una llamada. Si userID no está vacío envía sus cookies y procesa los Set-Cookie. Las
// llamadas del mismo usuario se serializan: los refresh tokens de Brightspace son de un solo uso y
// dos llamadas simultáneas se pisarían las cookies.
func (c *Client) do(ctx context.Context, method, path, userID string, body, out any, timeout time.Duration) error {
	if userID != "" {
		defer c.lockUser(userID)()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return &Error{Code: CodeConfig, Message: "no se pudo armar la petición a Python"}
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target := *c.base
	target.Path += path
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return &Error{Code: CodeConfig, Message: "no se pudo armar la petición a Python"}
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if userID != "" {
		if c.base.Scheme != "https" {
			return &Error{Code: CodeConfig, Message: "las cookies del usuario solo se envían por https: PYTHON_BASE_URL debe ser https://"}
		}
		if c.jar == nil {
			return &Error{Code: CodeConfig, Message: "no hay almacén de cookies configurado"}
		}
		cookies, err := c.jar.Load(userID)
		if err != nil {
			return &Error{Code: CodeConfig, Message: "no se pudieron leer las cookies guardadas del usuario: " + err.Error()}
		}
		if header := c.cookieHeader(cookies); header != "" {
			req.Header.Set("Cookie", header)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// El error de net/http incluye la URL pero no cabeceras ni cookies.
		return &Error{Code: CodeDown, Message: "no se pudo contactar al servicio Python: " + sanitizeNetErr(err)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if userID != "" {
		c.applySetCookies(userID, resp.Header.Values("Set-Cookie"))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil && len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return &Error{Code: CodeDown, Status: resp.StatusCode, Message: "Python respondió algo que no es JSON"}
			}
		}
		return nil
	}
	return decodeError(resp, raw)
}

func sanitizeNetErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "tiempo de espera agotado"
	}
	return err.Error()
}

// decodeError traduce la respuesta de error de Python (§5.1 y §8) a *Error.
func decodeError(resp *http.Response, raw []byte) error {
	var body struct {
		Error string `json:"error"`
		Code string `json:"code"`
		RetryAfter json.Number `json:"retry_after"`
	}
	_ = json.Unmarshal(raw, &body)
	e := &Error{Code: strings.TrimSpace(body.Code), Status: resp.StatusCode, Message: clip(strings.TrimSpace(body.Error), 300)}
	// 401 y 408 sin código claro: el token de Brightspace del usuario venció (contrato, decisión del cliente).
	if resp.StatusCode == http.StatusRequestTimeout || (resp.StatusCode == http.StatusUnauthorized && e.Code == "") {
		e.Code = CodeInvalidToken
	}
	if e.Code == "" {
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			e.Code = CodeRateLimit
		case http.StatusRequestEntityTooLarge:
			e.Code = CodeTooLarge
		}
	}
	if e.Code == CodeRateLimit {
		if secs, err := strconv.ParseFloat(body.RetryAfter.String(), 64); err == nil && secs > 0 {
			e.RetryAfter = secondsToDuration(secs)
		} else if secs, err := strconv.ParseFloat(strings.TrimSpace(resp.Header.Get("Retry-After")), 64); err == nil && secs > 0 {
			e.RetryAfter = secondsToDuration(secs)
		}
	}
	if e.Message == "" {
		e.Message = describe(e.Code, resp.StatusCode)
	}
	return e
}

func secondsToDuration(s float64) time.Duration {
	const max = 24 * time.Hour
	if s*float64(time.Second) > float64(max) {
		return max
	}
	return time.Duration(s * float64(time.Second))
}

func describe(code string, status int) string {
	switch code {
	case CodeInvalidToken:
		return "el token de Brightspace del usuario venció o fue revocado: debe volver a iniciar sesión"
	case CodeOutOfWindow:
		return "la carpeta de Brightspace está fuera de su ventana de entrega"
	case CodeNoFiles:
		return "la carpeta de Brightspace no admite archivos"
	case CodeTooLarge:
		return "el archivo supera el tamaño que admite Brightspace"
	case CodeRateLimit:
		return "Brightspace limitó las peticiones"
	case CodeDown:
		return "Brightspace no está disponible"
	case CodeNotTeacher:
		return "el usuario no es docente en ese curso de Brightspace"
	}
	return "Python respondió HTTP " + strconv.Itoa(status)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
