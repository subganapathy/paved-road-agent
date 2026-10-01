// change-agent reviews the impact of a change.
//
//	change-agent setup                      create or update the agents and the environment
//	change-agent classify --pr o/r#N        what the rules say, without the model
//	change-agent review   --pr o/r#N        a live assessment
//	change-agent review   --case evals/cases/x.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/subganapathy/paved-road-agent/internal/agents"
	"github.com/subganapathy/paved-road-agent/internal/change"
	"github.com/subganapathy/paved-road-agent/internal/classify"
	"github.com/subganapathy/paved-road-agent/internal/evalcase"
	"github.com/subganapathy/paved-road-agent/internal/findings"
	"github.com/subganapathy/paved-road-agent/internal/github"
	"github.com/subganapathy/paved-road-agent/internal/ledger"
	"github.com/subganapathy/paved-road-agent/internal/tools"
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
	case "classify":
		err = runClassify(ctx, os.Args[2:])
	case "review":
		err = runReview(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: change-agent setup|classify|review [flags]")
	os.Exit(2)
}

// lock records what setup created, so review can find it and a prompt
// change is a new version rather than a new agent.
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

// ---- setup ---------------------------------------------------------------

func runSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	lockPath := fs.String("lock", "agents.lock.json", "where to record the agent and environment ids")
	envName := fs.String("env", "paved-road-agent", "environment name")
	fs.Parse(args)

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return errors.New("ANTHROPIC_API_KEY is not set")
	}
	client := anthropic.NewClient()
	l, err := readLock(*lockPath)
	if err != nil {
		return err
	}

	// The environment: cloud, no network. The repository is mounted by the
	// platform and everything live comes through our custom tools, so the
	// sandbox has no reason to reach anything.
	if l.EnvironmentID == "" {
		env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
			Name:        *envName,
			Description: anthropic.String("Read-only change-impact review. No network: evidence comes through host-side tools."),
			Config: anthropic.BetaEnvironmentNewParamsConfigUnion{OfCloud: &anthropic.BetaCloudConfigParams{
				Networking: anthropic.BetaCloudConfigParamsNetworkingUnion{OfLimited: &anthropic.BetaLimitedNetworkParams{}},
			}},
		})
		if err != nil {
			return fmt.Errorf("create environment: %w", err)
		}
		l.EnvironmentID = env.ID
		fmt.Println("environment", env.ID)
	}

	// Tool definitions come from the same registry that serves them; the
	// backends are irrelevant here, only the schemas.
	reg, err := tools.Registry(tools.Env{})
	if err != nil {
		return err
	}
	upsert := func(d agents.Def, roster []string) error {
		custom, err := tools.Definitions(reg, d.Custom)
		if err != nil {
			return err
		}
		p := agents.Params(d, custom, roster)
		if cur, ok := l.Agents[d.Key]; ok {
			a, err := client.Beta.Agents.Update(ctx, cur.ID, updateFrom(p))
			if err != nil {
				return fmt.Errorf("update %s: %w", d.Key, err)
			}
			l.Agents[d.Key] = lockedAgent{ID: a.ID, Version: a.Version}
			fmt.Printf("%-22s %s v%d (updated)\n", d.Key, a.ID, a.Version)
			return nil
		}
		a, err := client.Beta.Agents.New(ctx, p)
		if err != nil {
			return fmt.Errorf("create %s: %w", d.Key, err)
		}
		l.Agents[d.Key] = lockedAgent{ID: a.ID, Version: a.Version}
		fmt.Printf("%-22s %s v%d\n", d.Key, a.ID, a.Version)
		return nil
	}
	var roster []string
	for _, s := range agents.Specialists {
		if err := upsert(s, nil); err != nil {
			return err
		}
		roster = append(roster, l.Agents[s.Key].ID)
	}
	if err := upsert(agents.Lead, roster); err != nil {
		return err
	}
	return writeLock(*lockPath, l)
}

// updateFrom carries a create request over to an update: same content,
// different union types in the SDK.
func updateFrom(p anthropic.BetaAgentNewParams) anthropic.BetaAgentUpdateParams {
	u := anthropic.BetaAgentUpdateParams{
		Name:        anthropic.String(p.Name),
		Description: p.Description,
		System:      p.System,
		Model:       p.Model,
		Multiagent:  p.Multiagent,
	}
	for _, t := range p.Tools {
		u.Tools = append(u.Tools, anthropic.BetaAgentUpdateParamsToolUnion{
			OfAgentToolset20260401: t.OfAgentToolset20260401,
			OfMCPToolset:           t.OfMCPToolset,
			OfCustom:               t.OfCustom,
		})
	}
	return u
}

