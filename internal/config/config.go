package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration.
type Config struct {
	Host string
	Port string
	GppPath string
	Sandbox string // "bwrap" (aislamiento real) o "none" (solo desarrollo)
	DataDir string
	SessionTTL time.Duration
	CookieSecure bool
	CompileTimeout time.Duration
	RunTimeout time.Duration
	MaxOutputSize int
	MaxSandboxes int
	OllamaBaseURL string
	OllamaModel string
	OllamaTimeout int
	OllamaContextSize int
	OllamaThink string // "auto" (según el modelo), "true" o "false": razonamiento antes de responder
	OllamaKeepAlive string // cuánto tiempo mantiene Ollama el modelo cargado en la GPU
	AllowedOrigins string
	TrustProxy bool
	AIRequestsPerHour int
	RetentionDays int
	PublicURL string
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPassword string
	SMTPFrom string
	DiskMinFreePercent int

	// Integración con el servicio Python (Brightspace).
	InternalAPIKey string // clave Python → Go para /internal/v1/*; vacía = rutas internas deshabilitadas
	PythonAPIKey string // clave Go → Python (Authorization: Bearer en el cliente pyclient)
	PythonBaseURL string // URL https del servicio Python; vacía = Go no llama a Python (sin estado Brightspace)
	PythonCAFile string // PEM opcional (CA de mkcert) que se suma al almacén del sistema para confiar en Python
	PythonAllowHTTP bool // solo desarrollo: permite PYTHON_BASE_URL con http:// (las cookies de usuario nunca viajan por http)
	RosterMaxDeactivatePercent int // la lista de clase no puede desactivar a más de este % de los inscritos
	TLSCertFile string
	TLSKeyFile string
	TeacherRoleNames []string // nombres de rol de Brightspace que equivalen a docente
	StudentRoleNames []string // ... a estudiante
	CookieJarKey []byte // AES-256 (COOKIE_JAR_KEY, base64) para cifrar las cookies que Python guarda en Go
	PythonCookieNames []string // cookies de Python que Go captura del navegador al abrir sesión

	cookieJarKeyErr error
	roleMapErr error
}

// MinInternalKeyBytes es el tamaño mínimo de INTERNAL_API_KEY.
const MinInternalKeyBytes = 32

// TLSEnabled indica que Go sirve HTTPS por sí mismo.
func (c *Config) TLSEnabled() bool { return c.TLSCertFile != "" && c.TLSKeyFile != "" }

// Load reads configuration from environment variables with defaults.
func Load() *Config {
	teacherRoles, studentRoles, roleErr := parseRoleMap(envOrDefault("D2L_ROLE_MAP", defaultRoleMap))
	var jarKey []byte
	var jarKeyErr error
	if v := os.Getenv("COOKIE_JAR_KEY"); v != "" {
		if jarKey, jarKeyErr = base64.StdEncoding.DecodeString(v); jarKeyErr != nil || len(jarKey) != 32 {
			jarKey, jarKeyErr = nil, errors.New("COOKIE_JAR_KEY debe ser una clave de 32 bytes en base64 (openssl rand -base64 32)")
		}
	}
	return &Config{
		Host: envOrDefault("HOST", "127.0.0.1"),
		Port: envOrDefault("PORT", "8080"),
		GppPath: envOrDefault("GPP_PATH", "g++"),
		Sandbox: envOrDefault("SANDBOX", "bwrap"),
		DataDir: envOrDefault("DATA_DIR", "data"),
		SessionTTL: time.Duration(intOrDefault("SESSION_TTL_HOURS", 72)) * time.Hour,
		CookieSecure: os.Getenv("COOKIE_SECURE") == "true" || (os.Getenv("TLS_CERT_FILE") != "" && os.Getenv("TLS_KEY_FILE") != ""),
		CompileTimeout: durationOrDefault("COMPILE_TIMEOUT_SECS", 10*time.Second),
		RunTimeout: durationOrDefault("RUN_TIMEOUT_SECS", 5*time.Second),
		MaxOutputSize: intOrDefault("MAX_OUTPUT_SIZE", 10240),
		MaxSandboxes: intOrDefault("MAX_SANDBOXES", runtime.NumCPU()),
		OllamaBaseURL: envOrDefault("OLLAMA_BASE_URL", "http://localhost:11434"),
		OllamaModel: envOrDefault("OLLAMA_MODEL", "llama3.1:8b"),
		OllamaTimeout: intOrDefault("OLLAMA_TIMEOUT", 180),
		OllamaContextSize: intOrDefault("OLLAMA_CONTEXT_SIZE", 6144),
		OllamaThink: envOrDefault("OLLAMA_THINK", "auto"),
		OllamaKeepAlive: envOrDefault("OLLAMA_KEEP_ALIVE", "30m"),
		AllowedOrigins: envOrDefault("ALLOWED_ORIGINS", "http://localhost:3000"),
		TrustProxy: os.Getenv("TRUST_PROXY") == "true",
		AIRequestsPerHour: intOrDefault("AI_REQUESTS_PER_HOUR", 20),
		RetentionDays: intOrDefault("RETENTION_DAYS", 180),
		PublicURL: strings.TrimRight(envOrDefault("PUBLIC_URL", firstOrigin(os.Getenv("ALLOWED_ORIGINS"))), "/"),
		SMTPHost: os.Getenv("SMTP_HOST"),
		SMTPPort: intOrDefault("SMTP_PORT", 587),
		SMTPUser: os.Getenv("SMTP_USER"),
		SMTPPassword: os.Getenv("SMTP_PASSWORD"),
		SMTPFrom: os.Getenv("SMTP_FROM"),
		DiskMinFreePercent: intOrDefault("DISK_MIN_FREE_PERCENT", 10),
		InternalAPIKey: os.Getenv("INTERNAL_API_KEY"),
		PythonAPIKey: os.Getenv("PYTHON_API_KEY"),
		PythonBaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv("PYTHON_BASE_URL")), "/"),
		PythonCAFile: os.Getenv("PYTHON_CA_FILE"),
		PythonAllowHTTP: os.Getenv("PYTHON_ALLOW_HTTP") == "true",
		RosterMaxDeactivatePercent: intOrDefault("ROSTER_MAX_DEACTIVATE_PERCENT", 30),
		TLSCertFile: os.Getenv("TLS_CERT_FILE"),
		TLSKeyFile: os.Getenv("TLS_KEY_FILE"),
		TeacherRoleNames: teacherRoles,
		StudentRoleNames: studentRoles,
		CookieJarKey: jarKey,
		PythonCookieNames: splitList(envOrDefault("PYTHON_COOKIE_NAMES", "auth_code,access_token,refresh_token,response_time"), ","),
		cookieJarKeyErr: jarKeyErr,
		roleMapErr: roleErr,
	}
}

