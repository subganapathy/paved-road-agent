// change-agent reviews pull requests against the properties of a good
// change.
//
//	change-agent setup   --config proxy.yaml            create or update the environment and the agents
//	change-agent proxy   --config proxy.yaml            serve the connectors
//	change-agent worker  --config proxy.yaml --once     claim one session and serve its tools
//	change-agent review  --config proxy.yaml --pr org/repo#N
//	change-agent serve   --config proxy.yaml [--poll 60s]  proxy + controller + worker: reviews watched repos' PRs
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"go.yaml.in/yaml/v3"

	"github.com/subganapathy/paved-road-agent/internal/agents"
	"github.com/subganapathy/paved-road-agent/internal/connectors"
	"github.com/subganapathy/paved-road-agent/internal/controller"
	"github.com/subganapathy/paved-road-agent/internal/findings"
	"github.com/subganapathy/paved-road-agent/internal/github"
	"github.com/subganapathy/paved-road-agent/internal/identifiers"
	"github.com/subganapathy/paved-road-agent/internal/proxy"
	"github.com/subganapathy/paved-road-agent/internal/sandbox"
	"github.com/subganapathy/paved-road-agent/internal/session"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "setup":
		err = runSetup(ctx, os.Args[2:])
	case "proxy":
		err = runProxy(ctx, os.Args[2:])
	case "worker":
		err = runWorker(ctx, os.Args[2:])
	case "review":
		err = runReview(ctx, os.Args[2:])
	case "serve":
		err = runServe(ctx, os.Args[2:])
	case "trace":
		err = runTrace(ctx, os.Args[2:])
	case "cost":
		err = runCost(ctx, os.Args[2:])
	case "sessions":
		err = runSessions(ctx, os.Args[2:])
	case "report":
		err = runReport(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: change-agent setup|proxy|worker|review|serve|trace|cost|sessions|report [flags]")
	os.Exit(2)
}

// ---- shared ---------------------------------------------------------------

// Agent-side settings live next to the proxy's in the same file, under
// `agent:`, so one file configures a deployment.
type agentConfig struct {
	Agent struct {
		APIKey         string  `yaml:"api_key"`         // credential source for the Anthropic API
		WorkspaceID    string  `yaml:"workspace_id"`    // required unless the key is scoped to a workspace
		EnvironmentID  string  `yaml:"environment_id"`  // filled by setup
		EnvironmentKey string  `yaml:"environment_key"` // credential source for the self-hosted environment's key
		Proxy          string  `yaml:"proxy"`           // where the worker reaches the proxy
		Workdir        string  `yaml:"workdir"`
		BudgetUSD      float64 `yaml:"budget_usd"`
		LeadEffort     string  `yaml:"lead_effort"` // low | medium | high; default medium
	} `yaml:"agent"`
	// Watch is the controller's part: which repositories' PRs to review
	// when serving, and how to write back.
	Watch struct {
		Repos         []string `yaml:"repos"`
		WebhookSecret string   `yaml:"webhook_secret"` // credential source; empty = unsigned (poll mode only)
		CommitFiles   bool     `yaml:"commit_files"`   // may the controller commit .paved-agent/ to PR branches
		StateDir      string   `yaml:"state_dir"`
	} `yaml:"watch"`
}

func loadAgentConfig(path string) (agentConfig, error) {
	var a agentConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	return a, yamlUnmarshal(b, &a)
}

func anthropicClient(a agentConfig) (anthropic.Client, error) {
	key, err := proxy.Credential(a.Agent.APIKey)
	if err != nil {
		return anthropic.Client{}, fmt.Errorf("agent.api_key: %w", err)
	}
	if key == "" {
		return anthropic.Client{}, errors.New("agent.api_key is required (a credential source such as keychain:anthropic-managed-agent)")
	}
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if a.Agent.WorkspaceID != "" {
		// Managed Agents resources live in a workspace. A key that is not
		// scoped to one must say which on every request.
		opts = append(opts, option.WithHeader("anthropic-workspace-id", a.Agent.WorkspaceID))
	}
	return anthropic.NewClient(opts...), nil
}

// lock records what setup created.
type lock struct {
	EnvironmentID string                 `json:"environment_id"`
	Agents        map[string]lockedAgent `json:"agents"`
}

