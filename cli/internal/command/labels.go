// SPDX-License-Identifier: AGPL-3.0-only

package command

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

func labelsCommand(e *env) *cobra.Command {
	var since, startFlag, endFlag string
	cmd := &cobra.Command{
		Use:   "labels [NAME]",
		Short: "List the labels you can match logs on, or one label's values",
		Long: `List the label names seen on the active project's logs, or, given a name,
the values that label takes. Use them to build a query for
'datumctl telemetry logs'.`,
		Example: `  # Which labels can I match on?
  datumctl telemetry labels

  # Which services logged in the last day?
  datumctl telemetry labels service_name --since 24h

  # Which labels were set in an explicit window?
  datumctl telemetry labels --start 2026-10-06T09:00:00Z --end 2026-10-06T10:00:00Z`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			start, end, err := resolveWindow(e.now(), since, startFlag, endFlag)
			if err != nil {
				return err
			}
			api, err := e.client(project(cmd))
			if err != nil {
				return err
			}
			var values []string
			if len(args) == 0 {
				values, err = api.LabelNames(cmd.Context(), start, end)
			} else {
				values, err = api.LabelValues(cmd.Context(), args[0], start, end)
			}
			if err != nil {
				return err
			}
			slices.Sort(values)

			if format == "json" {
				if values == nil {
					values = []string{}
				}
				return json.NewEncoder(e.streams.Out).Encode(values)
			}
			if len(values) == 0 {
				_, _ = fmt.Fprintf(e.streams.ErrOut, "Nothing logged between %s and %s.\n",
					start.Format("2006-01-02T15:04:05Z07:00"), end.Format("2006-01-02T15:04:05Z07:00"))
				return nil
			}
			_, err = fmt.Fprintln(e.streams.Out, strings.Join(values, "\n"))
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&since, "since", "", "Only labels on lines newer than this, e.g. 15m, 2h, 7d (default 1h)")
	f.StringVar(&startFlag, "start", "", "Only labels on lines at or after this RFC3339 time or unix seconds")
	f.StringVar(&endFlag, "end", "", "Only labels on lines before this RFC3339 time or unix seconds (default now)")
	return cmd
}
