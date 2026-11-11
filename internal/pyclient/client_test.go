package pyclient

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "k-0123456789abcdef0123456789abcdef"

var names = []string{"auth_code", "access_token", "refresh_token", "response_time"}

type memJar struct {
	mu sync.Mutex
	m map[string]map[string]Cookie
}

func newJar() *memJar { return &memJar{m: map[string]map[string]Cookie{}} }

func (j *memJar) Load(u string) ([]Cookie, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Cookie
	for _, c := range j.m[u] {
		out = append(out, c)
	}
	return out, nil
}
func (j *memJar) Save(u string, c Cookie) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.m[u] == nil {
		j.m[u] = map[string]Cookie{}
	}
	j.m[u][c.Name] = c
	return nil
}
func (j *memJar) Delete(u, n string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.m[u], n)
	return nil
}
func (j *memJar) set(u, n, v string) { _ = j.Save(u, Cookie{Name: n, Value: v, Path: "/"}) }
func (j *memJar) get(u, n string) (Cookie, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	c, ok := j.m[u][n]
	return c, ok
}

// newTLS levanta un Python falso con TLS y escribe su certificado como si fuera la CA (PYTHON_CA_FILE).
func newTLS(t *testing.T, h http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, ca
}

func newClient(t *testing.T, srv *httptest.Server, ca string, jar Jar) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: srv.URL, APIKey: testKey, CAFile: ca, CookieNames: names}, jar)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeErr(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	body := map[string]any{"error": msg, "code": code}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func submitReq() SubmitRequest {
	return SubmitRequest{StudentExternalID: "d2l:user:3101", CourseExternalID: "d2l:ou:6650", AssignmentExternalID: "d2l:folder:701",
		Filename: "main.cpp", Content: "int main(){}", Comment: "Entrega desde EduTrace", GoSubmissionID: "ee90"}
}

func TestRejectsPlainHTTP(t *testing.T) {
	if _, err := New(Config{BaseURL: "http://localhost:8000", APIKey: testKey}, newJar()); err == nil {
		t.Fatal("http:// debe rechazarse por defecto")
	}
	if _, err := New(Config{BaseURL: "ftp://x", APIKey: testKey, AllowHTTP: true}, newJar()); err == nil {
		t.Fatal("esquema ftp debe rechazarse")
	}
	if _, err := New(Config{BaseURL: "https://u:p@host", APIKey: testKey}, newJar()); err == nil {
		t.Fatal("credenciales en la URL deben rechazarse")
	}
	c, err := New(Config{BaseURL: "http://localhost:8000", APIKey: testKey, AllowHTTP: true, CookieNames: names}, newJar())
	if err != nil {
		t.Fatalf("PYTHON_ALLOW_HTTP debe permitir http: %v", err)
	}
	// Aun con el permiso de desarrollo, las cookies de un usuario no viajan por http.
	_, err = c.Submit(context.Background(), "u1", submitReq())
	var pe *Error
	if err == nil || !asErr(err, &pe) || pe.Code != CodeConfig || !strings.Contains(pe.Message, "https") {
		t.Fatalf("submit por http: %v", err)
	}
}

func asErr(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestTLSIsVerified(t *testing.T) {
	srv, _ := newTLS(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok"}`)) })
	c := newClient(t, srv, "", newJar()) // sin la CA de la prueba: el certificado no es de confianza
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("debe fallar la verificación del certificado (nunca InsecureSkipVerify)")
	}
	if _, err := New(Config{BaseURL: srv.URL, APIKey: testKey, CAFile: filepath.Join(t.TempDir(), "no-existe.pem")}, nil); err == nil {
		t.Fatal("PYTHON_CA_FILE inexistente debe ser error")
	}
	bad := filepath.Join(t.TempDir(), "mal.pem")
	_ = os.WriteFile(bad, []byte("no es pem"), 0o600)
	if _, err := New(Config{BaseURL: srv.URL, APIKey: testKey, CAFile: bad}, nil); err == nil {
		t.Fatal("PEM inválido debe ser error")
	}
}

func TestSubmitSendsBearerBodyAndOwnCookiesAndAppliesSetCookie(t *testing.T) {
	var gotCookie, gotAuth string
	var gotBody SubmitRequest
	var mu sync.Mutex
	srv, ca := newTLS(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotAuth, gotCookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/dropbox/submit" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("petición inesperada: %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// Python renueva access_token (Max-Age decimal), borra refresh_token, e intenta colar una cookie ajena.
		w.Header().Add("Set-Cookie", "access_token=NUEVO; Max-Age=90.0; Path=/; HttpOnly; Secure; SameSite=Lax")
		w.Header().Add("Set-Cookie", "refresh_token=; Max-Age=0; Path=/")
		w.Header().Add("Set-Cookie", "evil=1; Path=/")
		_, _ = w.Write([]byte(`{"status":"enviada","external_id":"d2l:sub:9001"}`))
	})
	jar := newJar()
	jar.set("alice", "access_token", "A-ACCESS")
	jar.set("alice", "refresh_token", "A-REFRESH")
	jar.set("alice", "ajena", "no-permitida")
	jar.set("bob", "access_token", "B-ACCESS")
	c := newClient(t, srv, ca, jar)

	res, err := c.Submit(context.Background(), "alice", submitReq())
	if err != nil || res.Status != "enviada" || res.ExternalID != "d2l:sub:9001" {
		t.Fatalf("submit: %+v %v", res, err)
	}
	if gotAuth != "Bearer "+testKey {
		t.Fatalf("Authorization: %q", gotAuth)
	}
	parts := strings.Split(gotCookie, "; ")
	if len(parts) != 2 || !contains(parts, "access_token=A-ACCESS") || !contains(parts, "refresh_token=A-REFRESH") {
		t.Fatalf("Cookie de alice: %q", gotCookie) // nada de bob ni cookies fuera de la lista
	}
	if gotBody != submitReq() {
		t.Fatalf("cuerpo: %+v", gotBody)
	}
	// Set-Cookie aplicado al jar de alice (Max-Age=90.0 tolerado), refresh_token borrado, evil ignorada.
	ac, ok := jar.get("alice", "access_token")
	if !ok || ac.Value != "NUEVO" || ac.ExpiresAt == nil || time.Until(*ac.ExpiresAt) < 60*time.Second || time.Until(*ac.ExpiresAt) > 100*time.Second {
		t.Fatalf("access_token de alice: %+v ok=%v", ac, ok)
	}
	if _, ok := jar.get("alice", "refresh_token"); ok {
		t.Fatal("refresh_token debía borrarse (Max-Age=0)")
	}
	if _, ok := jar.get("alice", "evil"); ok {
		t.Fatal("una cookie fuera de la lista no se guarda")
	}
	// Aislamiento: bob intacto, y su llamada solo manda sus cookies.
	if bc, _ := jar.get("bob", "access_token"); bc.Value != "B-ACCESS" {
		t.Fatalf("bob alterado: %+v", bc)
	}
	if _, err := c.Submit(context.Background(), "bob", submitReq()); err != nil {
		t.Fatal(err)
	}
	if gotCookie != "access_token=B-ACCESS" {
		t.Fatalf("Cookie de bob: %q", gotCookie)
	}
	if bc, _ := jar.get("bob", "access_token"); bc.Value != "NUEVO" {
		t.Fatalf("el Set-Cookie de la llamada de bob va al jar de bob: %+v", bc)
	}
	if ac, _ := jar.get("alice", "access_token"); ac.Value != "NUEVO" {
		t.Fatalf("alice no debe cambiar por la llamada de bob: %+v", ac)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestFeedbackBody(t *testing.T) {
	var got map[string]any
	srv, ca := newTLS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/dropbox/feedback" {
			t.Errorf("ruta %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"status":"publicada"}`))
	})
	c := newClient(t, srv, ca, newJar())
	score := Score(4.2, 100)
	err := c.Feedback(context.Background(), "teacher", FeedbackRequest{TeacherExternalID: "d2l:user:2205", StudentExternalID: "d2l:user:3101",
		CourseExternalID: "d2l:ou:6650", AssignmentExternalID: "d2l:folder:701", Score: &score, ScoreDenominator: 100,
		Feedback: "Bien", IsGraded: true, GoReviewID: "ee90"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"teacher_external_id", "student_external_id", "course_external_id", "assignment_external_id", "score", "score_denominator", "feedback", "is_graded", "go_review_id"} {
		if _, ok := got[k]; !ok {
			t.Errorf("falta %q en el cuerpo: %v", k, got)
		}
	}
	if got["score"].(float64) != 84.0 || got["is_graded"] != true {
		t.Fatalf("cuerpo: %v", got)
	}
}

func TestScore(t *testing.T) {
	cases := []struct{ grade, denom, want float64 }{
		{4.2, 100, 84.00}, {5, 100, 100}, {0, 100, 0}, {3.3, 20, 13.2}, {4.7, 30, 28.2}, {2.5, 0, 50}, {1.1, 7, 1.54},
	}
	for _, c := range cases {
		if got := Score(c.grade, c.denom); got != c.want {
			t.Errorf("Score(%v, %v) = %v, quería %v", c.grade, c.denom, got, c.want)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		status int
		code string
		extra map[string]any
		header string
		want string
		retry bool
	}{
		{"token", 502, "token_invalido", nil, "", CodeInvalidToken, false},
		{"ventana", 409, "fuera_de_ventana", nil, "", CodeOutOfWindow, false},
		{"carpeta", 409, "carpeta_no_acepta", nil, "", CodeNoFiles, false},
		{"grande", 413, "muy_grande", nil, "", CodeTooLarge, false},
		{"tasa", 503, "limite_tasa", map[string]any{"retry_after": 42}, "", CodeRateLimit, true},
		{"tasa decimal", 503, "limite_tasa", map[string]any{"retry_after": 2.5}, "", CodeRateLimit, true},
		{"tasa cabecera", 503, "limite_tasa", nil, "7", CodeRateLimit, true},
		{"caido", 502, "brightspace_caido", nil, "", CodeDown, true},
		{"401 sin codigo", 401, "", nil, "", CodeInvalidToken, false},
		{"408", 408, "", nil, "", CodeInvalidToken, false},
		{"408 con otro codigo", 408, "otro", nil, "", CodeInvalidToken, false},
		{"no docente", 403, "no_es_docente", nil, "", CodeNotTeacher, false},
		{"500 pelado", 500, "", nil, "", "", true},
		{"400 desconocido", 400, "raro", nil, "", "raro", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ca := newTLS(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				writeErr(w, tc.status, tc.code, "", tc.extra)
			})
			c := newClient(t, srv, ca, newJar())
			_, err := c.Submit(context.Background(), "u", submitReq())
			var pe *Error
			if !asErr(err, &pe) {
				t.Fatalf("error de tipo %T: %v", err, err)
			}
			if pe.Code != tc.want || pe.Retryable() != tc.retry {
				t.Fatalf("code=%q retry=%v, quería %q %v", pe.Code, pe.Retryable(), tc.want, tc.retry)
			}
			if pe.Message == "" {
				t.Fatal("el error debe llevar un mensaje legible")
			}
			if tc.want == CodeRateLimit && pe.RetryAfter <= 0 {
				t.Fatalf("limite_tasa sin RetryAfter: %+v", pe)
			}
			if tc.name == "tasa" && pe.RetryAfter != 42*time.Second {
				t.Fatalf("retry_after: %v", pe.RetryAfter)
			}
			if tc.want == CodeInvalidToken && !IsInvalidToken(err) {
				t.Fatal("IsInvalidToken")
			}
		})
	}
}

func TestNetworkFailureIsRetryable(t *testing.T) {
	srv, ca := newTLS(t, func(w http.ResponseWriter, r *http.Request) {})
	c := newClient(t, srv, ca, newJar())
	srv.Close()
	_, err := c.Submit(context.Background(), "u", submitReq())
	var pe *Error
	if !asErr(err, &pe) || pe.Code != CodeDown || !pe.Retryable() {
		t.Fatalf("caída: %v", err)
	}
}

func TestSetCookieAppliedOnErrorResponseToo(t *testing.T) {
	srv, ca := newTLS(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "access_token=; Max-Age=0; Path=/")
		writeErr(w, 502, "token_invalido", "", nil)
	})
	jar := newJar()
	jar.set("u", "access_token", "VIEJO")
	c := newClient(t, srv, ca, jar)
	if _, err := c.Submit(context.Background(), "u", submitReq()); !IsInvalidToken(err) {
		t.Fatal(err)
	}
	if _, ok := jar.get("u", "access_token"); ok {
		t.Fatal("el borrado de Python debe aplicarse aunque la respuesta sea un error")
	}
}

