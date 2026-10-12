//go:build !linux

// SPDX-License-Identifier: Apache-2.0
package lazyfs

import "errors"

type Mount struct{}

func Supported() error { return errors.New("lazy backend requires Linux, /dev/fuse, and fusermount3") }
func Start(backing, target string, files []File, hydrate Hydrate) (*Mount, error) {
	return nil, Supported()
}
func (*Mount) Close() error       { return nil }
func Recover(target string) error { return nil }
