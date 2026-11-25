#!/usr/bin/env bash
# Prueba el servicio Python real (EduTrace - D2L Connection) desde Go.
#
# Uso: ./scripts/probar_python.sh
#
# Lee PYTHON_BASE_URL, PYTHON_API_KEY y PYTHON_CA_FILE de .env (o del entorno) y corre las pruebas
# "Live" de internal/pyclient: salud, clave obligatoria y contrato de dropbox/submit y dropbox/feedback.
# Python tiene que estar corriendo. No toca Brightspace ni crea datos.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."

# Mismo criterio que start.sh: el entorno manda sobre .env, y las comillas se quitan.
if [ -f .env ]; then
    while IFS= read -r line || [ -n "$line" ]; do
        key="${line%%=*}"
        value="${line#*=}"
        [[ "$line" == *=* ]] || continue
        [[ "$key" =~ ^PYTHON_(BASE_URL|API_KEY|CA_FILE)$ ]] || continue
        value="${value%$'\r'}"
        value="${value#\"}"; value="${value%\"}"
        value="${value#\'}"; value="${value%\'}"
        if [ -z "${!key}" ] && [ -n "$value" ]; then
            export "$key=$value"
        fi
    done < .env
fi

# .env.example trae PYTHON_CA_FILE="$(mkcert -CAROOT)/rootCA.pem": la sustitución no la hace el bucle de arriba.
if [[ "$PYTHON_CA_FILE" == *'$(mkcert -CAROOT)'* ]]; then
    if command -v mkcert >/dev/null 2>&1; then
        PYTHON_CA_FILE="$(mkcert -CAROOT)/rootCA.pem"
    else
        PYTHON_CA_FILE=""
    fi
fi
export PYTHON_CA_FILE

export PYTHON_LIVE_URL="${PYTHON_LIVE_URL:-$PYTHON_BASE_URL}"

if [ -z "$PYTHON_LIVE_URL" ] || [ -z "$PYTHON_API_KEY" ]; then
    echo "Faltan PYTHON_BASE_URL y PYTHON_API_KEY (en .env o en el entorno)." >&2
    exit 1
fi

# Aviso claro si Python no está arriba, en vez de una pila de errores de conexión.
CURL_CA=()
[ -n "$PYTHON_CA_FILE" ] && CURL_CA=(--cacert "$PYTHON_CA_FILE")
if ! curl -s -m 5 "${CURL_CA[@]}" -o /dev/null "${PYTHON_LIVE_URL%/}/health"; then
    echo "No responde Python en $PYTHON_LIVE_URL. Arráncalo (README de EduTrace - D2L Connection) y vuelve a correr esto." >&2
    exit 1
fi

echo "Probando Python en $PYTHON_LIVE_URL"
go test ./internal/pyclient -run Live -count=1 -v
