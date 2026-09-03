#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../../.." && pwd)
go_bin=${GO_BIN:-go}
smolvm_runner=${FINAL_GATE_SMOLVM_RUNNER:-$repo_root/test/functional/smolvm/run.sh}
real_kernel_runner=${FINAL_GATE_REAL_KERNEL_RUNNER:-$repo_root/test/functional/real-kernel/remote.sh}
real_kernel_host=${RESMAN_REAL_KERNEL_HOST:-}
evidence_root=${FINAL_GATE_EVIDENCE_ROOT:-$repo_root/build/functional/final}
dispositions_source=$script_dir/systemd-containment-dispositions.tsv
run_id=
evidence_dir=
matrix_file=
attempts_file=
commands_log=
source_revision=
required_rows=0
failed_rows=0
blocked_rows=0

sanitize_field() {
	printf '%s' "$1" | tr '\t\r\n|' '   /'
}

record_command() {
	printf '%q ' "$@" >>"$commands_log"
	printf '\n' >>"$commands_log"
}

add_matrix_row() {
	local id=$1 status=$2 provenance=$3 evidence=$4 reason=$5
	required_rows=$((required_rows + 1))
	[[ $status != FAIL ]] || failed_rows=$((failed_rows + 1))
	[[ $status != BLOCKED ]] || blocked_rows=$((blocked_rows + 1))
	printf '%s\t%s\t%s\t%s\t%s\n' \
		"$(sanitize_field "$id")" "$status" "$(sanitize_field "$provenance")" \
		"$(sanitize_field "$evidence")" "$(sanitize_field "$reason")" >>"$matrix_file"
}

add_attempt() {
	local id=$1 status=$2 provenance=$3 evidence=$4 reason=$5
	printf '%s\t%s\t%s\t%s\t%s\n' \
		"$(sanitize_field "$id")" "$status" "$(sanitize_field "$provenance")" \
		"$(sanitize_field "$evidence")" "$(sanitize_field "$reason")" >>"$attempts_file"
}

latest_result_dir() {
	local root=$1
	find "$root" -mindepth 2 -maxdepth 2 -type f -name result -printf '%T@ %h\n' \
		| sort -nr | awk 'NR == 1 { print $2 }'
}

evidence_reason() {
	local dir=$1
	local reason=
	if [[ -r $dir/environment.txt ]]; then
		reason=$(sed -n 's/^detail=//p' "$dir/environment.txt" | tail -n 1)
	fi
	[[ -n $reason ]] || reason="no detail was recorded"
	printf '%s' "$reason"
}

run_local_contract() {
	local id=$1 reason=$2
	shift 2
	local log_file=$evidence_dir/local-$id.log
	record_command "$@"
	if "$@" >"$log_file" 2>&1; then
		add_matrix_row "$id" PASS local-focused-test "$log_file" "$reason"
	else
		add_matrix_row "$id" FAIL local-focused-test "$log_file" "focused contract test failed"
	fi
}

run_smolvm_attempt() {
	local attempt_id=$1 scenario=$2 require_psi=$3
	local root=$evidence_dir/smolvm/$attempt_id
	local log_file=$evidence_dir/smolvm-$attempt_id.log
	local status dir result reason
	mkdir -p "$root"
	record_command env "SMOLVM_SCENARIO=$scenario" "SMOLVM_REQUIRE_PSI=$require_psi" \
		"SMOLVM_EVIDENCE_ROOT=$root" "$smolvm_runner" run
	set +e
	env SMOLVM_SCENARIO="$scenario" SMOLVM_REQUIRE_PSI="$require_psi" \
		SMOLVM_EVIDENCE_ROOT="$root" "$smolvm_runner" run >"$log_file" 2>&1
	status=$?
	set -e
	dir=$(latest_result_dir "$root")
	if [[ -z $dir || ! -r $dir/result ]]; then
		add_attempt "$attempt_id" FAIL smolvm "$log_file" "runner retained no result evidence"
		SMOLVM_ATTEMPT_STATUS=FAIL
		SMOLVM_ATTEMPT_EVIDENCE=$log_file
		SMOLVM_ATTEMPT_REASON="runner retained no result evidence"
		return
	fi
	result=$(< "$dir/result")
	reason=$(evidence_reason "$dir")
	case "$status:$result" in
		0:PASS) result=PASS ;;
		77:BLOCKED) result=BLOCKED ;;
		*) result=FAIL; reason="runner exit $status with result $(< "$dir/result"): $reason" ;;
	esac
	add_attempt "$attempt_id" "$result" smolvm "$dir" "$reason"
	SMOLVM_ATTEMPT_STATUS=$result
	SMOLVM_ATTEMPT_EVIDENCE=$dir
	SMOLVM_ATTEMPT_REASON=$reason
}

