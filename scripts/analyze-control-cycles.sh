#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
	cat <<'EOF'
Usage:
  analyze-control-cycles.sh [options] LOG_FILE...

Summarize the CPU enforcement inputs recorded by completed ResMan control cycles.
LOG_FILE may be a plain log or a gzip-compressed .gz rotation.

Thresholds come from /etc/resman/resman.conf by default. Command-line values
override the corresponding configuration value.

Options:
  --config FILE             Read thresholds from FILE instead of /etc/resman/resman.conf
  --cpu-threshold PERCENT   Override the activation threshold
  --release-threshold PERCENT
                            Override the release threshold
  --cpus COUNT              Supply the online CPU count when it cannot be inferred
  --date YYYY-MM-DD         Include only cycles from this date
  -h, --help                Show this help

Example:
  scripts/analyze-control-cycles.sh \
    --config /etc/resman/resman.conf \
    --date 2026-10-21 \
    /var/log/resman.log.4.gz /var/log/resman.log.3.gz \
    /var/log/resman.log.2.gz /var/log/resman.log.1 /var/log/resman.log
EOF
}

die() {
	printf 'analyze-control-cycles: %s\n' "$*" >&2
	exit 2
}

read_config_value() {
	local key=$1
	local path=$2
	awk -v requested_key="$key" '
		function trim(value) {
			sub(/^[[:space:]]+/, "", value)
			sub(/[[:space:]]+$/, "", value)
			return value
		}
		{
			line = $0
			sub(/\r$/, "", line)
			sub(/[[:space:]]*#.*/, "", line)
			equals = index(line, "=")
			if (equals == 0) {
				next
			}
			key = trim(substr(line, 1, equals - 1))
			if (key != requested_key) {
				next
			}
			value = trim(substr(line, equals + 1))
			matches++
		}
		END {
			if (matches != 1 || value == "") {
				exit 2
			}
			print value
		}
	' "$path"
}

is_percentage() {
	awk -v value="$1" 'BEGIN {
		valid = value ~ /^[0-9]+([.][0-9]+)?$/ && value > 0 && value <= 100
		exit !valid
	}'
}

is_positive_integer() {
	[[ $1 =~ ^[1-9][0-9]*$ ]]
}

config_path=/etc/resman/resman.conf
cpu_threshold=
release_threshold=
cpu_count=
date_filter=
log_files=()

