package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
	"github.com/spf13/cobra"
)

const (
	ExitOK          = 0
	ExitGeneric     = 1
	ExitUsage       = 2
	ExitAuth        = 3
	ExitNotFound    = 4
	ExitRateLimited = 5
)

type cliError struct {
	Code    string
	Message string
	Exit    int
}

func (e *cliError) Error() string { return e.Message }

func usageError(code, message string) error {
	return &cliError{Code: code, Message: message, Exit: ExitUsage}
}

func authError(code, message string) error {
	return &cliError{Code: code, Message: message, Exit: ExitAuth}
}

func writeError(w io.Writer, err error) int {
	code := "internal_error"
	message := err.Error()
	exitCode := ExitGeneric

	var ce *cliError
	if errors.As(err, &ce) {
		code = ce.Code
		message = ce.Message
		exitCode = ce.Exit
	} else {
		var se *screenote.Error
		if errors.As(err, &se) {
			code = se.Code
			message = se.Message
			exitCode = exitForHTTP(se.StatusCode)
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
		"code":  code,
	})
	return exitCode
}

func exitForHTTP(status int) int {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ExitAuth
	case http.StatusNotFound:
		return ExitNotFound
	case http.StatusTooManyRequests:
		return ExitRateLimited
	default:
		return ExitGeneric
	}
}

func missingFlag(name string) error {
	return usageError("missing_"+name, fmt.Sprintf("--%s is required", name))
}

// rejectArgs maps stray positional args and unknown subcommands to a stable
// usage error (exit 2) instead of letting Cobra surface them as a generic
// failure. Assign it as every command's Args validator.
func rejectArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return usageError("unexpected_arguments", fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath()))
}

// showHelp is the RunE for pure grouping commands. Making them runnable ensures
// Cobra reaches the rejectArgs validator (it short-circuits to help before
// validating args on non-runnable commands), so an unknown subcommand exits 2
// while a bare invocation still prints help.
func showHelp(cmd *cobra.Command, _ []string) error {
	return cmd.Help()
}