run_required_smolvm() {
	local id=$1 scenario=$2 reason=$3
	run_smolvm_attempt "$id" "$scenario" 0
	add_matrix_row "$id" "$SMOLVM_ATTEMPT_STATUS" smolvm \
		"$SMOLVM_ATTEMPT_EVIDENCE" \
		"$reason; $SMOLVM_ATTEMPT_REASON"
}

field_value() {
	local file=$1 key=$2
	sed -n "s/^${key}=//p" "$file" | tail -n 1
}

validate_disposition_inventory() {
	local file=$1
	[[ $(head -n 1 "$file") == $'scenario\tprevious_contract\tcontainment_disposition\towner' ]] \
		|| return 1
	awk -F '\t' '
		NR == 1 { next }
		NF != 4 || $1 == "" || $2 == "" || $3 == "" { exit 1 }
		$4 != "resman-yom" && $4 != "resman-nq6" { exit 1 }
		seen[$1]++ { exit 1 }
		END { if (NR != 21) exit 1 }
	' "$file"
}

validate_external_evidence() {
	local scenario=$1 dir=$2
	local env_file=$dir/environment.txt
	[[ -r $dir/result && $(< "$dir/result") == PASS ]] || return 1
	[[ -r $env_file ]] || return 1
	[[ $(field_value "$env_file" scenario) == "$scenario" ]] || return 1
	[[ $(field_value "$env_file" source_revision) == "$source_revision" ]] || return 1
	[[ $(field_value "$env_file" cleanup) == PASS ]] || return 1
	case "$scenario" in
		psi-refresh-neutrality)
			local proof=$dir/psi-refresh-neutrality.txt
			[[ -r $proof ]] || return 1
			[[ $(field_value "$proof" psi_available) == true ]] || return 1
			[[ $(field_value "$proof" psi_event_driven_active) == true ]] || return 1
			[[ $(field_value "$proof" control_cycles_before) == \
				$(field_value "$proof" control_cycles_after) ]] || return 1
			[[ $(field_value "$proof" decision_cpu_before) == \
				$(field_value "$proof" decision_cpu_after) ]] || return 1
			[[ $(field_value "$proof" decision_ema_before) == \
				$(field_value "$proof" decision_ema_after) ]] || return 1
			[[ $(field_value "$proof" system_observation_before) != \
				$(field_value "$proof" system_observation_after) ]] || return 1
			;;
		block-io-all-dimensions)
			local proof=$dir/block-io-summary.txt
			[[ -r $proof ]] || return 1
			for key in io_max_available cached_and_socket_false_activation \
				read_bps write_bps read_iops write_iops; do
				case "$key" in
					io_max_available) [[ $(field_value "$proof" "$key") == true ]] || return 1 ;;
					*) [[ $(field_value "$proof" "$key") == PASS ]] || return 1 ;;
				esac
			done
			;;
		cpu-points-proportional)
			local proof=$dir/cpu-points-summary.txt
			[[ -r $proof ]] || return 1
			[[ -n $(field_value "$env_file" kernel) ]] || return 1
			for key in policy_equality stale_low_separation full_contention \
				class_priority_lending class_change_preservation partial_coverage \
				shutdown_restoration; do
				[[ $(field_value "$proof" "$key") == PASS ]] || return 1
			done
			case "$(field_value "$proof" hotplug)" in
				PASS|BLOCKED) ;;
				*) return 1 ;;
			esac
			;;
		systemd-ownership-preservation)
			local proof=$dir/systemd-ownership-summary.txt
			[[ -r $proof ]] || return 1
			for key in pam_session user_service transient_unit system_service \
				unchanged_membership terminate_session observation_continues \
				zero_active_limits recovery_upgrade; do
				[[ $(field_value "$proof" "$key") == PASS ]] || return 1
			done
			[[ $(field_value "$proof" enforcement_mode) == observation_only_systemd ]] \
				|| return 1
			;;
	esac
}

