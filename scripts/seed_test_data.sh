#!/usr/bin/env bash
# Carga usuarios de prueba, cursos, clases y talleres en EDU-TRACE.
# Ya no hay registro ni contraseñas: los datos entran por /internal/v1 (como los empujaría el servicio Python
# desde Brightspace) y las sesiones se abren con tickets de un solo uso. Requiere INTERNAL_API_KEY: del entorno,
# de .env o de data/internal_api_key (la genera ./start.sh).
set -e
API="${API:-http://127.0.0.1:8080}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="$ROOT/.env"
if [ -z "$INTERNAL_API_KEY" ] && [ -f "$ENV_FILE" ]; then
  INTERNAL_API_KEY=$(grep -E '^INTERNAL_API_KEY=.+' "$ENV_FILE" | tail -1 | cut -d= -f2-)
fi
if [ -z "$INTERNAL_API_KEY" ] && [ -s "$ROOT/${DATA_DIR:-data}/internal_api_key" ]; then
  INTERNAL_API_KEY=$(cat "$ROOT/${DATA_DIR:-data}/internal_api_key")
fi
if [ -z "$INTERNAL_API_KEY" ]; then
  echo "Falta INTERNAL_API_KEY (entorno, .env o data/internal_api_key). Arranca antes ./start.sh." >&2
  exit 1
fi
CURL=(curl -s)
case "$API" in https://*) CURL+=(-k);; esac

jfield() {  # jfield <campo> : lee un campo del JSON de stdin y falla si trae "error"
  python3 -c "import sys, json
d = json.load(sys.stdin)
if isinstance(d, dict) and 'error' in d:
    sys.exit('  ✗ ' + d['error'])
print(d[sys.argv[1]])" "$1"
}

internal() {  # internal <método> <ruta> <json>
  "${CURL[@]}" -X "$1" "$API/internal/v1/$2" -H "Authorization: Bearer $INTERNAL_API_KEY" \
    -H "Content-Type: application/json" -d "$3"
}

put_user() {  # put_user <id d2l> <nombre> <correo>
  python3 -c "import json, sys; print(json.dumps({'name': sys.argv[1], 'email': sys.argv[2]}))" "$2" "$3" \
    | internal PUT "users/d2l:user:$1" "$(cat)" | jfield id > /dev/null
}

# enroll <id d2l> <rol> <id curso> <nombre curso> ... (pares curso/nombre) → imprime el JSON de respuesta
enroll() {
  local user="$1" role="$2"; shift 2
  python3 -c "import json, sys
a = sys.argv[2:]
print(json.dumps({'enrollments': [{'course': {'external_id': 'd2l:ou:' + a[i], 'name': a[i + 1], 'is_active': True}, 'role': sys.argv[1]} for i in range(0, len(a), 2)]}))" "$role" "$@" \
    | internal PUT "users/d2l:user:$user/enrollments" "$(cat)"
}

group_of() {  # group_of <json de enroll> <id curso> <campo>
  python3 -c "import sys, json
d = json.load(sys.stdin)
if 'error' in d: sys.exit('  ✗ ' + d['error'])
print(next(g[sys.argv[2]] for g in d['groups'] if g['course_external_id'] == 'd2l:ou:' + sys.argv[1]))" "$1" "$2"
}

session_cookie() {  # session_cookie <id d2l> : pide un ticket y lo canjea (sin seguir la redirección)
  local ticket
  ticket=$(internal POST session-tickets "{\"user_external_id\":\"d2l:user:$1\"}" | jfield ticket)
  "${CURL[@]}" -D - -o /dev/null "$API/api/auth/session?ticket=$ticket" | tr -d '\r' \
    | sed -n 's/^[Ss]et-[Cc]ookie: edutrace_session=\([^;]*\).*/\1/p'
}

PROF=9001
echo "=== 1. Sincronizando al profesor ==="
put_user "$PROF" "Prof. María García" "maria.garcia@universidad.edu.co"
PROF_COOKIE=$(session_cookie "$PROF")
[ -n "$PROF_COOKIE" ] || { echo "  ✗ no se pudo abrir la sesión del profesor" >&2; exit 1; }
echo "  Cookie profesor: $PROF_COOKIE"

