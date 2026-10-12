// SPDX-License-Identifier: Apache-2.0
package fsutil

import (
	"golang.org/x/sys/unix"
	"os"
)

func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	closeErr := out.Close()
	if err != nil {
		_ = os.Remove(dst)
		return err
	}
	return closeErr
}
