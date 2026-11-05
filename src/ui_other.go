//go:build !windows

package main

import "os"

// enableVirtualTerminal is a no-op: Unix terminals understand ANSI escapes.
func enableVirtualTerminal(*os.File) bool { return true }
