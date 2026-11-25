package auth

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marlon/edu-trace/internal/model"
)

func extUser(t *testing.T, s *Store, ext, name, email string) *User {
	t.Helper()
	r, err := s.UpsertExternalUser(ext, name, email)
	mustNoErr(t, err)
	return r.User
}

func offering(ext, name string, active bool) ExternalCourse {
	return ExternalCourse{ExternalID: ext, Name: name, IsActive: active}
}

func enroll(t *testing.T, s *Store, user string, list ...ExternalEnrollment) *EnrollmentResult {
	t.Helper()
	r, err := s.UpsertEnrollments(user, list)
	mustNoErr(t, err)
	return r
}

func TestExternalUserUpsert(t *testing.T) {
	s := newTestStore(t)

	r, err := s.UpsertExternalUser("d2l:user:3101", "  Camila   Díaz ", "Camila.Diaz@Uni.example")
	mustNoErr(t, err)
	if !r.Created || r.User.Name != "Camila Díaz" || r.User.Email != "camila.diaz@uni.example" || r.User.Role != RoleStudent {
		t.Fatalf("alta: %+v %+v", r, r.User)
	}
	// Sin contraseña local: no puede entrar por el login local, ni con contraseña vacía.
	for _, pw := range []string{"", "password123"} {
		if _, err := s.Authenticate("camila.diaz@uni.example", pw); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("login local de cuenta externa con %q: %v", pw, err)
		}
	}

	r2, err := s.UpsertExternalUser("d2l:user:3101", "Camila D.", "camila.diaz@uni.example")
	mustNoErr(t, err)
	if r2.Created || r2.User.ID != r.User.ID || r2.User.Name != "Camila D." {
		t.Fatalf("repetido: %+v", r2.User)
	}

	// D5: el correo de una cuenta local no se fusiona.
	_, err = s.CreateUser("Local", "local@u.edu", "password123", RoleStudent)
	mustNoErr(t, err)
	if _, err := s.UpsertExternalUser("d2l:user:9", "Otro", "local@u.edu"); !errors.Is(err, ErrEmailTakenLocal) {
		t.Fatalf("correo de cuenta local: %v", err)
	}
	if _, err := s.UpsertExternalUser("d2l:user:9", "Otro", "camila.diaz@uni.example"); !errors.Is(err, ErrEmailTakenExternal) {
		t.Fatalf("correo de otro usuario externo: %v", err)
	}
	// ... ni al cambiar el correo de uno existente.
	if _, err := s.UpsertExternalUser("d2l:user:3101", "Camila", "local@u.edu"); !errors.Is(err, ErrEmailTakenLocal) {
		t.Fatalf("cambio a correo ajeno: %v", err)
	}
	// D4: sin correo se rechaza.
	if _, err := s.UpsertExternalUser("d2l:user:10", "Sin Correo", ""); !errors.Is(err, ErrEmailRequired) {
		t.Fatalf("sin correo: %v", err)
	}
	if _, err := s.UpsertExternalUser("d2l:user:10", "Mal Correo", "no-es-correo"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("correo inválido: %v", err)
	}
	if _, err := s.UpsertExternalUser("", "X", "x@u.edu"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("external_id vacío: %v", err)
	}
}

