package handler

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

	"github.com/marlon/edu-trace/internal/auth"
	"github.com/marlon/edu-trace/internal/compiler"
	"github.com/marlon/edu-trace/internal/config"
	"github.com/marlon/edu-trace/internal/pyclient"
)

const pyKey = "python-key-0123456789abcdef0123456789"

type pyCall struct {
	Path string
	Cookie string
	Auth string
	Body map[string]any
}

// fakePython es el servicio Python falso (HTTPS). respond decide qué contesta a cada llamada.
type fakePython struct {
	t *testing.T
	srv *httptest.Server
	ca string
	mu sync.Mutex
	calls []pyCall
	respond func(w http.ResponseWriter, c pyCall)
}

func newFakePython(t *testing.T) *fakePython {
	f := &fakePython{t: t}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := pyCall{Path: r.URL.Path, Cookie: r.Header.Get("Cookie"), Auth: r.Header.Get("Authorization"), Body: map[string]any{}}
		_ = json.NewDecoder(r.Body).Decode(&c.Body)
		if c.Auth != "Bearer "+pyKey {
			http.Error(w, `{"error":"clave"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		fn := f.respond
		f.mu.Unlock()
		if fn != nil {
			fn(w, c)
			return
		}
		ok(w, c)
	}))
	t.Cleanup(f.srv.Close)
	f.ca = filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(f.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}), 0o600)
	return f
}

func ok(w http.ResponseWriter, c pyCall) {
	switch c.Path {
	case "/internal/v1/dropbox/submit":
		_, _ = w.Write([]byte(`{"status":"enviada","external_id":"d2l:sub:9001"}`))
	case "/internal/v1/dropbox/feedback":
		_, _ = w.Write([]byte(`{"status":"publicada"}`))
	default:
		_, _ = w.Write([]byte(`{"status":"ok","brightspace":true}`))
	}
}

func pyFail(status int, code string, extra string) func(http.ResponseWriter, pyCall) {
	return func(w http.ResponseWriter, c pyCall) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		body := `{"error":"falla simulada","code":"` + code + `"` + extra + `}`
		_, _ = w.Write([]byte(body))
	}
}

func (f *fakePython) setRespond(fn func(http.ResponseWriter, pyCall)) {
	f.mu.Lock()
	f.respond = fn
	f.mu.Unlock()
}

func (f *fakePython) snapshot() []pyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pyCall(nil), f.calls...)
}

func (f *fakePython) client(store *auth.Store) *pyclient.Client {
	f.t.Helper()
	c, err := NewPythonClient(&config.Config{PythonBaseURL: f.srv.URL, PythonAPIKey: pyKey, PythonCAFile: f.ca,
		PythonCookieNames: []string{"auth_code", "access_token", "refresh_token", "response_time"}}, store)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

type d2lEnv struct {
	t *testing.T
	store *auth.Store
	py *fakePython
	q *D2LQueue
	teacher, student, other *auth.User
	groupID, assignmentID string
	clock time.Time
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newD2LEnv(t *testing.T, denominator *float64) *d2lEnv {
	t.Helper()
	store, err := auth.OpenStore(t.TempDir(), time.Hour)
	must(t, err)
	t.Cleanup(func() { store.Close() })
	must(t, store.SetCookieJarKey([]byte("0123456789abcdef0123456789abcdef")))
	store.SetD2LEnabled(true)
	e := &d2lEnv{t: t, store: store, py: newFakePython(t), clock: time.Now().UTC()}
	user := func(ext, name, email string) *auth.User {
		r, err := store.UpsertExternalUser(ext, name, email)
		must(t, err)
		return r.User
	}
	e.teacher = user("d2l:user:2205", "Elena Docente", "elena@uni.example")
	e.student = user("d2l:user:3101", "Camila Díaz", "camila@uni.example")
	e.other = user("d2l:user:3102", "Otro Estudiante", "otro@uni.example")
	course := auth.ExternalCourse{ExternalID: "d2l:ou:6650", Name: "Introducción", IsActive: true}
	if _, err := store.UpsertEnrollments("d2l:user:2205", []auth.ExternalEnrollment{{Course: course, Role: "teacher"}}); err != nil {
		t.Fatal(err)
	}
	var group string
	for _, who := range []string{"d2l:user:3101", "d2l:user:3102"} {
		r, err := store.UpsertEnrollments(who, []auth.ExternalEnrollment{{Course: course, Role: "student"}})
		must(t, err)
		group = r.Groups[0].GroupID
	}
	e.groupID = group
	due := time.Now().Add(48 * time.Hour)
	_, err = store.UpsertAssignments("d2l:ou:6650", []auth.ExternalAssignment{{ExternalID: "d2l:folder:701", Title: "Taller 1", DueAt: &due, ScoreDenominator: denominator}})
	must(t, err)
	list, err := store.GroupAssignments(e.teacher.ID, group)
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("talleres: %d", len(list))
	}
	e.assignmentID = list[0].ID
	e.q = NewD2LQueue(store, e.py.client(store))
	e.q.now = func() time.Time { return e.clock }
	return e
}

func (e *d2lEnv) login(u *auth.User, access string) {
	e.t.Helper()
	must(e.t, e.store.SaveJarCookie(u.ID, auth.JarCookie{Name: "access_token", Value: access, Path: "/"}))
	must(e.t, e.store.SaveJarCookie(u.ID, auth.JarCookie{Name: "refresh_token", Value: "R-" + access, Path: "/"}))
}

func (e *d2lEnv) submit(assignmentID, kind string) *auth.Submission {
	e.t.Helper()
	sub := &auth.Submission{SubmissionMeta: auth.SubmissionMeta{Kind: kind, GroupID: e.groupID, AssignmentID: assignmentID, StudentID: e.student.ID}, Code: "int main() { return 0; }"}
	must(e.t, e.store.AddSubmission(sub))
	return sub
}

func (e *d2lEnv) meta(id string) auth.StudentSubmission {
	e.t.Helper()
	for _, s := range e.store.StudentSubmissions(e.student.ID, 50) {
		if s.ID == id {
			return s
		}
	}
	e.t.Fatalf("la entrega %s no aparece", id)
	return auth.StudentSubmission{}
}

func (e *d2lEnv) returnReview(subID, feedback string, grade float64) *auth.Review {
	e.t.Helper()
	r, err := e.store.SaveTeacherReview(e.teacher.ID, subID, feedback, &grade, true, nil, nil)
	must(e.t, err)
	return r
}

func TestD2LSubmitSuccessUsesOnlyOwnerCookies(t *testing.T) {
	e := newD2LEnv(t, nil)
	e.login(e.student, "S-ACCESS")
	e.login(e.other, "O-ACCESS")
	e.py.setRespond(func(w http.ResponseWriter, c pyCall) {
		w.Header().Add("Set-Cookie", "access_token=S-NUEVO; Max-Age=90.0; Path=/; HttpOnly; Secure")
		ok(w, c)
	})

	sub := e.submit(e.assignmentID, auth.KindOfficial)
	if sub.BrightspaceStatus != "pendiente" || e.meta(sub.ID).BrightspaceStatus != "pendiente" {
		t.Fatalf("tras guardar debe quedar pendiente: %q", sub.BrightspaceStatus)
	}
	if !e.q.ProcessOne(context.Background()) {
		t.Fatal("la cola debía enviar la entrega")
	}
	calls := e.py.snapshot()
	if len(calls) != 1 || calls[0].Path != "/internal/v1/dropbox/submit" {
		t.Fatalf("llamadas: %+v", calls)
	}
	c := calls[0]
	if strings.Contains(c.Cookie, "O-ACCESS") || !strings.Contains(c.Cookie, "access_token=S-ACCESS") || !strings.Contains(c.Cookie, "refresh_token=R-S-ACCESS") {
		t.Fatalf("cookies enviadas: %q", c.Cookie)
	}
	want := map[string]any{"student_external_id": "d2l:user:3101", "course_external_id": "d2l:ou:6650", "assignment_external_id": "d2l:folder:701",
		"filename": "main.cpp", "content": sub.Code, "comment": "Entrega desde EduTrace", "go_submission_id": sub.ID}
	for k, v := range want {
		if c.Body[k] != v {
			t.Errorf("cuerpo[%s] = %v, quería %v", k, c.Body[k], v)
		}
	}
	m := e.meta(sub.ID)
	if m.BrightspaceStatus != "enviada" || m.BrightspaceError != "" {
		t.Fatalf("estado final: %+v", m.SubmissionMeta)
	}
	// El Set-Cookie fue al jar del estudiante, no al del otro.
	jar, _ := e.store.LoadJar(e.student.ID)
	var got string
	for _, ck := range jar {
		if ck.Name == "access_token" {
			got = ck.Value
		}
	}
	if got != "S-NUEVO" {
		t.Fatalf("jar del estudiante: %q", got)
	}
	if o, _ := e.store.LoadJar(e.other.ID); len(o) != 2 {
		t.Fatalf("jar ajeno alterado: %+v", o)
	}
	// Reenvío sin duplicar: ya no hay nada pendiente.
	if e.q.ProcessOne(context.Background()) || len(e.py.snapshot()) != 1 {
		t.Fatal("una entrega enviada no se vuelve a enviar")
	}
}

func TestD2LNotQueuedWhenNotApplicable(t *testing.T) {
	e := newD2LEnv(t, nil)
	e.login(e.student, "S")
	// Los intentos nunca van a Brightspace.
	if a := e.submit(e.assignmentID, auth.KindAttempt); a.BrightspaceStatus != "" {
		t.Fatalf("intento: %q", a.BrightspaceStatus)
	}
	// Entrega sin taller (práctica libre).
	if s := e.submit("", auth.KindOfficial); s.BrightspaceStatus != "" {
		t.Fatalf("sin taller: %q", s.BrightspaceStatus)
	}
	// Python no configurado: igual que antes, sin estado.
	e.store.SetD2LEnabled(false)
	sub := e.submit(e.assignmentID, auth.KindOfficial)
	if sub.BrightspaceStatus != "" || e.meta(sub.ID).BrightspaceStatus != "" {
		t.Fatalf("sin Python: %q", sub.BrightspaceStatus)
	}
	e.q.ProcessOne(context.Background())
	if n := len(e.py.snapshot()); n != 0 {
		t.Fatalf("no debía llamar a Python: %d llamadas", n)
	}
}

func TestD2LPythonNotConfigured(t *testing.T) {
	e := newD2LEnv(t, nil)
	store := e.store
	store.SetD2LEnabled(false)
	if c, err := NewPythonClient(&config.Config{}, store); c != nil || err != nil {
		t.Fatalf("sin PYTHON_BASE_URL no hay cliente ni error: %v %v", c, err)
	}
	q := NewD2LQueue(store, nil)
	done := make(chan struct{})
	go func() { q.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("el trabajador no debe correr sin Python")
	}
	if q.ProcessOne(context.Background()) {
		t.Fatal("ProcessOne sin cliente")
	}
	// Y las entregas siguen funcionando.
	e.submit(e.assignmentID, auth.KindOfficial)
	// Un Go sin Python rechaza http:// y la clave corta en la configuración.
	if _, err := NewPythonClient(&config.Config{PythonBaseURL: "http://localhost:1", PythonAPIKey: pyKey}, store); err == nil {
		t.Fatal("http:// debe rechazarse")
	}
}

func TestD2LPermanentErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		status int
		code string
	}{{"token", 502, "token_invalido"}, {"ventana", 409, "fuera_de_ventana"}, {"carpeta", 409, "carpeta_no_acepta"}, {"grande", 413, "muy_grande"}, {"401", 401, ""}, {"408", 408, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newD2LEnv(t, nil)
			e.login(e.student, "S")
			e.py.setRespond(pyFail(tc.status, tc.code, ""))
			sub := e.submit(e.assignmentID, auth.KindOfficial)
			e.q.ProcessOne(context.Background())
			m := e.meta(sub.ID)
			if m.BrightspaceStatus != "fallida" || m.BrightspaceError == "" {
				t.Fatalf("debe quedar fallida con mensaje: %+v", m.SubmissionMeta)
			}
			if e.q.ProcessOne(context.Background()) || len(e.py.snapshot()) != 1 {
				t.Fatal("un error definitivo no se reintenta")
			}
			if (tc.code == "token_invalido" || tc.code == "") && m.BrightspaceError != auth.D2LTokenMsg {
				t.Fatalf("token inválido debe pedir nuevo login: %q", m.BrightspaceError)
			}
			if tc.code == "token_invalido" || tc.code == "" {
				// Al volver a iniciar sesión (cookies nuevas) se reenvía sola.
				e.py.setRespond(nil)
				e.login(e.student, "S-NUEVO")
				must(t, e.store.WakeD2LFor(e.student.ID))
				if e.meta(sub.ID).BrightspaceStatus != "pendiente" || !e.q.ProcessOne(context.Background()) || e.meta(sub.ID).BrightspaceStatus != "enviada" {
					t.Fatalf("tras un nuevo login debía reenviarse: %+v", e.meta(sub.ID).SubmissionMeta)
				}
			}
		})
	}
}

func TestD2LRateLimitHonoursRetryAfter(t *testing.T) {
	e := newD2LEnv(t, nil)
	e.login(e.student, "S")
	e.py.setRespond(pyFail(503, "limite_tasa", `,"retry_after":600`))
	sub := e.submit(e.assignmentID, auth.KindOfficial)
	e.q.ProcessOne(context.Background())
	if m := e.meta(sub.ID); m.BrightspaceStatus != "pendiente" {
		t.Fatalf("limite_tasa debe quedar pendiente: %+v", m.SubmissionMeta)
	}
	// Python pidió 10 min (más que el 1 min del backoff): a los 9 min todavía no.
	e.clock = e.clock.Add(9 * time.Minute)
	if e.q.ProcessOne(context.Background()) {
		t.Fatal("no debía reintentar antes de retry_after")
	}
	e.py.setRespond(nil)
	e.clock = e.clock.Add(2 * time.Minute)
	if !e.q.ProcessOne(context.Background()) {
		t.Fatal("debía reintentar pasado retry_after")
	}
	if m := e.meta(sub.ID); m.BrightspaceStatus != "enviada" {
		t.Fatalf("tras el reintento: %+v", m.SubmissionMeta)
	}
}

func TestD2LBackoffAndAttemptCap(t *testing.T) {
	e := newD2LEnv(t, nil)
	e.login(e.student, "S")
	e.py.setRespond(pyFail(502, "brightspace_caido", ""))
	sub := e.submit(e.assignmentID, auth.KindOfficial)
	waits := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, wait := range waits {
		if !e.q.ProcessOne(context.Background()) {
			t.Fatalf("intento %d: no se procesó", i+1)
		}
		if m := e.meta(sub.ID); m.BrightspaceStatus != "pendiente" {
			t.Fatalf("intento %d: %+v", i+1, m.SubmissionMeta)
		}
		// Un segundo antes del plazo no se reintenta; al cumplirse, sí.
		e.clock = e.clock.Add(wait - time.Second)
		if e.q.ProcessOne(context.Background()) {
			t.Fatalf("intento %d: reintentó antes de %v", i+1, wait)
		}
		e.clock = e.clock.Add(time.Second)
	}
	if !e.q.ProcessOne(context.Background()) {
		t.Fatal("quinto intento")
	}
	m := e.meta(sub.ID)
	if m.BrightspaceStatus != "fallida" || !strings.Contains(m.BrightspaceError, "intentos") {
		t.Fatalf("tras 5 intentos debe quedar fallida: %+v", m.SubmissionMeta)
	}
	e.clock = e.clock.Add(24 * time.Hour)
	if e.q.ProcessOne(context.Background()) || len(e.py.snapshot()) != 5 {
		t.Fatalf("no más de 5 intentos: %d", len(e.py.snapshot()))
	}
}

func TestD2LReviewScoreConversionAndRepublish(t *testing.T) {
	e := newD2LEnv(t, nil) // sin score_denominator: 100 por defecto
	e.login(e.teacher, "T-ACCESS")
	e.login(e.student, "S-ACCESS")
	sub := e.submit(e.assignmentID, auth.KindOfficial)
	e.q.ProcessOne(context.Background()) // entrega

	rev := e.returnReview(sub.ID, "Buen trabajo con los ciclos", 4.2)
	if rev.BrightspacePublished == nil || *rev.BrightspacePublished {
		t.Fatalf("recién devuelta: debe estar sin publicar: %+v", rev.BrightspacePublished)
	}
	if !e.q.ProcessOne(context.Background()) {
		t.Fatal("la cola debía publicar")
	}
	calls := e.py.snapshot()
	c := calls[len(calls)-1]
	if c.Path != "/internal/v1/dropbox/feedback" {
		t.Fatalf("ruta: %s", c.Path)
	}
	if c.Body["score"].(float64) != 84.0 || c.Body["score_denominator"].(float64) != 100 || c.Body["is_graded"] != true ||
		c.Body["go_review_id"] != sub.ID || c.Body["teacher_external_id"] != "d2l:user:2205" || c.Body["student_external_id"] != "d2l:user:3101" ||
		c.Body["feedback"] != "Buen trabajo con los ciclos" || c.Body["course_external_id"] != "d2l:ou:6650" || c.Body["assignment_external_id"] != "d2l:folder:701" {
		t.Fatalf("cuerpo: %v", c.Body)
	}
	if !strings.Contains(c.Cookie, "T-ACCESS") || strings.Contains(c.Cookie, "S-ACCESS") {
		t.Fatalf("debe usar las cookies del docente: %q", c.Cookie)
	}
	got, err := e.store.ReviewForTeacher(e.teacher.ID, sub.ID)
	must(t, err)
	if got.BrightspacePublished == nil || !*got.BrightspacePublished || got.BrightspacePublishedAt == nil || got.BrightspaceError != "" {
		t.Fatalf("revisión del docente: %+v", got)
	}
	if sr := e.meta(sub.ID).Review; sr == nil || sr.BrightspacePublished == nil || !*sr.BrightspacePublished {
		t.Fatalf("revisión del estudiante: %+v", sr)
	}

	// El docente re-edita: vuelve a publicar todo (reemplazo completo).
	e.returnReview(sub.ID, "Corregí un detalle", 5)
	if sr := e.meta(sub.ID).Review; sr.BrightspacePublished == nil || *sr.BrightspacePublished {
		t.Fatal("tras re-editar queda pendiente de publicar")
	}
	e.q.ProcessOne(context.Background())
	calls = e.py.snapshot()
	c = calls[len(calls)-1]
	if c.Body["score"].(float64) != 100 || c.Body["feedback"] != "Corregí un detalle" || c.Body["go_review_id"] != sub.ID {
		t.Fatalf("segunda publicación: %v", c.Body)
	}
	// Guardar sin cambios y sin devolver no vuelve a publicar.
	before := len(e.py.snapshot())
	_, err = e.store.SaveTeacherReview(e.teacher.ID, sub.ID, "Corregí un detalle", ptr(5.0), false, nil, nil)
	must(t, err)
	e.q.ProcessOne(context.Background())
	if len(e.py.snapshot()) != before {
		t.Fatal("sin cambios no se republica")
	}
}

func ptr(f float64) *float64 { return &f }

func TestD2LReviewUsesScoreDenominator(t *testing.T) {
	e := newD2LEnv(t, ptr(20))
	e.login(e.teacher, "T")
	e.login(e.student, "S")
	sub := e.submit(e.assignmentID, auth.KindOfficial)
	e.q.ProcessOne(context.Background())
	e.returnReview(sub.ID, "ok", 3.3)
	e.q.ProcessOne(context.Background())
	calls := e.py.snapshot()
	if c := calls[len(calls)-1]; c.Body["score"].(float64) != 13.2 || c.Body["score_denominator"].(float64) != 20 {
		t.Fatalf("3.3 sobre 20: %v", c.Body)
	}
}

func TestD2LReviewWithoutTeacherSessionStaysPending(t *testing.T) {
	e := newD2LEnv(t, nil)
	e.login(e.student, "S")
	sub := e.submit(e.assignmentID, auth.KindOfficial)
	e.q.ProcessOne(context.Background())
	// El docente no tiene cookies guardadas: la devolución local funciona igual.
	rev := e.returnReview(sub.ID, "ok", 4)
	if rev.Status != auth.ReviewReturned {
		t.Fatal("la devolución local no debe fallar")
	}
	e.q.ProcessOne(context.Background())
	got, err := e.store.ReviewForTeacher(e.teacher.ID, sub.ID)
	must(t, err)
	if got.BrightspacePublished == nil || *got.BrightspacePublished || !strings.Contains(got.BrightspaceError, "sesión") {
		t.Fatalf("debe quedar pendiente con motivo: %+v %q", got.BrightspacePublished, got.BrightspaceError)
	}
	if n := len(e.py.snapshot()); n != 1 { // solo la entrega; ninguna llamada de feedback
		t.Fatalf("no debía llamar a Python sin cookies: %d", n)
	}
	// No gasta intentos y no se reintenta en bucle...
	if e.q.ProcessOne(context.Background()) {
		t.Fatal("no debe reintentar sin cookies hasta que pase la espera")
	}
	// ...y al volver a iniciar sesión el docente se adelanta solo.
	e.login(e.teacher, "T-NUEVO")
	must(t, e.store.WakeD2LFor(e.teacher.ID))
	if !e.q.ProcessOne(context.Background()) {
		t.Fatal("con cookies nuevas debía publicarse")
	}
	got, _ = e.store.ReviewForTeacher(e.teacher.ID, sub.ID)
	if !*got.BrightspacePublished {
		t.Fatalf("debía quedar publicada: %+v", got)
	}
}

func TestD2LStaleResultDoesNotOverwriteReEdit(t *testing.T) {
	e := newD2LEnv(t, nil)
	e.login(e.teacher, "T")
	e.login(e.student, "S")
	sub := e.submit(e.assignmentID, auth.KindOfficial)
	e.q.ProcessOne(context.Background())
	e.returnReview(sub.ID, "versión 1", 3)
	// Mientras Python atiende la primera publicación, el docente re-edita.
	var once sync.Once
	e.py.setRespond(func(w http.ResponseWriter, c pyCall) {
		once.Do(func() { e.returnReview(sub.ID, "versión 2", 4) })
		ok(w, c)
	})
	e.q.ProcessOne(context.Background())
	got, _ := e.store.ReviewForTeacher(e.teacher.ID, sub.ID)
	if *got.BrightspacePublished {
		t.Fatal("la versión 2 aún no se publicó: no puede figurar como publicada")
	}
	e.q.ProcessOne(context.Background())
	calls := e.py.snapshot()
	if last := calls[len(calls)-1]; last.Body["feedback"] != "versión 2" {
		t.Fatalf("debía publicarse la versión 2: %v", last.Body)
	}
	if got, _ = e.store.ReviewForTeacher(e.teacher.ID, sub.ID); !*got.BrightspacePublished {
		t.Fatal("al final debe quedar publicada")
	}
}

func TestD2LNoStateWithoutMirror(t *testing.T) {
	// Un taller sin external_id (local) nunca expone campos de Brightspace.
	e := newD2LEnv(t, nil)
	e.login(e.teacher, "T")
	sub := e.submit("", auth.KindOfficial)
	rev := e.returnReview(sub.ID, "ok", 4)
	raw, _ := json.Marshal(rev)
	if strings.Contains(string(raw), "brightspace") {
		t.Fatalf("revisión local con campos de Brightspace: %s", raw)
	}
	raw, _ = json.Marshal(e.meta(sub.ID))
	if strings.Contains(string(raw), "brightspace") {
		t.Fatalf("entrega local con campos de Brightspace: %s", raw)
	}
}

func TestHealthPythonCheckNeverDegrades(t *testing.T) {
	e := newD2LEnv(t, nil)
	mk := func(py *pyclient.Client) map[string]any {
		cfg := config.Load()
		cfg.DataDir = t.TempDir()
		drafts := NewDraftQueue(e.store, nil, time.Second)
		router := NewRouter(cfg, compiler.New(cfg), nil, e.store, drafts, false, py)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
		out := map[string]any{}
		must(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	base := mk(nil)
	if _, has := base["checks"].(map[string]any)["python"]; has {
		t.Fatal("sin Python no hay comprobación «python»")
	}
	up := mk(e.q.client)
	if up["checks"].(map[string]any)["python"] != true || up["status"] != base["status"] {
		t.Fatalf("Python arriba: %v (base %v)", up, base)
	}
	e.py.srv.Close()
	down := mk(pyclient2(t, e))
	if down["checks"].(map[string]any)["python"] != false || down["status"] != base["status"] {
		t.Fatalf("Python caído no debe cambiar el estado: %v (base %v)", down, base)
	}
}

// pyclient2 arma un cliente nuevo hacia el servidor ya cerrado (la caché del anterior seguiría diciendo true).
func pyclient2(t *testing.T, e *d2lEnv) *pyclient.Client {
	return e.py.client(e.store)
}
