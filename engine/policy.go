// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// DecodePolicy rejects misspelled fields and ambiguous documents before a caller
// can accidentally publish with a weaker persistence policy.
func DecodePolicy(reader io.Reader) (Policy, error) {
	var policy Policy
	data, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil {
		return policy, err
	}
	if len(data) > 1<<20 {
		return policy, fmt.Errorf("%w: policy exceeds 1 MiB", ErrInvalid)
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return policy, fmt.Errorf("%w: policy must be a JSON object", ErrInvalid)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	type policyJSON Policy
	var decoded policyJSON
	if err = decoder.Decode(&decoded); err != nil {
		return policy, fmt.Errorf("%w: policy: %v", ErrInvalid, err)
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return policy, fmt.Errorf("%w: policy must contain exactly one JSON object", ErrInvalid)
	}
	policy = Policy(decoded)
	var value any
	if err = json.Unmarshal(data, &value); err != nil {
		return policy, err
	}
	var hasNull func(any) bool
	hasNull = func(v any) bool {
		switch item := v.(type) {
		case nil:
			return true
		case []any:
			for _, child := range item {
				if hasNull(child) {
					return true
				}
			}
		case map[string]any:
			for _, child := range item {
				if hasNull(child) {
					return true
				}
			}
		}
		return false
	}
	if hasNull(value) {
		return policy, fmt.Errorf("%w: policy fields cannot be null", ErrInvalid)
	}
	return policy, policy.Validate()
}

// UnmarshalJSON keeps policy validation identical across CLI, HTTP and manifests.
func (p *Policy) UnmarshalJSON(data []byte) error {
	decoded, err := DecodePolicy(bytes.NewReader(data))
	if err == nil {
		*p = decoded
	}
	return err
}