type lockedAgent struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

func readLock(path string) (lock, error) {
	l := lock{Agents: map[string]lockedAgent{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(b, &l)
}

func writeLock(path string, l lock) error {
	b, _ := json.MarshalIndent(l, "", "  ")
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ---- setup ----------------------------------------------------------------

func runSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	cfgPath := fs.String("config", "proxy.yaml", "configuration file")
	lockPath := fs.String("lock", "agents.lock.json", "where to record ids")
	envName := fs.String("env-name", "paved-road-agent", "environment name")
	fs.Parse(args)

	a, err := loadAgentConfig(*cfgPath)
	if err != nil {
		return err
	}
	client, err := anthropicClient(a)
	if err != nil {
		return err
	}
	l, err := readLock(*lockPath)
	if err != nil {
		return err
	}
	if l.EnvironmentID == "" {
		env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
			Name:        *envName,
			Description: anthropic.String("Self-hosted: the sandbox pod serves the tools; the proxy holds the credentials."),
			Config:      anthropic.BetaEnvironmentNewParamsConfigUnion{OfSelfHosted: &anthropic.BetaSelfHostedConfigParams{}},
		})
		if err != nil {
			return fmt.Errorf("create environment: %w", err)
		}
		l.EnvironmentID = env.ID
		fmt.Printf("environment %s (self-hosted)\n", env.ID)
		fmt.Printf("  → create an environment key for it in the Console and store it where agent.environment_key points (e.g. the Keychain item it names)\n")
		fmt.Printf("  → set agent.environment_id: %s in %s\n", env.ID, *cfgPath)
	}

	// Tool definitions from the same registry the worker serves.
	reg, err := connectors.Tools(&connectors.Client{Base: "http://proxy", Session: "setup"}, os.TempDir())
	if err != nil {
		return err
	}
	upsert := func(d agents.Def, roster []string) error {
		custom, err := connectors.Definitions(reg, d.Connectors)
		if err != nil {
			return err
		}
		p := agents.Params(d, custom, roster)
		if cur, ok := l.Agents[d.Key]; ok {
			ag, err := client.Beta.Agents.Update(ctx, cur.ID, agents.UpdateParams(p))
			if err != nil {
				return fmt.Errorf("update %s: %w", d.Key, err)
			}
			l.Agents[d.Key] = lockedAgent{ID: ag.ID, Version: ag.Version}
			fmt.Printf("%-14s %s v%d (updated)\n", d.Key, ag.ID, ag.Version)
			return nil
		}
		ag, err := client.Beta.Agents.New(ctx, p)
		if err != nil {
			return fmt.Errorf("create %s: %w", d.Key, err)
		}
		l.Agents[d.Key] = lockedAgent{ID: ag.ID, Version: ag.Version}
		fmt.Printf("%-14s %s v%d\n", d.Key, ag.ID, ag.Version)
		return nil
	}
	var roster []string
	for _, s := range agents.Specialists {
		if err := upsert(s, nil); err != nil {
			return err
		}
		roster = append(roster, l.Agents[s.Key].ID)
	}
	program, err := agents.LoadProgram()
	if err != nil {
		return err
	}
	// Two leads from the same program: the release tier on the strongest
	// model, and a dev tier on the smaller one for iterating cheaply.
	if err := upsert(agents.Lead(program, "", a.Agent.LeadEffort), roster); err != nil {
		return err
	}
	dev := agents.Lead(program, agents.SpecialistModel, "medium")
	dev.Key, dev.Name = agents.LeadKey+"-dev", "Impact lead (dev tier)"
	if err := upsert(dev, roster); err != nil {
		return err
	}
	if err := upsert(agents.Instantiator(program), nil); err != nil {
		return err
	}
	return writeLock(*lockPath, l)
}

// ---- proxy ----------------------------------------------------------------

func runProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	cfgPath := fs.String("config", "proxy.yaml", "configuration file")
	fs.Parse(args)
	cfg, err := proxy.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	srv, err := proxy.New(cfg, log)
	if err != nil {
		return err
	}
	if err := mountAgentAPI(srv, *cfgPath); err != nil {
		return err
	}
	hs := &http.Server{Addr: cfg.Listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	log.Info("proxy listening", "addr", cfg.Listen)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// mountAgentAPI gives the proxy the platform credential the sandbox
// worker no longer holds, when the configuration has one.
func mountAgentAPI(srv *proxy.Server, cfgPath string) error {
	a, err := loadAgentConfig(cfgPath)
	if err != nil {
		return err
	}
	if a.Agent.EnvironmentID == "" || a.Agent.EnvironmentKey == "" {
		return nil
	}
	key, err := proxy.Credential(a.Agent.EnvironmentKey)
	if err != nil {
		return fmt.Errorf("agent.environment_key: %w", err)
	}
	return srv.MountAgentAPI(proxy.AgentAPI{EnvironmentID: a.Agent.EnvironmentID, EnvironmentKey: key})
}

// ---- worker ---------------------------------------------------------------

func workerOptions(a agentConfig, cfg *proxy.Config, once bool) (sandbox.Options, error) {
	envKey, err := proxy.Credential(a.Agent.EnvironmentKey)
	if err != nil {
		return sandbox.Options{}, fmt.Errorf("agent.environment_key: %w", err)
	}
	proxyTok, err := proxy.Credential(cfg.Token)
	if err != nil {
		return sandbox.Options{}, err
	}
	workdir := a.Agent.Workdir
	if workdir == "" {
		workdir = filepath.Join(os.TempDir(), "paved-road-agent", "work")
	}
	base := a.Agent.Proxy
	if base == "" {
		base = "http://127.0.0.1" + cfg.Listen
	}
	return sandbox.Options{
		EnvironmentID:  a.Agent.EnvironmentID,
		EnvironmentKey: envKey,
		Workdir:        workdir,
		ProxyBase:      base,
		ProxyToken:     proxyTok,
		Once:           once,
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}, nil
}

func runWorker(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	cfgPath := fs.String("config", "proxy.yaml", "configuration file (laptop mode)")
	once := fs.Bool("once", false, "claim one session, serve it, exit")
	confined := fs.Bool("confined", false, "assert this process runs in the sandbox pod; verified at start, and the only mode that serves a shell")
	shellURL := fs.String("shell", "", "the shell sidecar, e.g. http://127.0.0.1:9471 (confined only)")
	proxyBase := fs.String("proxy", "", "pod mode: the proxy's base URL; the platform is reached through it and no configuration file is read. The proxy token comes from $PRA_PROXY_TOKEN")
	envID := fs.String("environment", "", "pod mode: the self-hosted environment id")
	workdir := fs.String("workdir", "", "pod mode: the working directory shared with the shell container")
	fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *proxyBase != "" {
		// Pod mode. The sandbox holds the proxy token and nothing else;
		// the platform credential is the proxy's to add.
		tok := os.Getenv("PRA_PROXY_TOKEN")
		if *envID == "" || *workdir == "" {
			return errors.New("pod mode needs --environment and --workdir")
		}
		client := anthropic.NewClient(
			option.WithBaseURL(strings.TrimSuffix(*proxyBase, "/")+proxy.AgentAPIPrefix+"/"),
			option.WithHeader("X-Proxy-Token", tok),
		)
		return sandbox.Run(ctx, client, sandbox.Options{
			EnvironmentID:  *envID,
			EnvironmentKey: proxy.Placeholder,
			Workdir:        *workdir,
			ProxyBase:      *proxyBase,
			ProxyToken:     tok,
			Once:           *once,
			Shell:          *shellURL,
			Confined:       *confined,
			Logger:         log,
		})
	}
	a, err := loadAgentConfig(*cfgPath)
	if err != nil {
		return err
	}
	cfg, err := proxy.Load(*cfgPath)
	if err != nil {
		return err
	}
	client, err := anthropicClient(a)
	if err != nil {
		return err
	}
	o, err := workerOptions(a, cfg, *once)
	if err != nil {
		return err
	}
	o.Confined, o.Shell = *confined, *shellURL
	return sandbox.Run(ctx, client, o)
}

// ---- review ---------------------------------------------------------------

func runReview(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	cfgPath := fs.String("config", "proxy.yaml", "configuration file")
	lockPath := fs.String("lock", "agents.lock.json", "from setup")
	pr := fs.String("pr", "", "org/repo#N")
	budget := fs.Float64("budget", 0, "most this review may spend, in USD (default agent.budget_usd or 3)")
	withWorker := fs.Bool("worker", true, "run a one-session worker in this process (the proxy must already be running)")
	dryRun := fs.Bool("dry-run", false, "print the brief and stop; no session")
	out := fs.String("out", "", "also write the report JSON here")
	tier := fs.String("tier", "staged", "staged (the strongest model writes the obligations, the smaller executes them), auto (triage on the smaller, escalate to the strongest), dev (smaller only) or release (strongest only)")
	commitProposals := fs.Bool("commit-proposals", false, "commit the report's .paved-agent/ proposals to the PR branch (linted first)")
	fs.Parse(args)

	a, err := loadAgentConfig(*cfgPath)
	if err != nil {
		return err
	}
	cfg, err := proxy.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Slots.SCM == nil {
		return errors.New("slots.scm is required to load a pull request")
	}
	ref, err := github.ParsePR(*pr)
	if err != nil {
		return err
	}
	if ref.Owner != cfg.Slots.SCM.Org {
		return fmt.Errorf("%s is outside the org %s", *pr, cfg.Slots.SCM.Org)
	}
	ghTok, err := proxy.Credential(cfg.Slots.SCM.Auth)
	if err != nil {
		return err
	}
	gh := &github.Client{Token: ghTok, Base: cfg.Slots.SCM.API}
	loaded, err := gh.Load(ctx, ref)
	if err != nil {
		return err
	}
	change := session.Change{Org: ref.Owner, Repo: ref.Repo, PR: loaded}
	if text, err := gh.Contents(ctx, ref.Owner, ref.Repo, identifiers.Path, loaded.HeadSHA); err == nil {
		f, perr := identifiers.Parse([]byte(text))
		if perr != nil {
			return perr
		}
		change.Identifiers, change.IdentifiersText = f, text
	}
	if change.Identifiers != nil {
		// Run the file's measure: queries now, through the proxy, so the lead
		// starts with numbers instead of spending turns on them.
		change.Measured = session.Measure(ctx, measureClient(a, cfg), change.Identifiers)
	}
	brief := session.Brief(change)
	if *dryRun {
		fmt.Println(brief)
		return nil
	}

	client, err := anthropicClient(a)
	if err != nil {
		return err
	}
	lk, err := readLock(*lockPath)
	if err != nil {
		return err
	}
	release, okR := lk.Agents[agents.LeadKey]
	dev, okD := lk.Agents[agents.LeadKey+"-dev"]
	inst, okI := lk.Agents[agents.InstantiatorKey]
	if !okR || !okD || !okI || a.Agent.EnvironmentID == "" {
		return fmt.Errorf("run setup first, and set agent.environment_id in %s", *cfgPath)
	}
	if *budget == 0 {
		*budget = a.Agent.BudgetUSD
	}
	if *budget == 0 {
		*budget = 3
	}

	if *withWorker {
		// Loop, not once: if the lease is lost mid-session, this worker
		// reclaims it after the TTL instead of leaving the review hanging.
		o, err := workerOptions(a, cfg, false)
		if err != nil {
			return err
		}
		go func() {
			if err := sandbox.Run(ctx, client, o); err != nil {
				fmt.Fprintln(os.Stderr, "worker:", err)
			}
		}()
	}

	opts := func(l lockedAgent, budget float64) session.Options {
		return session.Options{LeadID: l.ID, LeadVersion: l.Version, EnvironmentID: a.Agent.EnvironmentID,
			BudgetUSD: budget, Title: "impact: " + *pr, Metadata: map[string]string{"change": *pr}}
	}
	var rep *findings.Report
	switch *tier {
	case "staged":
		// The strongest model writes the obligations (short); the smaller
		// model does everything else. Budgets: a third, then the rest.
		st := session.Staged{Instantiate: opts(inst, *budget/2), Execute: opts(dev, *budget)}
		fmt.Fprintf(os.Stderr, "instantiate on %s (budget $%.2f), execute on %s (budget $%.2f)\n", inst.ID, *budget/2, dev.ID, *budget)
		out, err := st.Run(ctx, client, brief)
		if out != nil {
			for _, u := range out.Usage {
				fmt.Fprintln(os.Stderr, u)
			}
		}
		if err != nil {
			if out != nil && out.LastText != "" {
				fmt.Fprintln(os.Stderr, "the final message was not what was expected; printing it as is:")
				fmt.Println(out.LastText)
			}
			return err
		}
		rep = out.Report
	case "auto":
		// Triage spends at most a third of the budget; the deep tier gets the rest.
		t := session.Tiered{Triage: opts(dev, *budget/3), Deep: opts(release, *budget)}
		fmt.Fprintf(os.Stderr, "triage on %s (budget $%.2f), deep on %s if warranted (budget $%.2f)\n", dev.ID, *budget/3, release.ID, *budget)
		out, err := t.Run(ctx, client, brief)
		if out != nil {
			for _, u := range out.Usage {
				fmt.Fprintln(os.Stderr, u)
			}
			if out.Escalated {
				fmt.Fprintln(os.Stderr, "escalated to the deep tier")
			}
		}
		if err != nil {
			if out != nil && out.LastText != "" {
				fmt.Fprintln(os.Stderr, "the lead's final message was not a report; printing it as is:")
				fmt.Println(out.LastText)
			}
			return err
		}
		rep = out.Report
	default:
		l := release
		if *tier == "dev" {
			l = dev
		}
		s, err := session.Create(ctx, client, opts(l, *budget), brief)
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		fmt.Fprintf(os.Stderr, "session %s (budget $%.2f)\n", s.ID, *budget)
		var final *anthropic.BetaManagedAgentsSession
		var text string
		rep, final, text, err = session.Collect(ctx, client, s.ID)
		if final != nil {
			fmt.Fprintf(os.Stderr, "session %s: %d input (%d cached), %d output tokens, %.0fs active\n",
				final.Status, final.Usage.InputTokens, final.Usage.CacheReadInputTokens, final.Usage.OutputTokens, final.Stats.ActiveSeconds)
		}
		if err != nil {
			if text != "" {
				fmt.Fprintln(os.Stderr, "the lead's final message was not a report; printing it as is:")
				fmt.Println(text)
			}
			return err
		}
		if dropped := rep.Normalize(); len(dropped) > 0 {
			for _, f := range dropped {
				fmt.Fprintf(os.Stderr, "dropped a finding without evidence: %s\n", f.Claim)
			}
		}
	}
	rep.Change = *pr + "@" + loaded.HeadSHA
	fmt.Print(rep.Render())
	if *commitProposals && len(rep.Proposals) > 0 {
		// The same write path the controller uses: in-process, through the
		// proxy's scm slot, restricted to .paved-agent/ on the PR branch.
		srv, err := proxy.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if err != nil {
			return err
		}
		for _, p := range rep.Proposals {
			if p.Path == identifiers.Path {
				if _, lerr := identifiers.Parse([]byte(p.Content)); lerr != nil {
					fmt.Fprintf(os.Stderr, "not committing %s: it fails the lint: %v\n", p.Path, lerr)
					continue
				}
			}
			res, _, err := srv.SCM().CommitFile(ctx, proxy.CommitFileReq{Repo: ref.Repo, Branch: loaded.HeadRef, Path: p.Path, Content: p.Content, Message: "paved-agent: " + firstLine(p.Reason)})
			if err != nil {
				fmt.Fprintf(os.Stderr, "commit %s: %v\n", p.Path, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "committed %s to %s: %v\n", p.Path, loaded.HeadRef, res)
		}
	}
	if len(rep.Questions) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d question(s): edit %s in the PR branch as shown, push, and run again.\n", len(rep.Questions), identifiers.Path)
	}
	if *out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		return os.WriteFile(*out, append(b, '\n'), 0o644)
	}
	return nil
}

