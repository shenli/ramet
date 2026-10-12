// Copyright 2026 Ramet contributors
// SPDX-License-Identifier: Apache-2.0

// Package engine provides snapshot-based state for parallel agent executions.
//
// Open a local Store, capture a Snapshot, and create a Pool to acquire isolated
// writable directories. Callers must stop all writers before reset, release,
// fork, checkpoint, or promotion. Ramet manages workspace state, not processes
// or sandbox security. Mount only execution directories into your sandbox.
package engine
