// Package sandbox is the worker: the process in the sandbox pod that
// claims a session's work item, serves every tool call the agents make,
// and exits. It holds no credential except the environment key it polls
// with and the proxy's bearer; neither reaches the model or the shell.
//
// The distributed-systems shape, in one paragraph: a session on a
// self-hosted environment is a *work item* in a queue at Anthropic. A
// worker claims one (a lease), heartbeats the lease while it runs, and
// serves the session's event stream — reconnecting when it drops,
// reconciling missed tool calls from the events list, answering each
// tool call exactly once, and stopping when the session goes idle or the
// lease is lost. The SDK's EnvironmentWorker does all of that; this
// package decides what tools it serves, binds them to the session, and
// enforces "one sandbox, one session".
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/lib/environments"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/tools/agenttoolset"

	"github.com/subganapathy/paved-road-agent/internal/connectors"
)

// Options configure the worker.
type Options struct {
	EnvironmentID  string
	EnvironmentKey string // the worker's standing credential for claiming work
	// Workdir is the base directory; each session gets its own subdirectory,
	// removed when the session ends.
	Workdir string
	// Proxy is where the connector tools go.
	ProxyBase  string
	ProxyToken string
	// Once makes the worker claim exactly one item, serve it, and return:
	// the one-sandbox-one-session shape. False loops forever, one item at
	// a time, for the laptop.
	Once bool
	// MaxIdle is how long after the session goes idle the worker stops
	// serving it. The lead ending its turn is the normal exit.
	MaxIdle time.Duration
	// ReclaimAfter is how stale a lease must be before this worker takes
	// the item over. The server's lease TTL is 300s; default 330s.
	ReclaimAfter time.Duration
	Logger       *slog.Logger
}

// Run claims work and serves it until ctx ends, or after one item when
// Once is set. It returns nil on a clean exit.
func Run(ctx context.Context, client anthropic.Client, o Options) error {
	if o.EnvironmentID == "" || o.EnvironmentKey == "" {
		return errors.New("environment id and key are required")
	}
	if o.Workdir == "" {
		return errors.New("workdir is required")
	}
	if o.MaxIdle == 0 {
		o.MaxIdle = 30 * time.Second
	}
	if o.ReclaimAfter == 0 {
		o.ReclaimAfter = 330 * time.Second
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "sandbox")

	// The poller claims; HandleItem owns the item's lifecycle from then on,
	// so AutoStop is off: the item must not be stopped twice.
	//
	// Reclaim: a lease is lost when a heartbeat times out on our side but
	// reaches the server, so the next heartbeat's precondition fails and
	// the SDK releases the item without stopping it (it cannot know whether
	// another worker now holds it). The session then sits with a tool call
	// nobody will answer. Asking the poll to reclaim items whose lease has
	// gone unheartbeated past the TTL lets this worker, or the next one,
	// pick the session up where it stopped.
	poller := environments.NewWorkPoller(ctx, client, environments.WorkPollerOptions{
		EnvironmentID:      o.EnvironmentID,
		EnvironmentKey:     o.EnvironmentKey,
		AutoStop:           param.NewOpt(false),
		ReclaimOlderThanMs: param.NewOpt(int64(o.ReclaimAfter / time.Millisecond)),
		Logger:             log,
	})
	defer poller.Close()

	for poller.Next() {
		work := poller.Current()
		if string(work.Data.Type) != "session" {
			continue
		}
		session := work.Data.ID
		log.Info("claimed", "work", work.ID, "session", session)
		if err := serve(ctx, client, o, log, work); err != nil {
			log.Error("session ended with error", "session", session, "err", err)
		} else {
			log.Info("session served", "session", session)
		}
		if o.Once {
			return nil
		}
	}
	return poller.Err()
}

// serve runs one session: a fresh workdir, the file tools confined to
// it, bash with a minimal environment, and the connector tools bound to
// this session's id so the proxy can budget and audit it.
func serve(ctx context.Context, client anthropic.Client, o Options, log *slog.Logger, work *anthropic.BetaSelfHostedWork) error {
	session := work.Data.ID
	dir := filepath.Join(o.Workdir, session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	proxy := &connectors.Client{Base: o.ProxyBase, Token: o.ProxyToken, Session: session}
	idle := o.MaxIdle
	w := environments.NewEnvironmentWorker(client, environments.EnvironmentWorkerOptions{
		EnvironmentID:  o.EnvironmentID,
		EnvironmentKey: o.EnvironmentKey,
		Workdir:        dir,
		MaxIdle:        &idle,
		Logger:         log,
		// Memory stores are not used: the only memory is in the repository.
		MemorySyncInterval: -1,
		ToolsFunc: func(env *agenttoolset.AgentToolContext) []anthropic.BetaTool {
			// Bash gets exactly these variables: no credentials of any kind.
			env.Env = map[string]string{
				"PATH":       os.Getenv("PATH"),
				"HOME":       dir,
				"GOFLAGS":    "-mod=mod",
				"GOMODCACHE": filepath.Join(dir, ".gomodcache"),
				"GOCACHE":    filepath.Join(dir, ".gocache"),
				"GOPROXY":    envOr("GOPROXY", "off"), // the sandbox has no route to the internet
				"GOSUMDB":    "off",
				"TERM":       "dumb",
			}
			tools := []anthropic.BetaTool{
				agenttoolset.BetaBashTool(env),
				agenttoolset.BetaReadTool(env),
				agenttoolset.BetaGlobTool(env),
				agenttoolset.BetaGrepTool(env),
			}
			conn, err := connectors.Tools(proxy, env.Workdir)
			if err != nil {
				log.Error("connector tools", "err", err)
				return tools
			}
			return append(tools, conn...)
		},
	})
	err := w.HandleItem(ctx, environments.HandleItemOptions{
		WorkID:         work.ID,
		EnvironmentID:  work.EnvironmentID,
		SessionID:      session,
		EnvironmentKey: o.EnvironmentKey,
		WorkSecret:     work.Secret,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("handle %s: %w", work.ID, err)
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
