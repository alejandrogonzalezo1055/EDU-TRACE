package config

import (
	"strings"
	"testing"
)

func pyCfg(t *testing.T, env map[string]string) *Config {
	t.Helper()
	for _, k := range []string{"PYTHON_BASE_URL", "PYTHON_API_KEY", "PYTHON_ALLOW_HTTP", "COOKIE_JAR_KEY", "PYTHON_CA_FILE", "TLS_CERT_FILE", "TLS_KEY_FILE", "INTERNAL_API_KEY"} {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	return Load()
}

const goodKey = "0123456789abcdef0123456789abcdef"
const jarKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes en base64

func TestValidatePython(t *testing.T) {
	cases := []struct {
		name string
		env map[string]string
		wantErr string
	}{
		{"sin Python", map[string]string{}, ""},
		{"https completo", map[string]string{"PYTHON_BASE_URL": "https://localhost:8000", "PYTHON_API_KEY": goodKey, "COOKIE_JAR_KEY": jarKey}, ""},
		{"http rechazado", map[string]string{"PYTHON_BASE_URL": "http://localhost:8000", "PYTHON_API_KEY": goodKey, "COOKIE_JAR_KEY": jarKey}, "https://"},
		{"http con permiso de desarrollo", map[string]string{"PYTHON_BASE_URL": "http://localhost:8000", "PYTHON_ALLOW_HTTP": "true", "PYTHON_API_KEY": goodKey, "COOKIE_JAR_KEY": jarKey}, ""},
		{"clave corta", map[string]string{"PYTHON_BASE_URL": "https://localhost:8000", "PYTHON_API_KEY": "corta", "COOKIE_JAR_KEY": jarKey}, "PYTHON_API_KEY"},
		{"sin clave", map[string]string{"PYTHON_BASE_URL": "https://localhost:8000", "COOKIE_JAR_KEY": jarKey}, "PYTHON_API_KEY"},
		{"sin jar", map[string]string{"PYTHON_BASE_URL": "https://localhost:8000", "PYTHON_API_KEY": goodKey}, "COOKIE_JAR_KEY"},
		{"esquema raro", map[string]string{"PYTHON_BASE_URL": "ftp://x", "PYTHON_API_KEY": goodKey, "COOKIE_JAR_KEY": jarKey}, "esquema"},
		{"sin host", map[string]string{"PYTHON_BASE_URL": "https://", "PYTHON_API_KEY": goodKey, "COOKIE_JAR_KEY": jarKey}, "inválida"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pyCfg(t, tc.env).Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("no debía fallar: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, esperaba algo con %q", err, tc.wantErr)
			}
		})
	}
}
