//go:build !linux

// Command dabberz-guest is the agent inside every Firecracker guest. Guests run
// Linux, so on any other platform it only says so.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "dabberz-guest runs inside a Linux guest only")
	os.Exit(1)
}