echo "=== 2. y 3. Creando 2 cursos con su clase (cada oferta de Brightspace = 1 curso + 1 clase) ==="
COURSE1_NAME="Introducción a la Programación 2026-1"
COURSE2_NAME="Programación Avanzada 2026-1"
# Una sola instantánea por usuario: el profesor da clase en las dos ofertas
R=$(enroll "$PROF" teacher 6650 "$COURSE1_NAME" 6700 "$COURSE2_NAME")
GROUP1_ID=$(echo "$R" | group_of 6650 group_id)
GROUP2_ID=$(echo "$R" | group_of 6700 group_id)
echo "  Curso 1 (Intro Prog): clase $GROUP1_ID"
echo "  Curso 2 (Prog Avanzada): clase $GROUP2_ID"
"${CURL[@]}" -b "edutrace_session=$PROF_COOKIE" -X PATCH "$API/api/teacher/groups/$GROUP2_ID/ai-mode" \
  -H "Content-Type: application/json" -d '{"ai_mode":"inmediata"}' > /dev/null
echo "  Grupo A en modo supervisado (predeterminado) · Grupo B en modo inmediato"

echo "=== 4. Sincronizando 5 estudiantes e inscribiéndolos ==="

sync_student() {
  local ext="$1" name="$2" email="$3" course="$4" course_name="$5" label="$6"
  put_user "$ext" "$name" "$email"
  enroll "$ext" student "$course" "$course_name" | jfield enrolled > /dev/null
  echo "  ✓ $name ($email) → $label"
}

sync_student 9101 "Carlos Pérez"    "carlos.perez@est.edu.co"    6650 "$COURSE1_NAME" "Intro Prog"
sync_student 9102 "Ana Rodríguez"   "ana.rodriguez@est.edu.co"   6650 "$COURSE1_NAME" "Intro Prog"
sync_student 9103 "Luis Martínez"   "luis.martinez@est.edu.co"   6650 "$COURSE1_NAME" "Intro Prog"
sync_student 9104 "Sofía López"     "sofia.lopez@est.edu.co"     6700 "$COURSE2_NAME" "Prog Avanzada"
sync_student 9105 "Diego Ramírez"   "diego.ramirez@est.edu.co"   6700 "$COURSE2_NAME" "Prog Avanzada"

field() { jfield "$1"; }

iso_utc() {
  date -u -d "$1" +%Y-%m-%dT%H:%M:%SZ
}

# Los talleres son carpetas de entrega de Brightspace: título, enunciado y fechas llegan por /internal;
# la rúbrica, los casos de prueba y la penalización son de EduTrace y los pone el profesor.
publish_folders() {
  local payload
  payload=$(python3 - "$(iso_utc '+5 days')" "$(iso_utc '+7 days')" "$(iso_utc '+20 hours')" "$(iso_utc '-27 hours')" "$(iso_utc '+2 days')" <<'PY'
import json, sys
due1, end1, due2, due3, end3 = sys.argv[1:6]
print(json.dumps({"assignments": [
  {"external_id": "d2l:folder:701", "title": "Taller 1: promedio de notas",
   "statement": "Lee tres notas (de 0.0 a 5.0) y muestra su promedio con dos decimales.\nEjemplo: entrada 4.5 3.0 5.0 -> salida Promedio: 4.17",
   "due_at": due1, "end_at": end1, "score_denominator": 100, "grade_item_id": "d2l:grade:5001", "is_hidden": False},
  {"external_id": "d2l:folder:702", "title": "Taller 2: suma con ciclos",
   "statement": "Lee un entero n y muestra la suma de 1 hasta n usando un ciclo.\nEjemplo: entrada 5 -> salida Suma: 15",
   "due_at": due2, "end_at": due2, "score_denominator": 100, "grade_item_id": "d2l:grade:5002", "is_hidden": False},
  {"external_id": "d2l:folder:703", "title": "Taller 3: el mayor de tres",
   "statement": "Lee tres enteros y muestra el mayor usando condicionales.\nEjemplo: entrada 7 12 3 -> salida Mayor: 12",
   "due_at": due3, "end_at": end3, "score_denominator": 100, "grade_item_id": "d2l:grade:5003", "is_hidden": False}]}))
PY
)
  internal PUT "courses/d2l:ou:6650/assignments" "$payload" | jfield created > /dev/null
}

