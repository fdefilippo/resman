#!/bin/bash

set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
scratch=$(mktemp -d)
server_pid=""
cleanup() {
    if [[ -n "$server_pid" ]]; then
        kill "$server_pid" 2>/dev/null || true
        wait "$server_pid" 2>/dev/null || true
    fi
    rm -rf -- "$scratch"
}
trap cleanup EXIT INT TERM

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
printf '%s\0' "$@" >"${SENDMAIL_TEST_ARGUMENTS:?}"
cat >"${SENDMAIL_TEST_CAPTURE:?}"
EOF
chmod 0700 "$fake_bin/curl"

attachment="$scratch/attachment.txt"
printf '%s\n' 'attachment content' >"$attachment"

capture="$scratch/message.eml"
arguments="$scratch/curl-arguments"
printf '%s\n' 'message body' | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$capture" \
    SENDMAIL_TEST_ARGUMENTS="$arguments" \
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
if ! tr '\0' '\n' <"$arguments" | grep -Fqx -- '--connect-timeout'; then
    echo "sendmail.sh did not pass a connection timeout to curl" >&2
    exit 1
fi
if ! tr '\0' '\n' <"$arguments" | grep -Fqx -- '--max-time'; then
    echo "sendmail.sh did not pass a total transfer timeout to curl" >&2
    exit 1
fi

credentials="$scratch/sendmail.netrc"
cat >"$credentials" <<'EOF'
machine smtp.example.test
login smtp-user
password test-only-secret
EOF
chmod 0600 "$credentials"

authenticated_capture="$scratch/authenticated.eml"
authenticated_arguments="$scratch/authenticated-arguments"
printf '%s\n' 'authenticated message' | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$authenticated_capture" \
    SENDMAIL_TEST_ARGUMENTS="$authenticated_arguments" \
    SENDMAIL_CONNECT_TIMEOUT_SECONDS=7 \
    SENDMAIL_MAX_TIME_SECONDS=19 \
    "$script_dir/sendmail.sh" \
    -f sender@example.test \
    -t recipient@example.test \
    -S smtp.example.test \
    -N "$credentials" \
    -A login

mapfile -d '' -t authenticated_argv <"$authenticated_arguments"
expected_authenticated_argv=(
    --url smtp://smtp.example.test:25
    --connect-timeout 7
    --max-time 19
    --mail-from sender@example.test
    --mail-rcpt recipient@example.test
    --netrc-file "$credentials"
    --login-options AUTH=LOGIN
    --upload-file -
)
if [[ "${authenticated_argv[*]}" != "${expected_authenticated_argv[*]}" ]]; then
    echo "unexpected authenticated curl argument contract" >&2
    printf 'got:  %q\n' "${authenticated_argv[@]}" >&2
    printf 'want: %q\n' "${expected_authenticated_argv[@]}" >&2
    exit 1
fi
if tr '\0' '\n' <"$authenticated_arguments" | grep -Fq 'test-only-secret'; then
    echo "sendmail.sh placed the SMTP password in curl arguments" >&2
    exit 1
fi

insecure_credentials="$scratch/insecure.netrc"
cp "$credentials" "$insecure_credentials"
chmod 0644 "$insecure_credentials"
if printf '%s\n' body | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$scratch/insecure.eml" \
    SENDMAIL_TEST_ARGUMENTS="$scratch/insecure-arguments" \
    "$script_dir/sendmail.sh" -f sender@example.test -t recipient@example.test -N "$insecure_credentials" \
    >"$scratch/insecure-output" 2>&1; then
    echo "sendmail.sh accepted a credential file readable by other users" >&2
    exit 1
fi
if ! grep -Fq 'must not grant permissions to group or others' "$scratch/insecure-output"; then
    echo "sendmail.sh did not diagnose an insecure credential-file mode" >&2
    exit 1
fi

linked_credentials="$scratch/linked.netrc"
ln -s "$credentials" "$linked_credentials"
if printf '%s\n' body | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$scratch/linked.eml" \
    SENDMAIL_TEST_ARGUMENTS="$scratch/linked-arguments" \
    "$script_dir/sendmail.sh" -f sender@example.test -t recipient@example.test -N "$linked_credentials" \
    >"$scratch/linked-output" 2>&1; then
    echo "sendmail.sh accepted a symbolic-link credential file" >&2
    exit 1
fi
if ! grep -Fq 'must not be a symbolic link' "$scratch/linked-output"; then
    echo "sendmail.sh did not diagnose a symbolic-link credential file" >&2
    exit 1
fi

if printf '%s\n' body | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$scratch/legacy.eml" \
    SENDMAIL_TEST_ARGUMENTS="$scratch/legacy-arguments" \
    "$script_dir/sendmail.sh" -f sender@example.test -t recipient@example.test -u smtp-user \
    >"$scratch/legacy-output" 2>&1; then
    echo "sendmail.sh retained the unsafe username/password argument interface" >&2
    exit 1
fi

if printf '%s\n' body | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$scratch/legacy-password.eml" \
    SENDMAIL_TEST_ARGUMENTS="$scratch/legacy-password-arguments" \
    "$script_dir/sendmail.sh" -f sender@example.test -t recipient@example.test -w password \
    >"$scratch/legacy-password-output" 2>&1; then
    echo "sendmail.sh retained the unsafe password argument interface" >&2
    exit 1
