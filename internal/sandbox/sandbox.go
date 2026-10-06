// Package sandbox is the worker: the process in the sandbox pod that
// claims a session's work item, serves every tool call the agents make,
// and exits. In the pod it holds one credential, the proxy's token; the
// platform credentials stay in the proxy, which forwards the worker's
// own calls. The shell runs in a second container that holds nothing.
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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/lib/environments"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/tools/agenttoolset"

	"github.com/subganapathy/paved-road-agent/internal/connectors"
	"github.com/subganapathy/paved-road-agent/internal/shell"
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
	// Shell is the sidecar that runs bash for the model, when the worker
	// runs in the sandbox pod; empty otherwise.
	Shell string
	// Confined asserts that this process runs inside the sandbox pod:
	// Run verifies it (no route anywhere but the proxy, not root, no
	// container runtime socket, read-only system) and refuses to start
	// otherwise. Only a confined worker serves a shell.
	Confined bool
	Logger   *slog.Logger
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
	if o.Confined {
		if err := VerifyConfinement(ctx, o.ProxyBase); err != nil {
			return fmt.Errorf("refusing to start: %w", err)
		}
		log.Info("confinement verified")
	} else if o.Shell != "" {
		return errors.New("a shell sidecar is served only by a confined worker")
	}

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
	bash := bashFor(o, session)
	if c, ok := bash.(interface{ Close() error }); ok {
		defer c.Close()
	}
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
				"PATH": os.Getenv("PATH"),
				"HOME": dir,
				"TERM": "dumb",
			}
			tools := []anthropic.BetaTool{
				bash,
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

// bashFor picks the model's shell: the sidecar in the pod, the local
// shell for a confined single-container worker, and outside the sandbox
// a tool that refuses and says why.
func bashFor(o Options, session string) anthropic.BetaTool {
	switch {
	case o.Shell != "":
		return &shell.Tool{Base: o.Shell, Session: session, HTTP: &http.Client{Timeout: 10 * time.Minute}}
	case o.Confined:
		return agenttoolset.BetaBashTool(&agenttoolset.AgentToolContext{Workdir: filepath.Join(o.Workdir, session), Env: map[string]string{"PATH": os.Getenv("PATH"), "HOME": filepath.Join(o.Workdir, session), "TERM": "dumb"}})
	}
	return shell.Unavailable{}
}

// VerifyConfinement proves the process is where a confined worker must
// be. Each check is something the model's shell could otherwise exploit;
// the proxy is the one address that must answer.
func VerifyConfinement(ctx context.Context, proxyBase string) error {
	if os.Geteuid() == 0 {
		return errors.New("running as root")
	}
	for _, sock := range []string{"/var/run/docker.sock", "/run/docker.sock", "/run/containerd/containerd.sock", "/var/run/crio/crio.sock"} {
		if _, err := os.Stat(sock); err == nil {
			return fmt.Errorf("container runtime socket present: %s", sock)
		}
	}
	if f, err := os.CreateTemp("/usr", "pra-*"); err == nil {
		f.Close()
		os.Remove(f.Name())
		return errors.New("/usr is writable: the root filesystem is not read-only")
	}
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		return errors.New("a service-account token is mounted")
	}
	dial := func(addr string) error {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		conn, err := (&net.Dialer{}).DialContext(c, "tcp", addr)
		if err == nil {
			conn.Close()
		}
		return err
	}
	// Routes that must not exist: the internet, public DNS, the cluster's
	// API server, the metadata service.
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:53", "169.254.169.254:80"} {
		if dial(addr) == nil {
			return fmt.Errorf("egress to %s succeeded: the network policy is not enforced", addr)
		}
	}
	if host := os.Getenv("KUBERNETES_SERVICE_HOST"); host != "" {
		if dial(net.JoinHostPort(host, firstNonEmpty(os.Getenv("KUBERNETES_SERVICE_PORT"), "443"))) == nil {
			return errors.New("egress to the cluster API server succeeded")
		}
	}
	if _, err := net.DefaultResolver.LookupHost(ctx, "api.anthropic.com"); err == nil {
		return errors.New("DNS resolution works: the sandbox should have no resolver")
	}
	// The one route that must exist.
	u, err := url.Parse(proxyBase)
	if err != nil || u.Host == "" {
		return fmt.Errorf("proxy base %q is not a URL", proxyBase)
	}
	host := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	if err := dial(host); err != nil {
		return fmt.Errorf("the proxy at %s is unreachable: %w", host, err)
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
