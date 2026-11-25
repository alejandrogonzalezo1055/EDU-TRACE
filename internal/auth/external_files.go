package auth

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Adjuntos de los talleres espejados de Brightspace. Python informa los adjuntos de cada carpeta dentro
// de PUT .../assignments (campo Attachments); Go responde cuáles le faltan (files_needed) y Python los
// descarga de Brightspace y los envía uno a uno con SaveExternalAssignmentFile.

var (
	ErrUnknownAssignment = errors.New("taller desconocido")
	ErrFileTooLarge = errors.New("el archivo supera el tamaño máximo permitido")
)

// externalFileIDPattern: el FileId de un adjunto de Brightspace (hoy un entero; se acepta texto corto
// sin separadores de ruta).
var externalFileIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}$`)

// ExternalAttachment es un adjunto de una carpeta de Brightspace (FileId, FileName, Size).
type ExternalAttachment struct {
	FileID string
	Name string
	Size int64
}

// FileNeeded es un adjunto que Go aún no tiene y que Python debe descargar de Brightspace y enviar
// con PUT .../assignments/{folder}/files/{file_id}.
type FileNeeded struct {
	Folder string `json:"folder"`
	FileID string `json:"file_id"`
	Name string `json:"name"`
	Size int64 `json:"size"`
}

type staleFile struct{ assignmentID, fileID string }

func validateAttachments(folder string, list *[]ExternalAttachment) error {
	if list == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, a := range *list {
		if !externalFileIDPattern.MatchString(a.FileID) || a.Size < 0 || seen[a.FileID] {
			return invalid("adjunto inválido o repetido en el taller %s", folder)
		}
		seen[a.FileID] = true
	}
	return nil
}

// reconcileAttachments compara los adjuntos que informa Brightspace con los archivos de origen externo
// que ya tiene el taller: devuelve los que faltan (o cambiaron de tamaño) y borra las filas de los que
// Brightspace ya no tiene. Los archivos que subió el docente (external_id NULL) no se tocan nunca.
// Un taller oculto no pide archivos nuevos (nadie los vería), pero sí limpia los que ya no existen.
func reconcileAttachments(tx *sql.Tx, assignmentID string, it ExternalAssignment) ([]FileNeeded, []staleFile, error) {
	type have struct {
		id string
		size int64
	}
	existing := map[string]have{}
	rows, err := tx.Query(`SELECT id, external_id, size FROM assignment_files
		WHERE assignment_id = ? AND external_id IS NOT NULL`, assignmentID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var h have
		var ext string
		if err := rows.Scan(&h.id, &ext, &h.size); err != nil {
			rows.Close()
			return nil, nil, err
		}
		existing[ext] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var needed []FileNeeded
	announced := map[string]bool{}
	for _, a := range *it.Attachments {
		announced[a.FileID] = true
		h, ok := existing[a.FileID]
		fresh := ok && (a.Size == 0 || h.size == a.Size)
		if fresh || it.IsHidden || a.Size > MaxAssignmentFileSize {
			continue
		}
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = a.FileID
		}
		needed = append(needed, FileNeeded{Folder: it.ExternalID, FileID: a.FileID, Name: name, Size: a.Size})
	}

	var stale []staleFile
	for ext, h := range existing {
		if announced[ext] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM assignment_files WHERE id = ?`, h.id); err != nil {
			return nil, nil, err
		}
		stale = append(stale, staleFile{assignmentID, h.id})
	}
	return needed, stale, nil
}

// SaveExternalAssignmentFile guarda un adjunto de Brightspace en un taller espejado. Es idempotente:
// el mismo fileExternalID reemplaza el contenido anterior. Respeta el tope de MaxAssignmentFiles por
// taller (contando también los archivos del docente) y de maxBytes por archivo.
func (s *Store) SaveExternalAssignmentFile(courseExternalID, folderExternalID, fileExternalID, name, contentType string, body io.Reader, maxBytes int64) (*AssignmentFile, error) {
	if !validExternalID(courseExternalID) || !validExternalID(folderExternalID) || !externalFileIDPattern.MatchString(fileExternalID) {
		return nil, invalid("identificador de curso, taller o archivo inválido")
	}
	var groupID string
	err := s.db.QueryRow(`SELECT g.id FROM class_groups g WHERE g.external_id = ? AND EXISTS (
		SELECT 1 FROM courses c WHERE c.id = g.course_id AND c.external_id = g.external_id)`, courseExternalID).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownCourse
	}
	if err != nil {
		return nil, err
	}
	var assignmentID string
	err = s.db.QueryRow(`SELECT id FROM assignments WHERE group_id = ? AND external_id = ?`, groupID, folderExternalID).Scan(&assignmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownAssignment
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		name = fileExternalID
	}

	dir := s.assignmentDir(assignmentID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Se lee un byte de más para distinguir «justo en el tope» de «se pasó».
	written, err := writeUploads(dir, []FileUpload{{
		Name: name, ContentType: clipRunes(strings.TrimSpace(contentType), 100), Body: io.LimitReader(body, maxBytes+1),
	}})
	if err != nil {
		return nil, err
	}
	f := written[0]
	fail := func(err error) (*AssignmentFile, error) {
		os.Remove(filepath.Join(dir, f.ID))
		return nil, err
	}
	if f.Size == 0 {
		return fail(invalid("el archivo está vacío"))
	}
	if f.Size > maxBytes {
		return fail(ErrFileTooLarge)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	var oldID string
	if err := tx.QueryRow(`SELECT id FROM assignment_files WHERE assignment_id = ? AND external_id = ?`,
		assignmentID, fileExternalID).Scan(&oldID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fail(err)
	}
	var count, last int
	if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(MAX(position), -1) FROM assignment_files WHERE assignment_id = ? AND id <> ?`,
		assignmentID, oldID).Scan(&count, &last); err != nil {
		return fail(err)
	}
	if count+1 > MaxAssignmentFiles {
		return fail(ErrTooManyFiles)
	}
	if oldID != "" {
		if _, err := tx.Exec(`DELETE FROM assignment_files WHERE id = ?`, oldID); err != nil {
			return fail(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO assignment_files (id, assignment_id, name, size, content_type, position, external_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, f.ID, assignmentID, f.Name, f.Size, f.ContentType, last+1, fileExternalID); err != nil {
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	if oldID != "" {
		os.Remove(filepath.Join(dir, oldID))
	}
	return &f, nil
}
