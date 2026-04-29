#!/bin/bash
# sendmail.sh — Send email via SMTP using curl
#
# Usage:
#   ./sendmail.sh [options] -a attachment.pdf recipient@example.com
#
# Options:
#   -f <email>       From address
#   -t <email>       To address
#   -c <email>       CC address (can be repeated)
#   -s <subject>     Email subject
#   -S <server>      SMTP server (default: localhost)
#   -P <port>        SMTP port (default: 25)
#   -u <user>        SMTP auth username
#   -w <password>    SMTP auth password
#   -A <type>        Auth type: plain|login (default: plain)
#   -T               Enable TLS/STARTTLS
#   -a <file>        Attachment (can be repeated)
#   -b <file>        Message body from file (default: stdin)
#   -h               Show this help
#
# Examples:
#   echo "test" | ./sendmail.sh -f me@ex.com -t you@ex.com -s "hello"
#   ./sendmail.sh -f me@ex.com -t you@ex.com -c cc@ex.com -s "subject" \
#                 -S smtp.ex.com -P 587 -u user -w pass -T < body.txt
#
# Requires: curl (with SMTP support), base64

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
    sed -n '/^# sendmail.sh/,/^Requires/p; /^$/q' "$0" | sed 's/^# //; s/^#$//'
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

[[ -z "$FROM" ]] && { echo "ERROR: -f (from) is required" >&2; exit 1; }
[[ -z "$TO" ]] && { echo "ERROR: -t (to) is required" >&2; exit 1; }
[[ "$AUTH_TYPE" != "plain" && "$AUTH_TYPE" != "login" ]] && {
    echo "ERROR: invalid auth type '$AUTH_TYPE' (use plain|login)" >&2
    exit 1
}

for a in "${ATTACHMENTS[@]}"; do
    [[ ! -f "$a" ]] && { echo "ERROR: attachment not found: $a" >&2; exit 1; }
done

# Build the email MIME structure
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

    # Body
    if [[ -n "$BODY_FILE" ]]; then
        cat "$BODY_FILE"
    else
        cat
    fi

    # Attachments
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

# Build recipient list for curl (--mail-rcpt can be repeated)
rcpt_args=()
for c in "${CC[@]}"; do
    rcpt_args+=(--mail-rcpt "$c")
done

# Build the SMTP URL
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

# Send the email
build_email | curl "${curl_args[@]}" --upload-file "-"
