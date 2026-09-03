#!/bin/bash
# sendmail.sh — Send email via SMTP using curl
#
# Usage:
#   ./sendmail.sh -f sender@example.com -t recipient@example.com [options]
#
# Options:
#   -f <email>       From address
#   -t <email>       To address
#   -c <email>       CC address (can be repeated)
#   -s <subject>     Email subject
#   -S <server>      SMTP server (default: localhost)
#   -P <port>        SMTP port (default: 25)
#   -N <file>        SMTP credentials in a protected netrc file
#   -A <type>        Auth type: plain|login (default: plain)
#   -T               Enable TLS/STARTTLS
#   -a <file>        Attachment (can be repeated)
#   -b <file>        Message body from file (default: stdin)
#   -h               Show this help
#
# Examples:
#   echo "test" | ./sendmail.sh -f me@ex.com -t you@ex.com -s "hello"
#   ./sendmail.sh -f me@ex.com -t you@ex.com -c cc@ex.com -s "subject" \
#                 -S smtp.ex.com -P 587 -N /secure/sendmail.netrc -T < body.txt
#
# Environment:
#   SENDMAIL_CONNECT_TIMEOUT_SECONDS  Connection timeout (default: 10)
#   SENDMAIL_MAX_TIME_SECONDS         Total transfer timeout (default: 60)
#
# Requires: curl (with SMTP support), base64, stat

set -euo pipefail

FROM=""
TO=""
CC=()
SUBJECT=""
SERVER="localhost"
PORT=25
NETRC_FILE=""
AUTH_TYPE="plain"
AUTH_TYPE_SET=false
USE_TLS=false
ATTACHMENTS=()
BODY_FILE=""
CONNECT_TIMEOUT_SECONDS="${SENDMAIL_CONNECT_TIMEOUT_SECONDS-10}"
MAX_TIME_SECONDS="${SENDMAIL_MAX_TIME_SECONDS-60}"

usage() {
    sed -n '/^# sendmail.sh/,/^Requires/p; /^$/q' "$0" | sed 's/^# //; s/^#$//'
    exit "${1:-0}"
}

while getopts "f:t:c:s:S:P:N:A:Ta:b:h" opt; do
    case "$opt" in
        f) FROM="$OPTARG" ;;
        t) TO="$OPTARG" ;;
        c) CC+=("$OPTARG") ;;
        s) SUBJECT="$OPTARG" ;;
        S) SERVER="$OPTARG" ;;
        P) PORT="$OPTARG" ;;
        N) NETRC_FILE="$OPTARG" ;;
        A) AUTH_TYPE="$OPTARG"; AUTH_TYPE_SET=true ;;
        T) USE_TLS=true ;;
        a) ATTACHMENTS+=("$OPTARG") ;;
        b) BODY_FILE="$OPTARG" ;;
        h) usage ;;
        *) usage 2 ;;
    esac
done

[[ -z "$FROM" ]] && { echo "ERROR: -f (from) is required" >&2; exit 1; }
[[ -z "$TO" ]] && { echo "ERROR: -t (to) is required" >&2; exit 1; }
[[ "$AUTH_TYPE" != "plain" && "$AUTH_TYPE" != "login" ]] && {
    echo "ERROR: invalid auth type '$AUTH_TYPE' (use plain|login)" >&2
    exit 1
}
[[ "$AUTH_TYPE_SET" == true && -z "$NETRC_FILE" ]] && {
    echo "ERROR: -A requires -N with a protected netrc file" >&2
    exit 1
}

validate_positive_timeout() {
    local name="$1"
    local value="$2"
    if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
        echo "ERROR: $name must be a positive integer number of seconds" >&2
        exit 1
    fi
}

validate_positive_timeout SENDMAIL_CONNECT_TIMEOUT_SECONDS "$CONNECT_TIMEOUT_SECONDS"
validate_positive_timeout SENDMAIL_MAX_TIME_SECONDS "$MAX_TIME_SECONDS"
if (( CONNECT_TIMEOUT_SECONDS > MAX_TIME_SECONDS )); then
    echo "ERROR: SENDMAIL_CONNECT_TIMEOUT_SECONDS must not exceed SENDMAIL_MAX_TIME_SECONDS" >&2
    exit 1
fi

if [[ -n "$NETRC_FILE" ]]; then
    [[ -L "$NETRC_FILE" ]] && {
        echo "ERROR: SMTP credential file must not be a symbolic link: $NETRC_FILE" >&2
        exit 1
    }
    [[ ! -f "$NETRC_FILE" || ! -r "$NETRC_FILE" ]] && {
        echo "ERROR: SMTP credential file must be a readable regular file: $NETRC_FILE" >&2
        exit 1
    }
    netrc_owner=$(stat -c '%u' -- "$NETRC_FILE")
    netrc_mode=$(stat -c '%a' -- "$NETRC_FILE")
    if [[ "$netrc_owner" != "$EUID" ]]; then
        echo "ERROR: SMTP credential file must be owned by effective UID $EUID: $NETRC_FILE" >&2
        exit 1
    fi
    if (( (8#$netrc_mode & 077) != 0 )); then
        echo "ERROR: SMTP credential file must not grant permissions to group or others: $NETRC_FILE" >&2
        exit 1
    fi
fi

for a in "${ATTACHMENTS[@]}"; do
    [[ ! -f "$a" ]] && { echo "ERROR: attachment not found: $a" >&2; exit 1; }
done

# Build the email MIME structure
build_email() {
    local boundary
    boundary="==BOUNDARY_$(date +%s)_$$=="
    local message_date
    message_date=$(date -R)
    local has_attach=$(( ${#ATTACHMENTS[@]} > 0 ? 1 : 0 ))

    # Headers
    echo "From: $FROM"
    echo "To: $TO"
    for c in "${CC[@]}"; do
        echo "Cc: $c"
    done
    echo "Subject: $SUBJECT"
    echo "Date: $message_date"
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
    --connect-timeout "$CONNECT_TIMEOUT_SECONDS"
    --max-time "$MAX_TIME_SECONDS"
    --mail-from "$FROM"
    --mail-rcpt "$TO"
    "${rcpt_args[@]}"
)

if [[ "$USE_TLS" == true ]]; then
    curl_args+=(--ssl-reqd)
fi

if [[ -n "$NETRC_FILE" ]]; then
    curl_args+=(--netrc-file "$NETRC_FILE")
    if [[ "$AUTH_TYPE" == "login" ]]; then
        curl_args+=(--login-options "AUTH=LOGIN")
    else
        curl_args+=(--login-options "AUTH=PLAIN")
    fi
fi

# Send the email
build_email | curl "${curl_args[@]}" --upload-file "-"
