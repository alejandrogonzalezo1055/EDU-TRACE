package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/marlon/edu-trace/internal/ai"
	"github.com/marlon/edu-trace/internal/auth"
	"github.com/marlon/edu-trace/internal/compiler"
	"github.com/marlon/edu-trace/internal/config"
	"github.com/marlon/edu-trace/internal/handler"
	"github.com/marlon/edu-trace/internal/mail"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		slog.Error("configuración inválida", "error", err)
		os.Exit(1)
	}

	if err := compiler.CheckSandbox(cfg.Sandbox); err != nil {
		slog.Error("sandbox no disponible — el servidor no ejecutará código sin aislamiento", "error", err)
		os.Exit(1)
	}
	if cfg.Sandbox == "none" {
		slog.Warn("SANDBOX=none: el código de los estudiantes se ejecuta SIN aislamiento, úsalo solo en desarrollo")
	} else if !compiler.TmpLimited() {
		slog.Warn("esta versión de bubblewrap no limita el tamaño de /tmp en el sandbox; actualízala a 0.8 o superior")
	}
	if !cfg.CookieSecure && strings.HasPrefix(cfg.PublicURL, "https://") {
		slog.Warn("PUBLIC_URL usa HTTPS pero COOKIE_SECURE no es true: la cookie de sesión podría viajar sin cifrar")
	}
	if !cfg.TLSEnabled() {
		slog.Warn("sin TLS_CERT_FILE/TLS_KEY_FILE: Go sirve HTTP y no fuerza cookies Secure; úsalo solo en desarrollo local")
	}
	if cfg.InternalAPIKey == "" {
		slog.Warn("INTERNAL_API_KEY vacía: las rutas /internal/v1/* están deshabilitadas y no se puede iniciar sesión (el servicio Python no puede sincronizar)")
	}

	store, err := auth.OpenStore(cfg.DataDir, cfg.SessionTTL)
	if err != nil {
		slog.Error("no se pudo abrir el almacén de usuarios", "error", err)
		os.Exit(1)
	}
	if len(cfg.CookieJarKey) > 0 {
		if err := store.SetCookieJarKey(cfg.CookieJarKey); err != nil {
			slog.Error("COOKIE_JAR_KEY inválida", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Warn("COOKIE_JAR_KEY vacía: no se guardan las cookies de Python (el cliente Go → Python no tendrá tokens)")
	}
	store.SetRetention(time.Duration(cfg.RetentionDays) * 24 * time.Hour)

	// Cliente hacia el servicio Python (Brightspace). Sin PYTHON_BASE_URL no hay cliente, ni cola, ni
	// estado Brightspace: las entregas y revisiones funcionan como siempre.
	pyClient, err := handler.NewPythonClient(cfg, store)
	if err != nil {
		slog.Error("cliente de Python inválido", "error", err)
		os.Exit(1)
	}
	store.SetD2LEnabled(pyClient != nil)
	if pyClient == nil {
		slog.Info("PYTHON_BASE_URL vacía: Go no envía entregas ni notas a Brightspace")
	}

	comp := compiler.New(cfg)

	// Initialize Ollama client (non-fatal if unavailable).
	var ollama *ai.OllamaClient
	ollamaClient, err := ai.NewOllamaClient(cfg)
	if err != nil {
		slog.Warn("Ollama not available — feedback disabled", "error", err)
	} else {
		ollama = ollamaClient
	}

	drafts := handler.NewDraftQueue(store, ollama, time.Duration(cfg.OllamaTimeout)*time.Second)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go drafts.Run(workerCtx)
	mailCfg := mail.Config{Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom}
	go handler.NewMailer(store, mailCfg, cfg.PublicURL).Run(workerCtx)
	// TEMPORAL: con la cola pausada el trabajador no arranca, así que nada se envía a Python. Los envíos
	// siguen guardándose como 'pendiente' en la base (SetD2LEnabled sigue activo) y se enviarán cuando se
	// ponga D2L_QUEUE_PAUSED=false.
	if pyClient != nil && cfg.D2LQueuePaused {
		slog.Warn("cola hacia Brightspace PAUSADA (D2L_QUEUE_PAUSED): las entregas y revisiones quedan pendientes en la base y no se envían a Python")
	} else {
		go handler.NewD2LQueue(store, pyClient).Run(workerCtx)
	}
	if cfg.RetentionDays > 0 {
		go purgeLoop(workerCtx, store)
	} else {
		slog.Warn("RETENTION_DAYS=0: los datos de los periodos cerrados no se borran automáticamente")
	}

	router := handler.NewRouter(cfg, comp, ollama, store, drafts, mailCfg.Enabled(), pyClient)

	srv := &http.Server{
		Addr: net.JoinHostPort(cfg.Host, cfg.Port),
		Handler: router,
		ReadTimeout: 15 * time.Second,
		WriteTimeout: 200 * time.Second, // Long for Ollama responses
		IdleTimeout: 60 * time.Second,
	}

	// Graceful shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig.String())
		stopWorker()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("shutdown error", "error", err)
		}
	}()

	slog.Info("EDU-TRACE server starting",
		"addr", srv.Addr,
		"sandbox", cfg.Sandbox,
		"g++", cfg.GppPath,
		"ollama_url", cfg.OllamaBaseURL,
		"ollama_model", cfg.OllamaModel,
		"ollama_enabled", ollama != nil,
		"tls", cfg.TLSEnabled(),
		"internal_api", cfg.InternalAPIKey != "",
		"python", pyClient != nil,
	)

	var serveErr error
	if cfg.TLSEnabled() {
		serveErr = srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	} else {
		serveErr = srv.ListenAndServe()
	}
	if err := serveErr; err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}

	if err := store.Close(); err != nil {
		slog.Error("close store", "error", err)
	}
	slog.Info("server stopped")
}

func purgeLoop(ctx context.Context, store *auth.Store) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		report, err := store.Purge(time.Now())
		if err != nil {
			slog.Error("borrado de datos vencidos", "error", err)
		} else if !report.Empty() {
			slog.Info("datos vencidos borrados", "materias", report.Courses, "cuentas", report.Accounts,
				"avisos", report.Notifications, "registros_ia", report.AIRequests, "sesiones", report.Sessions)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
