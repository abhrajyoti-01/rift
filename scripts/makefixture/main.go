package main

import (
	"fmt"
	"os"
)

// makefixture writes a deterministic media fixture for local testing and
// benchmarks. The content is a repeating byte pattern so that a range
// request can be verified against exact file offsets.
func main() {
	const dir = "bench/fixtures"
	const path = dir + "/sample.bin"

	// Create the directory first: a fresh clone has no bench/fixtures, and
	// writing before creating it fails.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "rift: makefixture:", err)
		os.Exit(1)
	}

	const size = 1 << 20
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "rift: makefixture:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", path, size)
}