assignment_id() {  # assignment_id <título>
  "${CURL[@]}" -b "edutrace_session=$PROF_COOKIE" "$API/api/teacher/groups/$GROUP1_ID/assignments" \
    | python3 -c "import sys, json; print(next(a['id'] for a in json.load(sys.stdin) if a['title'] == sys.argv[1]))" "$1"
}

update_assignment() {  # update_assignment <id> <título> <enunciado> <rúbrica> [campo=valor ...]
  local id="$1" extra=()
  for kv in "${@:5}"; do extra+=(--form-string "$kv"); done
  "${CURL[@]}" -b "edutrace_session=$PROF_COOKIE" -X PUT "$API/api/teacher/assignments/$id" \
    --form-string "title=$2" --form-string "statement=$3" --form-string "rubric=$4" "${extra[@]}" | field id
}

login_cookie() {  # login_cookie <id d2l>
  session_cookie "$1"
}

attempt() {
  python3 -c "import json, sys; print(json.dumps({'group_id': sys.argv[1], 'assignment_id': sys.argv[2], 'code': sys.argv[3], 'stdin': sys.argv[4]}))" "$2" "$3" "$4" "$5" \
    | "${CURL[@]}" -b "edutrace_session=$1" "$API/api/compile" -H "Content-Type: application/json" --data @- > /dev/null
}

submit() {
  python3 -c "import json, sys; print(json.dumps({'group_id': sys.argv[1], 'assignment_id': sys.argv[2], 'code': sys.argv[3], 'stdin': sys.argv[4], 'ai_feedback': '', 'ai_feedback_informal': ''}))" "$2" "$3" "$4" "$5" \
    | "${CURL[@]}" -b "edutrace_session=$1" "$API/api/student/submissions" -H "Content-Type: application/json" --data @- | field id
}

review() {
  python3 -c "import json, sys; print(json.dumps({'feedback': sys.argv[1], 'grade': float(sys.argv[2]), 'return': sys.argv[3] == 'true'}))" "$4" "$2" "$3" \
    | "${CURL[@]}" -b "edutrace_session=$PROF_COOKIE" -X PUT "$API/api/teacher/submissions/$1/review" -H "Content-Type: application/json" --data @- | field status
}

echo "=== 5. Carpetas de entrega (Brightspace) y criterios de EduTrace en Intro a la Programación ==="
publish_folders > /dev/null
T1=$(assignment_id "Taller 1: promedio de notas")
T2=$(assignment_id "Taller 2: suma con ciclos")
T3=$(assignment_id "Taller 3: el mayor de tres")
update_assignment "$T1" "Taller 1: promedio de notas" "." \
  "Compila (1.0), Lee bien la entrada (1.0), Promedio correcto (2.0), Legibilidad (1.0)" \
  'tests=[{"name":"Ejemplo del enunciado","stdin":"4.5 3.0 5.0","expected":"Promedio: 4.17","match":"contiene","hidden":false},{"name":"Notas enteras","stdin":"4 5 5","expected":"Promedio: 4.67","match":"contiene","hidden":false},{"name":"Promedio bajo","stdin":"0 0 1","expected":"Promedio: 0.33","match":"contiene","hidden":true}]' > /dev/null
