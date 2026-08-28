package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ivankuznetsov/screenote-cli/internal/updater"
	"github.com/spf13/cobra"
)

type fakeUpdateService struct {
	checkResult        updater.CheckResult
	checkErr           error
	notificationResult updater.CheckResult
	notificationDue    bool
	notificationErr    error
	notificationLimit  time.Duration
	updateResult       updater.UpdateResult
	updateErr          error
	checkCalls         int
	notificationCalls  int
	updateCalls        int
}

func (f *fakeUpdateService) Check(context.Context, string) (updater.CheckResult, error) {
	f.checkCalls++
	return f.checkResult, f.checkErr
}

func (f *fakeUpdateService) Notification(ctx context.Context, _ string) (updater.CheckResult, bool, error) {
	f.notificationCalls++
	if deadline, ok := ctx.Deadline(); ok {
		f.notificationLimit = time.Until(deadline)
	}
	return f.notificationResult, f.notificationDue, f.notificationErr
}

func (f *fakeUpdateService) Update(context.Context, string, io.Reader) (updater.UpdateResult, error) {
	f.updateCalls++
	return f.updateResult, f.updateErr
}

func TestUpdateCheckReportsAvailableVersionWithoutInstalling(t *testing.T) {
	service := &fakeUpdateService{checkResult: updater.CheckResult{
		CurrentVersion:  "1.0.0",
		LatestVersion:   "1.2.0",
		UpdateAvailable: true,
		Known:           true,
		Channel:         "package",
	}}
	stdout, stderr, err := runCommandWithUpdater(t, service, []string{"update", "--check"})
	if err != nil {
		t.Fatalf("err=%v stderr=%s", err, stderr)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
	if payload["current_version"] != "1.0.0" || payload["latest_version"] != "1.2.0" || payload["update_available"] != true || payload["command"] != "screenote update" {
		t.Fatalf("payload=%#v", payload)
	}
	if _, exists := payload["channel"]; exists {
		t.Fatalf("update --check exposed internal install channel: %#v", payload)
	}
	if service.checkCalls != 1 || service.updateCalls != 0 {
		t.Fatalf("checkCalls=%d updateCalls=%d", service.checkCalls, service.updateCalls)
	}
}

func TestUpdateCommandInstallsLatestVersion(t *testing.T) {
	service := &fakeUpdateService{updateResult: updater.UpdateResult{
		CheckResult: updater.CheckResult{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "1.2.0",
			UpdateAvailable: true,
			Known:           true,
			Channel:         "direct",
		},
		Updated: true,
	}}
	stdout, stderr, err := runCommandWithUpdater(t, service, []string{"update"})
	if err != nil {
		t.Fatalf("err=%v stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, `"updated":true`) || !strings.Contains(stdout, `"channel":"direct"`) {
		t.Fatalf("stdout=%s", stdout)
	}
	if service.updateCalls != 1 || service.notificationCalls != 0 {
		t.Fatalf("updateCalls=%d notificationCalls=%d", service.updateCalls, service.notificationCalls)
	}
}

func TestAutomaticUpdateSuggestionKeepsVersionJSONOnStdout(t *testing.T) {
	service := &fakeUpdateService{
		notificationResult: updater.CheckResult{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "1.2.0",
			UpdateAvailable: true,
			Known:           true,
		},
		notificationDue: true,
	}
	stdout, stderr, err := runCommandWithUpdater(t, service, []string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("version stdout is not JSON: %q", stdout)
	}
	if !strings.Contains(stderr, "Screenote CLI 1.2.0 is available") || !strings.Contains(stderr, "screenote update") {
		t.Fatalf("stderr=%q", stderr)
	}
	if service.notificationCalls != 1 {
		t.Fatalf("notificationCalls=%d", service.notificationCalls)
	}
	if service.notificationLimit < automaticUpdateTimeout-250*time.Millisecond || service.notificationLimit > automaticUpdateTimeout {
		t.Fatalf("automatic update check deadline remaining=%s, want approximately %s", service.notificationLimit, automaticUpdateTimeout)
	}
}

func TestAutomaticUpdateSuggestionUsesPackageManagerHint(t *testing.T) {
	service := &fakeUpdateService{
		notificationResult: updater.CheckResult{
			CurrentVersion:  "1.0.0",
			LatestVersion:   "1.2.0",
			UpdateAvailable: true,
			Known:           true,
			Channel:         "package",
		},
		notificationDue: true,
	}
	_, stderr, err := runCommandWithUpdater(t, service, []string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "update it with your package manager") {
		t.Fatalf("stderr=%q", stderr)
	}
	if strings.Contains(stderr, "screenote update") {
		t.Fatalf("package-managed update should not recommend the built-in updater: %q", stderr)
	}
}

func TestAutomaticUpdateCheckFailureNeverFailsCommand(t *testing.T) {
	service := &fakeUpdateService{notificationErr: context.DeadlineExceeded}
	stdout, stderr, err := runCommandWithUpdater(t, service, []string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(stdout)) || stderr != "" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestAutomaticUpdateCheckSkipsCompletionCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "generated script", args: []string{"completion", "bash"}},
		{name: "hidden completion", args: []string{cobra.ShellCompRequestCmd, "ver"}},
		{name: "hidden completion without descriptions", args: []string{cobra.ShellCompNoDescRequestCmd, "ver"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &fakeUpdateService{}
			_, _, err := runCommandWithUpdater(t, service, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if service.notificationCalls != 0 {
				t.Fatalf("notificationCalls=%d", service.notificationCalls)
			}
		})
	}
}

func TestAutomaticUpdateCheckRejectsDevNull(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("SCREENOTE_NO_UPDATE_CHECK", "")
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	if automaticUpdateCheckEnabled(devNull) {
		t.Fatal("/dev/null must not enable automatic update checks")
	}
}

func TestAutomaticUpdateCheckEnvironmentGates(t *testing.T) {
	for _, gate := range []string{"CI", "SCREENOTE_NO_UPDATE_CHECK"} {
		t.Run(gate, func(t *testing.T) {
			t.Setenv("CI", "")
			t.Setenv("SCREENOTE_NO_UPDATE_CHECK", "")
			t.Setenv(gate, "true")
			terminalChecked := false
			enabled := automaticUpdateCheckEnabledWithTerminal(os.Stderr, func(int) bool {
				terminalChecked = true
				return true
			})
			if enabled {
				t.Fatalf("%s must disable automatic update checks", gate)
			}
			if terminalChecked {
				t.Fatalf("%s should suppress the check before terminal probing", gate)
			}
		})
	}

	t.Setenv("CI", "")
	t.Setenv("SCREENOTE_NO_UPDATE_CHECK", "")
	if !automaticUpdateCheckEnabledWithTerminal(os.Stderr, func(int) bool { return true }) {
		t.Fatal("a terminal with no environment gate should enable automatic update checks")
	}
}

func runCommandWithUpdater(t *testing.T, service cliUpdateService, args []string) (string, string, error) {
	t.Helper()
	previousVersion := Version
	Version = "1.0.0"
	t.Cleanup(func() { Version = previousVersion })

	var stdout, stderr bytes.Buffer
	a := &app{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,
		httpClient: http.DefaultClient, updateService: service, automaticUpdateCheck: true,
	}
	cmd := a.rootCommand(context.Background())
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}
