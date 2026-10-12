// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"strings"
	"testing"
)

func TestStrictPolicyDecode(t *testing.T) {
	for _, input := range []string{`null`, `[]`, `{"rules":null}`, `{"rules":[{"path":"x","class":"durable","runtime":null}]}`, `{"rulez":[]}`, `{"rules":[{"path":"deps","class":"rebuildable","recipe":"build","inputz":["lock"]}]}`, `{} {}`, `{"rules":[{"path":"../escape","class":"ephemeral"}]}`} {
		if _, err := DecodePolicy(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	policy, err := DecodePolicy(strings.NewReader(`{"rules":[{"path":"scratch","class":"ephemeral"}]}`))
	if err != nil || policy.Classify("scratch/file") != Ephemeral {
		t.Fatal(policy, err)
	}
}