acquire_external_evidence() {
	local scenario=$1 supplied_dir=$2
	local root=$evidence_dir/real-kernel/$scenario
	local log_file=$evidence_dir/real-kernel-$scenario.log
	local status dir
	if [[ -n $supplied_dir ]]; then
		EXTERNAL_EVIDENCE_DIR=$supplied_dir
		EXTERNAL_EVIDENCE_PROVENANCE=real-kernel-supplied
		return 0
	fi
	if [[ -z $real_kernel_host ]]; then
		return 77
	fi
	mkdir -p "$root"
	record_command env "REAL_KERNEL_EVIDENCE_ROOT=$root" \
		"$real_kernel_runner" "$scenario" "$real_kernel_host"
	set +e
	env REAL_KERNEL_EVIDENCE_ROOT="$root" GO_BIN="$go_bin" \
		"$real_kernel_runner" "$scenario" "$real_kernel_host" >"$log_file" 2>&1
	status=$?
	set -e
	dir=$(latest_result_dir "$root")
	if [[ $status -ne 0 ]]; then
		return "$status"
	fi
	[[ -n $dir ]] || return 1
	EXTERNAL_EVIDENCE_DIR=$dir
	EXTERNAL_EVIDENCE_PROVENANCE=remote-real-kernel
}

run_fallback_contract() {
	local id=$1 smolvm_scenario=$2 require_psi=$3 external_scenario=$4
	local supplied_dir=$5 reason=$6 external_status
	run_smolvm_attempt "$id-smolvm" "$smolvm_scenario" "$require_psi"
	if [[ $SMOLVM_ATTEMPT_STATUS == PASS ]]; then
		add_matrix_row "$id" PASS smolvm "$SMOLVM_ATTEMPT_EVIDENCE" "$reason"
		return
	fi
	if [[ $SMOLVM_ATTEMPT_STATUS == FAIL ]]; then
		add_matrix_row "$id" FAIL smolvm "$SMOLVM_ATTEMPT_EVIDENCE" \
			"SmolVM execution failed: $SMOLVM_ATTEMPT_REASON"
		return
	fi
	set +e
	acquire_external_evidence "$external_scenario" "$supplied_dir"
	external_status=$?
	set -e
	if [[ $external_status -eq 0 ]] \
		&& validate_external_evidence "$external_scenario" "$EXTERNAL_EVIDENCE_DIR"; then
		add_attempt "$id-real-kernel" PASS "$EXTERNAL_EVIDENCE_PROVENANCE" \
			"$EXTERNAL_EVIDENCE_DIR" "current-revision substitute for unavailable guest capability"
		add_matrix_row "$id" PASS "$EXTERNAL_EVIDENCE_PROVENANCE" \
			"$EXTERNAL_EVIDENCE_DIR" \
			"$reason; SmolVM was BLOCKED: $SMOLVM_ATTEMPT_REASON"
	else
		local evidence=${EXTERNAL_EVIDENCE_DIR:-none}
		add_attempt "$id-real-kernel" BLOCKED real-kernel "$evidence" \
			"no valid current-revision substitute evidence"
		add_matrix_row "$id" BLOCKED combined "$SMOLVM_ATTEMPT_EVIDENCE" \
			"$reason remains unproved: SmolVM was BLOCKED and no valid real-kernel evidence was available"
	fi
}

