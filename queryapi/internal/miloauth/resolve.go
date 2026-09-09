// SPDX-License-Identifier: AGPL-3.0-only

package miloauth

import (
	"regexp"

	"k8s.io/apiserver/pkg/authentication/user"
)

const (
	parentTypeExtra = "iam.miloapis.com/parent-type"
	parentNameExtra = "iam.miloapis.com/parent-name"
)

// validProjectID guards the value before it reaches a query parameter or a
// ClickHouse setting.
var validProjectID = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// Resolve returns the project recorded in the caller's user extras.
//
// That identity is the apiserver's: the delegating authenticator turns Milo's
// forwarded X-Remote-* headers back into a user.Info only after verifying the
// connection's client certificate against the front proxy's CA, and it also
// answers for a caller presenting a bearer token directly. The extras
// therefore come from a verified source.
//
// There is no other source. Nothing a client can set on its own -- a header, a
// path segment, a query parameter -- names the project, so there is no mode in
// which a caller chooses its own tenant. Milo sets the same extras for
// organization parents, so the type check keeps an organization id from being
// read as a project id.
func Resolve(u user.Info) (string, bool) {
	if u == nil {
		return "", false
	}
	extra := u.GetExtra()
	if first(extra[parentTypeExtra]) != "Project" {
		return "", false
	}
	if id := first(extra[parentNameExtra]); validID(id) {
		return id, true
	}
	return "", false
}

func validID(id string) bool {
	return id != "" && len(id) <= 253 && validProjectID.MatchString(id)
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
