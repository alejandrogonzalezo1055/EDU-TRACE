package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const usersWithMonitorSQL = `CREATE TABLE users_new (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  email TEXT NOT NULL UNIQUE,
  role TEXT NOT NULL CHECK (role IN ('student', 'teacher', 'monitor')),
  password_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  compiles INTEGER NOT NULL DEFAULT 0,
  feedbacks INTEGER NOT NULL DEFAULT 0,
  last_active_at TEXT
)`

var addedColumns = []struct{ table, column, ddl string }{
	{"users", "recovery_hash", `ALTER TABLE users ADD COLUMN recovery_hash TEXT NOT NULL DEFAULT ''`},
	{"assignments", "allow_late", `ALTER TABLE assignments ADD COLUMN allow_late INTEGER NOT NULL DEFAULT 1`},
	{"assignments", "late_until", `ALTER TABLE assignments ADD COLUMN late_until TEXT`},
	{"assignments", "late_penalty", `ALTER TABLE assignments ADD COLUMN late_penalty REAL`},
	{"users", "consent_at", `ALTER TABLE users ADD COLUMN consent_at TEXT`},
	{"users", "consent_version", `ALTER TABLE users ADD COLUMN consent_version TEXT NOT NULL DEFAULT ''`},
	{"courses", "review_minutes_max", `ALTER TABLE courses ADD COLUMN review_minutes_max REAL`},
	{"courses", "batch_hours_max", `ALTER TABLE courses ADD COLUMN batch_hours_max REAL`},
	{"courses", "feedback_hours_max", `ALTER TABLE courses ADD COLUMN feedback_hours_max REAL`},
	{"class_groups", "ai_mode", `ALTER TABLE class_groups ADD COLUMN ai_mode TEXT NOT NULL DEFAULT 'supervisada' CHECK (ai_mode IN ('supervisada', 'inmediata'))`},
	{"reviews", "ai_discarded_at", `ALTER TABLE reviews ADD COLUMN ai_discarded_at TEXT`},
	{"reviews", "feedback_origin", `ALTER TABLE reviews ADD COLUMN feedback_origin TEXT NOT NULL DEFAULT ''`},
	{"reviews", "review_ms", `ALTER TABLE reviews ADD COLUMN review_ms INTEGER NOT NULL DEFAULT 0`},
	{"ai_requests", "quality_ok", `ALTER TABLE ai_requests ADD COLUMN quality_ok INTEGER`},
	{"reviews", "ai_minutes", `ALTER TABLE reviews ADD COLUMN ai_minutes INTEGER`},
	{"feedback_ratings", "assignment_id", `ALTER TABLE feedback_ratings ADD COLUMN assignment_id TEXT REFERENCES assignments(id) ON DELETE SET NULL`},
	{"users", "email_notifications", `ALTER TABLE users ADD COLUMN email_notifications INTEGER NOT NULL DEFAULT 1`},
	{"users", "reset_hash", `ALTER TABLE users ADD COLUMN reset_hash TEXT NOT NULL DEFAULT ''`},
	{"users", "reset_expires_at", `ALTER TABLE users ADD COLUMN reset_expires_at TEXT`},
	{"courses", "closed_at", `ALTER TABLE courses ADD COLUMN closed_at TEXT`},
	{"assignments", "evaluation", `ALTER TABLE assignments ADD COLUMN evaluation INTEGER NOT NULL DEFAULT 0`},
	{"reviews", "ai_model", `ALTER TABLE reviews ADD COLUMN ai_model TEXT NOT NULL DEFAULT ''`},
	{"reviews", "ai_prompt", `ALTER TABLE reviews ADD COLUMN ai_prompt TEXT NOT NULL DEFAULT ''`},
	{"ai_requests", "model", `ALTER TABLE ai_requests ADD COLUMN model TEXT NOT NULL DEFAULT ''`},
	{"ai_requests", "prompt_version", `ALTER TABLE ai_requests ADD COLUMN prompt_version TEXT NOT NULL DEFAULT ''`},

	// Integración con Brightspace (Python → Go). Los ID externos llevan índice único en externalIndexes.
	{"users", "external_id", `ALTER TABLE users ADD COLUMN external_id TEXT`},
	{"courses", "external_id", `ALTER TABLE courses ADD COLUMN external_id TEXT`},
	{"courses", "starts_at", `ALTER TABLE courses ADD COLUMN starts_at TEXT`},
	{"courses", "ends_at", `ALTER TABLE courses ADD COLUMN ends_at TEXT`},
	{"courses", "external_active", `ALTER TABLE courses ADD COLUMN external_active INTEGER NOT NULL DEFAULT 1`},
	{"class_groups", "external_id", `ALTER TABLE class_groups ADD COLUMN external_id TEXT`},
	{"group_students", "active", `ALTER TABLE group_students ADD COLUMN active INTEGER NOT NULL DEFAULT 1`},
	{"group_students", "ended_at", `ALTER TABLE group_students ADD COLUMN ended_at TEXT`},
	{"course_teachers", "active", `ALTER TABLE course_teachers ADD COLUMN active INTEGER NOT NULL DEFAULT 1`},
	{"course_teachers", "ended_at", `ALTER TABLE course_teachers ADD COLUMN ended_at TEXT`},
	{"assignments", "external_id", `ALTER TABLE assignments ADD COLUMN external_id TEXT`},
	{"assignments", "score_denominator", `ALTER TABLE assignments ADD COLUMN score_denominator REAL`},
	{"assignments", "grade_item_id", `ALTER TABLE assignments ADD COLUMN grade_item_id TEXT`},
	{"assignments", "is_hidden", `ALTER TABLE assignments ADD COLUMN is_hidden INTEGER NOT NULL DEFAULT 0`},
	{"submissions", "external_id", `ALTER TABLE submissions ADD COLUMN external_id TEXT`},
	{"submissions", "d2l_status", `ALTER TABLE submissions ADD COLUMN d2l_status TEXT NOT NULL DEFAULT '' CHECK (d2l_status IN ('', 'pendiente', 'enviada', 'fallida'))`},
	{"submissions", "d2l_attempts", `ALTER TABLE submissions ADD COLUMN d2l_attempts INTEGER NOT NULL DEFAULT 0`},
	{"submissions", "d2l_next_retry_at", `ALTER TABLE submissions ADD COLUMN d2l_next_retry_at TEXT`},
	{"submissions", "d2l_error", `ALTER TABLE submissions ADD COLUMN d2l_error TEXT NOT NULL DEFAULT ''`},
	{"submissions", "d2l_sent_at", `ALTER TABLE submissions ADD COLUMN d2l_sent_at TEXT`},
	{"reviews", "d2l_status", `ALTER TABLE reviews ADD COLUMN d2l_status TEXT NOT NULL DEFAULT '' CHECK (d2l_status IN ('', 'pendiente', 'enviada', 'fallida'))`},
	{"reviews", "d2l_attempts", `ALTER TABLE reviews ADD COLUMN d2l_attempts INTEGER NOT NULL DEFAULT 0`},
	{"reviews", "d2l_next_retry_at", `ALTER TABLE reviews ADD COLUMN d2l_next_retry_at TEXT`},
	{"reviews", "d2l_error", `ALTER TABLE reviews ADD COLUMN d2l_error TEXT NOT NULL DEFAULT ''`},
	{"reviews", "d2l_published_at", `ALTER TABLE reviews ADD COLUMN d2l_published_at TEXT`},
	{"reviews", "d2l_teacher_id", `ALTER TABLE reviews ADD COLUMN d2l_teacher_id TEXT NOT NULL DEFAULT ''`},
}

