package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Sincronización desde el servicio Python (Brightspace → EduTrace). Todo lo de este archivo es
// idempotente y NUNCA borra historial: quitar a alguien es marcar su inscripción como inactiva.

// PlaceholderOwnerEmail es el correo de la cuenta técnica que figura como dueña (courses.teacher_id,
// NOT NULL) de una materia espejada mientras ningún docente real haya entrado. No tiene contraseña,
// no tiene external_id (no se le pueden emitir tickets) y no pertenece a course_staff.
const PlaceholderOwnerEmail = "sin-docente@edutrace.invalid"

const (
	maxExternalName = 120
	maxCourseName = 200
	maxTitle = 120
	maxStatement = 20000
	maxEmail = 254
)

var (
	ErrInvalidInput = errors.New("datos inválidos")
	ErrUnknownUser = errors.New("usuario desconocido")
	ErrUnknownCourse = errors.New("curso desconocido")
	ErrEmptyRoster = errors.New("la lista de clase está vacía")
	ErrEmptySnapshot = errors.New("la instantánea no trae ninguna inscripción válida y el usuario tenía inscripciones activas")
	ErrRosterTooDestructive = errors.New("la lista de clase desactivaría a demasiados inscritos")
	ErrExternalConflict = errors.New("conflicto con datos existentes")
	ErrEmailTakenLocal = errors.New("correo ya usado por una cuenta local")
	ErrEmailTakenExternal = errors.New("correo ya usado por otro usuario de Brightspace")
)

var externalIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.\-]{0,127}$`)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

func validExternalID(id string) bool { return externalIDPattern.MatchString(id) }

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func cleanName(name string) string {
	return clipRunes(strings.Join(strings.Fields(name), " "), maxExternalName)
}

func validEmail(email string) error {
	if email == "" {
		return ErrEmailRequired
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || strings.ToLower(addr.Address) != email || len(email) > maxEmail {
		return invalid("correo inválido")
	}
	return nil
}

// ErrEmailRequired: Brightspace no entregó correo (D4: se rechaza el ingreso).
var ErrEmailRequired = fmt.Errorf("%w: falta el correo del usuario", ErrInvalidInput)

// ---------- Usuarios ----------

// ExternalUserResult es el usuario espejado tras PUT /internal/v1/users/{external_id}.
type ExternalUserResult struct {
	User *User
	ExternalID string
	Created bool
}

func userByExternalID(q querier, externalID string) (string, error) {
	var id string
	err := q.QueryRow(`SELECT id FROM users WHERE external_id = ?`, externalID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnknownUser
	}
	return id, err
}

// upsertUserTx crea o actualiza el usuario con ese external_id. El correo ya usado por otra cuenta
// (local o de otro usuario externo) da 409 y NO se fusionan cuentas (D5).
func upsertUserTx(tx querier, externalID, name, email string) (id string, created bool, err error) {
	if !validExternalID(externalID) {
		return "", false, invalid("external_id inválido")
	}
	name, email = cleanName(name), normalizeEmail(email)
	if name == "" {
		return "", false, invalid("falta el nombre del usuario")
	}
	if err := validEmail(email); err != nil {
		return "", false, err
	}
	var owner, ownerExternal sql.NullString
	err = tx.QueryRow(`SELECT id, external_id FROM users WHERE email = ?`, email).Scan(&owner, &ownerExternal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	taken := func() error {
		if ownerExternal.Valid {
			return ErrEmailTakenExternal
		}
		return ErrEmailTakenLocal
	}
	id, err = userByExternalID(tx, externalID)
	switch {
	case err == nil:
		if owner.Valid && owner.String != id {
			return "", false, taken()
		}
		_, err = tx.Exec(`UPDATE users SET name = ?, email = ? WHERE id = ?`, name, email, id)
		return id, false, err
	case !errors.Is(err, ErrUnknownUser):
		return "", false, err
	}
	if owner.Valid {
		return "", false, taken()
	}
	if id, err = randomHex(12); err != nil {
		return "", false, err
	}
	// password_hash vacío: la cuenta no puede entrar por el login local (CheckPassword rechaza '').
	_, err = tx.Exec(`INSERT INTO users (id, name, email, role, password_hash, created_at, external_id)
		VALUES (?, ?, ?, 'student', '', ?, ?)`, id, name, email, fmtTime(time.Now().UTC()), externalID)
	if isUnique(err) {
		return "", false, ErrEmailTakenLocal
	}
	return id, true, err
}

// recomputeRole fija users.role como «perfil principal» (D2): docente si lo es en algún curso activo;
// si no, monitor mientras siga asignado como monitor local (D3); si no, estudiante.
func recomputeRole(tx querier, userID string) error {
	var teaching, monitoring int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM course_staff st JOIN courses c ON c.id = st.course_id
		WHERE st.teacher_id = ? AND c.external_active = 1`, userID).Scan(&teaching); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM group_monitors WHERE monitor_id = ?`, userID).Scan(&monitoring); err != nil {
		return err
	}
	role := RoleStudent
	switch {
	case teaching > 0:
		role = RoleTeacher
	case monitoring > 0:
		role = RoleMonitor
	}
	_, err := tx.Exec(`UPDATE users SET role = ? WHERE id = ? AND role != ?`, role, userID, role)
	return err
}

// UpsertExternalUser crea o actualiza el espejo de un usuario de Brightspace.
func (s *Store) UpsertExternalUser(externalID, name, email string) (*ExternalUserResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id, created, err := upsertUserTx(tx, externalID, name, email)
	if err != nil {
		return nil, err
	}
	if err := recomputeRole(tx, id); err != nil {
		return nil, err
	}
	u, err := scanUser(tx.QueryRow(`SELECT `+userCols+` FROM users u WHERE u.id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ExternalUserResult{User: u, ExternalID: externalID, Created: created}, nil
}