func TestEnrollmentsKeepHistory(t *testing.T) {
	s := newTestStore(t)
	prof := extUser(t, s, "d2l:user:1", "Elena Torres", "elena@uni.example")
	ana := extUser(t, s, "d2l:user:2", "Ana Alumna", "ana@uni.example")
	cs := offering("d2l:ou:6650", "Programación", true)
	ds := offering("d2l:ou:6700", "Datos", true)

	// Una estudiante entra antes que cualquier docente: el curso se crea con dueño técnico y nadie lo administra.
	r := enroll(t, s, "d2l:user:2", ExternalEnrollment{Course: cs, Role: "student"})
	if r.CoursesCreated != 1 || r.Enrolled != 1 || r.Deactivated != 0 || len(r.Groups) != 1 || r.Groups[0].CourseExternalID != "d2l:ou:6650" {
		t.Fatalf("alta de estudiante: %+v", r)
	}
	group := r.Groups[0].GroupID
	if got := s.TeacherCourses(prof.ID); len(got) != 0 {
		t.Fatalf("el docente ve cursos sin estar inscrito: %+v", got)
	}
	if !s.IsEnrolled(ana.ID, group) {
		t.Fatal("la estudiante debía quedar inscrita")
	}

	// Llega el docente: pasa a ser dueño y su perfil, docente.
	r = enroll(t, s, "d2l:user:1", ExternalEnrollment{Course: cs, Role: "teacher"})
	if r.CoursesCreated != 0 || r.Enrolled != 1 {
		t.Fatalf("alta de docente: %+v", r)
	}
	courses := s.TeacherCourses(prof.ID)
	if len(courses) != 1 || !courses[0].Owner || len(courses[0].Groups) != 1 || courses[0].Groups[0].JoinCode != "" {
		t.Fatalf("cursos del docente: %+v", courses)
	}
	if courses[0].Groups[0].StudentCount != 1 {
		t.Fatalf("estudiantes: %+v", courses[0].Groups[0])
	}
	if u, _ := s.UpsertExternalUser("d2l:user:1", "Elena Torres", "elena@uni.example"); u.User.Role != RoleTeacher {
		t.Fatalf("rol: %s", u.User.Role)
	}

	// La estudiante entrega.
	sub := &Submission{SubmissionMeta: SubmissionMeta{GroupID: group, StudentID: ana.ID, Success: true}, Code: "int main(){}"}
	mustNoErr(t, s.AddSubmission(sub))

	// Una instantánea sin nada válido no desactiva lo que ya tenía.
	if _, err := s.UpsertEnrollments("d2l:user:2", nil); !errors.Is(err, ErrEmptySnapshot) {
		t.Fatalf("instantánea vacía: %v", err)
	}
	if !s.IsEnrolled(ana.ID, group) {
		t.Fatal("la instantánea rechazada desactivó la inscripción")
	}

	// Cambia de curso: se desactiva la inscripción vieja SIN borrar su historial.
	r = enroll(t, s, "d2l:user:2", ExternalEnrollment{Course: ds, Role: "student"})
	if r.Enrolled != 1 || r.Deactivated != 1 || r.CoursesCreated != 1 {
		t.Fatalf("cambio de curso: %+v", r)
	}
	if s.IsEnrolled(ana.ID, group) {
		t.Fatal("la inscripción desactivada sigue activa")
	}
	subs, err := s.GroupSubmissions(prof.ID, group, ana.ID, "all")
	mustNoErr(t, err)
	if len(subs) != 1 || subs[0].ID != sub.ID {
		t.Fatalf("el historial debía conservarse: %+v", subs)
	}
	if err := s.AddSubmission(&Submission{SubmissionMeta: SubmissionMeta{GroupID: group, StudentID: ana.ID}, Code: "x"}); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("entrega sin inscripción activa: %v", err)
	}
	if got := s.StudentGroups(ana.ID); len(got) != 1 || got[0].CourseName != "Datos" {
		t.Fatalf("clases de la estudiante: %+v", got)
	}

	// Vuelve: se reactiva la misma fila y el historial sigue ahí.
	r = enroll(t, s, "d2l:user:2", ExternalEnrollment{Course: cs, Role: "student"}, ExternalEnrollment{Course: ds, Role: "student"})
	if r.Enrolled != 2 || r.Deactivated != 0 || !s.IsEnrolled(ana.ID, group) {
		t.Fatalf("reactivación: %+v", r)
	}
	// Un curso inactivo en Brightspace no inscribe.
	r = enroll(t, s, "d2l:user:2", ExternalEnrollment{Course: offering("d2l:ou:6650", "Programación", false), Role: "student"}, ExternalEnrollment{Course: ds, Role: "student"})
	if r.Enrolled != 1 || r.Deactivated != 1 || s.IsEnrolled(ana.ID, group) {
		t.Fatalf("curso inactivo: %+v", r)
	}

	// El docente deja el curso: pierde el acceso y su perfil vuelve a estudiante, el curso y los datos siguen.
	enroll(t, s, "d2l:user:1", ExternalEnrollment{Course: ds, Role: "student"})
	if got := s.TeacherCourses(prof.ID); len(got) != 0 {
		t.Fatalf("docente desactivado sigue viendo cursos: %+v", got)
	}
	if u, _ := s.UpsertExternalUser("d2l:user:1", "Elena Torres", "elena@uni.example"); u.User.Role != RoleStudent {
		t.Fatalf("rol tras dejar de ser docente: %s", u.User.Role)
	}
	var n int
	mustNoErr(t, s.db.QueryRow(`SELECT COUNT(*) FROM submissions WHERE group_id = ?`, group).Scan(&n))
	if n != 1 {
		t.Fatalf("entregas tras desinscribir al docente: %d", n)
	}

	// Validaciones.
	if _, err := s.UpsertEnrollments("d2l:user:404", nil); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("usuario desconocido: %v", err)
	}
	if _, err := s.UpsertEnrollments("d2l:user:2", []ExternalEnrollment{{Course: cs, Role: "rector"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("rol inválido: %v", err)
	}
}

func TestEnrollmentOwnerHandover(t *testing.T) {
	s := newTestStore(t)
	a := extUser(t, s, "d2l:user:1", "Profe A", "a@uni.example")
	b := extUser(t, s, "d2l:user:2", "Profe B", "b@uni.example")
	cs := offering("d2l:ou:1", "Curso", true)
	enroll(t, s, "d2l:user:1", ExternalEnrollment{Course: cs, Role: "teacher"})
	enroll(t, s, "d2l:user:2", ExternalEnrollment{Course: cs, Role: "teacher"})
	if got := s.TeacherCourses(b.ID); len(got) != 1 || got[0].Owner || len(got[0].Teachers) != 2 {
		t.Fatalf("co-docente: %+v", got)
	}
	// El dueño sale: la propiedad pasa al docente activo.
	enroll(t, s, "d2l:user:1", ExternalEnrollment{Course: offering("d2l:ou:2", "Otro", true), Role: "student"})
	if got := s.TeacherCourses(b.ID); len(got) != 1 || !got[0].Owner {
		t.Fatalf("traspaso de dueño: %+v", got)
	}
	if got := s.TeacherCourses(a.ID); len(got) != 0 {
		t.Fatalf("ex docente: %+v", got)
	}
}

func TestMonitorKeptAcrossSync(t *testing.T) {
	s := newTestStore(t)
	prof := extUser(t, s, "d2l:user:1", "Profe", "p@uni.example")
	moni := extUser(t, s, "d2l:user:2", "Moni", "m@uni.example")
	other := extUser(t, s, "d2l:user:3", "Otra", "o@uni.example")
	c1, c2 := offering("d2l:ou:1", "Uno", true), offering("d2l:ou:2", "Dos", true)
	r := enroll(t, s, "d2l:user:1", ExternalEnrollment{Course: c1, Role: "teacher"})
	enroll(t, s, "d2l:user:2", ExternalEnrollment{Course: c2, Role: "student"})
	enroll(t, s, "d2l:user:3", ExternalEnrollment{Course: c1, Role: "student"})
	g := r.Groups[0].GroupID
	// El monitor es local: el docente elige a una persona ya registrada que no estudia en esa materia.
	if _, err := s.AddMonitor(prof.ID, g, "o@uni.example"); !errors.Is(err, ErrStudentOfCourse) {
		t.Fatalf("estudiante de la misma materia: %v", err)
	}
	if _, err := s.AddMonitor(prof.ID, g, "m@uni.example"); err != nil {
		t.Fatal(err)
	}
	// Una sincronización posterior conserva el perfil de monitor y la asignación.
	enroll(t, s, "d2l:user:2", ExternalEnrollment{Course: c2, Role: "student"})
	if got := s.MonitorGroups(moni.ID); len(got) != 1 {
		t.Fatalf("asignación de monitor: %+v", got)
	}
	if u, _ := s.UpsertExternalUser("d2l:user:2", "Moni", "m@uni.example"); u.User.Role != RoleMonitor {
		t.Fatalf("rol del monitor: %s", u.User.Role)
	}
	// Si Brightspace lo inscribe como estudiante de esa misma materia, manda Brightspace.
	enroll(t, s, "d2l:user:2", ExternalEnrollment{Course: c1, Role: "student"}, ExternalEnrollment{Course: c2, Role: "student"})
	if got := s.MonitorGroups(moni.ID); len(got) != 0 {
		t.Fatalf("monitor y estudiante a la vez: %+v", got)
	}
	_ = other
}

func members(ms ...RosterMember) []RosterMember { return ms }

func rosterStudent(ext, name, email string) RosterMember {
	return RosterMember{ExternalID: ext, Name: name, Email: email, Role: "student", IsActive: true}
}

func TestRoster(t *testing.T) {
	s := newTestStore(t)
	prof := extUser(t, s, "d2l:user:1", "Profe", "p@uni.example")
	r := enroll(t, s, "d2l:user:1", ExternalEnrollment{Course: offering("d2l:ou:1", "Curso", true), Role: "teacher"})
	g := r.Groups[0].GroupID
	teacher := RosterMember{ExternalID: "d2l:user:1", Name: "Profe", Email: "p@uni.example", Role: "teacher", IsActive: true}

	if _, err := s.UpsertRoster("d2l:ou:1", nil, 30); !errors.Is(err, ErrEmptyRoster) {
		t.Fatalf("lista vacía: %v", err)
	}
	if _, err := s.UpsertRoster("d2l:ou:999", members(teacher), 30); !errors.Is(err, ErrUnknownCourse) {
		t.Fatalf("curso desconocido: %v", err)
	}

	var all []RosterMember
	all = append(all, teacher)
	for i, n := range []string{"a", "b", "c", "d", "e"} {
		all = append(all, rosterStudent("d2l:user:"+string(rune('2'+i)), "Estudiante "+n, n+"@uni.example"))
	}
	res, err := s.UpsertRoster("d2l:ou:1", all, 30)
	mustNoErr(t, err)
	if res.CreatedUsers != 5 || res.Enrolled != 6 || res.Deactivated != 0 {
		t.Fatalf("alta de la lista: %+v", res)
	}
	detail, err := s.GroupForTeacher(prof.ID, g)
	mustNoErr(t, err)
	if len(detail.Students) != 5 {
		t.Fatalf("estudiantes: %d", len(detail.Students))
	}

	// Una lista que dejaría fuera a casi todos se rechaza y no cambia nada.
	if _, err := s.UpsertRoster("d2l:ou:1", members(teacher, all[1]), 30); !errors.Is(err, ErrRosterTooDestructive) {
		t.Fatalf("desactivación masiva: %v", err)
	}
	detail, _ = s.GroupForTeacher(prof.ID, g)
	if len(detail.Students) != 5 {
		t.Fatalf("la lista rechazada cambió datos: %d", len(detail.Students))
	}
	// Dentro del umbral (1 de 6 = 16 %) se desactiva sin borrar; el correo en conflicto se salta y conserva su inscripción.
	_, err = s.CreateUser("Local", "local@u.edu", "password123", RoleStudent)
	mustNoErr(t, err)
	conflict := rosterStudent("d2l:user:3", "Estudiante b", "local@u.edu")
	res, err = s.UpsertRoster("d2l:ou:1", members(append(all[:5:5], conflict)...), 30)
	mustNoErr(t, err)
	if res.Deactivated != 1 || len(res.Skipped) != 1 || res.Skipped[0].ExternalID != "d2l:user:3" {
		t.Fatalf("lista parcial: %+v", res)
	}
	detail, _ = s.GroupForTeacher(prof.ID, g)
	if len(detail.Students) != 4 {
		t.Fatalf("estudiantes tras desactivar: %d", len(detail.Students))
	}
	// Con umbral 100 % se acepta cualquier lista no vacía.
	res, err = s.UpsertRoster("d2l:ou:1", members(teacher), 100)
	mustNoErr(t, err)
	if res.Deactivated != 4 {
		t.Fatalf("umbral 100: %+v", res)
	}
	var inactive int
	mustNoErr(t, s.db.QueryRow(`SELECT COUNT(*) FROM group_students WHERE group_id = ? AND active = 0`, g).Scan(&inactive))
	if inactive != 5 {
		t.Fatalf("filas inactivas: %d", inactive)
	}
}

func TestUpsertAssignments(t *testing.T) {
	s := newTestStore(t)
	prof := extUser(t, s, "d2l:user:1", "Profe", "p@uni.example")
	ana := extUser(t, s, "d2l:user:2", "Ana", "a@uni.example")
	enroll(t, s, "d2l:user:1", ExternalEnrollment{Course: offering("d2l:ou:1", "Curso", true), Role: "teacher"})
	r := enroll(t, s, "d2l:user:2", ExternalEnrollment{Course: offering("d2l:ou:1", "Curso", true), Role: "student"})
	g := r.Groups[0].GroupID

	due := time.Date(2026, 11, 20, 4, 59, 0, 0, time.UTC)
	end := due.Add(48 * time.Hour)
	den := 100.0
	items := []ExternalAssignment{
		{ExternalID: "d2l:folder:701", Title: "Taller 1", Statement: "Enunciado", DueAt: &due, EndAt: &end, ScoreDenominator: &den, GradeItemID: "d2l:grade:5001"},
		{ExternalID: "d2l:folder:702", Title: "Taller 2", Statement: "Otro", DueAt: &due, EndAt: &due},
		{ExternalID: "d2l:folder:703", Title: "Borrador", IsHidden: true},
	}
	res, err := s.UpsertAssignments("d2l:ou:1", items)
	mustNoErr(t, err)
	if res.Created != 3 || res.Updated != 0 || res.Hidden != 1 {
		t.Fatalf("alta: %+v", res)
	}
	list, err := s.GroupAssignments(prof.ID, g)
	mustNoErr(t, err)
	if len(list) != 2 {
		t.Fatalf("talleres visibles del docente: %d", len(list))
	}
	by := map[string]Assignment{}
	for _, a := range list {
		by[a.Title] = a
	}
	t1, t2 := by["Taller 1"], by["Taller 2"]
	if !t1.AllowLate || t1.LateUntil == nil || !t1.LateUntil.Equal(end) || t2.AllowLate || t2.LateUntil != nil {
		t.Fatalf("plazos derivados: %+v %+v", t1, t2)
	}
	var denom float64
	var grade string
	mustNoErr(t, s.db.QueryRow(`SELECT score_denominator, grade_item_id FROM assignments WHERE id = ?`, t1.ID).Scan(&denom, &grade))
	if denom != 100 || grade != "d2l:grade:5001" {
		t.Fatalf("denominador/ítem: %v %q", denom, grade)
	}
	if got := s.StudentAssignments(ana.ID); len(got) != 2 {
		t.Fatalf("talleres del estudiante: %d", len(got))
	}

	// El docente completa lo que es de EduTrace; título y plazos son de solo lectura.
	pen := 0.5
	got, err := s.UpdateAssignment(prof.ID, t1.ID, "Cambiado", "Cambiado", "Criterios del docente", nil,
		AssignmentPolicy{AllowLate: false, LatePenalty: &pen, Evaluation: false}, nil, nil)
	mustNoErr(t, err)
	if got.Title != "Taller 1" || got.Statement != "Enunciado" || got.DueAt == nil || !got.DueAt.Equal(due) || !got.AllowLate ||
		got.Rubric != "Criterios del docente" || got.LatePenalty == nil || *got.LatePenalty != 0.5 {
		t.Fatalf("edición de un taller espejado: %+v", got)
	}
	_, err = s.SetAssignmentTests(prof.ID, t1.ID, []model.TestCase{{Name: "caso", Stdin: "1", Expected: "1", Match: "contiene"}})
	mustNoErr(t, err)

	// Re-sincronizar cambia título/plazos pero no toca rubric, penalización ni casos; lo que falta se oculta.
	due2 := due.Add(24 * time.Hour)
	res, err = s.UpsertAssignments("d2l:ou:1", []ExternalAssignment{{ExternalID: "d2l:folder:701", Title: "Taller 1 (nuevo título)", DueAt: &due2, EndAt: &end}})
	mustNoErr(t, err)
	if res.Created != 0 || res.Updated != 1 || res.Hidden != 1 {
		t.Fatalf("segunda sincronización: %+v", res)
	}
	list, _ = s.GroupAssignments(prof.ID, g)
	if len(list) != 1 || list[0].Title != "Taller 1 (nuevo título)" || list[0].Rubric != "Criterios del docente" ||
		list[0].LatePenalty == nil || len(list[0].Tests) != 1 || !list[0].DueAt.Equal(due2) {
		t.Fatalf("tras re-sincronizar: %+v", list)
	}
	var total int
	mustNoErr(t, s.db.QueryRow(`SELECT COUNT(*) FROM assignments WHERE group_id = ?`, g).Scan(&total))
	if total != 3 {
		t.Fatalf("no se borra nada: %d", total)
	}
	if got := s.StudentAssignments(ana.ID); len(got) != 1 {
		t.Fatalf("el estudiante no debe ver talleres ocultos: %d", len(got))
	}
	// Un taller oculto que reaparece vuelve a verse.
	_, err = s.UpsertAssignments("d2l:ou:1", []ExternalAssignment{{ExternalID: "d2l:folder:702", Title: "Taller 2", DueAt: &due}})
	mustNoErr(t, err)
	if got := s.StudentAssignments(ana.ID); len(got) != 1 || got[0].Title != "Taller 2" {
		t.Fatalf("reaparece: %+v", got)
	}

	if _, err := s.UpsertAssignments("d2l:ou:999", nil); !errors.Is(err, ErrUnknownCourse) {
		t.Fatalf("curso desconocido: %v", err)
	}
	if _, err := s.UpsertAssignments("d2l:ou:1", []ExternalAssignment{{ExternalID: "d2l:folder:1", Title: " "}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("sin título: %v", err)
	}
	if _, err := s.UpsertAssignments("d2l:ou:1", []ExternalAssignment{{ExternalID: "d2l:folder:1", Title: "A"}, {ExternalID: "d2l:folder:1", Title: "B"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("repetido: %v", err)
	}
	zero := 0.0
	if _, err := s.UpsertAssignments("d2l:ou:1", []ExternalAssignment{{ExternalID: "d2l:folder:1", Title: "A", ScoreDenominator: &zero}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("denominador 0: %v", err)
	}
	// Una carpeta de otro curso no se puede mover a este.
	enroll(t, s, "d2l:user:1", ExternalEnrollment{Course: offering("d2l:ou:1", "Curso", true), Role: "teacher"}, ExternalEnrollment{Course: offering("d2l:ou:2", "Otro", true), Role: "teacher"})
	if _, err := s.UpsertAssignments("d2l:ou:2", []ExternalAssignment{{ExternalID: "d2l:folder:701", Title: "Robado"}}); !errors.Is(err, ErrExternalConflict) {
		t.Fatalf("carpeta de otro curso: %v", err)
	}
}

// attachmentCourse deja un curso espejado con un docente y un estudiante, y devuelve el id de la clase.
func attachmentCourse(t *testing.T, s *Store) (prof, ana *User, group string) {
	t.Helper()
	prof = extUser(t, s, "d2l:user:1", "Profe", "p@uni.example")
	ana = extUser(t, s, "d2l:user:2", "Ana", "a@uni.example")
	enroll(t, s, "d2l:user:1", ExternalEnrollment{Course: offering("d2l:ou:1", "Curso", true), Role: "teacher"})
	r := enroll(t, s, "d2l:user:2", ExternalEnrollment{Course: offering("d2l:ou:1", "Curso", true), Role: "student"})
	return prof, ana, r.Groups[0].GroupID
}

func assignmentIDByExternal(t *testing.T, s *Store, ext string) string {
	t.Helper()
	var id string
	mustNoErr(t, s.db.QueryRow(`SELECT id FROM assignments WHERE external_id = ?`, ext).Scan(&id))
	return id
}

func atts(list ...ExternalAttachment) *[]ExternalAttachment { return &list }

func TestUpsertAssignmentsReportsFilesNeeded(t *testing.T) {
	s := newTestStore(t)
	attachmentCourse(t, s)

	items := []ExternalAssignment{
		{ExternalID: "d2l:folder:701", Title: "Taller 1", Attachments: atts(
			ExternalAttachment{FileID: "11", Name: "uno.pdf", Size: 5}, ExternalAttachment{FileID: "12", Name: "dos.pdf", Size: 6})},
		{ExternalID: "d2l:folder:702", Title: "Sin datos de adjuntos"}, // Attachments ausente: no se toca nada
		{ExternalID: "d2l:folder:703", Title: "Oculto", IsHidden: true, Attachments: atts(ExternalAttachment{FileID: "21", Name: "x.pdf", Size: 1})},
		{ExternalID: "d2l:folder:704", Title: "Enorme", Attachments: atts(ExternalAttachment{FileID: "31", Name: "grande.pdf", Size: MaxAssignmentFileSize + 1})},
	}
	res, err := s.UpsertAssignments("d2l:ou:1", items)
	mustNoErr(t, err)
	// Solo se piden los de talleres visibles y que Go aceptaría (<= 10 MB).
	if len(res.FilesNeeded) != 2 || res.FilesNeeded[0] != (FileNeeded{Folder: "d2l:folder:701", FileID: "11", Name: "uno.pdf", Size: 5}) ||
		res.FilesNeeded[1].FileID != "12" {
		t.Fatalf("archivos pedidos: %+v", res.FilesNeeded)
	}

	// Tras guardar el 11 (5 bytes), ya no se pide. El 12 se guarda con otro tamaño del anunciado y se vuelve a pedir si D2L lo cambia.
	_, err = s.SaveExternalAssignmentFile("d2l:ou:1", "d2l:folder:701", "11", "uno.pdf", "application/pdf", strings.NewReader("12345"), MaxAssignmentFileSize)
	mustNoErr(t, err)
	_, err = s.SaveExternalAssignmentFile("d2l:ou:1", "d2l:folder:701", "12", "dos.pdf", "application/pdf", strings.NewReader("123456"), MaxAssignmentFileSize)
	mustNoErr(t, err)
	res, err = s.UpsertAssignments("d2l:ou:1", items)
	mustNoErr(t, err)
	if len(res.FilesNeeded) != 0 {
		t.Fatalf("no debe pedir lo que ya tiene: %+v", res.FilesNeeded)
	}
	items[0].Attachments = atts(ExternalAttachment{FileID: "11", Name: "uno.pdf", Size: 5}, ExternalAttachment{FileID: "12", Name: "dos.pdf", Size: 7})
	res, err = s.UpsertAssignments("d2l:ou:1", items)
	mustNoErr(t, err)
	if len(res.FilesNeeded) != 1 || res.FilesNeeded[0].FileID != "12" || res.FilesNeeded[0].Size != 7 {
		t.Fatalf("un archivo cambiado de tamaño se pide otra vez: %+v", res.FilesNeeded)
	}

	// Un archivo que el docente subió a mano (sin external_id) nunca se toca; el que D2L quitó sí se borra.
	aid := assignmentIDByExternal(t, s, "d2l:folder:701")
	mustNoErr(t, os.MkdirAll(s.assignmentDir(aid), 0o700))
	mustNoErr(t, os.WriteFile(filepath.Join(s.assignmentDir(aid), "manual1"), []byte("mio"), 0o600))
	_, err = s.db.Exec(`INSERT INTO assignment_files (id, assignment_id, name, size, content_type, position) VALUES ('manual1', ?, 'mio.txt', 3, 'text/plain', 99)`, aid)
	mustNoErr(t, err)
	var oldID string
	mustNoErr(t, s.db.QueryRow(`SELECT id FROM assignment_files WHERE assignment_id = ? AND external_id = '11'`, aid).Scan(&oldID))
	items[0].Attachments = atts(ExternalAttachment{FileID: "12", Name: "dos.pdf", Size: 7})
	_, err = s.UpsertAssignments("d2l:ou:1", items)
	mustNoErr(t, err)
	var remaining []string
	rows, err := s.db.Query(`SELECT name FROM assignment_files WHERE assignment_id = ? ORDER BY position`, aid)
	mustNoErr(t, err)
	for rows.Next() {
		var n string
		mustNoErr(t, rows.Scan(&n))
		remaining = append(remaining, n)
	}
	rows.Close()
	if strings.Join(remaining, ",") != "dos.pdf,mio.txt" {
		t.Fatalf("archivos tras quitar el 11 en D2L: %v", remaining)
	}
	if _, err := os.Stat(filepath.Join(s.assignmentDir(aid), oldID)); !os.IsNotExist(err) {
		t.Fatalf("el archivo quitado debe borrarse del disco: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.assignmentDir(aid), "manual1")); err != nil {
		t.Fatalf("el archivo del docente no se toca: %v", err)
	}

	// Una lista de adjuntos vacía (no ausente) quita todos los de D2L y deja el del docente.
	items[0].Attachments = atts()
	_, err = s.UpsertAssignments("d2l:ou:1", items)
	mustNoErr(t, err)
	var n int
	mustNoErr(t, s.db.QueryRow(`SELECT COUNT(*) FROM assignment_files WHERE assignment_id = ?`, aid).Scan(&n))
	if n != 1 {
		t.Fatalf("solo debe quedar el archivo del docente: %d", n)
	}

	// Datos inválidos.
	for _, bad := range []ExternalAttachment{{FileID: "", Name: "a.pdf", Size: 1}, {FileID: "../x", Name: "a.pdf", Size: 1}, {FileID: "1", Name: "a.pdf", Size: -1}} {
		if _, err := s.UpsertAssignments("d2l:ou:1", []ExternalAssignment{{ExternalID: "d2l:folder:701", Title: "T", Attachments: atts(bad)}}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("adjunto inválido %+v: %v", bad, err)
		}
	}
}

func TestSaveExternalAssignmentFile(t *testing.T) {
	s := newTestStore(t)
	_, ana, _ := attachmentCourse(t, s)
	_, err := s.UpsertAssignments("d2l:ou:1", []ExternalAssignment{{ExternalID: "d2l:folder:701", Title: "Taller 1"}})
	mustNoErr(t, err)
	aid := assignmentIDByExternal(t, s, "d2l:folder:701")
	save := func(course, folder, file, body string, max int64) (*AssignmentFile, error) {
		return s.SaveExternalAssignmentFile(course, folder, file, "../Taller 1.pdf", "application/pdf", strings.NewReader(body), max)
	}

	if _, err := save("d2l:ou:404", "d2l:folder:701", "11", "x", 100); !errors.Is(err, ErrUnknownCourse) {
		t.Fatalf("curso desconocido: %v", err)
	}
	if _, err := save("d2l:ou:1", "d2l:folder:404", "11", "x", 100); !errors.Is(err, ErrUnknownAssignment) {
		t.Fatalf("taller desconocido: %v", err)
	}
	if _, err := save("d2l:ou:1", "d2l:folder:701", "../11", "x", 100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("id de archivo inválido: %v", err)
	}
	if _, err := save("d2l:ou:1", "d2l:folder:701", "11", "", 100); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("archivo vacío: %v", err)
	}
	if _, err := save("d2l:ou:1", "d2l:folder:701", "11", "0123456789", 4); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("archivo grande: %v", err)
	}
	if entries, _ := os.ReadDir(s.assignmentDir(aid)); len(entries) != 0 {
		t.Fatalf("un rechazo no debe dejar archivos en disco: %d", len(entries))
	}

	f, err := save("d2l:ou:1", "d2l:folder:701", "11", "contenido-1", 100)
	mustNoErr(t, err)
	if f.Name != "Taller 1.pdf" || f.Size != 11 || f.ContentType != "application/pdf" {
		t.Fatalf("archivo guardado: %+v", f)
	}
	got, err := os.ReadFile(filepath.Join(s.assignmentDir(aid), f.ID))
	mustNoErr(t, err)
	if string(got) != "contenido-1" {
		t.Fatalf("contenido en disco: %q", got)
	}
	// El estudiante lo ve en el taller y puede descargarlo.
	list := s.StudentAssignments(ana.ID)
	if len(list) != 1 || len(list[0].Files) != 1 || list[0].Files[0].ID != f.ID {
		t.Fatalf("el estudiante debe ver el archivo: %+v", list)
	}
	path, meta, err := s.AssignmentFilePath(ana, aid, f.ID)
	mustNoErr(t, err)
	if meta.Name != "Taller 1.pdf" {
		t.Fatalf("meta: %+v", meta)
	}
	if b, _ := os.ReadFile(path); string(b) != "contenido-1" {
		t.Fatalf("descarga: %q", b)
	}

	// Repetir el mismo file_id reemplaza el contenido sin duplicar la fila ni dejar el archivo viejo.
	f2, err := save("d2l:ou:1", "d2l:folder:701", "11", "nuevo", 100)
	mustNoErr(t, err)
	var n int
	mustNoErr(t, s.db.QueryRow(`SELECT COUNT(*) FROM assignment_files WHERE assignment_id = ?`, aid).Scan(&n))
	if n != 1 || f2.Size != 5 {
		t.Fatalf("reemplazo: filas=%d archivo=%+v", n, f2)
	}
	if entries, _ := os.ReadDir(s.assignmentDir(aid)); len(entries) != 1 {
		t.Fatalf("el archivo viejo debe borrarse: %d", len(entries))
	}

	// Tope de 10 archivos por taller (contando los del docente).
	for i := 1; i < MaxAssignmentFiles; i++ {
		_, err := s.db.Exec(`INSERT INTO assignment_files (id, assignment_id, name, size, content_type, position) VALUES (?, ?, 'm.txt', 1, '', ?)`,
			"manual"+strings.Repeat("x", i), aid, 10+i)
		mustNoErr(t, err)
	}
	if _, err := save("d2l:ou:1", "d2l:folder:701", "12", "otro", 100); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("más de 10 archivos: %v", err)
	}
	// Reemplazar uno existente sigue permitido con el taller lleno.
	if _, err := save("d2l:ou:1", "d2l:folder:701", "11", "otra vez", 100); err != nil {
		t.Fatalf("reemplazo con el taller lleno: %v", err)
	}
}

func TestSessionTickets(t *testing.T) {
	s := newTestStore(t)
	u := extUser(t, s, "d2l:user:3101", "Camila", "c@uni.example")

	if _, _, err := s.IssueSessionTicket("d2l:user:404"); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("usuario desconocido: %v", err)
	}
	ticket, ttl, err := s.IssueSessionTicket("d2l:user:3101")
	mustNoErr(t, err)
	if len(ticket) != 64 || ttl != TicketTTL {
		t.Fatalf("ticket: %q %v", ticket, ttl)
	}
	var stored int
	mustNoErr(t, s.db.QueryRow(`SELECT COUNT(*) FROM session_tickets WHERE token_hash = ? OR token_hash = ?`, ticket, hashToken(ticket)).Scan(&stored))
	var plain int
	mustNoErr(t, s.db.QueryRow(`SELECT COUNT(*) FROM session_tickets WHERE token_hash = ?`, ticket).Scan(&plain))
	if stored != 1 || plain != 0 {
		t.Fatalf("solo debe guardarse el hash (guardados=%d, en claro=%d)", stored, plain)
	}
	got, err := s.ConsumeSessionTicket(ticket)
	mustNoErr(t, err)
	if got.ID != u.ID {
		t.Fatalf("usuario del ticket: %+v", got)
	}
	if _, err := s.ConsumeSessionTicket(ticket); !errors.Is(err, ErrInvalidTicket) {
		t.Fatalf("segundo uso: %v", err)
	}
	for _, bad := range []string{"", "nada", strings.Repeat("a", 64), strings.Repeat("a", 500)} {
		if _, err := s.ConsumeSessionTicket(bad); !errors.Is(err, ErrInvalidTicket) {
			t.Fatalf("ticket %q: %v", bad, err)
		}
	}

	s.SetTicketTTL(-time.Second)
	expired, _, err := s.IssueSessionTicket("d2l:user:3101")
	mustNoErr(t, err)
	if _, err := s.ConsumeSessionTicket(expired); !errors.Is(err, ErrInvalidTicket) {
		t.Fatalf("ticket vencido: %v", err)
	}
	s.SetTicketTTL(TicketTTL)

	// Revocar: sesiones y tickets pendientes del usuario.
	pending, _, err := s.IssueSessionTicket("d2l:user:3101")
	mustNoErr(t, err)
	token, _, err := s.CreateSession(u.ID)
	mustNoErr(t, err)
	n, err := s.RevokeExternalSessions("d2l:user:3101")
	mustNoErr(t, err)
	if n != 1 {
		t.Fatalf("sesiones revocadas: %d", n)
	}
	if _, ok := s.UserForSession(token); ok {
		t.Fatal("la sesión sigue viva")
	}
	if _, err := s.ConsumeSessionTicket(pending); !errors.Is(err, ErrInvalidTicket) {
		t.Fatalf("ticket pendiente tras revocar: %v", err)
	}
	if _, err := s.RevokeExternalSessions("d2l:user:404"); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("revocar desconocido: %v", err)
	}
}

func TestCookieJar(t *testing.T) {
	s := newTestStore(t)
	u := extUser(t, s, "d2l:user:1", "Camila", "c@uni.example")
	v := extUser(t, s, "d2l:user:2", "Otro", "o@uni.example")
	if err := s.SaveJarCookie(u.ID, JarCookie{Name: "access_token", Value: "x"}); !errors.Is(err, ErrJarDisabled) {
		t.Fatalf("sin clave: %v", err)
	}
	key := []byte(strings.Repeat("k", 32))
	mustNoErr(t, s.SetCookieJarKey(key))
	mustNoErr(t, s.SaveJarCookie(u.ID, JarCookie{Name: "access_token", Value: "secreto-1"}))
	mustNoErr(t, s.SaveJarCookie(u.ID, JarCookie{Name: "refresh_token", Value: "secreto-2", Path: "/auth"}))
	mustNoErr(t, s.SaveJarCookie(u.ID, JarCookie{Name: "access_token", Value: "secreto-3"})) // reemplaza
	past := time.Now().Add(-time.Hour)
	mustNoErr(t, s.SaveJarCookie(u.ID, JarCookie{Name: "viejo", Value: "z", ExpiresAt: &past}))
	mustNoErr(t, s.SaveJarCookie(v.ID, JarCookie{Name: "access_token", Value: "ajeno"}))

	var raw string
	mustNoErr(t, s.db.QueryRow(`SELECT value FROM python_cookie_jar WHERE user_id = ? AND name = 'access_token'`, u.ID).Scan(&raw))
	if strings.Contains(raw, "secreto") {
		t.Fatalf("el valor debe ir cifrado: %q", raw)
	}
	jar, err := s.LoadJar(u.ID)
	mustNoErr(t, err)
	if len(jar) != 2 || jar[0].Name != "access_token" || jar[0].Value != "secreto-3" || jar[1].Name != "refresh_token" || jar[1].Value != "secreto-2" || jar[1].Path != "/auth" {
		t.Fatalf("jar: %+v", jar)
	}
	// Una fila copiada a otro usuario no descifra.
	_, err = s.db.Exec(`UPDATE python_cookie_jar SET value = (SELECT value FROM python_cookie_jar WHERE user_id = ? AND name = 'access_token') WHERE user_id = ?`, u.ID, v.ID)
	mustNoErr(t, err)
	if _, err := s.LoadJar(v.ID); !errors.Is(err, ErrJarCorrupt) {
		t.Fatalf("fila trasplantada: %v", err)
	}
	mustNoErr(t, s.DeleteJar(u.ID))
	if jar, _ := s.LoadJar(u.ID); len(jar) != 0 {
		t.Fatalf("tras borrar: %+v", jar)
	}
	// Revocar sesiones también vacía el jar.
	mustNoErr(t, s.SaveJarCookie(u.ID, JarCookie{Name: "access_token", Value: "otra"}))
	_, err = s.RevokeExternalSessions("d2l:user:1")
	mustNoErr(t, err)
	if jar, _ := s.LoadJar(u.ID); len(jar) != 0 {
		t.Fatalf("tras revocar: %+v", jar)
	}
}

// Una base creada con el esquema anterior (sin columnas externas ni inscripciones activas) debe abrirse,
// migrarse y seguir funcionando con sus datos.
func TestMigrateLegacyDatabase(t *testing.T) {
	dir := t.TempDir()
	ddl, err := os.ReadFile(filepath.Join("testdata", "schema_legacy.sql"))
	mustNoErr(t, err)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "edutrace.db")+"?_pragma=foreign_keys(1)")
	mustNoErr(t, err)
	_, err = db.Exec(string(ddl))
	mustNoErr(t, err)
	for _, q := range []string{
		`INSERT INTO users (id, name, email, role, password_hash, created_at) VALUES ('t1', 'Profe', 'p@u.edu', 'teacher', 'x', '2026-01-01T00:00:00.000000000Z')`,
		`INSERT INTO users (id, name, email, role, password_hash, created_at) VALUES ('s1', 'Ana', 'a@u.edu', 'student', 'x', '2026-01-01T00:00:00.000000000Z')`,
		`INSERT INTO courses (id, teacher_id, name, created_at) VALUES ('c1', 't1', 'Curso', '2026-01-01T00:00:00.000000000Z')`,
		`INSERT INTO class_groups (id, course_id, name, join_code, created_at) VALUES ('g1', 'c1', 'G', 'ABC234', '2026-01-01T00:00:00.000000000Z')`,
		`INSERT INTO group_students (group_id, student_id, joined_at) VALUES ('g1', 's1', '2026-01-01T00:00:00.000000000Z')`,
	} {
		_, err = db.Exec(q)
		mustNoErr(t, err)
	}
	mustNoErr(t, db.Close())

	s, err := OpenStore(dir, time.Hour)
	mustNoErr(t, err)
	defer s.Close()
	if !s.IsEnrolled("s1", "g1") {
		t.Fatal("la inscripción existente debe quedar activa")
	}
	if got := s.TeacherCourses("t1"); len(got) != 1 || len(got[0].Groups) != 1 || got[0].Groups[0].JoinCode != "ABC234" {
		t.Fatalf("curso local tras migrar: %+v", got)
	}
	for _, c := range []struct{ table, column string }{
		{"users", "external_id"}, {"courses", "external_id"}, {"class_groups", "external_id"}, {"assignments", "external_id"},
		{"submissions", "external_id"}, {"assignments", "score_denominator"}, {"assignments", "grade_item_id"}, {"assignments", "is_hidden"},
		{"group_students", "active"}, {"course_teachers", "active"}, {"submissions", "d2l_status"}, {"reviews", "d2l_status"},
		{"assignment_files", "external_id"},
	} {
		var n int
		mustNoErr(t, s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n))
		if n != 1 {
			t.Fatalf("falta %s.%s", c.table, c.column)
		}
	}
	// Y la integración funciona sobre la base migrada; reabrirla es idempotente.
	r, err := s.UpsertExternalUser("d2l:user:1", "Nuevo", "n@uni.example")
	mustNoErr(t, err)
	if !r.Created {
		t.Fatal("alta sobre base migrada")
	}
	mustNoErr(t, s.Close())
	s, err = OpenStore(dir, time.Hour)
	mustNoErr(t, err)
	var users int
	mustNoErr(t, s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE external_id IS NOT NULL`).Scan(&users))
	if users != 1 {
		t.Fatalf("usuarios externos: %d", users)
	}
}