// Índices únicos parciales: las filas locales (sin ID externo) no entran. Van aquí y no en schema.sql
// porque en una base existente las columnas se agregan después de ejecutar el esquema.
var externalIndexes = []string{
	`CREATE UNIQUE INDEX IF NOT EXISTS users_external_id ON users(external_id) WHERE external_id IS NOT NULL`,
	`CREATE UNIQUE INDEX IF NOT EXISTS courses_external_id ON courses(external_id) WHERE external_id IS NOT NULL`,
	`CREATE UNIQUE INDEX IF NOT EXISTS class_groups_external_id ON class_groups(external_id) WHERE external_id IS NOT NULL`,
	`CREATE UNIQUE INDEX IF NOT EXISTS assignments_external_id ON assignments(external_id) WHERE external_id IS NOT NULL`,
	`CREATE UNIQUE INDEX IF NOT EXISTS submissions_external_id ON submissions(external_id) WHERE external_id IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS submissions_d2l_pending ON submissions(d2l_status, d2l_next_retry_at) WHERE d2l_status IN ('pendiente', 'fallida')`,
	`CREATE INDEX IF NOT EXISTS reviews_d2l_pending ON reviews(d2l_status, d2l_next_retry_at) WHERE d2l_status IN ('pendiente', 'fallida')`,
	`CREATE INDEX IF NOT EXISTS session_tickets_expires ON session_tickets(expires_at)`,
}