// ---------- Cursos e inscripciones ----------

type ExternalCourse struct {
	ExternalID string
	Name string
	StartDate *time.Time
	EndDate *time.Time
	IsActive bool
}

type ExternalEnrollment struct {
	Course ExternalCourse
	Role string
}

type EnrollmentGroup struct {
	CourseExternalID string `json:"course_external_id"`
	GroupID string `json:"group_id"`
	CourseID string `json:"course_id"`
}

type EnrollmentResult struct {
	CoursesCreated int `json:"courses_created"`
	Enrolled int `json:"enrolled"`
	Deactivated int `json:"deactivated"`
	Groups []EnrollmentGroup `json:"groups"`
}

func ensurePlaceholderOwner(tx querier) (string, error) {
	var id string
	err := tx.QueryRow(`SELECT id FROM users WHERE email = ?`, PlaceholderOwnerEmail).Scan(&id)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return id, err
	}
	if id, err = randomHex(12); err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO users (id, name, email, role, password_hash, created_at)
		VALUES (?, 'Sin docente asignado', ?, 'teacher', '', ?)`, id, PlaceholderOwnerEmail, fmtTime(time.Now().UTC()))
	return id, err
}

// upsertCourseTx crea (o actualiza) el curso espejado y su única clase: un offering = un curso + una clase.
func upsertCourseTx(tx querier, c ExternalCourse) (courseID, groupID string, created bool, err error) {
	if !validExternalID(c.ExternalID) {
		return "", "", false, invalid("external_id de curso inválido")
	}
	name := clipRunes(strings.Join(strings.Fields(c.Name), " "), maxCourseName)
	if name == "" {
		return "", "", false, invalid("falta el nombre del curso %s", c.ExternalID)
	}
	now := fmtTime(time.Now().UTC())
	err = tx.QueryRow(`SELECT id FROM courses WHERE external_id = ?`, c.ExternalID).Scan(&courseID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		owner, err := ensurePlaceholderOwner(tx)
		if err != nil {
			return "", "", false, err
		}
		if courseID, err = randomHex(12); err != nil {
			return "", "", false, err
		}
		if _, err := tx.Exec(`INSERT INTO courses (id, teacher_id, name, created_at, external_id, starts_at, ends_at, external_active)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, courseID, owner, name, now, c.ExternalID,
			fmtTimePtr(c.StartDate), fmtTimePtr(c.EndDate), c.IsActive); err != nil {
			return "", "", false, err
		}
		created = true
	case err != nil:
		return "", "", false, err
	default:
		if _, err := tx.Exec(`UPDATE courses SET name = ?, starts_at = ?, ends_at = ?, external_active = ? WHERE id = ?`,
			name, fmtTimePtr(c.StartDate), fmtTimePtr(c.EndDate), c.IsActive, courseID); err != nil {
			return "", "", false, err
		}
	}
	err = tx.QueryRow(`SELECT id FROM class_groups WHERE external_id = ?`, c.ExternalID).Scan(&groupID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if groupID, err = randomHex(12); err != nil {
			return "", "", false, err
		}
		// join_code es UNIQUE NOT NULL y estas clases no se unen por código: se rellena con un valor inalcanzable.
		if _, err := tx.Exec(`INSERT INTO class_groups (id, course_id, name, join_code, created_at, external_id)
			VALUES (?, ?, ?, ?, ?, ?)`, groupID, courseID, name, "d2l-"+groupID, now, c.ExternalID); err != nil {
			return "", "", false, err
		}
	case err != nil:
		return "", "", false, err
	default:
		if _, err := tx.Exec(`UPDATE class_groups SET name = ? WHERE id = ?`, name, groupID); err != nil {
			return "", "", false, err
		}
	}
	return courseID, groupID, created, nil
}

