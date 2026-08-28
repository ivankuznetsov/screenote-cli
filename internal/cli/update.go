package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ivankuznetsov/screenote-cli/internal/updater"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const automaticUpdateTimeout = 2 * time.Second

type cliUpdateService interface {
	Check(context.Context, string) (updater.CheckResult, error)
	Notification(context.Context, string) (updater.CheckResult, bool, error)
	Update(context.Context, string, io.Reader) (updater.UpdateResult, error)
}

func (a *app) updateCommand() *cobra.Command {
	var checkOnly bool
	command := &cobra.Command{
		Use:   "update",
		Short: "Update the Screenote CLI to the latest release",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if checkOnly {
				result, err := a.updates().Check(cmd.Context(), Version)
				if err != nil {
					return updateCLIError("update_check_failed", err)
				}
				return writeJSON(a.stdout, updateCheckOutput(result))
			}

			result, err := a.updates().Update(cmd.Context(), Version, a.stdin)
			if err != nil {
				return updateCLIError("update_failed", err)
			}
			return writeJSON(a.stdout, struct {
				CurrentVersion string `json:"current_version"`
				LatestVersion  string `json:"latest_version"`
				Updated        bool   `json:"updated"`
				Channel        string `json:"channel,omitempty"`
			}{
				CurrentVersion: result.CurrentVersion,
				LatestVersion:  result.LatestVersion,
				Updated:        result.Updated,
				Channel:        result.Channel,
			})
		},
	}
	command.Flags().BoolVar(&checkOnly, "check", false, "Check for an update without installing it")
	return command
}

func updateCheckOutput(result updater.CheckResult) any {
	command := ""
	if result.UpdateAvailable {
		command = "screenote update"
	}
	return struct {
		CurrentVersion  string `json:"current_version"`
		LatestVersion   string `json:"latest_version"`
		UpdateAvailable bool   `json:"update_available"`
		Command         string `json:"command,omitempty"`
	}{
		CurrentVersion:  result.CurrentVersion,
		LatestVersion:   result.LatestVersion,
		UpdateAvailable: result.UpdateAvailable,
		Command:         command,
	}
}

func updateCLIError(code string, err error) error {
	if errors.Is(err, updater.ErrDevelopmentBuild) {
		return usageError("development_build", "this development build has no release version; install a tagged release before using screenote update")
	}
	return genericError(code, err.Error())
}

func (a *app) maybeSuggestUpdate(command *cobra.Command) {
	if !a.automaticUpdateCheck || skipsAutomaticUpdateCheck(command) {
		return
	}
	ctx, cancel := context.WithTimeout(command.Context(), automaticUpdateTimeout)
	defer cancel()
	result, due, err := a.updates().Notification(ctx, Version)
	if err != nil || !due {
		return
	}
	if result.Channel == "package" {
		_, _ = fmt.Fprintf(a.stderr, "Screenote CLI %s is available (current %s); update it with your package manager.\n", result.LatestVersion, result.CurrentVersion)
		return
	}
	_, _ = fmt.Fprintf(a.stderr, "Screenote CLI %s is available (current %s); run `screenote update`.\n", result.LatestVersion, result.CurrentVersion)
}

func skipsAutomaticUpdateCheck(command *cobra.Command) bool {
	for current := command; current != nil; current = current.Parent() {
		switch current.Name() {
		case "update", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return true
		}
		switch current.CalledAs() {
		case cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return true
		}
	}
	return false
}

func (a *app) updates() cliUpdateService {
	if a.updateService == nil {
		a.updateService = updater.NewService(a.httpClient)
	}
	return a.updateService
}

func automaticUpdateCheckEnabled(stderr io.Writer) bool {
	return automaticUpdateCheckEnabledWithTerminal(stderr, term.IsTerminal)
}

func automaticUpdateCheckEnabledWithTerminal(stderr io.Writer, isTerminal func(int) bool) bool {
	if environmentTruthy("SCREENOTE_NO_UPDATE_CHECK") || environmentTruthy("CI") {
		return false
	}
	file, ok := stderr.(*os.File)
	if !ok {
		return false
	}
	return isTerminal(int(file.Fd()))
}

func environmentTruthy(name string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	return value != "" && value != "0" && value != "false" && value != "no" && value != "off"
}