func (s *Store) upgradeSchema() error {
	if err := s.upgradeUsersRole(); err != nil {
		return err
	}
	for _, c := range addedColumns {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := s.db.Exec(c.ddl); err != nil {
				return fmt.Errorf("columna %s.%s: %w", c.table, c.column, err)
			}
		}
	}
	for _, ddl := range externalIndexes {
		if _, err := s.db.Exec(ddl); err != nil {
			return fmt.Errorf("índice externo: %w", err)
		}
	}
	if err := s.upgradeCourseStaffView(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT INTO ai_jobs (submission_id, status, enqueued_at)
		SELECT s.id, 'pendiente', s.created_at FROM submissions s
		LEFT JOIN reviews r ON r.submission_id = s.id
		WHERE s.kind = 'official' AND NOT EXISTS (SELECT 1 FROM ai_jobs j WHERE j.submission_id = s.id)
			AND (r.submission_id IS NULL OR (r.status = 'draft' AND r.ai_at IS NULL AND r.ai_discarded_at IS NULL))`); err != nil {
		return fmt.Errorf("cola de propuestas: %w", err)
	}
	return nil
}

func (s *Store) upgradeUsersRole() error {
	var ddl string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'users'`).Scan(&ddl); err != nil {
		return err
	}
	if strings.Contains(ddl, "'monitor'") {
		return nil
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		usersWithMonitorSQL,
		`INSERT INTO users_new (id, name, email, role, password_hash, created_at, compiles, feedbacks, last_active_at)
			SELECT id, name, email, role, password_hash, created_at, compiles, feedbacks, last_active_at FROM users`,
		`DROP TABLE users`,
		`ALTER TABLE users_new RENAME TO users`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("tabla users: %w", err)
		}
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	broken := rows.Next()
	rows.Close()
	if broken {
		return errors.New("tabla users: referencias rotas")
	}
	return tx.Commit()
}

// upgradeCourseStaffView recrea la vista course_staff en bases creadas antes de las inscripciones
// activas/inactivas: la definición vieja no mira course_teachers.active ni courses.external_id.
func (s *Store) upgradeCourseStaffView() error {
	var ddl string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'view' AND name = 'course_staff'`).Scan(&ddl); err != nil {
		return err
	}
	if strings.Contains(ddl, "active = 1") {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DROP VIEW course_staff`,
		`CREATE VIEW course_staff AS
			SELECT id AS course_id, teacher_id FROM courses WHERE external_id IS NULL
			UNION SELECT course_id, teacher_id FROM course_teachers WHERE active = 1`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("vista course_staff: %w", err)
		}
	}
	return tx.Commit()
}
