//go:build ignore

// e2e_lb is a manual end-to-end check: it starts a real backend HTTP server
// and prints its address so `rift lb` can be pointed at it.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	addr := "127.0.0.1:18091"
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend-ok path=%s", r.URL.Path)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	fmt.Fprintf(os.Stderr, "backend listening on %s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
