#!/bin/bash
# resman-sendmail-hook.sh — Example email adapter for the ResMan limit-hook contract
#
# Copy this file to a trusted executable path, replace the CHANGE_ME values below,
# and configure that copy as LIMIT_HOOK_SCRIPT. Do not edit the packaged copy.

set -euo pipefail

readonly SENDMAIL_HELPER="/usr/share/doc/resman/scripts/sendmail.sh"
readonly MAIL_FROM="CHANGE_ME@example.invalid"
readonly MAIL_TO="CHANGE_ME@example.invalid"
readonly SMTP_SERVER="localhost"
readonly SMTP_PORT="25"
readonly REQUIRE_STARTTLS="false"

fail() {
	printf 'resman-sendmail-hook: %s\n' "$1" >&2
	exit 1
}

require_environment_variable() {
	local name=$1
	[[ -v "$name" ]] || fail "required environment variable is missing: $name"
}

reject_control_characters() {
	local name=$1
	local value=${!name}
	[[ ! "$value" =~ [[:cntrl:]] ]] || fail "$name contains control characters"
}

if (( $# != 0 )); then
	fail "this adapter accepts no arguments"
fi

readonly required_event_variables=(
	RESMAN_LIMIT_UID
	RESMAN_LIMIT_USERNAME
	RESMAN_LIMIT_TIMESTAMP
	RESMAN_LIMIT_SERVER_ROLE
	RESMAN_LIMIT_ENFORCEABLE_CPU_USAGE_PERCENT
	RESMAN_LIMIT_CPU_POINTS_CONFIGURED_CLASS
	RESMAN_LIMIT_CPU_POINTS_LIFECYCLE_STATE
	RESMAN_LIMIT_CPU_POINTS_APPLIED_CLASS
	RESMAN_LIMIT_CPU_POINTS_PROCESS_COVERAGE
	RESMAN_LIMIT_RAM_COVERAGE
)

for name in "${required_event_variables[@]}"; do
	require_environment_variable "$name"
	reject_control_characters "$name"
done

[[ "$RESMAN_LIMIT_UID" =~ ^[0-9]+$ ]] || fail "RESMAN_LIMIT_UID must be an unsigned decimal integer"
[[ -n "$RESMAN_LIMIT_USERNAME" ]] || fail "RESMAN_LIMIT_USERNAME must not be empty"
[[ -n "$RESMAN_LIMIT_TIMESTAMP" ]] || fail "RESMAN_LIMIT_TIMESTAMP must not be empty"

for name in MAIL_FROM MAIL_TO SMTP_SERVER; do
	reject_control_characters "$name"
	[[ -n "${!name}" ]] || fail "$name must not be empty"
done

if [[ "$MAIL_FROM" == CHANGE_ME* || "$MAIL_TO" == CHANGE_ME* ]]; then
	fail "copy the adapter and configure MAIL_FROM and MAIL_TO before use"
fi

[[ "$SMTP_PORT" =~ ^[0-9]{1,5}$ ]] || fail "SMTP_PORT must be an integer from 1 to 65535"
smtp_port=$((10#$SMTP_PORT))
(( smtp_port >= 1 && smtp_port <= 65535 )) || fail "SMTP_PORT must be an integer from 1 to 65535"
[[ "$REQUIRE_STARTTLS" == "true" || "$REQUIRE_STARTTLS" == "false" ]] ||
	fail "REQUIRE_STARTTLS must be true or false"
[[ "$SENDMAIL_HELPER" == /* ]] || fail "SENDMAIL_HELPER must be an absolute path"
[[ -f "$SENDMAIL_HELPER" && -x "$SENDMAIL_HELPER" && ! -L "$SENDMAIL_HELPER" ]] ||
	fail "SENDMAIL_HELPER must be an executable regular path without a final symlink"

readonly subject="ResMan limit applied to ${RESMAN_LIMIT_USERNAME} (UID ${RESMAN_LIMIT_UID})"
sendmail_arguments=(
	-f "$MAIL_FROM"
	-t "$MAIL_TO"
	-s "$subject"
	-S "$SMTP_SERVER"
	-P "$SMTP_PORT"
)
if [[ "$REQUIRE_STARTTLS" == "true" ]]; then
	sendmail_arguments+=(-T)
fi

{
	printf 'ResMan applied a resource limit.\n\n'
	printf 'User: %s\n' "$RESMAN_LIMIT_USERNAME"
	printf 'UID: %s\n' "$RESMAN_LIMIT_UID"
	printf 'Timestamp: %s\n' "$RESMAN_LIMIT_TIMESTAMP"
	printf 'Server role: %s\n' "$RESMAN_LIMIT_SERVER_ROLE"
	printf 'Enforceable CPU usage: %s%%\n' "$RESMAN_LIMIT_ENFORCEABLE_CPU_USAGE_PERCENT"
	printf 'Configured CPU Points class: %s\n' "$RESMAN_LIMIT_CPU_POINTS_CONFIGURED_CLASS"
	printf 'CPU Points lifecycle: %s\n' "$RESMAN_LIMIT_CPU_POINTS_LIFECYCLE_STATE"
	printf 'Applied CPU Points class: %s\n' "$RESMAN_LIMIT_CPU_POINTS_APPLIED_CLASS"
	printf 'CPU process coverage: %s\n' "$RESMAN_LIMIT_CPU_POINTS_PROCESS_COVERAGE"
	printf 'RAM coverage: %s\n' "$RESMAN_LIMIT_RAM_COVERAGE"
} | "$SENDMAIL_HELPER" "${sendmail_arguments[@]}"
