package layout

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestResManSendmailHookBuildsOneBoundedInvocation(t *testing.T) {
	adapter, argumentsPath, bodyPath := configuredSendmailHook(t, 0)
	cmd := exec.Command(adapter)
	cmd.Env = sendmailHookEnvironment(argumentsPath, bodyPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sendmail adapter failed: %v\n%s", err, output)
	}

	argumentData, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatalf("read captured sendmail arguments: %v", err)
	}
	arguments := strings.Split(strings.TrimSuffix(string(argumentData), "\x00"), "\x00")
	wantArguments := []string{
		"-f", "resman@example.test",
		"-t", "operator@example.test",
		"-s", "ResMan limit applied to john.smith (UID 1006)",
		"-S", "localhost",
		"-P", "25",
	}
	if !reflect.DeepEqual(arguments, wantArguments) {
		t.Fatalf("unexpected sendmail arguments:\n got: %#v\nwant: %#v", arguments, wantArguments)
	}

	bodyData, err := os.ReadFile(bodyPath)
	if err != nil {
		t.Fatalf("read captured message body: %v", err)
	}
	body := string(bodyData)
	for _, expected := range []string{
		"ResMan applied a resource limit.\n",
		"User: john.smith\n",
		"UID: 1006\n",
		"Timestamp: 2026-09-02T07:00:00Z\n",
		"Server role: build\n",
		"Configured CPU Points class: guaranteed\n",
		"CPU Points lifecycle: applied\n",
		"RAM coverage: complete\n",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("message body is missing %q:\n%s", expected, body)
		}
	}
}

func TestResManSendmailHookRejectsInvalidInvocationBeforeDelivery(t *testing.T) {
	tests := []struct {
		name       string
		arguments  []string
		mutateEnv  func([]string) []string
		wantOutput string
	}{
		{
			name:       "unexpected positional argument",
			arguments:  []string{"--recipient", "attacker@example.test"},
			wantOutput: "this adapter accepts no arguments",
		},
		{
			name: "missing uid",
			mutateEnv: func(environment []string) []string {
				return removeEnvironmentVariable(environment, "RESMAN_LIMIT_UID")
			},
			wantOutput: "required environment variable is missing: RESMAN_LIMIT_UID",
		},
		{
			name: "non-numeric uid",
			mutateEnv: func(environment []string) []string {
				return replaceEnvironmentVariable(environment, "RESMAN_LIMIT_UID", "not-a-uid")
			},
			wantOutput: "RESMAN_LIMIT_UID must be an unsigned decimal integer",
		},
		{
			name: "header injection through username",
			mutateEnv: func(environment []string) []string {
				return replaceEnvironmentVariable(environment, "RESMAN_LIMIT_USERNAME", "alice\nBcc: attacker@example.test")
			},
			wantOutput: "RESMAN_LIMIT_USERNAME contains control characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter, argumentsPath, bodyPath := configuredSendmailHook(t, 0)
			environment := sendmailHookEnvironment(argumentsPath, bodyPath)
			if tt.mutateEnv != nil {
				environment = tt.mutateEnv(environment)
			}
			cmd := exec.Command(adapter, tt.arguments...)
			cmd.Env = environment
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("sendmail adapter accepted invalid invocation; output=%s", output)
			}
			if !strings.Contains(string(output), tt.wantOutput) {
				t.Fatalf("failure is missing %q: %s", tt.wantOutput, output)
			}
			if _, err := os.Stat(argumentsPath); !os.IsNotExist(err) {
				t.Fatalf("invalid invocation reached sendmail helper: %v", err)
			}
		})
	}
}

func TestPackagedResManSendmailHookRequiresOperatorConfiguration(t *testing.T) {
	root := repositoryRoot(t)
	adapter := filepath.Join(root, "scripts/resman-sendmail-hook.sh")
	directory := t.TempDir()
	argumentsPath := filepath.Join(directory, "arguments")
	bodyPath := filepath.Join(directory, "body")
	cmd := exec.Command(adapter)
	cmd.Env = sendmailHookEnvironment(argumentsPath, bodyPath)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("packaged adapter accepted its placeholder addresses; output=%s", output)
	}
	if !strings.Contains(string(output), "copy the adapter and configure MAIL_FROM and MAIL_TO before use") {
		t.Fatalf("placeholder rejection is not diagnostic: %s", output)
	}
}

