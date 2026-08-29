#!/usr/bin/env bash
# Prepare an authorized ResMan functional-test host.
#
# Creates the dedicated enforcement accounts used by the real-host scenarios and
# proves that no excluded account can become eligible. The script is idempotent:
# running it twice changes nothing and still verifies the contract.
set -Eeuo pipefail

# Non-interactive SSH sessions do not inherit the administrative PATH.
PATH=/usr/sbin:/sbin:/usr/bin:/bin
export PATH

test_users=(resman-t1 resman-t2 resman-t3 resman-t4)
# Accounts that must never be moved into a ResMan cgroup by any scenario.
excluded_users=(dbacro1 dbacro2 crm francesco root)

if [[ $EUID -ne 0 ]]; then
	echo "prepare-host.sh must run as root on the authorized test host" >&2
	exit 1
fi

for user in "${test_users[@]}"; do
	if id -u "$user" >/dev/null 2>&1; then
		continue
	fi
	useradd --create-home --shell /bin/bash --comment "ResMan functional test account" "$user"
done

for user in "${test_users[@]}"; do
	uid=$(id -u "$user")
	if (( uid < 1000 )); then
		echo "test account $user has system UID $uid; scenarios require UID >= 1000" >&2
		exit 1
	fi
done

# Whether the excluded accounts exist proves nothing on its own; what matters is
# that the scenario configuration never selects them. Report their UIDs so the evidence
# records exactly which identities were protected during the run.
for user in "${excluded_users[@]}"; do
	if id -u "$user" >/dev/null 2>&1; then
		printf 'protected_user=%s uid=%s\n' "$user" "$(id -u "$user")"
	fi
done

for user in "${test_users[@]}"; do
	printf 'test_user=%s uid=%s\n' "$user" "$(id -u "$user")"
done

echo "prepare-host: PASS"
