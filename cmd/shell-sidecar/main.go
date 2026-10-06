// shell-sidecar is the sandbox pod's shell container: a persistent bash
// per session, driven by the worker over the pod's loopback. It holds no
// credential and is given none; what it can reach is the pod's business.
//
//	shell-sidecar --listen 127.0.0.1:9471 --workdir /work
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/subganapathy/paved-road-agent/internal/shell"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9471", "address to serve on; loopback, shared with the worker container")
	workdir := flag.String("workdir", "/work", "the directory shared with the worker, one subdirectory per session")
	idle := flag.Duration("max-idle", 15*time.Minute, "close a session's shell after this long unused")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := &shell.Server{Workdir: *workdir, MaxIdle: *idle, Logger: log}
	log.Info("shell sidecar", "listen", *listen, "workdir", *workdir)
	srv := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