func TestResManSendmailHookPropagatesDeliveryFailure(t *testing.T) {
	adapter, argumentsPath, bodyPath := configuredSendmailHook(t, 23)
	cmd := exec.Command(adapter)
	cmd.Env = sendmailHookEnvironment(argumentsPath, bodyPath)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("sendmail adapter reported success after helper failure; output=%s", output)
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok || exitError.ExitCode() != 23 {
		t.Fatalf("sendmail adapter did not preserve helper exit status 23: %v", err)
	}
}

func configuredSendmailHook(t *testing.T, helperExitCode int) (string, string, string) {
	t.Helper()
	root := repositoryRoot(t)
	source := readTextFile(t, filepath.Join(root, "scripts/resman-sendmail-hook.sh"))
	directory := t.TempDir()
	helperPath := filepath.Join(directory, "sendmail.sh")
	argumentsPath := filepath.Join(directory, "arguments")
	bodyPath := filepath.Join(directory, "body")
	helper := "#!/bin/bash\nset -euo pipefail\nprintf '%s\\0' \"$@\" > \"$TEST_ARGUMENTS_PATH\"\ncat > \"$TEST_BODY_PATH\"\nexit " + strconv.Itoa(helperExitCode) + "\n"
	if err := os.WriteFile(helperPath, []byte(helper), 0o755); err != nil {
		t.Fatalf("write fake sendmail helper: %v", err)
	}

	configured := strings.NewReplacer(
		`readonly SENDMAIL_HELPER="/usr/share/doc/resman/scripts/sendmail.sh"`, `readonly SENDMAIL_HELPER="`+helperPath+`"`,
		`readonly MAIL_FROM="CHANGE_ME@example.invalid"`, `readonly MAIL_FROM="resman@example.test"`,
		`readonly MAIL_TO="CHANGE_ME@example.invalid"`, `readonly MAIL_TO="operator@example.test"`,
	).Replace(source)
	adapterPath := filepath.Join(directory, "resman-sendmail-hook.sh")
	if err := os.WriteFile(adapterPath, []byte(configured), 0o755); err != nil {
		t.Fatalf("write configured sendmail adapter: %v", err)
	}
	return adapterPath, argumentsPath, bodyPath
}

func sendmailHookEnvironment(argumentsPath, bodyPath string) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"TEST_ARGUMENTS_PATH=" + argumentsPath,
		"TEST_BODY_PATH=" + bodyPath,
		"RESMAN_LIMIT_UID=1006",
		"RESMAN_LIMIT_USERNAME=john.smith",
		"RESMAN_LIMIT_TIMESTAMP=2026-09-02T07:00:00Z",
		"RESMAN_LIMIT_SERVER_ROLE=build",
		"RESMAN_LIMIT_ENFORCEABLE_CPU_USAGE_PERCENT=250.00",
		"RESMAN_LIMIT_SHARED_CGROUP=/sys/fs/cgroup/resman/cpu-points/guaranteed/1006",
		"RESMAN_LIMIT_CPU_POINTS_CONFIGURED_CLASS=guaranteed",
		"RESMAN_LIMIT_CPU_POINTS_LIFECYCLE_STATE=applied",
		"RESMAN_LIMIT_CPU_POINTS_APPLIED_CLASS=guaranteed",
		"RESMAN_LIMIT_CPU_POINTS_PROCESS_COVERAGE=complete",
		"RESMAN_LIMIT_RAM_COVERAGE=complete",
	}
}

func removeEnvironmentVariable(environment []string, name string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return result
}

func replaceEnvironmentVariable(environment []string, name, value string) []string {
	return append(removeEnvironmentVariable(environment, name), name+"="+value)
}