func enrollStudentTx(tx querier, userID, courseID, groupID string) error {
	// Un monitor de la materia no puede ser también su estudiante (ErrStudentOfCourse). Brightspace
	// manda: si ahora figura como estudiante, se retira su asignación local como monitor.
	res, err := tx.Exec(`DELETE FROM group_monitors WHERE monitor_id = ?
		AND group_id IN (SELECT id FROM class_groups WHERE course_id = ?)`, userID, courseID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Warn("monitor retirado: Brightspace lo inscribe como estudiante de la misma materia", "user_id", userID, "course_id", courseID)
	}
	_, err = tx.Exec(`INSERT INTO group_students (group_id, student_id, joined_at, active) VALUES (?, ?, ?, 1)
		ON CONFLICT (group_id, student_id) DO UPDATE SET active = 1, ended_at = NULL`,
		groupID, userID, fmtTime(time.Now().UTC()))
	return err
}

func enrollTeacherTx(tx querier, userID, courseID string) error {
	if _, err := tx.Exec(`INSERT INTO course_teachers (course_id, teacher_id, added_at, active) VALUES (?, ?, ?, 1)
		ON CONFLICT (course_id, teacher_id) DO UPDATE SET active = 1, ended_at = NULL`,
		courseID, userID, fmtTime(time.Now().UTC())); err != nil {
		return err
	}
	return fixOwnerTx(tx, courseID)
}

