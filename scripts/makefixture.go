package main

import "os"

// makefixture writes a deterministic media fixture for local testing.
func main() {
	const path = "bench/fixtures/sample.bin"
	if err := os.MkdirAll("bench/fixtures", 0o755); err != nil {
		panic(err)
	}
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
}
