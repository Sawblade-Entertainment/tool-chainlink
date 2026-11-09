//go:build !windows

package main

import "os"

// openLikeRunningBinary opens path the way a running executable holds it.
func openLikeRunningBinary(path string) (*os.File, error) {
	return os.Open(path)
}
