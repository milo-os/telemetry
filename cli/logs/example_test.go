// SPDX-License-Identifier: AGPL-3.0-only

package logs_test

import (
	"context"
	"fmt"
	"time"

	"go.miloapis.com/telemetry/cli/logql"
	"go.miloapis.com/telemetry/cli/logs"
)

// A datumctl plugin passes plugin.Context().APIHost and plugin.Token.
func Example() {
	ctx := context.Background()
	client := logs.New(logs.ProjectURL("api.datum.net", "my-project"),
		logs.WithToken(func() (string, error) { return "token", nil }))

	var sel logql.Selector
	sel.Eq("datum_workload_name", "api").OneOf("log_iostream", "stderr").Contains("panic")

	entries, err := logs.Tail(ctx, client, sel.String(), time.Now().Add(-time.Hour), time.Now(), 200)
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, e := range entries {
		fmt.Println(e.Time.Format(time.RFC3339), e.Line)
	}
}
