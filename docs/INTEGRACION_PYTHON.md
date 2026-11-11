# Go → Python (entregas y notas a Brightspace)

Go es cliente HTTPS del servicio Python (contrato §5). Código: `internal/pyclient` (cliente), `internal/handler/d2lsync.go` (cola) y `internal/auth/d2l.go` (estados). Si `PYTHON_BASE_URL` está vacía, Go no llama a Python, no corre el trabajador y las entregas y revisiones funcionan como siempre, sin estado Brightspace.

## Variables de entorno

| Variable | Obligatoria | Qué hace |
|---|---|---|
| `PYTHON_BASE_URL` | no | URL `https://` del servicio Python (p. ej. `https://localhost:8000`). Vacía = integración apagada. |
| `PYTHON_API_KEY` | con `PYTHON_BASE_URL` | Se envía como `Authorization: Bearer`. Mínimo 32 bytes (`openssl rand -hex 32`). Es la misma que Python valida. |
| `COOKIE_JAR_KEY` | con `PYTHON_BASE_URL` | AES-256 en base64 (`openssl rand -base64 32`). Cifra las cookies de Python guardadas por usuario. |
| `PYTHON_CA_FILE` | no | PEM de la CA que firmó el certificado de Python (mkcert: `$(mkcert -CAROOT)/rootCA.pem`). Se suma al almacén del sistema. |
| `PYTHON_ALLOW_HTTP` | no | `true` solo en desarrollo: permite `http://`. Aun así las cookies de usuario nunca viajan por http (las llamadas en nombre de un usuario fallan con un mensaje claro). |
| `PYTHON_COOKIE_NAMES` | no | Cookies que Go captura, envía y acepta (por defecto `auth_code,access_token,refresh_token,response_time`). |

Go nunca desactiva la verificación TLS. `start.sh` pasa estas variables desde `.env` y genera `COOKIE_JAR_KEY` si falta.

## Qué debe implementar Python

Todas llevan `Authorization: Bearer <PYTHON_API_KEY>`. Las dos de `dropbox` llevan además la cabecera `Cookie` del usuario en cuyo nombre se actúa (estudiante en `submit`, docente en `feedback`). Errores: `{"error": "...", "code": "..."}`.

| Método y ruta | Cuerpo | Éxito | Errores que Go entiende |
|---|---|---|---|
| `POST /internal/v1/dropbox/submit` (30 s) | `student_external_id`, `course_external_id`, `assignment_external_id`, `filename`, `content`, `comment`, `go_submission_id` | `200 {"status":"enviada","external_id":"d2l:sub:9001"}` | ver abajo |
| `POST /internal/v1/dropbox/feedback` (30 s) | `teacher_external_id`, `student_external_id`, `course_external_id`, `assignment_external_id`, `score` (número o `null`), `score_denominator`, `feedback`, `is_graded`, `go_review_id` | `200 {"status":"publicada"}` | los mismos y `no_es_docente` (403) |
| `GET /internal/v1/health` (10 s, sin cookies) | — | `200 {"status":"ok","brightspace":true}` | cualquier otra cosa cuenta como «python: false» |

| `code` | HTTP | Go hace |
|---|---|---|
| `token_invalido` (también `401` o `408` sin código) | 502 | `fallida` con «vuelve a iniciar sesión»; se revive sola cuando el usuario inicia sesión de nuevo |
| `fuera_de_ventana`, `carpeta_no_acepta`, `muy_grande` (y cualquier otro 4xx) | 409/413 | `fallida` con el mensaje |
| `limite_tasa` con `retry_after` (segundos, o cabecera `Retry-After`) | 503 | `pendiente`; espera lo mayor entre `retry_after` y el backoff |
| `brightspace_caido`, 5xx, error de red, timeout | 502 | `pendiente` con reintento |

Reintentos: espera de 1 min, 5 min y 30 min (30 min desde el cuarto), máximo 5 intentos; el quinto fallo deja la fila en `fallida`. Python debe ser idempotente por `go_submission_id` / `go_review_id`.

`score = nota ÷ 5,0 × score_denominator`, a 2 decimales (4,2 con denominador 100 = `84.0`). Sin `score_denominator` en el taller, 100. Sin nota: `score: null` e `is_graded: false`.

## Cookies

Al canjear el ticket (`GET /api/auth/session?ticket=`) Go guarda las cookies de `PYTHON_COOKIE_NAMES` que el navegador trae, por usuario y cifradas (AES-GCM, ligadas a usuario y nombre) en `python_cookie_jar`. En cada llamada en nombre de un usuario Go manda solo las de ese usuario, solo a `PYTHON_BASE_URL` y solo por https. Los `Set-Cookie` de la respuesta actualizan o borran las de ese usuario (nombre no permitido: se ignora; valor vacío, `Max-Age` ≤ 0 o `Expires` pasado: se borra; `Max-Age=90.0` se acepta). Los valores nunca se registran. El cierre de sesión borra el almacén del usuario.

## Probar a mano

Con Go sin Python, `curl -sk https://localhost:8080/api/health` no trae «python». Con la integración activa trae `"python": true|false` (informativo: nunca degrada el estado).

Llamar a Python como lo haría Go (la CA de mkcert evita el `-k`):

```bash
CA="$(mkcert -CAROOT)/rootCA.pem"
curl --cacert "$CA" -H "Authorization: Bearer $PYTHON_API_KEY" https://localhost:8000/internal/v1/health

curl --cacert "$CA" -X POST https://localhost:8000/internal/v1/dropbox/submit \
  -H "Authorization: Bearer $PYTHON_API_KEY" -H "Content-Type: application/json" \
  -H "Cookie: access_token=<valor>; refresh_token=<valor>" \
  -d '{"student_external_id":"d2l:user:3101","course_external_id":"d2l:ou:6650","assignment_external_id":"d2l:folder:701","filename":"main.cpp","content":"int main(){}","comment":"Entrega desde EduTrace","go_submission_id":"prueba1"}'
```

De punta a punta: inicia sesión por Python, entrega un taller que venga de Brightspace (con `external_id`) y revisa la fila: la respuesta de `GET /api/student/submissions` trae `brightspace_status` (`pendiente` → `enviada`). Cuando el docente devuelve la revisión, su `GET /api/teacher/submissions/{id}/review` trae `brightspace_published` (y `brightspace_published_at` o `brightspace_error`). En SQLite: `SELECT d2l_status, d2l_attempts, d2l_next_retry_at, d2l_error FROM submissions;`.

Pruebas automáticas: `go test ./internal/pyclient ./internal/handler -run 'D2L|Health'` (servidor falso con `httptest.NewTLSServer`).