func TestNoRedirectsFollowed(t *testing.T) {
	var leaked bool
	other, _ := newTLS(t, func(w http.ResponseWriter, r *http.Request) { leaked = true })
	srv, ca := newTLS(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect) })
	c := newClient(t, srv, ca, newJar())
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("una redirección debe ser error")
	}
	if leaked {
		t.Fatal("el cliente siguió una redirección: filtraría la clave")
	}
}

func TestHealthSendsNoCookies(t *testing.T) {
	srv, ca := newTLS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "Bearer "+testKey || r.URL.Path != "/internal/v1/health" {
			t.Errorf("health: cookie=%q auth=%q path=%s", r.Header.Get("Cookie"), r.Header.Get("Authorization"), r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"ok","brightspace":true}`))
	})
	jar := newJar()
	jar.set("u", "access_token", "X")
	h, err := newClient(t, srv, ca, jar).Health(context.Background())
	if err != nil || h.Status != "ok" || h.Brightspace == nil || !*h.Brightspace {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestParseSetCookie(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		line string
		remove, ok bool
		expires time.Duration
	}{
		{"access_token=abc; Max-Age=90.0; Path=/; HttpOnly", false, true, 90 * time.Second},
		{"access_token=abc; max-age=3600", false, true, time.Hour},
		{"access_token=abc; Max-Age=0", true, true, 0},
		{"access_token=abc; Max-Age=-5", true, true, 0},
		{"access_token=abc; Max-Age=-0.5", true, true, 0},
		{"access_token=; Max-Age=90", true, true, 0},
		{`access_token=""; Max-Age=90`, true, true, 0},
		{"access_token=abc; Expires=Wed, 21 Oct 2015 07:28:00 GMT", true, true, 0},
		{"access_token=abc; Expires=Thu, 12 Nov 2026 12:00:00 GMT", false, true, 24 * time.Hour},
		{"access_token=abc; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Max-Age=60", false, true, time.Minute}, // Max-Age manda
		{"access_token=abc", false, true, 0},
		{"sin-igual", false, false, 0},
		{"=x", false, false, 0},
	}
	for _, c := range cases {
		ck, remove, ok := parseSetCookie(c.line, now)
		if ok != c.ok || (ok && remove != c.remove) {
			t.Errorf("%q: remove=%v ok=%v", c.line, remove, ok)
			continue
		}
		if c.expires > 0 && (ck.ExpiresAt == nil || !ck.ExpiresAt.Equal(now.Add(c.expires))) {
			t.Errorf("%q: vence %v, quería +%v", c.line, ck.ExpiresAt, c.expires)
		}
		if c.expires == 0 && ok && !remove && ck.ExpiresAt != nil {
			t.Errorf("%q: cookie de sesión no debería vencer", c.line)
		}
	}
}

func TestCookieHeaderSkipsExpiredAndUnsafe(t *testing.T) {
	c := &Client{allowed: map[string]bool{"a": true, "b": true, "c": true, "d": true}}
	past := time.Now().Add(-time.Minute)
	h := c.cookieHeader([]Cookie{
		{Name: "a", Value: "ok"}, {Name: "b", Value: "x; evil=1"}, {Name: "c", Value: "viejo", ExpiresAt: &past},
		{Name: "d", Value: "con\r\nsalto"}, {Name: "zzz", Value: "no-permitida"},
	})
	if h != "a=ok" {
		t.Fatalf("cabecera: %q", h)
	}
}
