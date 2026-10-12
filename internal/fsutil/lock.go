//go:build linux || darwin

// SPDX-License-Identifier: Apache-2.0
package fsutil

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("store is already open (use its HTTP API): %w", err)
	}
	return f, nil
}

func Unlock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