run_required_external() {
	local id=$1 scenario=$2 supplied_dir=$3 reason=$4 status
	local attempt_id=$id
	if [[ $attempt_id != *-real-kernel ]]; then
		attempt_id+=-real-kernel
	fi
	set +e
	acquire_external_evidence "$scenario" "$supplied_dir"
	status=$?
	set -e
	if [[ $status -eq 0 ]] \
		&& validate_external_evidence "$scenario" "$EXTERNAL_EVIDENCE_DIR"; then
		add_attempt "$attempt_id" PASS "$EXTERNAL_EVIDENCE_PROVENANCE" \
			"$EXTERNAL_EVIDENCE_DIR" "$reason"
		add_matrix_row "$id" PASS "$EXTERNAL_EVIDENCE_PROVENANCE" \
			"$EXTERNAL_EVIDENCE_DIR" "$reason"
		return
	fi
	local evidence=${EXTERNAL_EVIDENCE_DIR:-none}
	if [[ $status -eq 77 ]]; then
		add_attempt "$attempt_id" BLOCKED real-kernel "$evidence" \
			"required real-kernel evidence was unavailable"
		add_matrix_row "$id" BLOCKED real-kernel "$evidence" \
			"$reason remains unproved"
	else
		add_attempt "$attempt_id" FAIL real-kernel "$evidence" \
			"required real-kernel scenario failed or retained invalid evidence"
		add_matrix_row "$id" FAIL real-kernel "$evidence" \
			"$reason failed"
	fi
}

write_summary() {
	local overall=$1
	local cpu_points_evidence cpu_points_kernel
	cpu_points_evidence=$(awk -F '\t' '$1 == "cpu-points-real-kernel" { print $4 }' "$matrix_file")
	if [[ -n $cpu_points_evidence && -r $cpu_points_evidence/environment.txt ]]; then
		cpu_points_kernel=$(field_value "$cpu_points_evidence/environment.txt" kernel)
	fi
	{
		printf '# ResMan final semantic regression gate\n\n'
		printf -- '- Result: **%s**\n' "$overall"
		printf -- "- Source revision: \`%s\`\n" "$source_revision"
		printf -- '- Required rows: %d\n' "$required_rows"
		printf -- '- Failed rows: %d\n' "$failed_rows"
		printf -- '- Blocked rows: %d\n\n' "$blocked_rows"
		if [[ -n ${cpu_points_kernel:-} ]]; then
			printf -- "- CPU Points scheduler evidence scope: \`%s\`; the proportional result applies to this running kernel and does not claim equivalent coverage for untested scheduler families.\n\n" \
				"$cpu_points_kernel"
		fi
		printf '| Required contract | Status | Provenance | Evidence | Reason |\n'
		printf '|---|---|---|---|---|\n'
		while IFS=$'\t' read -r id status provenance evidence reason; do
			printf "| \`%s\` | %s | %s | \`%s\` | %s |\n" \
				"$id" "$status" "$provenance" "$evidence" "$reason"
		done <"$matrix_file"
		printf '\n## Execution attempts\n\n'
		printf '| Attempt | Status | Provenance | Evidence | Reason |\n'
		printf '|---|---|---|---|---|\n'
		while IFS=$'\t' read -r id status provenance evidence reason; do
			printf "| \`%s\` | %s | %s | \`%s\` | %s |\n" \
				"$id" "$status" "$provenance" "$evidence" "$reason"
		done <"$attempts_file"
		printf '\n## Systemd containment dispositions\n\n'
		printf "This table accounts for every former real-kernel and migration-dependent scenario; displaced enforcement claims belong to \`resman-nq6\`.\n\n"
		printf '| Scenario | Previous contract | Containment disposition | Owner |\n'
		printf '|---|---|---|---|\n'
		tail -n +2 "$evidence_dir/systemd-containment-dispositions.tsv" \
			| while IFS=$'\t' read -r scenario contract disposition owner; do
				printf "| \`%s\` | %s | %s | \`%s\` |\n" \
					"$scenario" "$contract" "$disposition" "$owner"
			done
	} >"$evidence_dir/summary.md"
}

