# EduTrace

Plataforma de retroalimentación formativa para **Introducción a la Programación (C++)**. El estudiante escribe y compila su código en el navegador. Cada entrega se ejecuta con los casos de prueba del profesor, y una IA local (Ollama) prepara una propuesta de retroalimentación que el profesor revisa, edita, aprueba o descarta antes de que llegue al estudiante.

> EduTrace no califica sola. La IA propone; la decisión final siempre es del profesor.

EduTrace se conecta con el **Campus Virtual** de la universidad (Brightspace, D2L) a través de un servicio en Python, [EduTrace - Brightspace Hashi](https://github.com/alejandrogonzalezo1055/Brightspace-Hashi): se entra con la cuenta de Brightspace y las clases, los roles y los talleres llegan de allí. Esa integración se resume en [Campus Virtual (Brightspace)](#campus-virtual-brightspace); el detalle está en el repositorio del servicio.

## Qué hace

**Estudiante**
- Editor C++ con compilación y ejecución aisladas (`g++ -std=c++17` dentro de `bubblewrap`), con entrada estándar. Cuando un programa lee con `cin` después de mostrar un mensaje, la salida salta de línea sola, como en una terminal.
- Entra con su cuenta de Brightspace y ve sus clases y los talleres que su docente publicó allí, con sus archivos adjuntos.
- Talleres con enunciado, archivos, fecha límite y casos de prueba: al compilar ve qué casos pasa; de los ocultos, solo si pasan.
- Si no compila o falla al ejecutar, ve en español qué significa cada error y en qué línea, sin usar la IA.
- Agrega sus propios casos de prueba, que corren con los del profesor y se guardan en su navegador.
- Practica con un banco de 43 ejercicios que se corrigen solos, desde entrada y salida hasta archivos de texto, y ve su progreso: notas, niveles por RAE y ejercicios resueltos.
- Entregas oficiales y su historial, con la revisión del profesor, el nivel por RAE y quién la aprobó. Cada entrega queda lista para viajar a Brightspace (ver [Campus Virtual](#campus-virtual-brightspace)).
- Puede preguntar sobre cada retroalimentación y responder si la entendió.
- Si la clase está en modo inmediato, también puede pedir retroalimentación formal o informal, pistas graduadas (de la más general a la más concreta, sin la solución) y chatear con un tutor de IA (hasta `AI_REQUESTS_PER_HOUR` pedidos por hora).
- Recibe avisos en la campana, y por correo si lo permite, cuando le devuelven o actualizan una revisión, le responden una pregunta o un monitor comenta su código.

**Para todos**
- Página de inicio pública en `/inicio`, con qué es EduTrace, cómo funciona y cómo se cuidan los datos. Quien entra a `/` sin sesión llega allí.
- Barra de búsqueda en la parte superior (`Ctrl+K`). Sin importar tildes ni mayúsculas, encuentra lo que cada rol puede ver: estudiantes, clases, talleres, materias, ejercicios y secciones.

**Profesor**
- Sus materias, clases, estudiantes y talleres vienen de Brightspace, donde se administran (las carpetas de entrega de Brightspace son los talleres). En EduTrace configura lo pedagógico: casos de prueba, rúbrica y los 4 RAE oficiales del curso 33698.
- Cola automática: cada entrega oficial recibe una propuesta de la IA, con nota tentativa, niveles por RAE y una estimación de minutos de revisión.
- Panel de revisión para editar, aprobar o descartar la propuesta, con su historial en la bitácora. Con rúbrica de puntajes, la nota se calcula sola. Atajos: `Ctrl+S` guarda, `Ctrl+Enter` aprueba y devuelve, `Alt+↓` va a la siguiente entrega por revisar.
- En la clase:
  - modo de IA (supervisado o inmediato);
  - indicadores de tiempos, cola y comprensión con sus metas;
  - tiempo estimado de revisión por taller;
  - errores frecuentes;
  - rendimiento por RAE: el nivel lo marca el profesor y la nota también pesa. Si se aleja un poco del rango habitual del nivel, se señala con una flecha; si se aleja medio punto o más, queda en «Para revisar»;
  - exportación de notas y revisiones en CSV.
- **Historial de cursos** con las materias en curso y las de periodos anteriores, y **Cómo va el curso** con la participación, las revisiones pendientes, las notas y los RAE de todas las clases de una materia.
- Responde las preguntas de los estudiantes, y recibe un aviso si la IA falla.
- Marca un taller como **evaluación**: hasta su fecha límite, los estudiantes de la clase no pueden usar la IA ni el chat.
- Ve **alertas tempranas** (talleres vencidos sin entregar, inactividad, muchos errores seguidos, nivel bajo en RAE), detecta **código similar** entre entregas y genera un **reporte por estudiante** listo para imprimir o guardar en PDF.
- **Cierra el periodo** de una materia: ya no recibe entregas ni inscripciones y sus datos se borran solos después del plazo de conservación (`RETENTION_DAYS`).

**Monitor**
- Ve las entregas de las clases asignadas, comenta y marca el código, y responde preguntas. No califica ni ve las notas de los demás.
- Para su propio trabajo tiene todo lo de un estudiante: compila, entrega, practica y consulta su historial y su progreso, con los mismos límites de IA. No puede ser estudiante y monitor de la misma materia, porque para ser monitor ya debió cursarla.

**IA**
- Modelo local por Ollama (por defecto `llama3.1:8b`). Ningún dato sale del servidor y el modelo nunca recibe el nombre ni el email del estudiante.
- La retroalimentación sigue las tres preguntas de Hattie y Timperley e incluye observaciones de legibilidad, mantenibilidad y documentación. Si el modelo omite algo, se completa solo.
- Un filtro impide que la respuesta incluya la solución del ejercicio.
- Cada propuesta guarda qué modelo y qué versión del prompt la generó. No se muestra en pantalla; la exportación de revisiones de la clase incluye el modelo.

## Campus Virtual (Brightspace)

La universidad usa Campus Virtual, que funciona sobre Brightspace (D2L). EduTrace no habla con Brightspace directamente: lo hace el servicio en Python **[EduTrace - Brightspace Hashi](https://github.com/alejandrogonzalezo1055/Brightspace-Hashi)**, que se encarga de la autenticación OAuth2 y de las llamadas a la API de Brightspace. Cómo funciona por dentro, sus rutas y su configuración están en el README de ese repositorio.

Lo que esto significa para EduTrace:

| Tema | Cómo queda con la integración |
|---|---|
| **Ingreso** | Solo con Brightspace («Entrar con Brightspace»). Go ya no tiene registro, contraseñas ni códigos de recuperación: esas rutas responden 410. La sesión se abre con un ticket de un solo uso que pide el servicio Python. |
| **Clases y roles** | Las inscripciones de cada usuario llegan de Brightspace. El rol (docente o estudiante) sale del rol del curso allí (`D2L_ROLE_MAP`). Ya no hay códigos de inscripción. |
| **Talleres** | Cada carpeta de entrega de Brightspace es un taller, con sus archivos adjuntos (hasta 10 MB y 10 archivos por taller). Los talleres ocultos en Brightspace no se muestran. En EduTrace el docente añade casos de prueba y rúbrica. |
| **Entregas y notas** | Go deja cada entrega oficial y cada revisión devuelta en una cola (SQLite) para enviarlas a Brightspace por el servicio Python, con reintentos. |

Estado de la integración:

- **Funciona** (probado contra el Brightspace de pruebas): ingreso, inscripciones, talleres y archivos adjuntos.
- **Falta del lado de Python:** recibir las entregas (`dropbox/submit`) y las notas con su retroalimentación (`dropbox/feedback`). Mientras tanto la cola de Go está **pausada por defecto** (`D2L_QUEUE_PAUSED`; con `false` se reactiva), así que las entregas y las revisiones quedan guardadas y se siguen usando en EduTrace, pero no se envían a Brightspace.
- Si `PYTHON_BASE_URL` está vacía, Go no llama a Python y las entregas y revisiones funcionan sin estado de Brightspace.

El contrato entre Go y Python y cómo probarlo a mano están en [docs/INTEGRACION_PYTHON.md](docs/INTEGRACION_PYTHON.md).

## Arquitectura

```mermaid
flowchart LR
    W["Navegador (Next.js 15)"] -->|"REST + SSE"| B["Backend Go :8080"]
    B --> D[("SQLite<br/>data/edutrace.db")]
    B --> S["bubblewrap + g++"]
    B --> O["Ollama :11434"]
    W -->|"Entrar con Brightspace"| P["Servicio Python :9000<br/>EduTrace - Brightspace Hashi"]
    P -->|"/internal/v1/*"| B
    B -->|"entregas y notas (cola)"| P
    P -->|"OAuth2 + API"| L["Brightspace (Campus Virtual)"]
```

Es un monolito modular en Go, con un servicio Python aparte para Brightspace:

| Paquete | Qué hace |
|---|---|
| `cmd/server` | Punto de entrada |
| `internal/handler` | HTTP, cola de propuestas, rutas `/internal/v1/*` del servicio Python y cola hacia Brightspace |
| `internal/pyclient` | Cliente HTTPS de Go hacia el servicio Python |
| `internal/auth` | Acceso a datos y reglas de permisos en SQL; usuarios, inscripciones y talleres de Brightspace; tickets de sesión |
| `internal/ai` | Prompts, filtro anti-solución, secciones y calidad |
| `internal/compiler` | Sandbox y casos de prueba |
| `internal/mail` | Envío de avisos por SMTP |
| `internal/backup` | Respaldo cifrado, verificación y restauración de `data/` |
| `internal/practice` | Banco de ejercicios de práctica |
| `internal/similarity` | Detección de código similar |
| `internal/config` | Variables de entorno |

El frontend está en `web/`.

## Requisitos

- Go 1.26 o superior.
- Node.js 20 o superior y npm.
- `g++`, `bubblewrap` 0.8 o superior, y `prlimit` y `choom` (paquete `util-linux`).
- Ollama con el modelo `llama3.1:8b`. Se recomienda GPU: cada propuesta tarda unos 25 s. Sin GPU puede tardar minutos, y el chat espera detrás de la cola de propuestas.
- Para entrar con Brightspace: el servicio [EduTrace - D2L Connection](https://github.com/alejandrogonzalezo1055/D2L-Connection) en marcha (Python 3.12 o superior), una aplicación OAuth2 registrada en Brightspace y `mkcert` para el HTTPS local.

## Inicio rápido

```bash
cp .env.example .env
./start.sh
```

En `.env`, define `INTERNAL_API_KEY` (≥32 bytes): es la clave con la que el servicio Python sincroniza usuarios, inscripciones y talleres. Ya no hay registro ni código de profesor: se entra con Brightspace, así que para iniciar sesión hay que tener también el servicio Python corriendo (sigue las instrucciones de su [README](https://github.com/alejandrogonzalezo1055/D2L-Connection)). Las claves de uno y otro lado deben coincidir (`INTERNAL_API_KEY` y `PYTHON_API_KEY`, cada una de ≥32 bytes y distintas entre sí).

`start.sh`:
- verifica las dependencias;
- descarga el modelo si falta;
- genera `INTERNAL_API_KEY` y `COOKIE_JAR_KEY` si faltan;
- levanta el backend en `http://127.0.0.1:8080` (o `https://` si defines `TLS_CERT_FILE` y `TLS_KEY_FILE`) y el frontend en `http://localhost:3000`.

`start.sh` reinstala las dependencias del frontend y reconstruye su caché solo cuando cambian; `./start.sh --limpio` la borra a la fuerza.

Para cargar datos de prueba (un profesor, 5 estudiantes, talleres y entregas):

```bash
./scripts/seed_test_data.sh
```

Las cuentas y el detalle están en [docs/TEST_DATA.md](docs/TEST_DATA.md). Ojo: esas cuentas usan contraseña y el ingreso con contraseña ya no existe (responde 410), así que no sirven para iniciar sesión en la interfaz; allí se entra con Brightspace.

## Configuración (`.env`)

| Variable | Por defecto | Para qué |
|---|---|---|
| `PORT` / `HOST` | `8080` / `127.0.0.1` | Dirección del backend |
| `ALLOWED_ORIGINS` | `http://localhost:3000` | Orígenes del frontend permitidos (CORS) |
| `DATA_DIR` | `data` | Base SQLite y adjuntos (fuera de git) |
| `INTERNAL_API_KEY` | vacío | Clave Python → Go de `/internal/v1/*` (≥32 bytes); vacío = deshabilitado |
| `PYTHON_BASE_URL`, `PYTHON_API_KEY`, `PYTHON_CA_FILE`, `PYTHON_ALLOW_HTTP`, `COOKIE_JAR_KEY`, `PYTHON_COOKIE_NAMES`, `D2L_ROLE_MAP`, `ROSTER_MAX_DEACTIVATE_PERCENT`, `TLS_CERT_FILE`, `TLS_KEY_FILE` | ver `.env.example` | Conexión con el servicio Python y Brightspace (ver [docs/INTEGRACION_PYTHON.md](docs/INTEGRACION_PYTHON.md)) y HTTPS propio de Go |
| `D2L_QUEUE_PAUSED` | `true` | Con `true`, Go encola las entregas y revisiones para Brightspace pero no las envía; `false` activa el envío |
| `COOKIE_SECURE` | `false` | `true` en producción con HTTPS |
| `SANDBOX` | `bwrap` | `none` solo para desarrollo |
| `COMPILE_TIMEOUT_SECS` / `RUN_TIMEOUT_SECS` | `10` / `5` | Límites de compilación y ejecución |
| `OLLAMA_BASE_URL` | `http://localhost:11434` | Servidor de Ollama |
| `OLLAMA_MODEL` | `llama3.1:8b` | Modelo |
| `OLLAMA_CONTEXT_SIZE` | `6144` | Contexto del modelo |
| `OLLAMA_TIMEOUT` | `180` | Segundos máximos por respuesta |
| `PUBLIC_URL` | el primero de `ALLOWED_ORIGINS` | Dirección pública, para los enlaces de los correos |
| `TRUST_PROXY` | `false` | `true` detrás de Caddy, para usar la IP real del estudiante |
| `MAX_SANDBOXES` | núcleos de la CPU | Programas que compilan o corren a la vez |
| `AI_REQUESTS_PER_HOUR` | `20` | Pedidos a la IA por estudiante y por hora; `0` sin límite |
| `RETENTION_DAYS` | `180` | Días que se conservan los datos de un periodo cerrado; `0` no borra nada |
| `DISK_MIN_FREE_PERCENT` | `10` | Por debajo, `/api/health` avisa |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASSWORD`, `SMTP_FROM` | vacío, `587` | Avisos por correo; sin `SMTP_HOST` solo hay avisos en la plataforma |
| `BACKUP_DIR`, `BACKUP_KEEP`, `RESPALDO_REMOTO` | `respaldos`, `14`, vacío | Respaldos diarios |
| `BACKUP_RECIPIENT`, `BACKUP_IDENTITY` | vacío | Clave pública con la que se cifran los respaldos, y archivo de la clave privada para restaurarlos |
| `ALERTA_URL`, `ALERTA_JSON`, `LATIDO_URL` | vacío | Alertas de `scripts/vigilar.sh` |

## Pruebas, CI y CD

```bash
go test ./...
cd web && npx tsc --noEmit && npm run lint && npm run build
```

GitHub Actions corre esas mismas pruebas en cada *push* y pull request, además de `govulncheck`, `npm audit` y una revisión de accesibilidad WCAG 2.1 AA con axe sobre la aplicación funcionando (`.github/workflows/ci.yaml`). Cuando pasan en `main`, despliega en el servidor con `scripts/desplegar.sh`, que vuelve a la versión anterior si algo falla (`.github/workflows/cd.yaml`). La preparación del servidor, HTTPS, respaldos y alertas están en [docs/DESPLIEGUE.md](docs/DESPLIEGUE.md).

## Scripts

| Script | Para qué |
|---|---|
| `scripts/seed_test_data.sh` | Datos de prueba |
| `scripts/seed_rae_levels.py` | Carga los RAE oficiales y los niveles en una base que ya tenía datos de prueba |
| `scripts/prueba_carga.py N` | Envía N entregas y mide cuánto tarda la cola (RNF-10) |
| `scripts/medir_ancho_banda.py` | Peso de las páginas y de la API (RNF-12) |
| `scripts/desplegar.sh [commit]` | Despliega en el servidor y vuelve atrás si falla |
| `scripts/respaldar.sh` | Respalda y verifica `data/`, y borra los respaldos antiguos |
| `scripts/restaurar.sh archivo` | Restaura un respaldo y vuelve atrás si EduTrace no arranca |
| `scripts/vigilar.sh` | Revisa la salud de EduTrace y envía alertas |
| `scripts/limpiar_historial.sh` | Quita del historial de git una carpeta subida por error |
| `go run ./cmd/aieval` | Compara modelos de IA con casos de evaluación |

## Documentación

- [docs/RNF_COMPLIANCE.md](docs/RNF_COMPLIANCE.md): matriz de cumplimiento de los requisitos.
- [docs/POLITICA_DATOS.md](docs/POLITICA_DATOS.md): qué datos se tratan y qué recibe la IA.
- [docs/INTEGRACION_PYTHON.md](docs/INTEGRACION_PYTHON.md): contrato entre Go y el servicio Python de Brightspace, variables y cómo probarlo.
- [EduTrace - Brightspace Hashi](https://github.com/alejandrogonzalezo1055/Brightspace-Hashi): repositorio y README del servicio Python.
- [docs/INTEGRACION.md](docs/INTEGRACION.md): análisis inicial de la integración con Campus Virtual (LTI y API de Brightspace); anterior al servicio Python.
- [docs/TEST_DATA.md](docs/TEST_DATA.md): datos de prueba.
- [docs/DESPLIEGUE.md](docs/DESPLIEGUE.md): CI, CD, preparación del servidor, HTTPS, sandbox, respaldos, alertas y correo.

## Herramientas de seguimiento de grupos (Python)

`src/classroom`, `src/telegram`, `src/rag`, `requirements.txt` y el workflow `.github/workflows/jirafales.yaml` no forman parte de EduTrace, y el CI no los revisa. Son las herramientas de x19marlon para seguir los repositorios de los grupos del curso:
- el workflow corre cada lunes, analiza los commits de cada grupo y guarda los reportes en MongoDB;
- el bot de Telegram consulta esos reportes.