while (( $# > 0 )); do
	case "$1" in
		--config)
			(( $# >= 2 )) || die "--config requires a file"
			config_path=$2
			shift 2
			;;
		--cpu-threshold)
			(( $# >= 2 )) || die "--cpu-threshold requires a percentage"
			cpu_threshold=$2
			shift 2
			;;
		--release-threshold)
			(( $# >= 2 )) || die "--release-threshold requires a percentage"
			release_threshold=$2
			shift 2
			;;
		--cpus)
			(( $# >= 2 )) || die "--cpus requires a positive integer"
			cpu_count=$2
			shift 2
			;;
		--date)
			(( $# >= 2 )) || die "--date requires YYYY-MM-DD"
			date_filter=$2
			shift 2
			;;
		-h | --help)
			usage
			exit 0
			;;
		--)
			shift
			log_files+=("$@")
			break
			;;
		-*)
			die "unknown option: $1"
			;;
		*)
			log_files+=("$1")
			shift
			;;
	esac
done

(( ${#log_files[@]} > 0 )) || die "at least one log file is required"

if [[ -z $cpu_threshold || -z $release_threshold ]]; then
	[[ -f $config_path && -r $config_path ]] || die "configuration file is not readable: $config_path"
	if [[ -z $cpu_threshold ]]; then
		cpu_threshold=$(read_config_value CPU_THRESHOLD "$config_path") ||
			die "CPU_THRESHOLD is missing, empty, or duplicated in $config_path"
	fi
	if [[ -z $release_threshold ]]; then
		release_threshold=$(read_config_value CPU_RELEASE_THRESHOLD "$config_path") ||
			die "CPU_RELEASE_THRESHOLD is missing, empty, or duplicated in $config_path"
	fi
fi

[[ -n $cpu_threshold ]] || die "provide --config or --cpu-threshold"
[[ -n $release_threshold ]] || die "provide --config or --release-threshold"
is_percentage "$cpu_threshold" || die "CPU activation threshold must be greater than 0 and at most 100: $cpu_threshold"
is_percentage "$release_threshold" || die "CPU release threshold must be greater than 0 and at most 100: $release_threshold"

if [[ -n $cpu_count ]]; then
	is_positive_integer "$cpu_count" || die "online CPU count must be a positive integer: $cpu_count"
fi

if [[ -n $date_filter && ! $date_filter =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
	die "date must use YYYY-MM-DD: $date_filter"
fi

for path in "${log_files[@]}"; do
	[[ -f $path && -r $path ]] || die "log file is not readable: $path"
	if [[ $path == *.gz ]]; then
		gzip -t -- "$path" || die "gzip-compressed log is invalid: $path"
	fi
done

export LC_ALL=C

stream_logs() {
	local path
	for path in "${log_files[@]}"; do
		printf '\036RESMAN_SOURCE=%s\n' "$path"
		if [[ $path == *.gz ]]; then
			gzip -cd -- "$path"
		else
			cat -- "$path"
		fi
	done
}

stream_logs | awk \
	-v activation_threshold="$cpu_threshold" \
	-v configured_release_threshold="$release_threshold" \
	-v configured_cpu_count="$cpu_count" \
	-v requested_date="$date_filter" '
	function fail(message) {
		printf "analyze-control-cycles: %s:%d: %s\n", source, source_line, message > "/dev/stderr"
		parsing_failed = 1
		exit 2
	}
	function is_number(value) {
		return value ~ /^-?[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$/
	}
	function absolute(value) {
		return value < 0 ? -value : value
	}
	function field_value(key,    i, equals, candidate) {
		for (i = 1; i <= NF; i++) {
			equals = index($i, "=")
			if (equals == 0 || substr($i, 1, equals - 1) != key) {
				continue
			}
			candidate = substr($i, equals + 1)
			return candidate
		}
		return ""
	}
	BEGIN {
		cpu_count = configured_cpu_count + 0
	}
	/^\036RESMAN_SOURCE=/ {
		source = substr($0, length("\036RESMAN_SOURCE=") + 1)
		source_line = 0
		next
	}
	{
		source_line++
	}
	index($0, "Control cycle completed") == 0 {
		next
	}
	{
		timestamp = substr($0, 2, 19)
		cycle_date = substr(timestamp, 1, 10)
		if (requested_date != "" && cycle_date != requested_date) {
			next
		}

		total_text = field_value("total_cpu_usage")
		raw_text = field_value("cpu_eligible_users_cpu_usage")
		host_text = field_value("cpu_eligible_users_host_cpu_percent")
		intent = field_value("requested_policy_intent")
		action = field_value("applied_enforcement_action")

		if (!is_number(total_text) || total_text < 0) {
			fail("completed cycle has an invalid or missing total_cpu_usage")
		}
		if (!is_number(raw_text) || raw_text < 0) {
			fail("completed cycle has an invalid or missing cpu_eligible_users_cpu_usage")
		}
		if (host_text != "" && (!is_number(host_text) || host_text < 0)) {
			fail("completed cycle has an invalid cpu_eligible_users_host_cpu_percent")
		}

		total = total_text + 0
		raw = raw_text + 0
		if (host_text == "") {
			if (cpu_count == 0) {
				fail("completed cycle lacks cpu_eligible_users_host_cpu_percent; provide --cpus")
			}
			host = raw / cpu_count
		} else {
			host = host_text + 0
		}

		if (raw == 0 && host != 0) {
			fail("eligible-user aggregate CPU is zero but host-normalized CPU is not zero")
		}
		if (raw > 0 && host == 0) {
			fail("eligible-user aggregate CPU is nonzero but host-normalized CPU is zero")
		}
		if (raw > 0 && host > 0) {
			ratio = raw / host
			inferred = int(ratio + 0.5)
			if (inferred < 1 || absolute(ratio - inferred) > 0.02) {
				fail("CPU count inferred from aggregate and normalized CPU is not a positive integer")
			}
			if (cpu_count == 0) {
				cpu_count = inferred
			} else if (cpu_count != inferred) {
				fail("inconsistent online CPU count in completed cycles")
			}
		}

		cycles++
		if (cycles == 1 || timestamp < first_timestamp) {
			first_timestamp = timestamp
		}
		if (cycles == 1 || timestamp > last_timestamp) {
			last_timestamp = timestamp
		}
		if (cycles == 1 || total > maximum_total) {
			maximum_total = total
			maximum_total_timestamp = timestamp
		}
		if (cycles == 1 || host > maximum_eligible) {
			maximum_eligible = host
			maximum_raw = raw
			maximum_eligible_timestamp = timestamp
		}
		if (host >= activation_threshold) {
			at_or_above_activation++
		}
		if (intent == "activate") {
			requested_activations++
		}
		if (action == "activate") {
			applied_activations++
		}
	}
	END {
		if (parsing_failed) {
			exit 2
		}
		if (cycles == 0) {
			printf "analyze-control-cycles: no completed control cycles matched the input\n" > "/dev/stderr"
			exit 2
		}
		if (cpu_count == 0) {
			printf "analyze-control-cycles: online CPU count cannot be inferred from zero-only samples; provide --cpus\n" > "/dev/stderr"
			exit 2
		}

		printf "CPU enforcement summary\n"
		printf "Period: %s to %s\n", first_timestamp, last_timestamp
		printf "Online CPUs: %d\n", cpu_count
		printf "Control cycles: %d\n\n", cycles
		printf "%-40s %12s %12s %16s\n", "Metric", "Host %", "Cores", "Aggregate %"
		printf "%-40s %12s %12s %16s\n", "----------------------------------------", "------------", "------------", "----------------"
		printf "%-40s %12.2f %12.2f %16.2f\n", "Activation threshold", activation_threshold, cpu_count * activation_threshold / 100, cpu_count * activation_threshold
		printf "%-40s %12.2f %12.2f %16.2f\n", "Maximum eligible-user CPU observed", maximum_eligible, maximum_raw / 100, maximum_raw
		printf "%-40s %12.2f %12.2f %16.2f\n", "Release threshold", configured_release_threshold, cpu_count * configured_release_threshold / 100, cpu_count * configured_release_threshold
		printf "\nMaximum eligible-user CPU timestamp: %s\n", maximum_eligible_timestamp
		printf "Maximum total host CPU: %.2f%% at %s\n", maximum_total, maximum_total_timestamp
		printf "Samples at or above activation threshold: %d\n", at_or_above_activation + 0
		printf "Requested activation cycles: %d\n", requested_activations + 0
		printf "Applied activation cycles: %d\n", applied_activations + 0
	}
'