// ---- loading a change ----------------------------------------------------

// loaded is a change plus where its repository can be mounted.
type loaded struct {
	Change      change.Change
	Title, Body string
	RepoURL     string // empty: nothing to mount
	SHA         string
}

func loadChange(ctx context.Context, caseFile, pr string) (*loaded, error) {
	switch {
	case caseFile != "" && pr != "":
		return nil, errors.New("give --case or --pr, not both")
	case caseFile != "":
		c, err := evalcase.Load(caseFile)
		if err != nil {
			return nil, err
		}
		return &loaded{Change: c.Change, Title: c.Name, Body: c.Why, RepoURL: c.Repo, SHA: c.SHA}, nil
	case pr != "":
		ref, err := github.ParsePR(pr)
		if err != nil {
			return nil, err
		}
		gh := &github.Client{Token: os.Getenv("GITHUB_TOKEN")}
		p, err := gh.Load(ctx, ref)
		if err != nil {
			return nil, err
		}
		return &loaded{Change: p.Change, Title: p.Title, Body: p.Body, RepoURL: p.RepoURL, SHA: p.HeadSHA}, nil
	}
	return nil, errors.New("give --case <file> or --pr owner/repo#N")
}

// ---- classify ------------------------------------------------------------

func runClassify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("classify", flag.ExitOnError)
	caseFile := fs.String("case", "", "an evals/cases file")
	pr := fs.String("pr", "", "owner/repo#N")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Parse(args)

	l, err := loadChange(ctx, *caseFile, *pr)
	if err != nil {
		return err
	}
	r := classify.Classify(l.Change)
	if *asJSON {
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Print(classificationText(l.Change, r))
	return nil
}

func classificationText(c change.Change, r classify.Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "change %s: %d file(s)\n", c.Ref, len(c.Files))
	if r.NoImpact {
		sb.WriteString("no impact: nothing that runs changed\n")
		return sb.String()
	}
	if len(r.Aspects) == 0 {
		sb.WriteString("aspects: none by rule\n")
	}
	for _, a := range r.Aspects {
		fmt.Fprintf(&sb, "aspect %s\n", a)
		for _, why := range r.Reasons {
			if why.Aspect == a {
				fmt.Fprintf(&sb, "  %s: %s\n", why.File, why.Rule)
			}
		}
	}
	for _, f := range r.Unclassified {
		fmt.Fprintf(&sb, "unclassified %s\n", f)
	}
	return sb.String()
}

// ---- review --------------------------------------------------------------

const mountPath = "/mnt/repo"

