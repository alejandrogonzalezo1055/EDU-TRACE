package auth

import (
	"database/sql"
	"errors"
	"time"
)

// Cola de envíos hacia Brightspace (lado Go → Python, contrato §5 y §8). Las filas viven en las propias
// tablas: submissions.d2l_* para la entrega oficial y reviews.d2l_* para la revisión devuelta.
// Los estados son los del esquema: '' (no aplica), 'pendiente', 'enviada' y 'fallida'.

const (
	D2LPending = "pendiente"
	D2LSent = "enviada"
	D2LFailed = "fallida"

	// D2LTokenMsg es el motivo de una entrega o publicación fallida porque el token de Brightspace del usuario
	// venció o fue revocado. Al volver a iniciar sesión (cookies nuevas) estas filas se reviven solas.
	D2LTokenMsg = "Tu sesión de Brightspace venció o fue revocada: vuelve a iniciar sesión en EduTrace para que se reenvíe."

	// D2LMaxAttempts es el tope de intentos antes de dar el envío por fallido.
	D2LMaxAttempts = 5
)

// D2LBackoff es la espera tras el intento n (1 min, 5 min, 30 min, y 30 min en adelante).
func D2LBackoff(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return time.Minute
	case attempts == 2:
		return 5 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// SetD2LEnabled activa el encolado hacia Brightspace (solo si hay servicio Python configurado). Sin
// esto las entregas y revisiones se guardan como siempre, sin estado de Brightspace.
func (s *Store) SetD2LEnabled(on bool) { s.d2lEnabled = on }

// D2LEnabled indica si el encolado hacia Brightspace está activo.
func (s *Store) D2LEnabled() bool { return s.d2lEnabled }

// queueSubmissionForD2L marca la entrega oficial como pendiente de envío si su taller es espejo de una
// carpeta de Brightspace.
func (s *Store) queueSubmissionForD2L(q querier, sub *Submission) error {
	if !s.d2lEnabled || sub.AssignmentID == "" {
		return nil
	}
	var mirrored bool
	if err := q.QueryRow(`SELECT external_id IS NOT NULL FROM assignments WHERE id = ?`, sub.AssignmentID).Scan(&mirrored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if !mirrored {
		return nil
	}
	if _, err := q.Exec(`UPDATE submissions SET d2l_status = 'pendiente', d2l_attempts = 0, d2l_next_retry_at = NULL, d2l_error = '', d2l_sent_at = NULL
		WHERE id = ?`, sub.ID); err != nil {
		return err
	}
	sub.BrightspaceStatus = D2LPending
	return nil
}

// queueReviewForD2L marca la revisión como pendiente de publicar (reemplazo completo en cada llamada) y
// guarda qué docente la devuelve: sus cookies son las que se usan.
func (s *Store) queueReviewForD2L(q querier, actorID string, r *Review) error {
	if !s.d2lEnabled {
		return nil
	}
	var mirrored bool
	err := q.QueryRow(`SELECT a.external_id IS NOT NULL FROM submissions s JOIN assignments a ON a.id = s.assignment_id
		WHERE s.id = ?`, r.SubmissionID).Scan(&mirrored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !mirrored {
		return nil
	}
	if _, err := q.Exec(`UPDATE reviews SET d2l_status = 'pendiente', d2l_attempts = 0, d2l_next_retry_at = NULL, d2l_error = '',
		d2l_published_at = NULL, d2l_teacher_id = ? WHERE submission_id = ?`, actorID, r.SubmissionID); err != nil {
		return err
	}
	published := false
	r.BrightspacePublished, r.BrightspacePublishedAt, r.BrightspaceError = &published, nil, ""
	return nil
}

// D2LSubmissionJob es una entrega oficial lista para enviar a Brightspace.
type D2LSubmissionJob struct {
	ID string
	StudentID string
	StudentExternalID string
	CourseExternalID string
	AssignmentExternalID string
	Code string
	Attempts int
	CreatedAt time.Time
}

// D2LReviewJob es una revisión devuelta lista para publicar en Brightspace.
type D2LReviewJob struct {
	SubmissionID string
	TeacherID string // docente que devolvió la revisión: sus cookies se usan
	TeacherExternalID string
	StudentExternalID string
	CourseExternalID string
	AssignmentExternalID string
	Grade *float64
	ScoreDenominator *float64
	Feedback string
	Attempts int
	UpdatedAt time.Time // versión que se está enviando (ver FinishD2L)
	UpdatedAtRaw string
}

// NextD2LSubmission devuelve la siguiente entrega pendiente cuyo reintento ya venció, o nil.
func (s *Store) NextD2LSubmission(now time.Time) (*D2LSubmissionJob, error) {
	var j D2LSubmissionJob
	var created string
	err := s.db.QueryRow(`SELECT s.id, s.student_id, COALESCE(u.external_id, ''), COALESCE(c.external_id, ''), COALESCE(a.external_id, ''),
			s.code, s.d2l_attempts, s.created_at
		FROM submissions s
		JOIN users u ON u.id = s.student_id
		JOIN class_groups g ON g.id = s.group_id
		JOIN courses c ON c.id = g.course_id
		LEFT JOIN assignments a ON a.id = s.assignment_id
		WHERE s.kind = 'official' AND s.d2l_status = 'pendiente' AND (s.d2l_next_retry_at IS NULL OR s.d2l_next_retry_at <= ?)
		ORDER BY COALESCE(s.d2l_next_retry_at, s.created_at) LIMIT 1`, fmtTime(now)).
		Scan(&j.ID, &j.StudentID, &j.StudentExternalID, &j.CourseExternalID, &j.AssignmentExternalID, &j.Code, &j.Attempts, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.CreatedAt = parseTime(created)
	return &j, nil
}

// NextD2LReview devuelve la siguiente revisión devuelta pendiente de publicar, o nil.
func (s *Store) NextD2LReview(now time.Time) (*D2LReviewJob, error) {
	var j D2LReviewJob
	var grade, denom sql.NullFloat64
	err := s.db.QueryRow(`SELECT r.submission_id, COALESCE(NULLIF(r.d2l_teacher_id, ''), r.teacher_id),
			COALESCE(tu.external_id, ''), COALESCE(su.external_id, ''), COALESCE(c.external_id, ''), COALESCE(a.external_id, ''),
			r.grade, a.score_denominator, r.feedback, r.d2l_attempts, r.updated_at
		FROM reviews r
		JOIN submissions s ON s.id = r.submission_id
		JOIN users su ON su.id = s.student_id
		LEFT JOIN users tu ON tu.id = COALESCE(NULLIF(r.d2l_teacher_id, ''), r.teacher_id)
		JOIN class_groups g ON g.id = s.group_id
		JOIN courses c ON c.id = g.course_id
		LEFT JOIN assignments a ON a.id = s.assignment_id
		WHERE r.status = 'returned' AND r.d2l_status = 'pendiente' AND (r.d2l_next_retry_at IS NULL OR r.d2l_next_retry_at <= ?)
		ORDER BY COALESCE(r.d2l_next_retry_at, r.updated_at) LIMIT 1`, fmtTime(now)).
		Scan(&j.SubmissionID, &j.TeacherID, &j.TeacherExternalID, &j.StudentExternalID, &j.CourseExternalID, &j.AssignmentExternalID,
			&grade, &denom, &j.Feedback, &j.Attempts, &j.UpdatedAtRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Grade, j.ScoreDenominator = floatPtr(grade), floatPtr(denom)
	j.UpdatedAt = parseTime(j.UpdatedAtRaw)
	return &j, nil
}

// D2LKind elige la tabla de la cola.
type D2LKind int

const (
	D2LSubmission D2LKind = iota
	D2LReview
)

// D2LResult es el resultado de un intento de envío.
type D2LResult struct {
	Status string // D2LSent, D2LPending o D2LFailed
	Attempts int
	NextRetry *time.Time // solo con D2LPending
	Err string
}

// FinishD2L guarda el resultado de un intento. Para revisiones, version es la updated_at que se envió: si
// el docente re-editó mientras tanto, la fila ya volvió a 'pendiente' con la versión nueva y este
// resultado viejo se descarta (devuelve false) para no marcar como publicado algo que no lo está.
func (s *Store) FinishD2L(kind D2LKind, id, version string, r D2LResult) (bool, error) {
	now := time.Now().UTC()
	msg := r.Err
	var doneAt any
	var next any
	switch r.Status {
	case D2LSent:
		doneAt, msg = fmtTime(now), ""
	case D2LPending:
		next = fmtTimePtr(r.NextRetry)
	}
	var res sql.Result
	var err error
	if kind == D2LSubmission {
		res, err = s.db.Exec(`UPDATE submissions SET d2l_status = ?, d2l_attempts = ?, d2l_next_retry_at = ?, d2l_error = ?,
			d2l_sent_at = COALESCE(?, d2l_sent_at) WHERE id = ? AND d2l_status = 'pendiente'`,
			r.Status, r.Attempts, next, msg, doneAt, id)
	} else {
		res, err = s.db.Exec(`UPDATE reviews SET d2l_status = ?, d2l_attempts = ?, d2l_next_retry_at = ?, d2l_error = ?,
			d2l_published_at = COALESCE(?, d2l_published_at) WHERE submission_id = ? AND updated_at = ? AND d2l_status = 'pendiente'`,
			r.Status, r.Attempts, next, msg, doneAt, id, version)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// WakeD2LFor adelanta los reintentos pendientes de un usuario y revive lo que falló por token vencido (al
// iniciar sesión se renuevan sus cookies, así que ya puede ir).
func (s *Store) WakeD2LFor(userID string) error {
	if !s.d2lEnabled {
		return nil
	}
	if _, err := s.db.Exec(`UPDATE submissions SET d2l_next_retry_at = NULL WHERE d2l_status = 'pendiente' AND student_id = ?`, userID); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE submissions SET d2l_status = 'pendiente', d2l_attempts = 0, d2l_next_retry_at = NULL, d2l_error = ''
		WHERE d2l_status = 'fallida' AND d2l_error = ? AND student_id = ?`, D2LTokenMsg, userID); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE reviews SET d2l_next_retry_at = NULL WHERE d2l_status = 'pendiente'
		AND COALESCE(NULLIF(d2l_teacher_id, ''), teacher_id) = ?`, userID); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE reviews SET d2l_status = 'pendiente', d2l_attempts = 0, d2l_next_retry_at = NULL, d2l_error = ''
		WHERE d2l_status = 'fallida' AND d2l_error = ? AND COALESCE(NULLIF(d2l_teacher_id, ''), teacher_id) = ?`, D2LTokenMsg, userID)
	return err
}
