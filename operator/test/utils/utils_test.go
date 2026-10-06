/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package utils

import "testing"

func TestRequireLoopbackServer(t *testing.T) {
	for _, server := range []string{
		"https://127.0.0.1:52341",
		"https://127.0.0.2:6443",
		"https://[::1]:6443",
		"https://localhost:6443",
	} {
		if err := RequireLoopbackServer(server); err != nil {
			t.Errorf("RequireLoopbackServer(%q) = %v, want nil", server, err)
		}
	}

	// The cluster the suite reached on 2026-10-06 (#201), and other shapes a
	// real cluster's server takes.
	for _, server := range []string{
		"https://35.186.185.138",
		"https://api.example.com:6443",
		"https://10.0.0.1:443",
		"https://127.0.0.1.example.com",
		"",
		"not a url",
	} {
		if err := RequireLoopbackServer(server); err == nil {
			t.Errorf("RequireLoopbackServer(%q) = nil, want a refusal", server)
		}
	}
}
