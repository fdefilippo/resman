#!/usr/bin/env sh
set -eu

GO=${GO:-go}
OUTDIR=${OUTDIR:-build/dependencies}
VULN_PACKAGES=${VULN_PACKAGES:-./...}
GOVULNCHECK=${GOVULNCHECK:-}
GOVULNCHECK_VERSION=${GOVULNCHECK_VERSION:-}

if [ -z "$GOVULNCHECK" ]; then
	if command -v govulncheck >/dev/null 2>&1; then
		GOVULNCHECK=$(command -v govulncheck)
	elif [ -x "$HOME/go/bin/govulncheck" ]; then
		GOVULNCHECK="$HOME/go/bin/govulncheck"
	fi
fi

if [ -n "$GOVULNCHECK" ] && [ -n "$GOVULNCHECK_VERSION" ]; then
	installed_version=$("$GOVULNCHECK" -version | sed -n 's/^Scanner: govulncheck@//p')
	if [ "$installed_version" != "$GOVULNCHECK_VERSION" ]; then
		GOVULNCHECK=
		version_error="govulncheck $installed_version is installed; version $GOVULNCHECK_VERSION is required"
	fi
fi

mkdir -p "$OUTDIR"

modules_json="$OUTDIR/deps-report.modules.json"
active_modules="$OUTDIR/deps-report.active-modules.txt"
verify_log="$OUTDIR/deps-report.verify.txt"
vuln_log="$OUTDIR/deps-report.vuln.txt"
report="$OUTDIR/deps-report.txt"

"$GO" list -u -m -json all > "$modules_json"
# shellcheck disable=SC2086
"$GO" list -test -deps -f '{{with .Module}}{{.Path}}{{end}}' $VULN_PACKAGES | sort -u > "$active_modules"
"$GO" mod verify > "$verify_log" 2>&1

if [ -n "$GOVULNCHECK" ]; then
	set +e
	# shellcheck disable=SC2086
	"$GOVULNCHECK" $VULN_PACKAGES > "$vuln_log" 2>&1
	vuln_status=$?
	set -e
else
	vuln_status=127
	{
		echo "${version_error:-govulncheck is not installed}"
		echo "run: make deps-vuln-install"
	} > "$vuln_log"
fi

{
	echo "# resman Go dependency report"
	echo
	echo "Generated: $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
	echo "Go: $("$GO" version)"
	echo
	"$GO" run ./scripts/deps-report.go "$modules_json" "$active_modules"
	echo
	echo "## Module verification"
	sed 's/^/- /' "$verify_log"
	echo
	echo "## Vulnerability scan"
	echo "- exit status: $vuln_status"
	sed 's/^/- /' "$vuln_log"
	echo
	echo "## Recommended maintenance commands"
	echo "- one module: make deps-update MODULE=golang.org/x/sys"
	echo "- direct dependency set: make deps-update-core"
	echo "- validation after an update: make deps-test"
	echo
	echo "## Recommendation"
	if grep -q "No vulnerabilities found" "$vuln_log"; then
		echo "- no reachable vulnerability was reported"
		echo "- schedule ordinary direct-module updates separately from feature work"
	elif [ "$vuln_status" -eq 127 ]; then
		echo "- install the pinned govulncheck version before deciding dependency risk"
	else
		echo "- review the scanner findings before ordinary dependency maintenance"
		echo "- update the affected direct module or the smallest containing set first"
	fi
} > "$report"

echo "Dependency report written to $report"
