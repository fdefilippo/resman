#!/bin/bash
# sendmail.sh — Invia email via SMTP con curl
#
# Uso:
#   ./sendmail.sh [opzioni] -a allegato.pdf destinatario@example.com
#
# Opzioni:
#   -f <email>       Mittente (From)
#   -t <email>       Destinatario (To)
#   -c <email>       CC (può essere ripetuto)
#   -s <oggetto>     Oggetto (Subject)
#   -S <server>      Server SMTP (default: localhost)
#   -P <porta>       Porta SMTP (default: 25)
#   -u <user>        Utente per autenticazione
#   -w <password>    Password per autenticazione
#   -A <type>        Tipo autenticazione: plain|login (default: plain)
#   -T               Usa TLS/STARTTLS
#   -a <file>        Allega un file (può essere ripetuto)
#   -b <file>        Corpo del messaggio da file (default: stdin)
#   -h               Mostra questo aiuto
#
# Esempi:
#   echo "test" | ./sendmail.sh -f me@ex.com -t you@ex.com -s "ciao"
#   ./sendmail.sh -f me@ex.com -t you@ex.com -c cc@ex.com -s "oggetto" \
#                 -S smtp.ex.com -P 587 -u user -w pass -T < corpo.txt
#
# Dipende da: curl (con supporto SMTP), base64

set -euo pipefail

FROM=""
TO=""
CC=()
SUBJECT=""
SERVER="localhost"
PORT=25
AUTH_USER=""
AUTH_PASS=""
AUTH_TYPE="plain"
USE_TLS=false
ATTACHMENTS=()
BODY_FILE=""

usage() {
    sed -n '/^# sendmail.sh/,/^Dipende/p; /^$/q' "$0" | sed 's/^# //; s/^#$//'
    exit 0
}

while getopts "f:t:c:s:S:P:u:w:A:Ta:b:h" opt; do
    case "$opt" in
        f) FROM="$OPTARG" ;;
        t) TO="$OPTARG" ;;
        c) CC+=("$OPTARG") ;;
        s) SUBJECT="$OPTARG" ;;
        S) SERVER="$OPTARG" ;;
        P) PORT="$OPTARG" ;;
        u) AUTH_USER="$OPTARG" ;;
        w) AUTH_PASS="$OPTARG" ;;
        A) AUTH_TYPE="$OPTARG" ;;
        T) USE_TLS=true ;;
        a) ATTACHMENTS+=("$OPTARG") ;;
        b) BODY_FILE="$OPTARG" ;;
        h) usage ;;
        *) usage ;;
    esac
done

[[ -z "$FROM" ]] && { echo "ERRORE: -f (mittente) obbligatorio" >&2; exit 1; }
[[ -z "$TO" ]] && { echo "ERRORE: -t (destinatario) obbligatorio" >&2; exit 1; }
[[ "$AUTH_TYPE" != "plain" && "$AUTH_TYPE" != "login" ]] && {
    echo "ERRORE: tipo autenticazione '$AUTH_TYPE' non valido (plain|login)" >&2
    exit 1
}

for a in "${ATTACHMENTS[@]}"; do
    [[ ! -f "$a" ]] && { echo "ERRORE: allegato non trovato: $a" >&2; exit 1; }
done

# Costruisce l'email
build_email() {
    local boundary="==BOUNDARY_$(date +%s)_$$=="
    local has_attach=$(( ${#ATTACHMENTS[@]} > 0 ? 1 : 0 ))

    # Headers
    echo "From: $FROM"
    echo "To: $TO"
    for c in "${CC[@]}"; do
        echo "Cc: $c"
    done
    echo "Subject: $SUBJECT"
    echo "Date: $(date -R)"
    echo "MIME-Version: 1.0"
    if [[ "$has_attach" -eq 1 ]]; then
        echo "Content-Type: multipart/mixed; boundary=\"$boundary\""
        echo ""
        echo "--$boundary"
        echo "Content-Type: text/plain; charset=UTF-8"
        echo "Content-Transfer-Encoding: 8bit"
        echo ""
    else
        echo "Content-Type: text/plain; charset=UTF-8"
        echo "Content-Transfer-Encoding: 8bit"
        echo ""
    fi

    # Corpo
    if [[ -n "$BODY_FILE" ]]; then
        cat "$BODY_FILE"
    else
        cat
    fi

    # Allegati
    for a in "${ATTACHMENTS[@]}"; do
        echo ""
        echo "--$boundary"
        echo "Content-Type: application/octet-stream; name=\"$(basename "$a")\""
        echo "Content-Disposition: attachment; filename=\"$(basename "$a")\""
        echo "Content-Transfer-Encoding: base64"
        echo ""
        base64 "$a"
    done

    if [[ "$has_attach" -eq 1 ]]; then
        echo ""
        echo "--${boundary}--"
    fi
}

# Destinatari per curl (--mail-rcpt può essere usato più volte)
rcpt_args=()
for c in "${CC[@]}"; do
    rcpt_args+=(--mail-rcpt "$c")
done

# Costruisce l'URL
if [[ "$USE_TLS" == true ]]; then
    url="smtp://${SERVER}:${PORT}"
else
    url="smtp://${SERVER}:${PORT}"
fi

curl_args=(
    --url "$url"
    --mail-from "$FROM"
    --mail-rcpt "$TO"
    "${rcpt_args[@]}"
)

if [[ "$USE_TLS" == true ]]; then
    curl_args+=(--ssl-reqd)
fi

if [[ -n "$AUTH_USER" ]]; then
    curl_args+=(--user "${AUTH_USER}:${AUTH_PASS}")
    if [[ "$AUTH_TYPE" == "login" ]]; then
        curl_args+=(--login-options "AUTH=LOGIN")
    else
        curl_args+=(--login-options "AUTH=PLAIN")
    fi
fi

# Invia
build_email | curl "${curl_args[@]}" --upload-file "-"
