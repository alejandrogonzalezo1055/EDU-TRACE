package handler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/marlon/edu-trace/internal/auth"
	"github.com/marlon/edu-trace/internal/config"
	"github.com/marlon/edu-trace/internal/pyclient"
)

const (
	d2lIdleCheck = 10 * time.Second
	d2lNoSessionWait = 30 * time.Minute
	d2lNoSessionGiveUp = 7 * 24 * time.Hour
	d2lFilename = "main.cpp"
	d2lComment = "Entrega desde EduTrace"
	d2lDefaultDenominator = 100
	pythonHealthCacheFor = 30 * time.Second
)

// jarAdapter conecta el almacén cifrado de cookies de Go con el cliente de Python.
type jarAdapter struct{ store *auth.Store }

func (j jarAdapter) Load(userID string) ([]pyclient.Cookie, error) {
	list, err := j.store.LoadJar(userID)
	if err != nil {
		return nil, err
	}
	out := make([]pyclient.Cookie, len(list))
	for i, c := range list {
		out[i] = pyclient.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, ExpiresAt: c.ExpiresAt}
	}
	return out, nil
}

func (j jarAdapter) Save(userID string, c pyclient.Cookie) error {
	return j.store.SaveJarCookie(userID, auth.JarCookie{Name: c.Name, Value: c.Value, Path: c.Path, ExpiresAt: c.ExpiresAt})
}

func (j jarAdapter) Delete(userID, name string) error { return j.store.DeleteJarCookie(userID, name) }

// NewPythonClient arma el cliente hacia Python, o devuelve nil si PYTHON_BASE_URL está vacía (Go sin
// Python: nada de estado Brightspace y ningún trabajador).
func NewPythonClient(cfg *config.Config, store *auth.Store) (*pyclient.Client, error) {
	if cfg.PythonBaseURL == "" {
		return nil, nil
	}
	return pyclient.New(pyclient.Config{
		BaseURL: cfg.PythonBaseURL,
		APIKey: cfg.PythonAPIKey,
		CAFile: cfg.PythonCAFile,
		AllowHTTP: cfg.PythonAllowHTTP,
		CookieNames: cfg.PythonCookieNames,
	}, jarAdapter{store})
}

// D2LQueue envía a Brightspace (vía Python) las entregas oficiales y las revisiones devueltas que quedaron
// en 'pendiente', con espera creciente entre intentos (1, 5 y 30 min) y un máximo de 5.
type D2LQueue struct {
	store *auth.Store
	client *pyclient.Client
	now func() time.Time
}

func NewD2LQueue(store *auth.Store, client *pyclient.Client) *D2LQueue {
	return &D2LQueue{store: store, client: client, now: time.Now}
}

