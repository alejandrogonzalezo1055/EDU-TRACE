package handler

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/marlon/edu-trace/internal/auth"
	"github.com/marlon/edu-trace/internal/config"
)

// Rutas /internal/v1/*: las llama el servicio Python (servidor a servidor) con
// "Authorization: Bearer <INTERNAL_API_KEY>". Se registran aparte de /api/* y NO pasan por
// corsMiddleware (no traen Origin: ese filtro deja pasar todo lo que no lo trae), así que la clave
// es la única defensa. Aceptan el formato del contrato y también el JSON crudo de Brightspace
// (whoami, myenrollments, classlist, dropbox/folders): Go filtra y transforma.

const maxInternalBody = 4 << 20

type InternalHandler struct {
	store *auth.Store
	cfg *config.Config
	keyHash [32]byte
}

func NewInternalHandler(store *auth.Store, cfg *config.Config) *InternalHandler {
	return &InternalHandler{store: store, cfg: cfg, keyHash: sha256.Sum256([]byte(cfg.InternalAPIKey))}
}

// Handler devuelve el árbol /internal/v1/ completo, con la autenticación por clave delante de todo
// (una ruta inexistente también da 401 sin clave: no se revela qué existe).
func (h *InternalHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/v1/health", h.Health)
	mux.HandleFunc("PUT /internal/v1/users/{external_id}", h.PutUser)
	mux.HandleFunc("PUT /internal/v1/users/{external_id}/enrollments", h.PutEnrollments)
	mux.HandleFunc("PUT /internal/v1/courses/{external_id}/roster", h.PutRoster)
	mux.HandleFunc("PUT /internal/v1/courses/{external_id}/assignments", h.PutAssignments)
	mux.HandleFunc("PUT /internal/v1/courses/{external_id}/assignments/{folder}/files/{file_id}", h.PutAssignmentFile)
	mux.HandleFunc("POST /internal/v1/session-tickets", h.IssueTicket)
	mux.HandleFunc("POST /internal/v1/sessions/revoke", h.RevokeSessions)
	var next http.Handler = mux
	next = h.requireKey(next)
	next = securityHeaders(next, h.cfg.CookieSecure)
	next = loggingMiddleware(next)
	return proxyMiddleware(next, h.cfg.TrustProxy)
}

func (h *InternalHandler) requireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		// Se comparan los SHA-256: tiempo constante también respecto al largo de la clave enviada.
		got := sha256.Sum256([]byte(strings.TrimSpace(token)))
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(got[:], h.keyHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			internalError(w, http.StatusUnauthorized, "no_autorizado", "no autorizado")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func internalError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

func decodeInternal(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInternalBody))
	if err == nil {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		internalError(w, http.StatusBadRequest, "json_invalido", "cuerpo de la petición inválido")
		return false
	}
	return true
}

// storeFailure traduce los errores de internal/auth al formato {"error","code"}.
func storeFailure(w http.ResponseWriter, err error, doing string) {
	status, code := http.StatusInternalServerError, "error_interno"
	switch {
	case errors.Is(err, auth.ErrEmailRequired):
		status, code = http.StatusUnprocessableEntity, "correo_no_disponible"
	case errors.Is(err, auth.ErrInvalidInput):
		status, code = http.StatusBadRequest, "datos_invalidos"
	case errors.Is(err, auth.ErrUnknownUser):
		status, code = http.StatusNotFound, "usuario_no_encontrado"
	case errors.Is(err, auth.ErrUnknownCourse):
		status, code = http.StatusNotFound, "curso_no_encontrado"
	case errors.Is(err, auth.ErrUnknownAssignment):
		status, code = http.StatusNotFound, "taller_no_encontrado"
	case errors.Is(err, auth.ErrFileTooLarge):
		status, code = http.StatusRequestEntityTooLarge, "archivo_muy_grande"
	case errors.Is(err, auth.ErrTooManyFiles):
		status, code = http.StatusUnprocessableEntity, "demasiados_archivos"
	case errors.Is(err, auth.ErrEmailTakenLocal), errors.Is(err, auth.ErrEmailTakenExternal):
		status, code = http.StatusConflict, "correo_en_uso"
	case errors.Is(err, auth.ErrExternalConflict):
		status, code = http.StatusConflict, "conflicto"
	case errors.Is(err, auth.ErrEmptyRoster):
		status, code = http.StatusUnprocessableEntity, "lista_vacia"
	case errors.Is(err, auth.ErrRosterTooDestructive):
		status, code = http.StatusUnprocessableEntity, "desactivacion_masiva"
	case errors.Is(err, auth.ErrEmptySnapshot):
		status, code = http.StatusUnprocessableEntity, "instantanea_vacia"
	}
	if status == http.StatusInternalServerError {
		slog.Error("internal api", "action", doing, "error", err)
		internalError(w, status, code, "error interno")
		return
	}
	internalError(w, status, code, err.Error())
}