// ---- trace ----------------------------------------------------------------

func runTrace(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	cfgPath := fs.String("config", "proxy.yaml", "configuration file")
	id := fs.String("session", "", "session id")
	width := fs.Int("width", 300, "truncate each line to this many characters")
	fs.Parse(args)
	if *id == "" {
		return errors.New("--session is required")
	}
	a, err := loadAgentConfig(*cfgPath)
	if err != nil {
		return err
	}
	client, err := anthropicClient(a)
	if err != nil {
		return err
	}
	return session.Trace(ctx, client, *id, os.Stdout, *width)
}

// runCost attributes a session's tokens to the tool calls that caused
// each turn, so cost work rests on measurement.
func runCost(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cost", flag.ExitOnError)
	cfgPath := fs.String("config", "proxy.yaml", "configuration file")
	ids := fs.String("session", "", "session id, or several separated by commas (a staged review has two)")
	turns := fs.Bool("turns", false, "also print the turn-by-turn timeline")
	asJSON := fs.Bool("json", false, "print the analysis as JSON")
	fs.Parse(args)
	if *ids == "" {
		return errors.New("--session is required")
	}
	a, err := loadAgentConfig(*cfgPath)
	if err != nil {
		return err
	}
	client, err := anthropicClient(a)
	if err != nil {
		return err
	}
	for _, id := range strings.Split(*ids, ",") {
		c, err := session.Analyze(ctx, client, strings.TrimSpace(id), *turns)
		if err != nil {
			return err
		}
		if *asJSON {
			os.Stdout.Write(c.JSON())
			fmt.Println()
			continue
		}
		c.Render(os.Stdout)
	}
	return nil
}