// fixOwnerTx mantiene courses.teacher_id apuntando a un docente con inscripción activa: si el dueño
// actual (o la cuenta técnica) ya no lo es, pasa al docente activo más antiguo y le traslada los
// borradores de revisión pendientes. Si no hay docentes activos, no cambia nada.
func fixOwnerTx(tx querier, courseID string) error {
	var owner string
	if err := tx.QueryRow(`SELECT teacher_id FROM courses WHERE id = ?`, courseID).Scan(&owner); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM course_teachers WHERE course_id = ? AND teacher_id = ? AND active = 1`,
		courseID, owner).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return nil
	}
	var next string
	err := tx.QueryRow(`SELECT teacher_id FROM course_teachers WHERE course_id = ? AND active = 1
		ORDER BY added_at, teacher_id LIMIT 1`, courseID).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE courses SET teacher_id = ? WHERE id = ?`, next, courseID); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE reviews SET teacher_id = ? WHERE teacher_id = ? AND status = 'draft' AND submission_id IN (
		SELECT s.id FROM submissions s JOIN class_groups g ON g.id = s.group_id WHERE g.course_id = ?)`, next, owner, courseID)
	return err
}

// UpsertEnrollments aplica la instantánea completa de inscripciones de un usuario: da de alta o
// reactiva lo que viene y desactiva (sin borrar) lo que ya no viene. Solo toca cursos espejados.
func (s *Store) UpsertEnrollments(externalUserID string, enrollments []ExternalEnrollment) (*EnrollmentResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	userID, err := userByExternalID(tx, externalUserID)
	if err != nil {
		return nil, err
	}
	res := &EnrollmentResult{Groups: []EnrollmentGroup{}}
	seenGroups, seenCourses := map[string]bool{}, map[string]bool{} // inscripciones vigentes: clase (estudiante), curso (docente)
	listed := map[string]bool{}
	for _, e := range enrollments {
		switch e.Role {
		case "student", "teacher":
		case "monitor":
			// D3: los monitores son locales; Python no debería enviarlos. No se rechaza el ingreso por eso.
			slog.Warn("inscripción con rol monitor ignorada (los monitores son locales)", "course", e.Course.ExternalID)
			continue
		default:
			return nil, invalid("rol %q no soportado (student, teacher)", e.Role)
		}
		courseID, groupID, created, err := upsertCourseTx(tx, e.Course)
		if err != nil {
			return nil, err
		}
		if created {
			res.CoursesCreated++
		}
		if !listed[e.Course.ExternalID] {
			listed[e.Course.ExternalID] = true
			res.Groups = append(res.Groups, EnrollmentGroup{CourseExternalID: e.Course.ExternalID, GroupID: groupID, CourseID: courseID})
		}
		if !e.Course.IsActive {
			continue
		}
		if e.Role == "student" {
			if seenGroups[groupID] {
				continue
			}
			seenGroups[groupID] = true
			err = enrollStudentTx(tx, userID, courseID, groupID)
		} else {
			if seenCourses[courseID] {
				continue
			}
			seenCourses[courseID] = true
			err = enrollTeacherTx(tx, userID, courseID)
		}
		if err != nil {
			return nil, err
		}
		res.Enrolled++
	}

	now := fmtTime(time.Now().UTC())
	if len(res.Groups) == 0 {
		// Protección: una instantánea sin ninguna inscripción válida (p. ej. todo filtrado por rol o tipo)
		// no debe desactivar lo que el usuario ya tenía.
		var active int
		if err := tx.QueryRow(`SELECT
			(SELECT COUNT(*) FROM group_students gs JOIN class_groups g ON g.id = gs.group_id JOIN courses c ON c.id = g.course_id
				WHERE gs.student_id = ?1 AND gs.active = 1 AND c.external_id IS NOT NULL) +
			(SELECT COUNT(*) FROM course_teachers ct JOIN courses c ON c.id = ct.course_id
				WHERE ct.teacher_id = ?1 AND ct.active = 1 AND c.external_id IS NOT NULL)`, userID).Scan(&active); err != nil {
			return nil, err
		}
		if active > 0 {
			return nil, ErrEmptySnapshot
		}
	}
	staleGroups, err := queryIDs(tx, `SELECT gs.group_id FROM group_students gs
		JOIN class_groups g ON g.id = gs.group_id JOIN courses c ON c.id = g.course_id
		WHERE gs.student_id = ? AND gs.active = 1 AND c.external_id IS NOT NULL`, userID)
	if err != nil {
		return nil, err
	}
	for _, g := range staleGroups {
		if seenGroups[g] {
			continue
		}
		if _, err := tx.Exec(`UPDATE group_students SET active = 0, ended_at = ? WHERE group_id = ? AND student_id = ?`, now, g, userID); err != nil {
			return nil, err
		}
		res.Deactivated++
	}
	staleCourses, err := queryIDs(tx, `SELECT ct.course_id FROM course_teachers ct JOIN courses c ON c.id = ct.course_id
		WHERE ct.teacher_id = ? AND ct.active = 1 AND c.external_id IS NOT NULL`, userID)
	if err != nil {
		return nil, err
	}
	for _, c := range staleCourses {
		if seenCourses[c] {
			continue
		}
		if _, err := tx.Exec(`UPDATE course_teachers SET active = 0, ended_at = ? WHERE course_id = ? AND teacher_id = ?`, now, c, userID); err != nil {
			return nil, err
		}
		if err := fixOwnerTx(tx, c); err != nil {
			return nil, err
		}
		res.Deactivated++
	}
	if err := recomputeRole(tx, userID); err != nil {
		return nil, err
	}
	return res, tx.Commit()
}

// ---------- Lista de clase ----------

type RosterMember struct {
	ExternalID string
	Name string
	Email string
	Role string
	IsActive bool
}

type RosterSkip struct {
	ExternalID string `json:"external_id"`
	Reason string `json:"reason"`
}

type RosterResult struct {
	Members int `json:"members"`
	CreatedUsers int `json:"created_users"`
	Enrolled int `json:"enrolled"`
	Deactivated int `json:"deactivated"`
	Skipped []RosterSkip `json:"skipped"`
}

// UpsertRoster aplica la lista de clase completa de un curso espejado. Está protegida: una lista vacía
// se rechaza, y también una que desactive a más de maxDeactivatePercent % de los inscritos activos
// (un error de Python no debe vaciar un curso, que además dispara el borrado de Purge). En ambos casos
// no se cambia nada.
func (s *Store) UpsertRoster(courseExternalID string, members []RosterMember, maxDeactivatePercent int) (*RosterResult, error) {
	if len(members) == 0 {
		return nil, ErrEmptyRoster
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var courseID, groupID string
	err = tx.QueryRow(`SELECT c.id, g.id FROM courses c JOIN class_groups g ON g.course_id = c.id AND g.external_id = c.external_id
		WHERE c.external_id = ?`, courseExternalID).Scan(&courseID, &groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownCourse
	}
	if err != nil {
		return nil, err
	}

	res := &RosterResult{Members: len(members), Skipped: []RosterSkip{}}
	students, teachers := map[string]bool{}, map[string]bool{}
	touched := []string{}
	for _, m := range members {
		skip := func(reason string) {
			res.Skipped = append(res.Skipped, RosterSkip{ExternalID: m.ExternalID, Reason: reason})
			// Un miembro que se salta (dato malo, correo en conflicto) conserva su inscripción actual.
			if id, err := userByExternalID(tx, m.ExternalID); err == nil {
				students[id], teachers[id] = true, true
			}
		}
		if m.Role == "monitor" {
			continue
		}
		if m.Role != "student" && m.Role != "teacher" {
			skip("rol no soportado")
			continue
		}
		if !m.IsActive {
			continue
		}
		id, created, err := upsertUserTx(tx, m.ExternalID, m.Name, m.Email)
		if errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrEmailTakenLocal) || errors.Is(err, ErrEmailTakenExternal) {
			skip(err.Error())
			continue
		}
		if err != nil {
			return nil, err
		}
		if created {
			res.CreatedUsers++
		}
		if m.Role == "student" {
			err = enrollStudentTx(tx, id, courseID, groupID)
			students[id] = true
		} else {
			err = enrollTeacherTx(tx, id, courseID)
			teachers[id] = true
		}
		if err != nil {
			return nil, err
		}
		res.Enrolled++
		touched = append(touched, id)
	}

	currentStudents, err := queryIDs(tx, `SELECT student_id FROM group_students WHERE group_id = ? AND active = 1`, groupID)
	if err != nil {
		return nil, err
	}
	currentTeachers, err := queryIDs(tx, `SELECT teacher_id FROM course_teachers WHERE course_id = ? AND active = 1`, courseID)
	if err != nil {
		return nil, err
	}
	var dropStudents, dropTeachers []string
	for _, id := range currentStudents {
		if !students[id] {
			dropStudents = append(dropStudents, id)
		}
	}
	for _, id := range currentTeachers {
		if !teachers[id] {
			dropTeachers = append(dropTeachers, id)
		}
	}
	drop, total := len(dropStudents)+len(dropTeachers), len(currentStudents)+len(currentTeachers)
	if total > 0 && drop*100 > maxDeactivatePercent*total {
		return nil, fmt.Errorf("%w: desactivaría %d de %d inscritos (máximo %d %%)", ErrRosterTooDestructive, drop, total, maxDeactivatePercent)
	}
	now := fmtTime(time.Now().UTC())
	for _, id := range dropStudents {
		if _, err := tx.Exec(`UPDATE group_students SET active = 0, ended_at = ? WHERE group_id = ? AND student_id = ?`, now, groupID, id); err != nil {
			return nil, err
		}
	}
	for _, id := range dropTeachers {
		if _, err := tx.Exec(`UPDATE course_teachers SET active = 0, ended_at = ? WHERE course_id = ? AND teacher_id = ?`, now, courseID, id); err != nil {
			return nil, err
		}
	}
	if len(dropTeachers) > 0 {
		if err := fixOwnerTx(tx, courseID); err != nil {
			return nil, err
		}
	}
	res.Deactivated = drop
	for _, id := range append(touched, append(dropStudents, dropTeachers...)...) {
		if err := recomputeRole(tx, id); err != nil {
			return nil, err
		}
	}
	return res, tx.Commit()
}

// ---------- Talleres ----------

type ExternalAssignment struct {
	ExternalID string
	Title string
	Statement string
	DueAt *time.Time
	EndAt *time.Time
	ScoreDenominator *float64
	GradeItemID string
	IsHidden bool
}

type AssignmentsResult struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Hidden int `json:"hidden"`
}

// deadlinePolicy deriva allow_late/late_until de la carpeta: la entrega tardía existe si la ventana de
// disponibilidad (end_at) termina después de la fecha límite. Sin end_at Brightspace no cierra la
// carpeta, así que se aceptan tardías sin tope (lo contrario rechazaría entregas que D2L sí recibe).
func deadlinePolicy(due, end *time.Time) (allowLate bool, lateUntil *time.Time) {
	switch {
	case due == nil || end == nil:
		return true, nil
	case end.After(*due):
		return true, end
	}
	return false, nil
}

// UpsertAssignments espeja las carpetas de entrega de un curso. Solo toca título, enunciado, fechas,
// score_denominator, grade_item_id y la visibilidad; rubric, adjuntos, casos, criterios, late_penalty y
// evaluation son de EduTrace. Las carpetas que ya no vienen se ocultan, no se borran.
func (s *Store) UpsertAssignments(courseExternalID string, items []ExternalAssignment) (*AssignmentsResult, error) {
	seen := map[string]bool{}
	for i := range items {
		it := &items[i]
		if !validExternalID(it.ExternalID) {
			return nil, invalid("external_id de taller inválido")
		}
		if seen[it.ExternalID] {
			return nil, invalid("el taller %s viene repetido", it.ExternalID)
		}
		seen[it.ExternalID] = true
		it.Title = clipRunes(strings.Join(strings.Fields(it.Title), " "), maxTitle)
		if it.Title == "" {
			return nil, invalid("falta el título del taller %s", it.ExternalID)
		}
		it.Statement = clipRunes(strings.TrimSpace(it.Statement), maxStatement)
		if it.ScoreDenominator != nil && (math.IsNaN(*it.ScoreDenominator) || math.IsInf(*it.ScoreDenominator, 0) || *it.ScoreDenominator <= 0) {
			return nil, invalid("score_denominator del taller %s debe ser mayor que 0", it.ExternalID)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var groupID string
	err = tx.QueryRow(`SELECT g.id FROM class_groups g WHERE g.external_id = ? AND EXISTS (
		SELECT 1 FROM courses c WHERE c.id = g.course_id AND c.external_id = g.external_id)`, courseExternalID).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownCourse
	}
	if err != nil {
		return nil, err
	}
	res := &AssignmentsResult{}
	now := fmtTime(time.Now().UTC())
	for _, it := range items {
		allowLate, lateUntil := deadlinePolicy(it.DueAt, it.EndAt)
		var denominator, gradeItem any
		if it.ScoreDenominator != nil {
			denominator = *it.ScoreDenominator
		}
		if g := strings.TrimSpace(it.GradeItemID); g != "" {
			gradeItem = g
		}
		var id, owner string
		var hidden bool
		err := tx.QueryRow(`SELECT id, group_id, is_hidden FROM assignments WHERE external_id = ?`, it.ExternalID).Scan(&id, &owner, &hidden)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if id, err = randomHex(12); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`INSERT INTO assignments (id, group_id, title, statement, due_at, created_at, allow_late, late_until,
					external_id, score_denominator, grade_item_id, is_hidden)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, groupID, it.Title, it.Statement, fmtTimePtr(it.DueAt), now,
				allowLate, fmtTimePtr(lateUntil), it.ExternalID, denominator, gradeItem, it.IsHidden); err != nil {
				return nil, err
			}
			res.Created++
			if it.IsHidden {
				res.Hidden++
			}
		case err != nil:
			return nil, err
		default:
			if owner != groupID {
				return nil, fmt.Errorf("%w: el taller %s pertenece a otro curso", ErrExternalConflict, it.ExternalID)
			}
			if _, err := tx.Exec(`UPDATE assignments SET title = ?, statement = ?, due_at = ?, allow_late = ?, late_until = ?,
					score_denominator = ?, grade_item_id = ?, is_hidden = ? WHERE id = ?`,
				it.Title, it.Statement, fmtTimePtr(it.DueAt), allowLate, fmtTimePtr(lateUntil), denominator, gradeItem, it.IsHidden, id); err != nil {
				return nil, err
			}
			res.Updated++
			if it.IsHidden && !hidden {
				res.Hidden++
			}
		}
	}
	// Lo que desapareció de Brightspace se oculta: puede tener entregas y revisiones.
	existing, err := queryIDs(tx, `SELECT external_id FROM assignments WHERE group_id = ? AND external_id IS NOT NULL AND is_hidden = 0`, groupID)
	if err != nil {
		return nil, err
	}
	for _, ext := range existing {
		if seen[ext] {
			continue
		}
		if _, err := tx.Exec(`UPDATE assignments SET is_hidden = 1 WHERE group_id = ? AND external_id = ?`, groupID, ext); err != nil {
			return nil, err
		}
		res.Hidden++
	}
	return res, tx.Commit()
}

// mirroredAssignment devuelve los campos de solo lectura de un taller espejado de Brightspace,
// o nil si el taller no existe o es local (sin external_id).
func (s *Store) mirroredAssignment(assignmentID string) (*Assignment, error) {
	a, err := scanAssignment(s.db.QueryRow(`SELECT `+assignmentCols+` FROM assignments a WHERE a.id = ? AND a.external_id IS NOT NULL`, assignmentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}