// Run procesa la cola hasta que ctx se cancela. Sin cliente (Python no configurado) no hace nada.
func (q *D2LQueue) Run(ctx context.Context) {
	if q.client == nil {
		return
	}
	ticker := time.NewTicker(d2lIdleCheck)
	defer ticker.Stop()
	for {
		for ctx.Err() == nil && q.ProcessOne(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessOne atiende un envío vencido. Devuelve false si no había nada o no se pudo avanzar.
func (q *D2LQueue) ProcessOne(ctx context.Context) bool {
	if q.client == nil {
		return false
	}
	now := q.now()
	sub, err := q.store.NextD2LSubmission(now)
	if err != nil {
		slog.Error("cola de Brightspace: leer entregas", "error", err)
		return false
	}
	if sub != nil {
		return q.sendSubmission(ctx, sub)
	}
	rev, err := q.store.NextD2LReview(now)
	if err != nil {
		slog.Error("cola de Brightspace: leer revisiones", "error", err)
		return false
	}
	if rev != nil {
		return q.publishReview(ctx, rev)
	}
	return false
}

func (q *D2LQueue) sendSubmission(ctx context.Context, j *auth.D2LSubmissionJob) bool {
	if j.StudentExternalID == "" || j.CourseExternalID == "" || j.AssignmentExternalID == "" {
		return q.finish(auth.D2LSubmission, j.ID, "", auth.D2LResult{Status: auth.D2LFailed, Attempts: j.Attempts,
			Err: "la entrega no tiene los identificadores de Brightspace del estudiante, curso o taller"})
	}
	if !q.client.HasCookies(j.StudentID) {
		return q.noSession(auth.D2LSubmission, j.ID, "", j.Attempts, j.CreatedAt, "el estudiante no tiene sesión de Brightspace guardada")
	}
	_, err := q.client.Submit(ctx, j.StudentID, pyclient.SubmitRequest{
		StudentExternalID: j.StudentExternalID,
		CourseExternalID: j.CourseExternalID,
		AssignmentExternalID: j.AssignmentExternalID,
		Filename: d2lFilename,
		Content: j.Code,
		Comment: d2lComment,
		GoSubmissionID: j.ID,
	})
	if ctx.Err() != nil {
		return false // apagado: queda 'pendiente' y se reintenta al volver
	}
	return q.finish(auth.D2LSubmission, j.ID, "", q.outcome(err, j.Attempts, "submit", j.ID))
}

func (q *D2LQueue) publishReview(ctx context.Context, j *auth.D2LReviewJob) bool {
	if j.TeacherExternalID == "" || j.StudentExternalID == "" || j.CourseExternalID == "" || j.AssignmentExternalID == "" {
		return q.finish(auth.D2LReview, j.SubmissionID, j.UpdatedAtRaw, auth.D2LResult{Status: auth.D2LFailed, Attempts: j.Attempts,
			Err: "la revisión no tiene los identificadores de Brightspace del docente, estudiante, curso o taller"})
	}
	if !q.client.HasCookies(j.TeacherID) {
		return q.noSession(auth.D2LReview, j.SubmissionID, j.UpdatedAtRaw, j.Attempts, j.UpdatedAt,
			"el docente que devolvió la revisión no tiene sesión de Brightspace guardada; vuelve a iniciar sesión y se publicará sola")
	}
	denom := float64(d2lDefaultDenominator)
	if j.ScoreDenominator != nil && *j.ScoreDenominator > 0 {
		denom = *j.ScoreDenominator
	}
	req := pyclient.FeedbackRequest{
		TeacherExternalID: j.TeacherExternalID,
		StudentExternalID: j.StudentExternalID,
		CourseExternalID: j.CourseExternalID,
		AssignmentExternalID: j.AssignmentExternalID,
		ScoreDenominator: denom,
		Feedback: j.Feedback,
		GoReviewID: j.SubmissionID,
	}
	if j.Grade != nil {
		score := pyclient.Score(*j.Grade, denom)
		req.Score, req.IsGraded = &score, true
	}
	err := q.client.Feedback(ctx, j.TeacherID, req)
	if ctx.Err() != nil {
		return false
	}
	return q.finish(auth.D2LReview, j.SubmissionID, j.UpdatedAtRaw, q.outcome(err, j.Attempts, "feedback", j.SubmissionID))
}

// noSession deja el envío en 'pendiente' sin gastar un intento: no es un fallo de Brightspace sino que
// falta la sesión del usuario (se adelanta solo cuando vuelve a iniciar sesión, ver WakeD2LFor). Si lleva
// una semana así, se da por fallido.
func (q *D2LQueue) noSession(kind auth.D2LKind, id, version string, attempts int, since time.Time, reason string) bool {
	if q.now().Sub(since) > d2lNoSessionGiveUp {
		return q.finish(kind, id, version, auth.D2LResult{Status: auth.D2LFailed, Attempts: attempts, Err: reason + " desde hace más de una semana"})
	}
	next := q.now().Add(d2lNoSessionWait)
	return q.finish(kind, id, version, auth.D2LResult{Status: auth.D2LPending, Attempts: attempts, NextRetry: &next, Err: reason})
}

// outcome traduce la respuesta de Python a un estado (tabla §5.1 del contrato).
func (q *D2LQueue) outcome(err error, prev int, op, id string) auth.D2LResult {
	attempts := prev + 1
	if err == nil {
		slog.Info("brightspace "+op+" ok", "id", id, "attempts", attempts)
		return auth.D2LResult{Status: auth.D2LSent, Attempts: attempts}
	}
	var pe *pyclient.Error
	if !errors.As(err, &pe) {
		pe = &pyclient.Error{Code: pyclient.CodeDown, Message: err.Error()}
	}
	msg := auth.JobError(pe)
	if pe.Code == pyclient.CodeInvalidToken {
		msg = auth.D2LTokenMsg
	}
	if !pe.Retryable() {
		slog.Warn("brightspace "+op+" rechazado", "id", id, "code", pe.Code, "status", pe.Status)
		return auth.D2LResult{Status: auth.D2LFailed, Attempts: attempts, Err: msg}
	}
	if attempts >= auth.D2LMaxAttempts {
		slog.Error("brightspace "+op+" agotó los intentos", "id", id, "code", pe.Code)
		return auth.D2LResult{Status: auth.D2LFailed, Attempts: attempts, Err: msg + " (se agotaron los intentos)"}
	}
	wait := max(auth.D2LBackoff(attempts), pe.RetryAfter)
	next := q.now().Add(wait)
	slog.Warn("brightspace "+op+" se reintentará", "id", id, "code", pe.Code, "attempts", attempts, "in", wait.String())
	return auth.D2LResult{Status: auth.D2LPending, Attempts: attempts, NextRetry: &next, Err: msg}
}

func (q *D2LQueue) finish(kind auth.D2LKind, id, version string, r auth.D2LResult) bool {
	if _, err := q.store.FinishD2L(kind, id, version, r); err != nil {
		slog.Error("cola de Brightspace: guardar resultado", "id", id, "error", err)
		return false
	}
	return true
}

// pythonProbe consulta GET /internal/v1/health de Python con una caché corta. Su resultado se muestra en
// /api/health pero nunca lo degrada.
type pythonProbe struct {
	client *pyclient.Client
	mu sync.Mutex
	at time.Time
	ok bool
}

func (p *pythonProbe) check(ctx context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && time.Since(p.at) < pythonHealthCacheFor {
		return p.ok
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := p.client.Health(ctx)
	p.ok = err == nil && res.Status == "ok"
	p.at = time.Now()
	return p.ok
}