main() {
	cd "$repo_root"
	if [[ ${FINAL_GATE_ALLOW_DIRTY:-0} != 1 \
		&& -n $(git -C "$repo_root" status --porcelain --untracked-files=no) ]]; then
		echo "the tracked worktree must be clean before the revision-bound final gate" >&2
		return 1
	fi
	run_id=r$(date -u +%Y%m%d%H%M%S)-$$
	evidence_dir=$evidence_root/$run_id
	mkdir -p "$evidence_dir"
	chmod 0700 "$evidence_dir"
	matrix_file=$evidence_dir/matrix.tsv
	attempts_file=$evidence_dir/attempts.tsv
	commands_log=$evidence_dir/commands.log
	: >"$matrix_file"
	: >"$attempts_file"
	: >"$commands_log"
	[[ -r $dispositions_source ]] || {
		echo "systemd containment disposition inventory is missing" >&2
		return 1
	}
	validate_disposition_inventory "$dispositions_source" || {
		echo "systemd containment disposition inventory is malformed or incomplete" >&2
		return 1
	}
	cp "$dispositions_source" "$evidence_dir/systemd-containment-dispositions.tsv"
	source_revision=${FINAL_GATE_SOURCE_REVISION:-$(git -C "$repo_root" rev-parse HEAD)}
	{
		printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		printf 'run_id=%s\n' "$run_id"
		printf 'source_revision=%s\n' "$source_revision"
		printf 'smolvm_cpus=%s\n' "${SMOLVM_CPUS:-2}"
		printf 'smolvm_memory_mib=%s\n' "${SMOLVM_MEMORY_MIB:-2048}"
		printf 'real_kernel_host=%s\n' "${real_kernel_host:-not-configured}"
	} >"$evidence_dir/environment.txt"

	run_local_contract resource-semantics \
		"eligibility, intent, observation, exclusion, and standalone lifecycle agree" \
		"$go_bin" test -count=1 ./state -run \
		'^(TestCollectSystemMetricsUsesIndependentEligibilityAggregates|TestCollectSystemMetricsKeepsExcludedUsageOutOfEveryDecisionAggregate|TestMakeDecisionUsesIndependentResourceAggregates|TestUserLimitStateSeparatesRequestedFromObservedEnforcement|TestResourceOnlyUsersUseStandaloneCgroupsWithoutCPUThrottle)$'
	run_local_contract io-decision-dimensions \
		"read/write bandwidth and IOPS share activation and release semantics" \
		"$go_bin" test -count=1 ./state -run \
		'^(TestMakeDecisionEvaluatesEveryIODimension|TestMakeDecisionReleasesIOOnlyWhenEveryConfiguredDimensionIsBelow|TestBlockIOPSIncompleteCoverageUsesLowerBoundForActivationButBlocksRelease|TestBlockIOPSDecisionIgnoresSyscallsAndUsesDeviceOperations)$'
	run_local_contract sampling-and-cache \
		"refreshes are decision-neutral and cache windows follow their owning cadence" \
		"$go_bin" test -count=1 ./metrics ./state ./internal/app -run \
		'^(TestObservationSamplesDoNotAdvanceDecisionTemporalState|TestObservationAndDecisionSamplesUseIndependentCacheEntries|TestUpdateFallbackCPUSampleUsesSamplingCadence|TestFallbackCPUSampleMaxGapDerivesFromDecisionCadence|TestInterleavedMetricsRefreshDoesNotChangeControlDecisionSample|TestApplyReloadedConfigPublishesEffectiveCPUSamplingCadence)$'
	run_local_contract reload-publication \
		"reload acknowledgement proves publication and rejects failed or stale application" \
		"$go_bin" test -count=1 ./config ./mcp -run \
		'^(TestWatcherRecordsFailedApplyVersion|TestWatcherKeepsCurrentConfigAfterInvalidEnvironmentOverride|TestWatcherReloadRejectsFileChangedDuringApplication|TestUserFilterUpdateRejectsSymlinkedConfigBeforeWatcherReload)$'
	run_local_contract mcp-latest-stateless \
		"MCP 2026-07-28 is latest-only and HTTP requests are interchangeable across instances" \
		"$go_bin" test -count=1 ./mcp -run \
		'^(TestLatestOnlyHTTPConformance|TestLatestOnlyHTTPIsStatelessAcrossInstances|TestLatestOnlyStdioConformance|TestMCPStatusContractsAgreeAcrossSurfacesAndTransports)$'
	run_local_contract prometheus-and-database-truth \
		"Prometheus transitions count confirmed outcomes and SQLite failure cannot report success" \
		"$go_bin" test -count=1 ./state -run \
		'^(TestControlCycleRecordsOperationalOutcomes|TestDeactivationMetricsRequireConfirmedTransition|TestWriteDatabaseMetricsReportsTransactionFailureAndRetries)$'
	run_local_contract cpu-points-invariants \
		"CPU Points parsing, topology, mutation ordering, reload, capacity, persistence, and bounded observations agree" \
		"$go_bin" test -count=1 ./internal/cpupoints ./cgroup ./config ./reloader ./state ./metrics ./database -run \
		'^(TestCPUPointConstructorsEnforceDistinctRanges|TestPlanParentQuotaUsesLiveDenominatorAndExactFloor|TestKernelCPUWeightRejectsInsteadOfClamping|TestPolicyMapFirstEqualsPreservesCompleteUsername|TestPolicyLoaderValidatesTheCompleteCapacityInvariant|TestLiveCapacityProviderRequiresTrustworthyInitialRead|TestLiveCapacityProviderSeesTopologyChangesWithoutObservationCache|TestLiveCapacityProviderRetainsExactPlanAcrossFailureAndRetries|TestEnsureCPUPointsHierarchyProgramsAndVerifiesEverySchedulingLevel|TestCPUPointsLeafIsFullyConfiguredBeforeIngress|TestCPUPointsReloadRejectsActiveClassChangeBeforeAnyKernelWrite|TestCPUPointsReloadFailureAtEveryMutationRetainsSafeRetryIntent|TestCPUPointsOnlineCPUChangeOnlyReprogramsParentQuota|TestCPUPointsAdmissionAndDeparturePreserveAggregateOrdering|TestCPUPointsFailedAdmissionRetainsConservativeHighWaterMark|TestCPUPointsRAMActiveTransitionsFailBeforeCgroupMutation|TestWatcherTreatsMainConfigAndCPUPointsMapAsOneCandidateEpoch|TestCompositeReloadRollsBackWhenMapChangesDuringApplicationBeforeAcknowledgement|TestCPUPointsMetricsBatchRoundTripsTypedAllocationAndAccounting|TestOperationalCPUPointsStatesRequireCompleteComparableDeltas|TestCPUPointsPrometheusSystemSnapshotUsesEffectiveParentIntervalAndDeletesStaleValues)$'

	run_required_smolvm missing-io-startup missing-io-startup \
		"enabled I/O fails startup when a real child lacks io.max"
	run_required_smolvm mcp-filter-reload mcp-filter-reload \
		"MCP 2026-07-28 acknowledges persisted and effective filter reload"
	run_fallback_contract psi-refresh-neutrality psi-refresh-neutrality 1 psi-refresh-neutrality \
		"${FINAL_GATE_PSI_EVIDENCE:-}" \
		"PSI observation refreshes do not advance decision CPU or EMA"
	run_required_external systemd-ownership-preservation systemd-ownership-preservation \
		"${FINAL_GATE_SYSTEMD_OWNERSHIP_EVIDENCE:-}" \
		"PAM session, user service, transient unit, and system service ownership remain authoritative while observation continues and inherited recovery stays stranded"

	local overall exit_code
	if [[ $failed_rows -gt 0 ]]; then
		overall=FAIL
		exit_code=1
	elif [[ $blocked_rows -gt 0 ]]; then
		overall=BLOCKED
		exit_code=77
	else
		overall=PASS
		exit_code=0
	fi
	printf '%s\n' "$overall" >"$evidence_dir/result"
	printf 'finished_at=%s\nrequired_rows=%d\nfailed_rows=%d\nblocked_rows=%d\nresult=%s\nexit_code=%d\n' \
		"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$required_rows" "$failed_rows" \
		"$blocked_rows" "$overall" "$exit_code" >>"$evidence_dir/environment.txt"
	write_summary "$overall"
	echo "$overall: final semantic regression gate"
	echo "Evidence: $evidence_dir"
	return "$exit_code"
}

if [[ ${RESMAN_FINAL_GATE_LIBRARY_ONLY:-0} != 1 ]]; then
	main "$@"
fi