update_assignment "$T2" "Taller 2: suma con ciclos" "." \
  "Compila (1.0), Usa un ciclo (1.0), Resultado correcto (2.0), Legibilidad (1.0)" \
  'tests=[{"name":"Ejemplo del enunciado","stdin":"5","expected":"Suma: 15","match":"contiene","hidden":false},{"name":"n = 1","stdin":"1","expected":"Suma: 1","match":"contiene","hidden":false},{"name":"n = 10","stdin":"10","expected":"Suma: 55","match":"contiene","hidden":true}]' > /dev/null
update_assignment "$T3" "Taller 3: el mayor de tres" "." \
  "Compila (1.0), Usa condicionales (1.0), Resultado correcto (2.0), Legibilidad (1.0)" \
  "late_penalty=0.5" \
  'tests=[{"name":"Ejemplo del enunciado","stdin":"7 12 3","expected":"Mayor: 12","match":"contiene","hidden":false},{"name":"Repetidos","stdin":"5 5 2","expected":"Mayor: 5","match":"contiene","hidden":false},{"name":"Negativos","stdin":"-3 -8 -1","expected":"Mayor: -1","match":"contiene","hidden":true}]' > /dev/null
echo "  ✓ 3 carpetas con 3 casos de prueba cada una (1 oculto): la 1 vence en 5 días, la 2 en 20 h (sin tardías) y la 3 venció hace 27 h (tardías con −0.5 por día)"

ANA_COOKIE=$(login_cookie 9102)
CARLOS_COOKIE=$(login_cookie 9101)
LUIS_COOKIE=$(login_cookie 9103)

CODE_PROMEDIO=$(cat <<'EOF'
#include <iostream>
#include <iomanip>
using namespace std;

int main() {
    double n1, n2, n3;
    cin >> n1 >> n2 >> n3;
    double promedio = (n1 + n2 + n3) / 3;
    cout << fixed << setprecision(2);
    cout << "Promedio: " << promedio << endl;
    return 0;
}
EOF
)

CODE_PROMEDIO_ENTERO=$(cat <<'EOF'
#include <iostream>
using namespace std;

int main() {
    int a, b, c;
    cin >> a >> b >> c;
    double promedio = (a + b + c) / 3;
    cout << "Promedio: " << promedio << endl;
    return 0;
}
EOF
)

CODE_SUMA_SIN_PUNTO=$(cat <<'EOF'
#include <iostream>
using namespace std;

int main() {
    int n;
    cin >> n;
    int suma = 0
    for (int i = 1; i < n; i++) {
        suma += i;
    }
    cout << "Suma: " << suma << endl;
    return 0;
}
EOF
)

CODE_SUMA=$(cat <<'EOF'
#include <iostream>
using namespace std;

int main() {
    int n;
    cin >> n;
    int suma = 0;
    for (int i = 1; i < n; i++) {
        suma += i;
    }
    cout << "Suma: " << suma << endl;
    return 0;
}
EOF
)

CODE_SUMA_OK=$(cat <<'EOF'
#include <iostream>
using namespace std;

int main() {
    int n;
    cin >> n;
    int suma = 0;
    for (int i = 1; i <= n; i++) {
        suma += i;
    }
    cout << "Suma: " << suma << endl;
    return 0;
}
EOF
)

CODE_MAYOR=$(cat <<'EOF'
#include <iostream>
using namespace std;

int main() {
    int a, b, c;
    cin >> a >> b >> c;
    int mayor = a;
    if (b > mayor) mayor = b;
    if (c > mayor) mayor = c;
    cout << "Mayor: " << mayor << endl;
    return 0;
}
EOF
)