// ---------- Normalización de lo que llega de Python/Brightspace ----------

// normExt convierte un número crudo de Brightspace ("3101") al external_id del contrato
// ("d2l:<kind>:3101"); lo que ya trae prefijo se deja igual.
func normExt(kind, v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if _, err := strconv.ParseUint(v, 10, 64); err == nil {
		return "d2l:" + kind + ":" + v
	}
	return v
}

// rawString lee un valor JSON que puede venir como cadena o como número.
func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

var (
	breakTags = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</li>`)
	anyTag = regexp.MustCompile(`<[^>]*>`)
)

// stripHTML deja el texto de un RichText que solo trae Html.
func stripHTML(s string) string {
	s = anyTag.ReplaceAllString(breakTags.ReplaceAllString(s, "\n"), "")
	return strings.TrimSpace(html.UnescapeString(s))
}

// roleName lee un rol que puede venir como cadena ("Estudiante") u objeto ({"Name": "..."}).
func roleName(raw json.RawMessage) string {
	if s := rawString(raw); s != "" {
		return s
	}
	var o struct{ Name string }
	_ = json.Unmarshal(raw, &o)
	return strings.TrimSpace(o.Name)
}

// resolveRole acepta los roles del contrato tal cual y traduce los nombres de Brightspace con D2L_ROLE_MAP.
func (h *InternalHandler) resolveRole(name string, lis []string) (string, bool) {
	switch n := strings.ToLower(strings.TrimSpace(name)); n {
	case "student", "teacher", "monitor":
		return n, true
	}
	return h.cfg.MapRole(name, lis)
}

type ignoredItem struct {
	ExternalID string `json:"external_id,omitempty"`
	Name string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// ---------- Salud ----------

func (h *InternalHandler) Health(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Ping(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "caido"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------- Usuarios ----------

type userBody struct {
	// Contrato
	Name string `json:"name"`
	Email string `json:"email"`
	RoleHint string `json:"role_hint"`
	// whoami crudo
	Identifier json.RawMessage `json:"Identifier"`
	FirstName string `json:"FirstName"`
	LastName string `json:"LastName"`
	UniqueName string `json:"UniqueName"`
}

type userResponse struct {
	ID string `json:"id"`
	ExternalID string `json:"external_id"`
	Name string `json:"name"`
	Email string `json:"email"`
	Role auth.Role `json:"role"`
	ConsentAt *time.Time `json:"consent_at"`
	ConsentOutdated bool `json:"consent_outdated"`
	Created bool `json:"created"`
}

func (h *InternalHandler) PutUser(w http.ResponseWriter, r *http.Request) {
	var b userBody
	if !decodeInternal(w, r, &b) {
		return
	}
	extID := normExt("user", r.PathValue("external_id"))
	if id := normExt("user", rawString(b.Identifier)); id != "" && id != extID {
		internalError(w, http.StatusBadRequest, "identificador_no_coincide", "el Identifier del cuerpo no coincide con el de la URL")
		return
	}
	name := strings.TrimSpace(b.Name)
	if name == "" {
		name = strings.TrimSpace(b.FirstName + " " + b.LastName)
	}
	email := strings.TrimSpace(b.Email)
	if email == "" && strings.Contains(b.UniqueName, "@") {
		email = strings.TrimSpace(b.UniqueName)
	}
	// role_hint se ignora: users.role lo calcula Go a partir de las inscripciones (D2).
	res, err := h.store.UpsertExternalUser(extID, name, email)
	if err != nil {
		storeFailure(w, err, "upsert de usuario")
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	u := res.User
	resp := userResponse{ID: u.ID, ExternalID: res.ExternalID, Name: u.Name, Email: u.Email, Role: u.Role, Created: res.Created}
	if u.ConsentCurrent() {
		resp.ConsentAt = u.ConsentAt
	} else if u.ConsentAt != nil {
		resp.ConsentOutdated = true
	}
	slog.Info("external user upserted", "user_id", u.ID, "external_id", res.ExternalID, "created", res.Created)
	writeJSON(w, status, resp)
}

// ---------- Inscripciones ----------

type contractEnrollment struct {
	Course struct {
		ExternalID string `json:"external_id"`
		Name string `json:"name"`
		StartDate *time.Time `json:"start_date"`
		EndDate *time.Time `json:"end_date"`
		IsActive *bool `json:"is_active"`
	} `json:"course"`
	Role string `json:"role"`
}

type rawEnrollment struct {
	OrgUnit struct {
		ID json.RawMessage `json:"Id"`
		Type struct {
			ID json.RawMessage `json:"Id"`
			Code string `json:"Code"`
		} `json:"Type"`
		Name string `json:"Name"`
		Code string `json:"Code"`
	} `json:"OrgUnit"`
	Access struct {
		IsActive *bool `json:"IsActive"`
		CanAccess *bool `json:"CanAccess"`
		StartDate *time.Time `json:"StartDate"`
		EndDate *time.Time `json:"EndDate"`
		ClasslistRoleName string `json:"ClasslistRoleName"`
		LISRoles []string `json:"LISRoles"`
	} `json:"Access"`
}

type enrollmentsBody struct {
	Enrollments *[]contractEnrollment `json:"enrollments"`
	Items *[]rawEnrollment `json:"Items"`
}

type enrollmentsResponse struct {
	*auth.EnrollmentResult
	Ignored []ignoredItem `json:"ignored"`
}

func (h *InternalHandler) PutEnrollments(w http.ResponseWriter, r *http.Request) {
	var b enrollmentsBody
	if !decodeInternal(w, r, &b) {
		return
	}
	if b.Enrollments == nil && b.Items == nil {
		internalError(w, http.StatusBadRequest, "datos_invalidos", `falta "enrollments" (contrato) o "Items" (myenrollments)`)
		return
	}
	var list []auth.ExternalEnrollment
	ignored := []ignoredItem{}
	if b.Enrollments != nil {
		for _, e := range *b.Enrollments {
			active := e.Course.IsActive == nil || *e.Course.IsActive
			list = append(list, auth.ExternalEnrollment{Role: e.Role, Course: auth.ExternalCourse{
				ExternalID: normExt("ou", e.Course.ExternalID), Name: e.Course.Name,
				StartDate: utcPtr(e.Course.StartDate), EndDate: utcPtr(e.Course.EndDate), IsActive: active}})
		}
	}
	if b.Items != nil {
		for _, it := range *b.Items {
			ext := normExt("ou", rawString(it.OrgUnit.ID))
			name := strings.TrimSpace(it.OrgUnit.Name)
			if name == "" {
				name = strings.TrimSpace(it.OrgUnit.Code)
			}
			// Solo cuentan las ofertas de curso (Type.Id 3 / "Course Offering"): se ignoran la organización y demás tipos.
			if typeID := rawString(it.OrgUnit.Type.ID); typeID != "3" && !strings.EqualFold(strings.TrimSpace(it.OrgUnit.Type.Code), "Course Offering") {
				ignored = append(ignored, ignoredItem{ExternalID: ext, Name: name, Reason: "no es una oferta de curso (Type " + typeID + ")"})
				continue
			}
			role, ok := h.cfg.MapRole(it.Access.ClasslistRoleName, it.Access.LISRoles)
			if !ok {
				ignored = append(ignored, ignoredItem{ExternalID: ext, Name: name, Reason: "rol no reconocido: " + strconv.Quote(it.Access.ClasslistRoleName)})
				continue
			}
			active := (it.Access.IsActive == nil || *it.Access.IsActive) && (it.Access.CanAccess == nil || *it.Access.CanAccess)
			list = append(list, auth.ExternalEnrollment{Role: role, Course: auth.ExternalCourse{
				ExternalID: ext, Name: name, StartDate: utcPtr(it.Access.StartDate), EndDate: utcPtr(it.Access.EndDate), IsActive: active}})
		}
	}
	res, err := h.store.UpsertEnrollments(normExt("user", r.PathValue("external_id")), list)
	if err != nil {
		storeFailure(w, err, "inscripciones")
		return
	}
	slog.Info("external enrollments synced", "external_id", r.PathValue("external_id"), "enrolled", res.Enrolled, "deactivated", res.Deactivated, "ignored", len(ignored))
	writeJSON(w, http.StatusOK, enrollmentsResponse{EnrollmentResult: res, Ignored: ignored})
}

// ---------- Lista de clase ----------

type rosterEntry struct {
	// Contrato
	User *struct {
		ExternalID string `json:"external_id"`
		Name string `json:"name"`
		Email string `json:"email"`
	} `json:"user"`
	Role json.RawMessage `json:"role"`
	IsActive *bool `json:"is_active"`
	// classlist crudo
	Identifier json.RawMessage `json:"Identifier"`
	DisplayName string `json:"DisplayName"`
	FirstName string `json:"FirstName"`
	LastName string `json:"LastName"`
	Email string `json:"Email"`
	RoleName json.RawMessage `json:"RoleName"`
}

func (h *InternalHandler) PutRoster(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if !decodeInternal(w, r, &raw) {
		return
	}
	entries, ok := listFrom[rosterEntry](raw, "members", "Items", "Objects")
	if !ok {
		internalError(w, http.StatusBadRequest, "datos_invalidos", `falta "members" (contrato) o la lista de clase de Brightspace`)
		return
	}
	var members []auth.RosterMember
	ignored := []ignoredItem{}
	for _, e := range entries {
		m := auth.RosterMember{IsActive: e.IsActive == nil || *e.IsActive}
		if e.User != nil {
			m.ExternalID, m.Name, m.Email = normExt("user", e.User.ExternalID), e.User.Name, e.User.Email
		}
		if id := normExt("user", rawString(e.Identifier)); id != "" && m.ExternalID == "" {
			m.ExternalID = id
		}
		if m.Name == "" {
			if m.Name = strings.TrimSpace(e.DisplayName); m.Name == "" {
				m.Name = strings.TrimSpace(e.FirstName + " " + e.LastName)
			}
		}
		if m.Email == "" {
			m.Email = strings.TrimSpace(e.Email)
		}
		name := roleName(e.Role)
		if name == "" {
			name = roleName(e.RoleName)
		}
		role, ok := h.resolveRole(name, nil)
		if !ok {
			ignored = append(ignored, ignoredItem{ExternalID: m.ExternalID, Name: m.Name, Reason: "rol no reconocido: " + strconv.Quote(name)})
			continue
		}
		m.Role = role
		members = append(members, m)
	}
	// Si el filtro de roles deja la lista vacía, UpsertRoster responde 422 lista_vacia (nada cambia).
	res, err := h.store.UpsertRoster(normExt("ou", r.PathValue("external_id")), members, h.cfg.RosterMaxDeactivatePercent)
	if err != nil {
		storeFailure(w, err, "lista de clase")
		return
	}
	slog.Info("external roster synced", "course", r.PathValue("external_id"), "enrolled", res.Enrolled, "deactivated", res.Deactivated, "skipped", len(res.Skipped))
	writeJSON(w, http.StatusOK, struct {
		*auth.RosterResult
		Ignored []ignoredItem `json:"ignored"`
	}{res, ignored})
}

// listFrom lee una lista que puede venir como arreglo suelto o dentro de un objeto con alguna de las claves dadas.
func listFrom[T any](raw json.RawMessage, keys ...string) ([]T, bool) {
	var direct []T
	if json.Unmarshal(raw, &direct) == nil {
		return direct, true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, false
	}
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			var out []T
			if json.Unmarshal(v, &out) != nil {
				return nil, false
			}
			return out, true
		}
	}
	return nil, false
}

// ---------- Talleres ----------

type assignmentEntry struct {
	// Contrato
	ExternalID string `json:"external_id"`
	Title string `json:"title"`
	Statement string `json:"statement"`
	DueAt *time.Time `json:"due_at"`
	EndAt *time.Time `json:"end_at"`
	ScoreDenominator *float64 `json:"score_denominator"`
	GradeItemID json.RawMessage `json:"grade_item_id"`
	// dropbox/folders crudo
	ID json.RawMessage `json:"Id"`
	Name string `json:"Name"`
	CustomInstructions *struct {
		Text string `json:"Text"`
		HTML string `json:"Html"`
	} `json:"CustomInstructions"`
	DueDate *time.Time `json:"DueDate"`
	Availability *struct {
		EndDate *time.Time `json:"EndDate"`
	} `json:"Availability"`
	Assessment *struct {
		ScoreDenominator *float64 `json:"ScoreDenominator"`
		GradeItemID json.RawMessage `json:"GradeItemId"`
	} `json:"Assessment"`
	// IsHidden sirve para las dos formas (is_hidden y IsHidden se emparejan sin distinguir mayúsculas
	// solo si el nombre coincide; por eso hay dos campos).
	IsHiddenContract bool `json:"is_hidden"`
	IsHiddenRaw bool `json:"IsHidden"`
	// Adjuntos de la carpeta en Brightspace (FileId, FileName, Size). Ausente = no se toca nada.
	Attachments *[]attachmentEntry `json:"Attachments"`
}

type attachmentEntry struct {
	FileID json.RawMessage `json:"FileId"`
	FileName string `json:"FileName"`
	Size int64 `json:"Size"`
}

func (h *InternalHandler) PutAssignments(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if !decodeInternal(w, r, &raw) {
		return
	}
	entries, ok := listFrom[assignmentEntry](raw, "assignments", "Items", "Objects")
	if !ok {
		internalError(w, http.StatusBadRequest, "datos_invalidos", `falta "assignments" (contrato) o la lista de carpetas de Brightspace`)
		return
	}
	items := make([]auth.ExternalAssignment, 0, len(entries))
	for _, e := range entries {
		it := auth.ExternalAssignment{
			ExternalID: normExt("folder", e.ExternalID), Title: e.Title, Statement: e.Statement,
			DueAt: utcPtr(e.DueAt), EndAt: utcPtr(e.EndAt), ScoreDenominator: e.ScoreDenominator,
			GradeItemID: normExt("grade", rawString(e.GradeItemID)), IsHidden: e.IsHiddenContract || e.IsHiddenRaw,
		}
		if it.ExternalID == "" {
			it.ExternalID = normExt("folder", rawString(e.ID))
		}
		if it.Title == "" {
			it.Title = e.Name
		}
		if it.Statement == "" && e.CustomInstructions != nil {
			if it.Statement = strings.TrimSpace(e.CustomInstructions.Text); it.Statement == "" {
				it.Statement = stripHTML(e.CustomInstructions.HTML)
			}
		}
		if it.DueAt == nil {
			it.DueAt = utcPtr(e.DueDate)
		}
		if it.EndAt == nil && e.Availability != nil {
			it.EndAt = utcPtr(e.Availability.EndDate)
		}
		if e.Assessment != nil {
			if it.ScoreDenominator == nil {
				it.ScoreDenominator = e.Assessment.ScoreDenominator
			}
			if it.GradeItemID == "" {
				it.GradeItemID = normExt("grade", rawString(e.Assessment.GradeItemID))
			}
		}
		if e.Attachments != nil {
			list := make([]auth.ExternalAttachment, 0, len(*e.Attachments))
			for _, a := range *e.Attachments {
				list = append(list, auth.ExternalAttachment{FileID: rawString(a.FileID), Name: a.FileName, Size: a.Size})
			}
			it.Attachments = &list
		}
		items = append(items, it)
	}
	res, err := h.store.UpsertAssignments(normExt("ou", r.PathValue("external_id")), items)
	if err != nil {
		storeFailure(w, err, "talleres")
		return
	}
	slog.Info("external assignments synced", "course", r.PathValue("external_id"), "created", res.Created, "updated", res.Updated, "hidden", res.Hidden)
	writeJSON(w, http.StatusOK, res)
}

// PutAssignmentFile recibe un adjunto de una carpeta de Brightspace: el cuerpo es el archivo tal cual
// (Content-Type del archivo) y el nombre va en ?name=. Python lo llama por cada elemento de
// files_needed que devuelve PutAssignments. Es idempotente: el mismo file_id reemplaza el contenido.
func (h *InternalHandler) PutAssignmentFile(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > auth.MaxAssignmentFileSize {
		internalError(w, http.StatusRequestEntityTooLarge, "archivo_muy_grande", auth.ErrFileTooLarge.Error())
		return
	}
	// El almacén lee a lo sumo un byte de más para detectar el exceso; este límite es solo una red de seguridad.
	body := http.MaxBytesReader(w, r.Body, auth.MaxAssignmentFileSize+1)
	f, err := h.store.SaveExternalAssignmentFile(
		normExt("ou", r.PathValue("external_id")), normExt("folder", r.PathValue("folder")), r.PathValue("file_id"),
		r.URL.Query().Get("name"), r.Header.Get("Content-Type"), body, auth.MaxAssignmentFileSize)
	if err != nil {
		storeFailure(w, err, "adjunto de taller")
		return
	}
	slog.Info("external assignment file saved", "course", r.PathValue("external_id"), "folder", r.PathValue("folder"),
		"file_id", r.PathValue("file_id"), "bytes", f.Size)
	writeJSON(w, http.StatusOK, f)
}

// ---------- Tickets y sesiones ----------

type externalUserBody struct {
	UserExternalID json.RawMessage `json:"user_external_id"`
}

func (h *InternalHandler) IssueTicket(w http.ResponseWriter, r *http.Request) {
	var b externalUserBody
	if !decodeInternal(w, r, &b) {
		return
	}
	ext := normExt("user", rawString(b.UserExternalID))
	if ext == "" {
		internalError(w, http.StatusBadRequest, "datos_invalidos", "falta user_external_id")
		return
	}
	ticket, ttl, err := h.store.IssueSessionTicket(ext)
	if err != nil {
		storeFailure(w, err, "ticket de sesión")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": ticket, "expires_in": int(ttl / time.Second)})
}

func (h *InternalHandler) RevokeSessions(w http.ResponseWriter, r *http.Request) {
	var b externalUserBody
	if !decodeInternal(w, r, &b) {
		return
	}
	ext := normExt("user", rawString(b.UserExternalID))
	if ext == "" {
		internalError(w, http.StatusBadRequest, "datos_invalidos", "falta user_external_id")
		return
	}
	n, err := h.store.RevokeExternalSessions(ext)
	if err != nil {
		storeFailure(w, err, "revocar sesiones")
		return
	}
	slog.Info("external sessions revoked", "external_id", ext, "sessions", n)
	w.WriteHeader(http.StatusNoContent)
}
