// SPDX-License-Identifier: Apache-2.0
package fsutil

import "golang.org/x/sys/unix"

func cloneFile(src, dst string) error { return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW) }