echo "=== 6. Enviando entregas (se compilan y ejecutan en el sandbox) ==="
ANA_T1=$(submit "$ANA_COOKIE" "$GROUP1_ID" "$T1" "$CODE_PROMEDIO" "4.5 3.0 5.0")
attempt "$ANA_COOKIE" "$GROUP1_ID" "$T2" "$CODE_SUMA_SIN_PUNTO" "5"
attempt "$ANA_COOKIE" "$GROUP1_ID" "$T2" "$CODE_SUMA" "5"
ANA_T2=$(submit "$ANA_COOKIE" "$GROUP1_ID" "$T2" "$CODE_SUMA" "5")
ANA_T3=$(submit "$ANA_COOKIE" "$GROUP1_ID" "$T3" "$CODE_MAYOR" "7 12 3")
CARLOS_T1=$(submit "$CARLOS_COOKIE" "$GROUP1_ID" "$T1" "$CODE_PROMEDIO_ENTERO" "4 5 5")
LUIS_T2=$(submit "$LUIS_COOKIE" "$GROUP1_ID" "$T2" "$CODE_SUMA_OK" "5")
echo "  ✓ Ana: 3 entregas y 2 intentos · Carlos: 1 entrega · Luis: 1 entrega"
echo "  ✓ Casos de prueba: Ana pasa 3/3, 0/3 y 3/3 · Carlos 0/3 · Luis 3/3"
echo "  ✓ Las 5 entregas oficiales entraron a la cola: la IA prepara sus propuestas en segundo plano"

echo "=== 7. Calificando ==="
review "$ANA_T1" 4.5 true "Muy bien: lees las tres notas con \`double\` y usas paréntesis para sumar antes de dividir, por eso el promedio sale correcto (4.17 con 4.5, 3.0 y 5.0). La salida con dos decimales cumple el enunciado.

Para el 5.0: ¿qué debería pasar si alguien escribe una nota mayor que 5 o negativa? Piensa cómo validarlo antes de calcular." > /dev/null
review "$ANA_T2" 3.2 true "Compila y el acumulador \`suma\` empieza bien en 0. Con n = 5 tu programa imprime 10 y el enunciado espera 15.

Revisa la condición del ciclo de la línea 8: escribe a mano qué valores toma \`i\` en cada vuelta. ¿Se alcanza a sumar el 5?" > /dev/null
review "$ANA_T3" 4.0 false "Correcto con 7, 12 y 3. Pendiente revisar los casos con números repetidos antes de devolverla." > /dev/null
review "$LUIS_T2" 5.0 true "Excelente: el ciclo recorre de 1 hasta n incluido y el resultado es correcto. El código es claro y bien indentado." > /dev/null
echo "  ✓ Ana: 4.5 (Taller 1) y 3.2 (Taller 2) devueltas, Taller 3 en revisión · Luis: 5.0"

echo "=== 8. RAE de la materia y niveles de logro ==="
API="$API" TEACHER_SESSION="$PROF_COOKIE" python3 "$(dirname "$0")/seed_rae_levels.py"

echo ""
echo "======================================================"
echo "  ✅ Datos de prueba cargados exitosamente"
echo "======================================================"
echo ""
echo "  PROFESOR (cuenta de Brightspace simulada, sin contraseña):"
echo "    maria.garcia@universidad.edu.co  (external_id d2l:user:9001)"
echo ""
echo "  ESTUDIANTES (external_id d2l:user:9101 a 9105):"
echo "    Clase 1 - Intro a la Programación:"
echo "      carlos.perez@est.edu.co, ana.rodriguez@est.edu.co, luis.martinez@est.edu.co"
echo "    Clase 2 - Programación Avanzada:"
echo "      sofia.lopez@est.edu.co, diego.ramirez@est.edu.co"
echo ""
echo "  Para entrar como cualquiera: POST /internal/v1/session-tickets con su external_id y abrir"
echo "  $API/api/auth/session?ticket=<ticket> en el navegador (el servicio Python hace esto tras el login OAuth)."
echo ""
echo "  NOTAS (escala 0.0 a 5.0):"
echo "    ana.rodriguez@est.edu.co: Taller 1 = 4.5, Taller 2 = 3.2, Taller 3 en revisión (entregado tarde)"
echo "    luis.martinez@est.edu.co: Taller 2 = 5.0"
echo "    carlos.perez@est.edu.co:  Taller 1 = 2.8"
echo "  RAE: los 4 oficiales del curso 33698 en Introducción a la Programación, con niveles en cada revisión"
echo "  IA: Grupo A supervisado (el estudiante solo ve lo que aprueba el profesor) · Grupo B inmediato"
echo ""