fi

if printf '%s\n' body | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$scratch/invalid-timeout.eml" \
    SENDMAIL_TEST_ARGUMENTS="$scratch/invalid-timeout-arguments" \
    SENDMAIL_MAX_TIME_SECONDS=0 \
    "$script_dir/sendmail.sh" -f sender@example.test -t recipient@example.test \
    >"$scratch/invalid-timeout-output" 2>&1; then
    echo "sendmail.sh accepted an unbounded total transfer timeout" >&2
    exit 1
fi

timestamp_failure_capture="$scratch/timestamp-failure.eml"
if printf '%s\n' 'message body' | env \
    PATH="$fake_bin:/usr/bin:/bin" \
    SENDMAIL_TEST_CAPTURE="$timestamp_failure_capture" \
    SENDMAIL_TEST_ARGUMENTS="$scratch/timestamp-failure-arguments" \
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
    SENDMAIL_TEST_ARGUMENTS="$scratch/header-date-failure-arguments" \
    SENDMAIL_TEST_FAIL_HEADER_DATE=1 \
    "$script_dir/sendmail.sh" \
    -f sender@example.test \
    -t recipient@example.test \
    -s subject \
    -a "$attachment"; then
    echo "sendmail.sh masked a failed RFC 5322 date command" >&2
    exit 1
fi

go_command=${GO:-go}
silent_server="$scratch/silent-smtp-server"
"$go_command" build -o "$silent_server" "$script_dir/testdata/silent_smtp_server.go"
server_address_file="$scratch/server-address"
server_accepted_file="$scratch/server-accepted"
"$silent_server" "$server_address_file" "$server_accepted_file" &
server_pid=$!

for _ in {1..500}; do
    [[ -s "$server_address_file" ]] && break
    sleep 0.01
done
if [[ ! -s "$server_address_file" ]]; then
    echo "silent SMTP test server did not publish its address" >&2
    exit 1
fi
server_address=$(<"$server_address_file")
server_port=${server_address##*:}

live_credentials="$scratch/live.netrc"
live_secret="sendmail-proc-secret-$RANDOM-$$"
cat >"$live_credentials" <<EOF
machine 127.0.0.1
login smtp-user
password $live_secret
EOF
chmod 0600 "$live_credentials"

live_output="$scratch/live-output"
SECONDS=0
# The outer timeout is a test watchdog. The helper must still return curl's own
# timeout status before this independent bound expires.
printf '%s\n' 'message body' | timeout --signal=TERM 8s env \
    PATH="/usr/bin:/bin" \
    SENDMAIL_CONNECT_TIMEOUT_SECONDS=1 \
    SENDMAIL_MAX_TIME_SECONDS=2 \
    "$script_dir/sendmail.sh" \
    -f sender@example.test \
    -t recipient@example.test \
    -S 127.0.0.1 \
    -P "$server_port" \
    -N "$live_credentials" \
    >"$live_output" 2>&1 &
send_pid=$!

for _ in {1..500}; do
    [[ -e "$server_accepted_file" ]] && break
    if ! kill -0 "$send_pid" 2>/dev/null; then
        break
    fi
    sleep 0.01
done
if [[ ! -e "$server_accepted_file" ]]; then
    echo "authenticated send did not reach the silent SMTP listener" >&2
    wait "$send_pid" || true
    exit 1
fi

process_tree=("$send_pid")
for ((i = 0; i < ${#process_tree[@]}; i++)); do
    parent=${process_tree[$i]}
    children_file="/proc/$parent/task/$parent/children"
    [[ -r "$children_file" ]] || continue
    read -r -a children <"$children_file" || true
    process_tree+=("${children[@]}")
done

found_netrc_argument=false
for process_pid in "${process_tree[@]}"; do
    cmdline="/proc/$process_pid/cmdline"
    [[ -r "$cmdline" ]] || continue
    if tr '\0' '\n' <"$cmdline" | grep -Fq "$live_secret"; then
        echo "SMTP password leaked through /proc/$process_pid/cmdline" >&2
        exit 1
    fi
    if tr '\0' '\n' <"$cmdline" | grep -Fqx -- '--netrc-file'; then
        found_netrc_argument=true
    fi
done
if [[ "$found_netrc_argument" != true ]]; then
    echo "live authenticated send did not expose the expected netrc path argument" >&2
    exit 1
fi

send_status=0
if wait "$send_pid"; then
    echo "sendmail.sh reported success against a silent SMTP listener" >&2
    exit 1
else
    send_status=$?
fi
elapsed=$SECONDS
if (( send_status == 0 )); then
    echo "sendmail.sh did not propagate the SMTP timeout failure" >&2
    exit 1
fi
if (( send_status != 28 )); then
    echo "sendmail.sh returned $send_status instead of curl timeout status 28" >&2
    cat "$live_output" >&2
    exit 1
fi
if (( elapsed < 1 || elapsed > 8 )); then
    echo "sendmail.sh did not terminate within the configured total timeout: ${elapsed}s" >&2
    exit 1
fi

wait "$server_pid"
server_pid=""

echo "PASS: sendmail MIME, credential, process-argv, and timeout contracts"