func runReview(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	caseFile := fs.String("case", "", "an evals/cases file")
	pr := fs.String("pr", "", "owner/repo#N")
	lockPath := fs.String("lock", "agents.lock.json", "from setup")
	ledgerPath := fs.String("ledger", ".paved-road/ledger.json", "the fact ledger")
	budget := fs.Float64("budget", 3, "most this assessment may spend, in USD")
	out := fs.String("out", "", "also write the report JSON here")
	dryRun := fs.Bool("dry-run", false, "print the brief the lead would receive and stop; no API calls")
	fs.Parse(args)

	l, err := loadChange(ctx, *caseFile, *pr)
	if err != nil {
		return err
	}
	cls := classify.Classify(l.Change)
	brief := briefText(l, cls)
	if *dryRun {
		fmt.Print(classificationText(l.Change, cls))
		fmt.Println("---- brief ----")
		fmt.Println(brief)
		return nil
	}
	if cls.NoImpact {
		rep := findings.Report{Change: l.Change.Ref, Verdict: findings.Info, Summary: "No impact: the rules found nothing that runs changed, so no session was started."}
		return emit(rep, *out)
	}

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return errors.New("ANTHROPIC_API_KEY is not set")
	}
	lk, err := readLock(*lockPath)
	if err != nil {
		return err
	}
	lead, ok := lk.Agents[agents.LeadKey]
	if !ok || lk.EnvironmentID == "" {
		return fmt.Errorf("%s has no lead agent or environment: run setup first", *lockPath)
	}
	store, err := ledger.Open(*ledgerPath)
	if err != nil {
		return err
	}
	reg, err := tools.Registry(tools.FromEnv(l.Change, store))
	if err != nil {
		return err
	}

	client := anthropic.NewClient()
	params := anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{OfBetaManagedAgentsAgents: &anthropic.BetaManagedAgentsAgentParams{
			ID: lead.ID, Version: anthropic.Int(lead.Version), Type: anthropic.BetaManagedAgentsAgentParamsTypeAgent,
		}},
		EnvironmentID: lk.EnvironmentID,
		Title:         anthropic.String("impact: " + l.Change.Ref),
		Budget: anthropic.BetaManagedAgentsBudgetLimitParam{
			Type:        anthropic.BetaManagedAgentsBudgetLimitTypeLimit,
			MaxListCost: anthropic.BetaMonetaryAmountParam{Amount: fmt.Sprintf("%d", int64(*budget*100+0.5)), Currency: anthropic.BetaCurrencyUSD},
		},
		Metadata: map[string]string{"change": l.Change.Ref},
		InitialEvents: []anthropic.BetaSessionNewParamsInitialEventUnion{{OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
			Type:    anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
			Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{OfText: &anthropic.BetaManagedAgentsTextBlockParam{Type: anthropic.BetaManagedAgentsTextBlockTypeText, Text: brief}}},
		}}},
	}
	if l.RepoURL != "" && l.SHA != "" {
		params.Resources = []anthropic.BetaSessionNewParamsResourceUnion{{OfGitHubRepository: &anthropic.BetaManagedAgentsGitHubRepositoryResourceParams{
			Type:               anthropic.BetaManagedAgentsGitHubRepositoryResourceParamsTypeGitHubRepository,
			URL:                l.RepoURL,
			MountPath:          anthropic.String(mountPath),
			Checkout:           anthropic.BetaManagedAgentsGitHubRepositoryResourceParamsCheckoutUnion{OfCommit: &anthropic.BetaManagedAgentsCommitCheckoutParam{Type: anthropic.BetaManagedAgentsCommitCheckoutTypeCommit, Sha: l.SHA}},
			AuthorizationToken: os.Getenv("GITHUB_TOKEN"),
		}}}
	}
	sess, err := client.Beta.Sessions.New(ctx, params)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	fmt.Fprintf(os.Stderr, "session %s (budget $%.2f)\n", sess.ID, *budget)

	// Serve tools until the lead's turn ends. A specialist's silence while
	// another is still working does not end the session, so the runner
	// stops only once the whole session has been idle this long.
	idle := 20 * time.Second
	runner := client.Beta.Sessions.Events.NewToolRunner(ctx, sess.ID, anthropic.SessionToolRunnerOptions{Tools: reg, MaxIdle: &idle})
	defer runner.Close()
	for call := range runner.All() {
		who := "lead"
		if call.CustomToolUse.SessionThreadID != "" {
			who = "thread " + short(call.CustomToolUse.SessionThreadID)
		}
		state := "ok"
		if call.IsError {
			state = "error"
		}
		if !call.Posted {
			state = "not posted"
		}
		fmt.Fprintf(os.Stderr, "  %-14s %-16s %s %s\n", who, call.Name, compact(call.CustomToolUse.Input), state)
	}
	if err := runner.Err(); err != nil && !errors.Is(err, anthropic.ErrIdleTimeout) && !errors.Is(err, anthropic.ErrSessionTerminated) {
		return fmt.Errorf("session runner: %w", err)
	}

	// The session may still be running if it went briefly quiet; wait for
	// the lead to finish before reading its last message.
	for {
		s, err := client.Beta.Sessions.Get(ctx, sess.ID, anthropic.BetaSessionGetParams{})
		if err != nil {
			return err
		}
		if s.Status == anthropic.BetaManagedAgentsSessionStatusIdle || s.Status == anthropic.BetaManagedAgentsSessionStatusTerminated {
			fmt.Fprintf(os.Stderr, "session %s: %d input, %d output tokens, %.0fs active\n", s.Status, s.Usage.InputTokens+s.Usage.CacheReadInputTokens, s.Usage.OutputTokens, s.Stats.ActiveSeconds)
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	text, err := lastLeadMessage(ctx, client, sess.ID)
	if err != nil {
		return err
	}
	rep, err := parseReport(text)
	if err != nil {
		fmt.Fprintln(os.Stderr, "the lead's final message was not a report; printing it as is:")
		fmt.Println(text)
		return err
	}
	rep.Change = l.Change.Ref
	return emit(*rep, *out)
}

// briefText is the lead's task: everything it needs that the system prompt
// cannot know in advance.
func briefText(l *loaded, r classify.Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Assess the impact of change %s.\n\n", l.Change.Ref)
	if l.Title != "" {
		fmt.Fprintf(&sb, "Title: %s\n", l.Title)
	}
	if b := strings.TrimSpace(l.Body); b != "" {
		fmt.Fprintf(&sb, "Description from the author:\n%s\n", indent(b))
	}
	sb.WriteString("\nFiles changed:\n")
	for _, f := range l.Change.Files {
		fmt.Fprintf(&sb, "- %s (%s)\n", f.Path, f.Status)
	}
	if l.RepoURL != "" {
		fmt.Fprintf(&sb, "\nThe repository is mounted read-only at %s, checked out at the change's head commit %s. The diff itself comes from change_diff.\n", mountPath, l.SHA)
	} else {
		sb.WriteString("\nNo repository is mounted for this change; change_diff is the whole of it.\n")
	}
	sb.WriteString("\nRule-based classification:\n")
	if len(r.Aspects) == 0 {
		sb.WriteString("- no aspect matched a rule\n")
	}
	for _, a := range r.Aspects {
		fmt.Fprintf(&sb, "- %s:", a)
		sep := " "
		for _, why := range r.Reasons {
			if why.Aspect == a {
				fmt.Fprintf(&sb, "%s%s (%s)", sep, why.File, why.Rule)
				sep = "; "
			}
		}
		sb.WriteString("\n")
	}
	if len(r.Unclassified) > 0 {
		sb.WriteString("Files no rule understood (read them and decide):\n")
		for _, f := range r.Unclassified {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
	}
	sb.WriteString("\nSpecialists you can delegate to:\n")
	sb.WriteString(agents.RosterText())
	sb.WriteString("\nFinish with the report JSON in one fenced block.")
	return sb.String()
}

// lastLeadMessage returns the text of the last agent.message on the main
// thread: the lead's synthesis.
func lastLeadMessage(ctx context.Context, client anthropic.Client, sessionID string) (string, error) {
	var last string
	pager := client.Beta.Sessions.Events.ListAutoPaging(ctx, sessionID, anthropic.BetaSessionEventListParams{
		Order: anthropic.BetaSessionEventListParamsOrderAsc,
		Limit: anthropic.Int(100),
	})
	for pager.Next() {
		ev := pager.Current()
		if ev.Type != "agent.message" || ev.SessionThreadID != "" {
			continue
		}
		var sb strings.Builder
		for _, c := range ev.AsAgentMessage().Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		if sb.Len() > 0 {
			last = sb.String()
		}
	}
	if err := pager.Err(); err != nil {
		return "", err
	}
	if last == "" {
		return "", errors.New("the lead sent no message")
	}
	return last, nil
}

var fence = regexp.MustCompile("(?s)```json\\s*(.*?)```")

func parseReport(text string) (*findings.Report, error) {
	body := text
	if m := fence.FindAllStringSubmatch(text, -1); len(m) > 0 {
		body = m[len(m)-1][1]
	}
	var rep findings.Report
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &rep); err != nil {
		// Maybe it is only the findings array.
		fs, perr := findings.Parse([]byte(body))
		if perr != nil {
			return nil, fmt.Errorf("parse report: %w", err)
		}
		rep.Findings = fs
	}
	var kept []findings.Finding
	for _, f := range rep.Findings {
		if err := f.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "dropping finding without evidence: %s\n", f.Claim)
			continue
		}
		kept = append(kept, f)
	}
	rep.Findings = kept
	if rep.Verdict == "" {
		rep.Verdict = findings.Verdict(kept)
	}
	return &rep, nil
}

func emit(rep findings.Report, out string) error {
	fmt.Printf("verdict: %s\n%s\n", strings.ToUpper(string(rep.Verdict)), rep.Summary)
	for _, f := range rep.Findings {
		fmt.Printf("\n[%s] %s (%s, confidence %.2f)\n  %s\n", f.Severity, f.Aspect, strings.Join(f.Studied, ", "), f.Confidence, f.Claim)
		for _, e := range f.Evidence {
			src := e.Source
			if e.Query != "" {
				src += " " + e.Query
			}
			fmt.Printf("  evidence (%s): %s = %s\n", e.Kind, src, oneLine(e.Value))
		}
		if f.Recommendation != "" {
			fmt.Printf("  recommend: %s\n", f.Recommendation)
		}
	}
	if out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		return os.WriteFile(out, append(b, '\n'), 0o644)
	}
	return nil
}

func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

func short(id string) string {
	if len(id) > 12 {
		return id[len(id)-8:]
	}
	return id
}

func compact(in map[string]any) string {
	if len(in) == 0 {
		return ""
	}
	b, _ := json.Marshal(in)
	return oneLine(string(b))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 100 {
		return s[:97] + "..."
	}
	return s
}