const defaultRoleMap = "teacher=Profesor,Docente,Instructor,Teacher,Facilitador;student=Estudiante,Student,Alumno"

// parseRoleMap lee "teacher=A,B;student=C,D".
func parseRoleMap(spec string) (teachers, students []string, err error) {
	for _, part := range strings.Split(spec, ";") {
		key, list, ok := strings.Cut(part, "=")
		if !ok {
			if strings.TrimSpace(part) == "" {
				continue
			}
			return nil, nil, fmt.Errorf("D2L_ROLE_MAP: %q no tiene la forma rol=Nombre1,Nombre2", part)
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "teacher":
			teachers = append(teachers, splitList(list, ",")...)
		case "student":
			students = append(students, splitList(list, ",")...)
		default:
			return nil, nil, fmt.Errorf("D2L_ROLE_MAP: rol %q desconocido (teacher, student)", key)
		}
	}
	return teachers, students, nil
}

func splitList(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// MapRole traduce el rol de Brightspace de una inscripción a "teacher" o "student". Se mira primero
// ClasslistRoleName (tabla D2L_ROLE_MAP, sin distinguir mayúsculas) y, si no coincide, los LISRoles
// (…/Instructor = docente, …/Learner = estudiante). ok=false: rol desconocido, se ignora.
func (c *Config) MapRole(classlistRole string, lisRoles []string) (role string, ok bool) {
	name := strings.TrimSpace(classlistRole)
	for _, n := range c.TeacherRoleNames {
		if strings.EqualFold(n, name) {
			return "teacher", true
		}
	}
	for _, n := range c.StudentRoleNames {
		if strings.EqualFold(n, name) {
			return "student", true
		}
	}
	for _, r := range lisRoles {
		r = strings.ToLower(strings.TrimSpace(r))
		switch {
		case strings.HasSuffix(r, "/instructor"):
			return "teacher", true
		case strings.HasSuffix(r, "/learner"):
			return "student", true
		}
	}
	return "", false
}

// Validate comprueba la configuración de la integración; main sale si falla.
func (c *Config) Validate() error {
	if c.roleMapErr != nil {
		return c.roleMapErr
	}
	if c.InternalAPIKey != "" && len(c.InternalAPIKey) < MinInternalKeyBytes {
		return fmt.Errorf("INTERNAL_API_KEY debe tener al menos %d bytes (genera una con: openssl rand -hex 32)", MinInternalKeyBytes)
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("TLS_CERT_FILE y TLS_KEY_FILE deben definirse juntas")
	}
	if c.cookieJarKeyErr != nil {
		return c.cookieJarKeyErr
	}
	if len(c.CookieJarKey) == 0 && (c.TLSEnabled() || c.PythonAPIKey != "" || c.PythonBaseURL != "") {
		return errors.New("COOKIE_JAR_KEY es obligatoria cuando hay HTTPS, PYTHON_API_KEY o PYTHON_BASE_URL (32 bytes en base64: openssl rand -base64 32)")
	}
	if c.PythonBaseURL != "" {
		u, err := url.Parse(c.PythonBaseURL)
		switch {
		case err != nil || u.Host == "" || u.User != nil:
			return errors.New("PYTHON_BASE_URL inválida: debe ser una URL como https://localhost:8000")
		case u.Scheme == "http" && !c.PythonAllowHTTP:
			return errors.New("PYTHON_BASE_URL debe ser https:// (solo en desarrollo, PYTHON_ALLOW_HTTP=true permite http://)")
		case u.Scheme != "http" && u.Scheme != "https":
			return fmt.Errorf("PYTHON_BASE_URL con esquema %q no permitido: usa https://", u.Scheme)
		}
		if len(c.PythonAPIKey) < MinInternalKeyBytes {
			return fmt.Errorf("PYTHON_API_KEY debe tener al menos %d bytes cuando hay PYTHON_BASE_URL (genera una con: openssl rand -hex 32)", MinInternalKeyBytes)
		}
	}
	return nil
}

func firstOrigin(origins string) string {
	first, _, _ := strings.Cut(origins, ",")
	if first = strings.TrimSpace(first); first != "" {
		return first
	}
	return "http://localhost:3000"
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intOrDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func durationOrDefault(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return fallback
}