// runSessions lists recent sessions with what each cost: the ledger.
func runSessions(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	cfgPath := fs.String("config", "proxy.yaml", "configuration file")
	lockPath := fs.String("lock", "agents.lock.json", "from setup, to name agents")
	limit := fs.Int("limit", 30, "how many")
	fs.Parse(args)
	a, err := loadAgentConfig(*cfgPath)
	if err != nil {
		return err
	}
	client, err := anthropicClient(a)
	if err != nil {
		return err
	}
	names := map[string]string{}
	if b, err := os.ReadFile(*lockPath); err == nil {
		var l lock
		if json.Unmarshal(b, &l) == nil {
			for key, ag := range l.Agents {
				names[ag.ID] = key
			}
		}
	}
	pager := client.Beta.Sessions.ListAutoPaging(ctx, anthropic.BetaSessionListParams{Order: anthropic.BetaSessionListParamsOrderDesc, Limit: anthropic.Int(int64(*limit))})
	var total int64
	n := 0
	fmt.Printf("%-30s %-16s %-14s %-10s %7s %8s %8s %8s  %s\n", "session", "created", "agent", "status", "cost", "turns-in", "cached", "out", "title")
	for pager.Next() && n < *limit {
		s := pager.Current()
		n++
		var cents int64
		fmt.Sscan(s.Usage.ListCost.Amount, &cents)
		total += cents
		agent := names[s.Agent.ID]
		if agent == "" {
			agent = s.Agent.ID
		}
		fmt.Printf("%-30s %-16s %-14s %-10s %7s %8s %8s %8s  %s\n", s.ID, s.CreatedAt.Local().Format("01-02 15:04"), clipStr(agent, 14), s.Status,
			fmt.Sprintf("$%.2f", float64(cents)/100), kTokens(s.Usage.InputTokens+s.Usage.CacheCreation.Ephemeral1hInputTokens+s.Usage.CacheCreation.Ephemeral5mInputTokens), kTokens(s.Usage.CacheReadInputTokens), kTokens(s.Usage.OutputTokens), clipStr(s.Title, 60))
	}
	if err := pager.Err(); err != nil {
		return err
	}
	fmt.Printf("%d sessions, $%.2f at list price\n", n, float64(total)/100)
	return nil
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func kTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// runReport re-collects a finished session's report without spending
// anything: the lead's last message that parses, with the instantiation
// from a staged review's first session attached.
func runReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	cfgPath := fs.String("config", "proxy.yaml", "configuration file")
	id := fs.String("session", "", "the executing session")
	instID := fs.String("instantiation", "", "the instantiating session of a staged review, to attach its obligations")
	out := fs.String("out", "", "also write the report JSON here")
	fs.Parse(args)
	if *id == "" {
		return errors.New("--session is required")
	}
	a, err := loadAgentConfig(*cfgPath)
	if err != nil {
		return err
	}
	client, err := anthropicClient(a)
	if err != nil {
		return err
	}
	rep, _, err := session.LastLeadReport(ctx, client, *id)
	if err != nil {
		return err
	}
	rep.Normalize()
	if *instID != "" {
		text, err := session.LastLeadMessage(ctx, client, *instID)
		if err != nil {
			return err
		}
		in, err := findings.ParseInstantiation(text)
		if err != nil {
			return fmt.Errorf("instantiation session: %w", err)
		}
		rep.Adopt(*in)
	}
	if *out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
	}
	fmt.Print(rep.Render())
	return nil
}

