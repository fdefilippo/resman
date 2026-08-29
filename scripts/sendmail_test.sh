#!/bin/bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT INT TERM

fake_bin="$scratch/bin"
mkdir -p "$fake_bin"

cat >"$fake_bin/date" <<'EOF'
#!/bin/bash
set -euo pipefail
if [[ "${SENDMAIL_TEST_FAIL_TIMESTAMP:-0}" == "1" && "${1:-}" == "+%s" ]]; then
    exit 42
fi
if [[ "${SENDMAIL_TEST_FAIL_HEADER_DATE:-0}" == "1" && "${1:-}" == "-R" ]]; then
    exit 43
fi
case "${1:-}" in
    +%s) printf "%s\n" "1700000000" ;;
    -R) printf "%s\n" "Tue, 14 Nov 2023 22:13:20 +0000" ;;
    *) exit 2 ;;
esac
EOF
chmod 0700 "$fake_bin/date"

cat >"$fake_bin/curl" <<'EOF'
#!/bin/bash
set -euo pipefail
cat >"${SENDMAIL_TEST_CAPTURE:?}"
EOF
chmod 0700 "$fake_bin/curl"

attachment="$scratch/attachment.txt"
printf '%s\n' 'attachment content' >"$attachment"

capture="$scratch/message.eml"
printf '%s\n' 'message body' | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$capture" \
    "$script_dir/sendmail.sh" \
    -f sender@example.test \
    -t recipient@example.test \
    -s subject \
    -a "$attachment"

boundary=$(sed -n 's/^Content-Type: multipart\/mixed; boundary="\(.*\)"$/\1/p' "$capture")
if [[ ! "$boundary" =~ ^==BOUNDARY_1700000000_[0-9]+==$ ]]; then
    echo "unexpected MIME boundary: $boundary" >&2
    exit 1
fi
if ! grep -Fqx -- "--${boundary}--" "$capture"; then
    echo "MIME message does not close the generated boundary" >&2
    exit 1
fi
if ! grep -Fqx 'Date: Tue, 14 Nov 2023 22:13:20 +0000' "$capture"; then
    echo "MIME message does not contain the generated RFC 5322 date" >&2
    exit 1
fi

timestamp_failure_capture="$scratch/timestamp-failure.eml"
if printf '%s\n' 'message body' | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$timestamp_failure_capture" \
    SENDMAIL_TEST_FAIL_TIMESTAMP=1 \
    "$script_dir/sendmail.sh" \
    -f sender@example.test \
    -t recipient@example.test \
    -s subject \
    -a "$attachment"; then
    echo "sendmail.sh masked a failed MIME-boundary timestamp command" >&2
    exit 1
fi

header_date_failure_capture="$scratch/header-date-failure.eml"
if printf '%s\n' 'message body' | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$header_date_failure_capture" \
    SENDMAIL_TEST_FAIL_HEADER_DATE=1 \
    "$script_dir/sendmail.sh" \
    -f sender@example.test \
    -t recipient@example.test \
    -s subject \
    -a "$attachment"; then
    echo "sendmail.sh masked a failed RFC 5322 date command" >&2
    exit 1
fi

echo "PASS: sendmail MIME boundary timestamp contract"
