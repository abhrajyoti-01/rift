// Command e2ebackend starts a small, real backend HTTP server for manual
// end-to-end verification of the load balancer.
//
// It exists because "the proxy works" is only a claim until a request has
// actually traversed it. This server is the far side of that request.
//
// Usage:
//
//	go run ./scripts/e2ebackend              # listens on 127.0.0.1:18091
//	go run ./scripts/e2ebackend -addr :9001  # listen elsewhere
//
// Then, with a configuration whose pool points at that address:
//
//	rift lb --config rift.example.yaml
//	curl http://127.0.0.1:18080/hello
//	# backend-ok path=/hello
//
// /healthz always answers 200 so an HTTP health check can be exercised
// against it as well.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18091", "address to listen on")
	slow := flag.Duration("slow", 0, "delay every response by this much (exercises timeouts and drain)")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if *slow > 0 {
			select {
			case <-time.After(*slow):
			case <-r.Context().Done():
				// The client (or the proxy in front of it) gave up. Return
				// quietly rather than writing into a dead connection.
				return
			}
		}
		// Echo the path back so a caller can confirm the request that
		// arrived is the one it sent.
		fmt.Fprintf(w, "backend-ok path=%s", r.URL.Path)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "backend listening on %s\n", *addr)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "backend:", err)
		os.Exit(1)
	}
}
