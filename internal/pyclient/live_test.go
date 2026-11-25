package pyclient

// Pruebas contra el servicio Python REAL (EduTrace - D2L Connection), no contra uno falso.
//
// Se saltan solas si no hay un Python al que llamar. Para correrlas:
//
//	PYTHON_LIVE_URL=https://localhost:9000 PYTHON_API_KEY=<clave> \
//	PYTHON_CA_FILE="$(mkcert -CAROOT)/rootCA.pem" go test ./internal/pyclient -run Live -v
//
// o simplemente ./scripts/probar_python.sh, que lee esas variables de .env.
//
// Solo hacen lecturas y peticiones que Python debe rechazar: no tocan Brightspace ni crean datos.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// livePython devuelve un cliente apuntando al Python real, o salta la prueba si no está configurado.
func livePython(t *testing.T, key string) (*Client, string) {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("PYTHON_LIVE_URL"))
	if base == "" {
		t.Skip("PYTHON_LIVE_URL vacía: no hay un servicio Python real contra el cual probar")
	}
	if key == "" {
		key = os.Getenv("PYTHON_API_KEY")
	}
	if key == "" {
		t.Skip("PYTHON_API_KEY vacía: no se puede autenticar contra Python")
	}
	cfg := Config{BaseURL: base, APIKey: key, CAFile: os.Getenv("PYTHON_CA_FILE"), CookieNames: names, Timeout: 10 * time.Second}
	c, err := New(cfg, newJar())
	if err != nil {
		t.Fatalf("configuración del cliente: %v", err)
	}
	return c, base
}

// liveHTTP es un cliente HTTP plano (sin el de pyclient) que confía en la misma CA, para probar
// peticiones que pyclient nunca haría, como una sin clave.
func liveHTTP(t *testing.T) *http.Client {
	t.Helper()
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if ca := os.Getenv("PYTHON_CA_FILE"); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			t.Fatalf("leer PYTHON_CA_FILE: %v", err)
		}
		pool.AppendCertsFromPEM(pem)
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func TestLivePythonHealth(t *testing.T) {
	c, _ := livePython(t, "")
	res, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("GET /internal/v1/health contra Python real: %v", err)
	}
	if res.Status != "ok" {
		t.Errorf("status = %q, quería ok", res.Status)
	}
	if res.Brightspace == nil {
		t.Error("Python no informa si Brightspace responde (falta el campo brightspace)")
	} else if !*res.Brightspace {
		t.Error("Python dice que Brightspace no responde")
	}
}

func TestLivePythonPublicHealth(t *testing.T) {
	_, base := livePython(t, "")
	resp, err := liveHTTP(t).Get(strings.TrimRight(base, "/") + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health = %d, quería 200", resp.StatusCode)
	}
}

func TestLivePythonRejectsWrongKey(t *testing.T) {
	c, _ := livePython(t, "clave-equivocada-0123456789abcdef0123456789abcdef")
	_, err := c.Health(context.Background())
	if err == nil {
		t.Fatal("Python aceptó una clave equivocada en /internal/v1/health")
	}
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != http.StatusUnauthorized {
		t.Errorf("error = %v, quería un 401 de Python", err)
	}
}

func TestLivePythonRejectsMissingKey(t *testing.T) {
	_, base := livePython(t, "")
	resp, err := liveHTTP(t).Get(strings.TrimRight(base, "/") + "/internal/v1/health")
	if err != nil {
		t.Fatalf("GET sin clave: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("sin Authorization Python respondió %d, quería 401", resp.StatusCode)
	}
}

// Contrato de las rutas que Go usa para enviar entregas y notas. Mientras Python no las tenga
// (hoy responde 404 o 405) la prueba se salta con ese aviso; cuando existan, exige que respondan con
// un error del contrato y no con algo que Go no entienda. Sin cookies de usuario, Python no puede
// actuar en nombre de nadie, así que lo esperado es un rechazo, nunca un éxito.
func TestLivePythonSubmitContract(t *testing.T) {
	c, _ := livePython(t, "")
	_, err := c.Submit(context.Background(), "usuario-sin-cookies", submitReq())
	assertLiveRejection(t, "dropbox/submit", err)
}

func TestLivePythonFeedbackContract(t *testing.T) {
	c, _ := livePython(t, "")
	score := 4.0
	err := c.Feedback(context.Background(), "docente-sin-cookies", FeedbackRequest{
		TeacherExternalID: "d2l:user:2001", StudentExternalID: "d2l:user:3101", CourseExternalID: "d2l:ou:6650",
		AssignmentExternalID: "d2l:folder:701", Score: &score, ScoreDenominator: 100, Feedback: "prueba", IsGraded: true, GoReviewID: "r1",
	})
	assertLiveRejection(t, "dropbox/feedback", err)
}

func assertLiveRejection(t *testing.T, route string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s aceptó una petición sin cookies de usuario: Python no puede actuar en nombre de nadie sin ellas", route)
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("%s: error que no es del contrato: %v", route, err)
	}
	if pe.Status == http.StatusNotFound || pe.Status == http.StatusMethodNotAllowed {
		t.Skipf("Python todavía no implementa %s (responde HTTP %d)", route, pe.Status)
	}
	if pe.Code == "" {
		t.Errorf("%s respondió HTTP %d sin el campo code del contrato", route, pe.Status)
	}
}
