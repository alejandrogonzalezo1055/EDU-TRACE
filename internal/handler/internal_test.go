package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marlon/edu-trace/internal/auth"
	"github.com/marlon/edu-trace/internal/compiler"
	"github.com/marlon/edu-trace/internal/config"
)

const testKey = "k3y-de-prueba-con-mas-de-treinta-y-dos-bytes!!"

type env struct {
	t *testing.T
	store *auth.Store
	cfg *config.Config
	router http.Handler
}

func newEnv(t *testing.T, mutate ...func(*config.Config)) *env {
	t.Helper()
	store, err := auth.OpenStore(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	t.Setenv("D2L_ROLE_MAP", "")
	cfg := config.Load()
	cfg.InternalAPIKey = testKey
	cfg.PublicURL = "http://localhost:3000"
	cfg.AllowedOrigins = "http://localhost:3000"
	for _, m := range mutate {
		m(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	drafts := NewDraftQueue(store, nil, time.Second)
	return &env{t: t, store: store, cfg: cfg, router: NewRouter(cfg, compiler.New(cfg), nil, store, drafts, false, nil)}
}

func (e *env) do(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// call hace una llamada interna autenticada y decodifica el JSON de la respuesta.
func (e *env) call(method, path, body string, want int) map[string]any {
	e.t.Helper()
	rec := e.do(method, path, body, "Authorization", "Bearer "+testKey, "Content-Type", "application/json")
	if rec.Code != want {
		e.t.Fatalf("%s %s: %d (se esperaba %d): %s", method, path, rec.Code, want, rec.Body)
	}
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			e.t.Fatalf("%s %s: respuesta no es un objeto JSON: %s", method, path, rec.Body)
		}
	}
	return out
}

func (e *env) failure(method, path, body string, want int, code string) {
	e.t.Helper()
	out := e.call(method, path, body, want)
	if out["code"] != code || out["error"] == "" {
		e.t.Fatalf("%s %s: error %v (código esperado %q)", method, path, out, code)
	}
}

func TestInternalRequiresKey(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/internal/v1/health"},
		{"PUT", "/internal/v1/users/d2l:user:1"},
		{"PUT", "/internal/v1/users/d2l:user:1/enrollments"},
		{"PUT", "/internal/v1/courses/d2l:ou:1/roster"},
		{"PUT", "/internal/v1/courses/d2l:ou:1/assignments"},
		{"POST", "/internal/v1/session-tickets"},
		{"POST", "/internal/v1/sessions/revoke"},
		{"GET", "/internal/v1/no-existe"},
	} {
		for _, auth := range []string{"", "Bearer", "Bearer mala", "Bearer " + testKey + "x", "Basic " + testKey, testKey} {
			rec := e.do(c.method, c.path, `{}`, "Authorization", auth, "Origin", "http://evil.example")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s con %q: %d", c.method, c.path, auth, rec.Code)
			}
			var body map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body["code"] != "no_autorizado" || strings.Contains(rec.Body.String(), testKey) {
				t.Fatalf("el 401 debe ser genérico: %s", rec.Body)
			}
		}
		if rec := e.do(c.method, c.path, `{}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s sin cabecera: %d", c.method, c.path, rec.Code)
		}
	}
	// La clave correcta funciona aunque traiga un Origin ajeno: esta ruta no depende del filtro de Origin.
	rec := e.do("GET", "/internal/v1/health", "", "Authorization", "Bearer "+testKey, "Origin", "http://evil.example")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("health: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("las rutas internas no deben emitir cabeceras CORS")
	}
}

func TestInternalDisabledWithoutKey(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.InternalAPIKey = "" })
	if rec := e.do("GET", "/internal/v1/health", "", "Authorization", "Bearer "+testKey); rec.Code != http.StatusNotFound {
		t.Fatalf("sin INTERNAL_API_KEY las rutas internas no existen: %d", rec.Code)
	}
	short := config.Load()
	short.InternalAPIKey = strings.Repeat("a", 31)
	if err := short.Validate(); err == nil {
		t.Fatal("una clave de 31 bytes debe rechazarse al arrancar")
	}
	short.InternalAPIKey = strings.Repeat("a", 32)
	if err := short.Validate(); err != nil {
		t.Fatalf("clave de 32 bytes: %v", err)
	}
}

func TestInternalPutUser(t *testing.T) {
	e := newEnv(t)
	out := e.call("PUT", "/internal/v1/users/d2l:user:3101", `{"name":"Camila Díaz","email":"camila.diaz@uni.example","role_hint":"student"}`, http.StatusCreated)
	if out["created"] != true || out["external_id"] != "d2l:user:3101" || out["role"] != "student" || out["email"] != "camila.diaz@uni.example" ||
		out["name"] != "Camila Díaz" || len(out["id"].(string)) != 24 || out["consent_at"] != nil || out["consent_outdated"] != false {
		t.Fatalf("alta: %v", out)
	}
	again := e.call("PUT", "/internal/v1/users/d2l:user:3101", `{"name":"Camila Díaz R.","email":"camila.diaz@uni.example"}`, http.StatusOK)
	if again["created"] != false || again["id"] != out["id"] || again["name"] != "Camila Díaz R." {
		t.Fatalf("repetido: %v", again)
	}
	// JSON crudo de whoami: número sin prefijo en la URL, correo en UniqueName, campos desconocidos ignorados.
	raw := e.call("PUT", "/internal/v1/users/3102", `{"Identifier":"3102","FirstName":"Luis","LastName":"Pérez","UniqueName":"luis.perez@uni.example","Otro":{"x":1}}`, http.StatusCreated)
	if raw["external_id"] != "d2l:user:3102" || raw["name"] != "Luis Pérez" || raw["email"] != "luis.perez@uni.example" {
		t.Fatalf("whoami crudo: %v", raw)
	}
	// El correo explícito gana sobre UniqueName.
	raw = e.call("PUT", "/internal/v1/users/3103", `{"Identifier":3103,"FirstName":"A","LastName":"B","UniqueName":"usuario","Email":"ab@uni.example"}`, http.StatusCreated)
	if raw["email"] != "ab@uni.example" {
		t.Fatalf("correo explícito: %v", raw)
	}
	e.failure("PUT", "/internal/v1/users/3104", `{"Identifier":"3104","FirstName":"Sin","LastName":"Correo","UniqueName":"sincorreo"}`, http.StatusUnprocessableEntity, "correo_no_disponible")
	e.failure("PUT", "/internal/v1/users/3105", `{"name":"X Y"}`, http.StatusUnprocessableEntity, "correo_no_disponible")
	e.failure("PUT", "/internal/v1/users/3101", `{"Identifier":"999","name":"X Y","email":"x@uni.example"}`, http.StatusBadRequest, "identificador_no_coincide")
	e.failure("PUT", "/internal/v1/users/3106", `no es json`, http.StatusBadRequest, "json_invalido")
	e.failure("PUT", "/internal/v1/users/3106", `{"email":"x@uni.example"}`, http.StatusBadRequest, "datos_invalidos")

	// Correo de una cuenta local: 409 y no se fusiona.
	if _, err := e.store.CreateUser("Local", "local@u.edu", "password123", auth.RoleStudent); err != nil {
		t.Fatal(err)
	}
	e.failure("PUT", "/internal/v1/users/3200", `{"name":"Otro Nombre","email":"local@u.edu"}`, http.StatusConflict, "correo_en_uso")
	// Y la cuenta externa no entra por el login local, que ya no existe.
	if rec := e.do("POST", "/api/auth/login", `{"email":"camila.diaz@uni.example","password":""}`, "Content-Type", "application/json"); rec.Code != http.StatusGone {
		t.Fatalf("login local: %d", rec.Code)
	}
}

const myEnrollments = `{
 "PagingInfo": {"Bookmark": "191743", "HasMoreItems": false},
 "Items": [
  {"OrgUnit": {"Id": 6940, "Type": {"Id": 1, "Code": "Organization", "Name": "Organización"}, "Name": "U Javeriana Bogota", "Code": "U Javeriana Bogota"},
   "Access": {"IsActive": true, "StartDate": null, "EndDate": null, "CanAccess": true, "ClasslistRoleName": "Profesor", "LISRoles": ["urn:lti:instrole:ims/lis/Instructor"]}},
  {"OrgUnit": {"Id": 191743, "Type": {"Id": 3, "Code": "Course Offering", "Name": "Oferta de cursos"}, "Name": "Curso EduTrace", "Code": "2630_Curso_EduTrace"},
   "Access": {"IsActive": true, "StartDate": "2026-07-01T05:01:00.000Z", "EndDate": "2027-01-01T04:59:59.000Z", "CanAccess": true, "ClasslistRoleName": "Profesor", "LISRoles": ["urn:lti:instrole:ims/lis/Faculty", "urn:lti:instrole:ims/lis/Instructor"]}},
  {"OrgUnit": {"Id": 191744, "Type": {"Id": 3, "Code": "Course Offering"}, "Name": "", "Code": "2630_Respaldo"},
   "Access": {"IsActive": true, "CanAccess": true, "ClasslistRoleName": "Rol Raro", "LISRoles": ["urn:lti:instrole:ims/lis/Learner"]}},
  {"OrgUnit": {"Id": 191745, "Type": {"Id": 3, "Code": "Course Offering"}, "Name": "Curso sin rol"},
   "Access": {"IsActive": true, "CanAccess": true, "ClasslistRoleName": "Invitado", "LISRoles": []}},
  {"OrgUnit": {"Id": 191746, "Type": {"Id": 3, "Code": "Course Offering"}, "Name": "Curso bloqueado"},
   "Access": {"IsActive": true, "CanAccess": false, "ClasslistRoleName": "Estudiante"}}
 ]}`

func TestInternalEnrollments(t *testing.T) {
	e := newEnv(t)
	e.call("PUT", "/internal/v1/users/d2l:user:1", `{"name":"Elena Torres","email":"elena@uni.example"}`, http.StatusCreated)

	out := e.call("PUT", "/internal/v1/users/1/enrollments", myEnrollments, http.StatusOK)
	if out["courses_created"] != float64(3) || out["enrolled"] != float64(2) || out["deactivated"] != float64(0) {
		t.Fatalf("myenrollments crudo: %v", out)
	}
	groups := out["groups"].([]any)
	if len(groups) != 3 || groups[0].(map[string]any)["course_external_id"] != "d2l:ou:191743" {
		t.Fatalf("groups: %v", groups)
	}
	ignored := out["ignored"].([]any)
	if len(ignored) != 2 {
		t.Fatalf("ignoradas: %v", ignored)
	}
	reasons := ""
	for _, i := range ignored {
		reasons += i.(map[string]any)["reason"].(string) + "|"
	}
	if !strings.Contains(reasons, "oferta de curso") || !strings.Contains(reasons, "Invitado") {
		t.Fatalf("motivos: %s", reasons)
	}
	// "Rol Raro" se rescata por LISRoles ".../Learner" (estudiante) y el nombre cae a Code.
	if u := e.call("PUT", "/internal/v1/users/d2l:user:1", `{"name":"Elena Torres","email":"elena@uni.example"}`, http.StatusOK); u["role"] != "teacher" {
		t.Fatalf("rol principal: %v", u)
	}

	// Formato del contrato: instantánea completa; lo que no viene se desactiva (sin borrar).
	out = e.call("PUT", "/internal/v1/users/d2l:user:1/enrollments", `{"enrollments":[
		{"course":{"external_id":"d2l:ou:191743","name":"Curso EduTrace","start_date":"2026-07-01T00:00:00Z","end_date":null,"is_active":true},"role":"teacher"}]}`, http.StatusOK)
	if out["enrolled"] != float64(1) || out["deactivated"] != float64(1) || out["courses_created"] != float64(0) {
		t.Fatalf("contrato: %v", out)
	}
	// Protección: nada válido tras filtrar y el usuario tenía inscripciones => 422, no se desactiva todo.
	e.failure("PUT", "/internal/v1/users/d2l:user:1/enrollments", `{"Items":[{"OrgUnit":{"Id":6940,"Type":{"Id":1}},"Access":{"ClasslistRoleName":"Profesor"}}]}`, http.StatusUnprocessableEntity, "instantanea_vacia")
	e.failure("PUT", "/internal/v1/users/d2l:user:1/enrollments", `{"enrollments":[]}`, http.StatusUnprocessableEntity, "instantanea_vacia")
	e.failure("PUT", "/internal/v1/users/d2l:user:1/enrollments", `{"algo":1}`, http.StatusBadRequest, "datos_invalidos")
	e.failure("PUT", "/internal/v1/users/d2l:user:404/enrollments", `{"enrollments":[]}`, http.StatusNotFound, "usuario_no_encontrado")
	e.failure("PUT", "/internal/v1/users/d2l:user:1/enrollments", `{"enrollments":[{"course":{"external_id":"d2l:ou:5","name":"X"},"role":"rector"}]}`, http.StatusBadRequest, "datos_invalidos")
}

func TestInternalRosterAndAssignments(t *testing.T) {
	e := newEnv(t)
	e.call("PUT", "/internal/v1/users/d2l:user:1", `{"name":"Elena Torres","email":"elena@uni.example"}`, http.StatusCreated)
	e.call("PUT", "/internal/v1/users/d2l:user:1/enrollments", `{"enrollments":[{"course":{"external_id":"d2l:ou:6650","name":"Intro","is_active":true},"role":"teacher"}]}`, http.StatusOK)

	e.failure("PUT", "/internal/v1/courses/d2l:ou:6650/roster", `{"members":[]}`, http.StatusUnprocessableEntity, "lista_vacia")
	e.failure("PUT", "/internal/v1/courses/d2l:ou:404/roster", `{"members":[{"user":{"external_id":"d2l:user:2","name":"A","email":"a@uni.example"},"role":"student"}]}`, http.StatusNotFound, "curso_no_encontrado")

	// Contrato.
	out := e.call("PUT", "/internal/v1/courses/d2l:ou:6650/roster", `{"members":[
		{"user":{"external_id":"d2l:user:1","name":"Elena Torres","email":"elena@uni.example"},"role":"teacher","is_active":true},
		{"user":{"external_id":"d2l:user:2","name":"Ana A","email":"ana@uni.example"},"role":"student","is_active":true},
		{"user":{"external_id":"d2l:user:3","name":"Beto B","email":"beto@uni.example"},"role":"student","is_active":true}]}`, http.StatusOK)
	if out["created_users"] != float64(2) || out["enrolled"] != float64(3) || out["deactivated"] != float64(0) {
		t.Fatalf("roster del contrato: %v", out)
	}
	// classlist crudo (arreglo suelto): Identifier, DisplayName, Email, RoleName; los roles raros se ignoran.
	out = e.call("PUT", "/internal/v1/courses/6650/roster", `[
		{"Identifier":"1","DisplayName":"Elena Torres","Email":"elena@uni.example","RoleName":"Profesor"},
		{"Identifier":"2","DisplayName":"Ana A","Email":"ana@uni.example","RoleName":"Estudiante"},
		{"Identifier":"3","DisplayName":"Beto B","Email":"beto@uni.example","RoleName":"Student"},
		{"Identifier":"4","DisplayName":"Carla C","Email":"carla@uni.example","RoleName":"Alumno"},
		{"Identifier":"5","DisplayName":"Visita V","Email":"visita@uni.example","RoleName":"Invitado"}]`, http.StatusOK)
	if out["created_users"] != float64(1) || out["enrolled"] != float64(4) || len(out["ignored"].([]any)) != 1 {
		t.Fatalf("classlist crudo: %v", out)
	}
	// Desactivar 2 de 4 (50 %) supera el 30 % por defecto.
	e.failure("PUT", "/internal/v1/courses/d2l:ou:6650/roster", `{"members":[{"user":{"external_id":"d2l:user:1","name":"Elena Torres","email":"elena@uni.example"},"role":"teacher"},{"user":{"external_id":"d2l:user:2","name":"Ana A","email":"ana@uni.example"},"role":"student"}]}`, http.StatusUnprocessableEntity, "desactivacion_masiva")

	// Talleres: contrato.
	out = e.call("PUT", "/internal/v1/courses/d2l:ou:6650/assignments", `{"assignments":[
		{"external_id":"d2l:folder:701","title":"Taller 1: Ciclos","statement":"Escribe un programa que...","due_at":"2026-11-20T04:59:00Z","end_at":"2026-11-22T04:59:00Z","score_denominator":100,"grade_item_id":"d2l:grade:5001","is_hidden":false}]}`, http.StatusOK)
	if out["created"] != float64(1) || out["updated"] != float64(0) || out["hidden"] != float64(0) {
		t.Fatalf("talleres del contrato: %v", out)
	}
	// dropbox/folders crudo (arreglo suelto); solo Html => se quitan las etiquetas; el 701 desaparece => se oculta.
	out = e.call("PUT", "/internal/v1/courses/6650/assignments", `[
		{"Id":702,"Name":"Taller 2","CustomInstructions":{"Text":"","Html":"<p>Lee <b>n</b> y suma &amp; muestra</p><p>Fin</p>"},
		 "DueDate":"2026-12-01T04:59:00.000Z","Availability":{"StartDate":null,"EndDate":"2026-12-03T04:59:00.000Z"},
		 "Assessment":{"ScoreDenominator":50,"GradeItemId":5002},"IsHidden":false}]`, http.StatusOK)
	if out["created"] != float64(1) || out["updated"] != float64(0) || out["hidden"] != float64(1) {
		t.Fatalf("dropbox crudo: %v", out)
	}
	// El estudiante ve solo el 702, con el enunciado ya en texto plano.
	got := ""
	for _, a := range e.store.StudentAssignments(mustUserID(t, e, "d2l:user:2")) {
		got += a.Title + "=" + a.Statement + "|"
	}
	if got != "Taller 2=Lee n y suma & muestra\nFin|" {
		t.Fatalf("talleres del estudiante: %q", got)
	}
	e.failure("PUT", "/internal/v1/courses/d2l:ou:404/assignments", `{"assignments":[]}`, http.StatusNotFound, "curso_no_encontrado")
	e.failure("PUT", "/internal/v1/courses/d2l:ou:6650/assignments", `{"assignments":[{"external_id":"d2l:folder:9","title":""}]}`, http.StatusBadRequest, "datos_invalidos")
	e.failure("PUT", "/internal/v1/courses/d2l:ou:6650/assignments", `{"nada":1}`, http.StatusBadRequest, "datos_invalidos")
}

func mustUserID(t *testing.T, e *env, ext string) string {
	t.Helper()
	tok, _, err := e.store.IssueSessionTicket(ext)
	if err != nil {
		t.Fatal(err)
	}
	u, err := e.store.ConsumeSessionTicket(tok)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	return nil
}

func TestSessionTicketFlow(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.CookieSecure = true }) // como cuando Go sirve HTTPS
	e.call("PUT", "/internal/v1/users/d2l:user:3101", `{"name":"Camila Díaz","email":"camila@uni.example"}`, http.StatusCreated)

	e.failure("POST", "/internal/v1/session-tickets", `{"user_external_id":"d2l:user:404"}`, http.StatusNotFound, "usuario_no_encontrado")
	e.failure("POST", "/internal/v1/session-tickets", `{}`, http.StatusBadRequest, "datos_invalidos")
	out := e.call("POST", "/internal/v1/session-tickets", `{"user_external_id":"d2l:user:3101"}`, http.StatusOK)
	ticket, _ := out["ticket"].(string)
	if len(ticket) != 64 || out["expires_in"] != float64(60) {
		t.Fatalf("ticket: %v", out)
	}

	rec := e.do("GET", "/api/auth/session?ticket="+ticket, "")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "http://localhost:3000" {
		t.Fatalf("canje: %d %v", rec.Code, rec.Header())
	}
	c := sessionCookie(rec)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Value == "" || c.Path != "/" {
		t.Fatalf("cookie: %+v", c)
	}
	me := e.do("GET", "/api/auth/me", "", "Cookie", auth.CookieName+"="+c.Value)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), "camila@uni.example") {
		t.Fatalf("me: %d %s", me.Code, me.Body)
	}

	// Segundo uso del mismo ticket: 401, sin cookie y mandando a /login.
	rec = e.do("GET", "/api/auth/session?ticket="+ticket, "")
	if rec.Code != http.StatusUnauthorized || sessionCookie(rec) != nil || rec.Header().Get("Location") != "http://localhost:3000/login?error=invalid_ticket" ||
		!strings.Contains(rec.Header().Get("Refresh"), "/login") || !strings.Contains(rec.Body.String(), "/login") {
		t.Fatalf("ticket usado: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	for _, bad := range []string{"", "?ticket=", "?ticket=inventado", "?ticket=" + strings.Repeat("0", 64)} {
		if rec := e.do("GET", "/api/auth/session"+bad, ""); rec.Code != http.StatusUnauthorized || sessionCookie(rec) != nil {
			t.Fatalf("ticket %q: %d", bad, rec.Code)
		}
	}

	// Vencido.
	e.store.SetTicketTTL(time.Millisecond)
	out = e.call("POST", "/internal/v1/session-tickets", `{"user_external_id":"d2l:user:3101"}`, http.StatusOK)
	time.Sleep(10 * time.Millisecond)
	if rec := e.do("GET", "/api/auth/session?ticket="+out["ticket"].(string), ""); rec.Code != http.StatusUnauthorized || sessionCookie(rec) != nil ||
		rec.Header().Get("Location") != "http://localhost:3000/login?error=session_expired" {
		t.Fatalf("ticket vencido: %d %v", rec.Code, rec.Header())
	}
	e.store.SetTicketTTL(auth.TicketTTL)

	// Revocar: la sesión deja de servir y se cierran los tickets pendientes.
	pending := e.call("POST", "/internal/v1/session-tickets", `{"user_external_id":"3101"}`, http.StatusOK)["ticket"].(string)
	if rec := e.do("POST", "/internal/v1/sessions/revoke", `{"user_external_id":"d2l:user:3101"}`, "Authorization", "Bearer "+testKey); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if me := e.do("GET", "/api/auth/me", "", "Cookie", auth.CookieName+"="+c.Value); me.Code != http.StatusUnauthorized {
		t.Fatalf("me tras revocar: %d", me.Code)
	}
	if rec := e.do("GET", "/api/auth/session?ticket="+pending, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ticket pendiente tras revocar: %d", rec.Code)
	}
	e.failure("POST", "/internal/v1/sessions/revoke", `{"user_external_id":"d2l:user:404"}`, http.StatusNotFound, "usuario_no_encontrado")
}

func TestSessionCookieNotSecureWithoutTLS(t *testing.T) {
	e := newEnv(t)
	e.call("PUT", "/internal/v1/users/d2l:user:1", `{"name":"Camila Díaz","email":"camila@uni.example"}`, http.StatusCreated)
	tok := e.call("POST", "/internal/v1/session-tickets", `{"user_external_id":"d2l:user:1"}`, http.StatusOK)["ticket"].(string)
	c := sessionCookie(e.do("GET", "/api/auth/session?ticket="+tok, ""))
	if c == nil || c.Secure || !c.HttpOnly {
		t.Fatalf("en desarrollo sin TLS la cookie no es Secure: %+v", c)
	}
	// Con TLS configurado, Load fuerza CookieSecure.
	t.Setenv("TLS_CERT_FILE", "c.pem")
	t.Setenv("TLS_KEY_FILE", "k.pem")
	if cfg := config.Load(); !cfg.CookieSecure || !cfg.TLSEnabled() {
		t.Fatalf("TLS debe forzar cookies Secure: %+v", cfg)
	}
}

func TestPythonCookiesCapturedAndRemoved(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	e := newEnv(t, func(c *config.Config) { c.CookieJarKey = key })
	if err := e.store.SetCookieJarKey(key); err != nil {
		t.Fatal(err)
	}
	e.call("PUT", "/internal/v1/users/d2l:user:1", `{"name":"Camila Díaz","email":"camila@uni.example"}`, http.StatusCreated)
	tok := e.call("POST", "/internal/v1/session-tickets", `{"user_external_id":"d2l:user:1"}`, http.StatusOK)["ticket"].(string)
	rec := e.do("GET", "/api/auth/session?ticket="+tok, "", "Cookie", "access_token=AT; refresh_token=RT; auth_code=AC; response_time=1; otra=zzz")
	if rec.Code != http.StatusFound {
		t.Fatalf("canje: %d", rec.Code)
	}
	uid := mustUserID(t, e, "d2l:user:1")
	jar, err := e.store.LoadJar(uid)
	if err != nil || len(jar) != 4 {
		t.Fatalf("jar: %+v %v", jar, err)
	}
	got := map[string]string{}
	for _, c := range jar {
		got[c.Name] = c.Value
	}
	if got["access_token"] != "AT" || got["refresh_token"] != "RT" || got["auth_code"] != "AC" || got["response_time"] != "1" || got["otra"] != "" {
		t.Fatalf("cookies capturadas: %v", got)
	}

	// Logout borra el jar.
	cookie := sessionCookie(rec)
	if out := e.do("POST", "/api/auth/logout", "", "Cookie", auth.CookieName+"="+cookie.Value); out.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", out.Code)
	}
	if jar, _ := e.store.LoadJar(uid); len(jar) != 0 {
		t.Fatalf("jar tras logout: %+v", jar)
	}
	// Revocar también.
	if err := e.store.SaveJarCookie(uid, auth.JarCookie{Name: "access_token", Value: "x"}); err != nil {
		t.Fatal(err)
	}
	e.do("POST", "/internal/v1/sessions/revoke", `{"user_external_id":"d2l:user:1"}`, "Authorization", "Bearer "+testKey)
	if jar, _ := e.store.LoadJar(uid); len(jar) != 0 {
		t.Fatalf("jar tras revocar: %+v", jar)
	}
}

func TestConfigValidateJarKey(t *testing.T) {
	cfg := config.Load()
	cfg.PythonAPIKey = "algo"
	if err := cfg.Validate(); err == nil {
		t.Fatal("con PYTHON_API_KEY se exige COOKIE_JAR_KEY")
	}
	t.Setenv("COOKIE_JAR_KEY", "corta")
	if err := config.Load().Validate(); err == nil {
		t.Fatal("una COOKIE_JAR_KEY que no son 32 bytes en base64 debe rechazarse")
	}
	t.Setenv("COOKIE_JAR_KEY", "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	c := config.Load()
	c.PythonAPIKey = "algo"
	if err := c.Validate(); err != nil {
		t.Fatalf("clave válida: %v", err)
	}
	t.Setenv("D2L_ROLE_MAP", "teacher=Jefe;student=Pupilo")
	c = config.Load()
	if r, ok := c.MapRole("jefe", nil); !ok || r != "teacher" {
		t.Fatalf("D2L_ROLE_MAP: %v %v", r, ok)
	}
	if _, ok := c.MapRole("Profesor", nil); ok {
		t.Fatal("el mapa personalizado reemplaza al de fábrica")
	}
	t.Setenv("D2L_ROLE_MAP", "rector=X")
	if err := config.Load().Validate(); err == nil {
		t.Fatal("rol desconocido en D2L_ROLE_MAP")
	}
}

func TestRetiredLocalAuthRoutes(t *testing.T) {
	e := newEnv(t)
	for _, r := range []struct{ method, path string }{
		{"POST", "/api/auth/register"}, {"POST", "/api/auth/login"}, {"POST", "/api/auth/recover"},
		{"POST", "/api/auth/password"}, {"POST", "/api/auth/recovery-code"},
		{"POST", "/api/student/groups/join"}, {"POST", "/api/teacher/groups/g1/join-code"},
		{"POST", "/api/teacher/groups/g1/students/s1/reset-code"}, {"POST", "/api/teacher/courses"},
		{"POST", "/api/teacher/courses/c1/groups"}, {"PATCH", "/api/teacher/courses/c1"}, {"DELETE", "/api/teacher/courses/c1"},
		{"PATCH", "/api/teacher/groups/g1"}, {"DELETE", "/api/teacher/groups/g1"},
		{"DELETE", "/api/teacher/groups/g1/students/s1"}, {"POST", "/api/teacher/courses/c1/clone"},
		{"POST", "/api/teacher/courses/c1/teachers"}, {"DELETE", "/api/teacher/courses/c1/teachers/t1"},
		{"POST", "/api/teacher/groups/g1/assignments"}, {"DELETE", "/api/teacher/assignments/a1"},
	} {
		rec := e.do(r.method, r.path, `{}`, "Content-Type", "application/json")
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusGone || body["code"] != "funcion_retirada" || body["error"] == "" {
			t.Fatalf("%s %s: %d %s", r.method, r.path, rec.Code, rec.Body)
		}
	}
	// Lo que sigue igual: consentimiento, me y logout.
	for _, r := range []struct{ method, path string; want int }{
		{"GET", "/api/auth/me", http.StatusUnauthorized},
		{"POST", "/api/auth/consent", http.StatusUnauthorized},
		{"POST", "/api/auth/logout", http.StatusNoContent},
		{"GET", "/api/health", http.StatusOK},
	} {
		if rec := e.do(r.method, r.path, ""); rec.Code != r.want {
			t.Fatalf("%s %s: %d (se esperaba %d)", r.method, r.path, rec.Code, r.want)
		}
	}
}

func TestExternalAccountCannotDeleteWithPassword(t *testing.T) {
	e := newEnv(t)
	e.call("PUT", "/internal/v1/users/d2l:user:1", `{"name":"Camila Díaz","email":"camila@uni.example"}`, http.StatusCreated)
	tok := e.call("POST", "/internal/v1/session-tickets", `{"user_external_id":"d2l:user:1"}`, http.StatusOK)["ticket"].(string)
	c := sessionCookie(e.do("GET", "/api/auth/session?ticket="+tok, ""))
	rec := e.do("POST", "/api/auth/delete", `{"password":""}`, "Cookie", auth.CookieName+"="+c.Value, "Content-Type", "application/json")
	if rec.Code != http.StatusConflict {
		t.Fatalf("borrar cuenta sin contraseña local: %d %s", rec.Code, rec.Body)
	}
}