// ---- serve ----------------------------------------------------------------

// runServe is the deployment shape: the proxy, the controller (webhook at
// /github/webhook, or polling), and a worker loop, in one process.
func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "proxy.yaml", "configuration file")
	lockPath := fs.String("lock", "agents.lock.json", "from setup")
	poll := fs.Duration("poll", 0, "poll open PRs at this interval instead of relying on webhooks (laptops have no inbound URL)")
	noWorker := fs.Bool("no-worker", false, "do not run a worker loop in this process")
	fs.Parse(args)

	a, err := loadAgentConfig(*cfgPath)
	if err != nil {
		return err
	}
	cfg, err := proxy.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Slots.SCM == nil {
		return errors.New("slots.scm is required to serve")
	}
	client, err := anthropicClient(a)
	if err != nil {
		return err
	}
	lk, err := readLock(*lockPath)
	if err != nil {
		return err
	}
	lead, ok := lk.Agents[agents.LeadKey]
	if !ok || a.Agent.EnvironmentID == "" {
		return fmt.Errorf("run setup first, and set agent.environment_id in %s", *cfgPath)
	}
	devLead := lk.Agents[agents.LeadKey+"-dev"]
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	srv, err := proxy.New(cfg, log)
	if err != nil {
		return err
	}
	if err := mountAgentAPI(srv, *cfgPath); err != nil {
		return err
	}
	ghTok, err := proxy.Credential(cfg.Slots.SCM.Auth)
	if err != nil {
		return err
	}
	secret, err := proxy.Credential(a.Watch.WebhookSecret)
	if err != nil {
		return err
	}
	budget := a.Agent.BudgetUSD
	if budget == 0 {
		budget = 3
	}
	ctl, err := controller.New(controller.Config{
		Org: cfg.Slots.SCM.Org, Watch: a.Watch.Repos, WebhookSecret: secret, CommitFiles: a.Watch.CommitFiles, StateDir: a.Watch.StateDir,
		BudgetUSD: budget, LeadID: lead.ID, LeadVersion: lead.Version, DevLeadID: devLead.ID, DevVersion: devLead.Version, EnvironmentID: a.Agent.EnvironmentID,
	}, client, srv.SCM(), &github.Client{Token: ghTok, Base: cfg.Slots.SCM.API}, measureClient(a, cfg), log)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/github/webhook", ctl.Webhook())
	mux.Handle("/", srv)
	hs := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	if !*noWorker {
		o, err := workerOptions(a, cfg, false)
		if err != nil {
			return err
		}
		go func() {
			if err := sandbox.Run(ctx, client, o); err != nil {
				log.Error("worker", "err", err)
			}
		}()
	}
	if *poll > 0 {
		go ctl.Poll(ctx, *poll)
	}
	watch := "every repository in " + cfg.Slots.SCM.Org
	if len(a.Watch.Repos) > 0 {
		watch = strings.Join(a.Watch.Repos, ",")
	}
	log.Info("serving", "addr", cfg.Listen, "watch", watch, "poll", poll.String(), "webhook", "/github/webhook")
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// measureClient reaches the proxy for pre-measurement, as one synthetic session.
func measureClient(a agentConfig, cfg *proxy.Config) *connectors.Client {
	proxyTok, _ := proxy.Credential(cfg.Token)
	base := a.Agent.Proxy
	if base == "" {
		base = "http://" + cfg.Listen
	}
	return &connectors.Client{Base: base, Token: proxyTok, Session: "premeasure"}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:97] + "..."
	}
	return s
}

var _ = strings.TrimSpace

func yamlUnmarshal(b []byte, v any) error { return yaml.Unmarshal(b, v) }
