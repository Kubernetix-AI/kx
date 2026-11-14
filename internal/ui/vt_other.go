//go:build !windows

package ui

import "os"

// enableVT reports whether the terminal f writes to interprets escape
// sequences; outside Windows, terminals do.
func enableVT(*os.File) bool { return true }
