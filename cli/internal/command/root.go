// SPDX-License-Identifier: AGPL-3.0-only

// Package command implements the datumctl telemetry plugin's commands.
package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.datum.net/datumctl/plugin"

	"go.miloapis.com/telemetry/cli/logs"
)

// API is the part of the logs client the commands use.
type API interface {
	logs.Querier
	LabelNames(ctx context.Context, start, end time.Time) ([]string, error)
	LabelValues(ctx context.Context, label string, start, end time.Time) ([]string, error)
}

// ClientFactory builds an API client for a project.
type ClientFactory func(project string) (API, error)

// Streams are the command's output streams.
type Streams struct {
	Out    io.Writer
	ErrOut io.Writer
}

type env struct {
	streams Streams
	client  ClientFactory
	now     func() time.Time
}

// NewRoot returns the plugin's root command.
func NewRoot(streams Streams, client ClientFactory) *cobra.Command {
	return newRoot(&env{streams: streams, client: client, now: time.Now})
}

func newRoot(e *env) *cobra.Command {
	root := plugin.NewRootCmd("telemetry", "Query logs for the active project")
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.SetOut(e.streams.Out)
	root.SetErr(e.streams.ErrOut)
	root.AddCommand(logsCommand(e), labelsCommand(e))
	return root
}

// DatumctlClient builds a client from the context datumctl injects into the
// plugin's environment.
func DatumctlClient(userAgent string) ClientFactory {
	return func(project string) (API, error) {
		ctx := plugin.Context()
		if ctx.APIHost == "" || ctx.CredentialsHelper == "" {
			return nil, &UserError{
				Msg:  "no API host or credentials were passed in; this plugin runs under datumctl",
				Hint: "run it as 'datumctl telemetry ...'",
			}
		}
		if project == "" {
			return nil, &UserError{
				Msg:  "no project selected",
				Hint: "pass --project, or pick one with 'datumctl ctx use'",
			}
		}
		return logs.New(logs.ProjectURL(ctx.APIHost, project),
			logs.WithToken(plugin.Token),
			logs.WithUserAgent(userAgent),
		), nil
	}
}

// UserError is an error with advice on what to do next.
type UserError struct {
	Msg  string
	Hint string
}

func (e *UserError) Error() string { return e.Msg }

// Render prints err to w with its hint, if any.
func Render(w io.Writer, err error) {
	msg, hint := err.Error(), ""
	var ue *UserError
	var ae *logs.APIError
	switch {
	case errors.As(err, &ue):
		hint = ue.Hint
	case errors.As(err, &ae):
		hint = ae.Hint()
	}
	_, _ = fmt.Fprintf(w, "error: %s\n", msg)
	if hint != "" {
		_, _ = fmt.Fprintf(w, "\n%s\n", hint)
	}
}

func project(cmd *cobra.Command) string {
	p, _ := cmd.Flags().GetString("project")
	return p
}

func outputFormat(cmd *cobra.Command) (string, error) {
	o, _ := cmd.Flags().GetString("output")
	switch o {
	case "", "table", "text":
		return "text", nil
	case "json":
		return "json", nil
	}
	return "", &UserError{Msg: fmt.Sprintf("unsupported --output %q", o), Hint: "use one of: table, json"}
}
