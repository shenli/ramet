// SPDX-License-Identifier: Apache-2.0
// Package lazyfs exposes private writable trees with hydration on first open.
package lazyfs

import (
	"context"
	"os"
)

type File struct {
	Path    string
	Size    int64
	Mode    uint32
	ModTime int64
}

// Hydrate writes verified content into an existing private inode. It must not
// close the file. Failure leaves the inode pending and inaccessible for retry.
type Hydrate func(context.Context, File, *os.File) error
