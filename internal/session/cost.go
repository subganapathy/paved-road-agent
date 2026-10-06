package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// Cost is where a session's tokens went. Every model request is one
// turn; its tokens are attributed to the tool calls whose results the
// model was reading when it made the request (the "cause"). A turn with
// no preceding tool call was caused by a message: the brief, a nudge, or
// a specialist's reply.
//
// The quantities that matter for the bill:
//
//   - new input (input + cache_creation): tokens entering the context for
//     the first time — tool results, mostly. Paid once at full (or 1.25×)
//     price, then re-read every turn.
//   - cache read: the whole context re-read on each turn at 0.1×. Grows
//     with turns × context size; this is the quantity that polling and
//     over-verification inflate.
//   - output: the model's text, tool inputs and thinking.
type Cost struct {
	Session       string
	ListCostCents int64
	ActiveSeconds float64
	Threads       []*ThreadCost
	// Snapshot is the server's own cumulative count from the last
	// session.usage event; it should agree with the sum over threads.
	Snapshot Tokens
	// Requests is the turn-by-turn timeline when kept.
	Requests []RequestCost
}

// ThreadCost is one agent's share. The lead is the thread with no id.
type ThreadCost struct {
	ID, Agent string
	Requests  int
	Usage     Tokens
	ByCause   map[string]*CauseCost
	// Results is bytes of tool results by tool: what the tools put into
	// the context.
	Results map[string]*ResultSize
	Idle    map[string]int // stop reasons seen
}

// CauseCost is the turns attributed to one cause and their tokens.
type CauseCost struct {
	Requests int
	Usage    Tokens
}

// ResultSize is tool-result volume for one tool.
type ResultSize struct {
	Calls int
	Bytes int
}

// Tokens is the usage breakdown.
type Tokens struct {
	Input, CacheCreation, CacheRead, Output int64
}

func (t *Tokens) add(u anthropic.BetaManagedAgentsSpanModelUsage) {
	t.Input += u.InputTokens
	t.CacheCreation += u.CacheCreationInputTokens
	t.CacheRead += u.CacheReadInputTokens
	t.Output += u.OutputTokens
}

// New is the tokens that entered the context for the first time.
func (t Tokens) New() int64 { return t.Input + t.CacheCreation }

// RequestCost is one turn.
type RequestCost struct {
	Thread string
	At     time.Time
	Cause  string
	Usage  Tokens
}

// Analyze walks a session's events and attributes every model request.
// The session's own stream carries the lead's turns; each specialist
// thread has its own stream, fetched separately, because span events
// carry no thread id.
func Analyze(ctx context.Context, client anthropic.Client, id string, keepRequests bool) (*Cost, error) {
	c := &Cost{Session: id}
	lead := &ThreadCost{Agent: "lead", ByCause: map[string]*CauseCost{}, Results: map[string]*ResultSize{}, Idle: map[string]int{}}
	c.Threads = append(c.Threads, lead)

	var threads []*ThreadCost
	pager := client.Beta.Sessions.Events.ListAutoPaging(ctx, id, anthropic.BetaSessionEventListParams{
		Order: anthropic.BetaSessionEventListParamsOrderAsc,
		Limit: anthropic.Int(100),
	})
	w := newWalker(lead, c, keepRequests)
	for pager.Next() {
		ev := pager.Current()
		switch ev.Type {
		case "session.thread_created":
			tc := ev.AsSessionThreadCreated()
			threads = append(threads, &ThreadCost{ID: tc.SessionThreadID, Agent: tc.AgentName, ByCause: map[string]*CauseCost{}, Results: map[string]*ResultSize{}, Idle: map[string]int{}})
			continue
		case "session.status_idle":
			lead.Idle[ev.AsSessionStatusIdle().StopReason.Type]++
			continue
		case "session.usage":
			u := ev.AsSessionUsage().Usage
			c.ActiveSeconds = u.ActiveSeconds
			var cents int64
			fmt.Sscan(u.ListCost.Amount, &cents)
			c.ListCostCents = cents
			c.Snapshot = Tokens{Input: u.InputTokens, CacheCreation: u.CacheCreation.Ephemeral1hInputTokens + u.CacheCreation.Ephemeral5mInputTokens, CacheRead: u.CacheReadInputTokens, Output: u.OutputTokens}
			continue
		}
		if ev.SessionThreadID != "" {
			continue // a specialist's event; counted from its own stream
		}
		w.step(ev)
	}
	if err := pager.Err(); err != nil {
		return nil, err
	}
	for _, t := range threads {
		c.Threads = append(c.Threads, t)
		tw := newWalker(t, c, keepRequests)
		var events []anthropic.BetaManagedAgentsSessionEventUnion
		tp := client.Beta.Sessions.Threads.Events.ListAutoPaging(ctx, t.ID, anthropic.BetaSessionThreadEventListParams{SessionID: id, Limit: anthropic.Int(100)})
		for tp.Next() {
			events = append(events, tp.Current())
		}
		if err := tp.Err(); err != nil {
			return nil, fmt.Errorf("thread %s: %w", t.ID, err)
		}
		sort.SliceStable(events, func(i, j int) bool { return events[i].ProcessedAt.Before(events[j].ProcessedAt) })
		for _, ev := range events {
			if ev.Type == "session.thread_status_idle" {
				t.Idle[ev.StopReason.Type]++
				continue
			}
			tw.step(ev)
		}
	}
	if keepRequests {
		sort.SliceStable(c.Requests, func(i, j int) bool { return c.Requests[i].At.Before(c.Requests[j].At) })
	}
	return c, nil
}

// walker attributes one stream's model requests to their causes.
type walker struct {
	t            *ThreadCost
	c            *Cost
	keepRequests bool
	pending      []string          // tool calls since the last request ended: the next request's cause
	cause        string            // the cause when no tool call intervened
	seenMessage  bool              // the first user message is the brief; later ones are nudges
	resultTool   map[string]string // tool_use id → tool name, for sizing results
}

func newWalker(t *ThreadCost, c *Cost, keep bool) *walker {
	return &walker{t: t, c: c, keepRequests: keep, resultTool: map[string]string{}}
}

func (w *walker) step(ev anthropic.BetaManagedAgentsSessionEventUnion) {
	t := w.t
	switch ev.Type {
	case "user.message":
		if w.seenMessage {
			w.cause = "nudge"
		} else {
			w.cause = "brief"
			w.seenMessage = true
		}
	case "agent.thread_message_received":
		w.cause = "reply from " + ev.FromAgentName
	case "agent.tool_use":
		tu := ev.AsAgentToolUse()
		w.pending = append(w.pending, tu.Name)
		w.resultTool[tu.ID] = tu.Name
	case "agent.custom_tool_use":
		tu := ev.AsAgentCustomToolUse()
		w.pending = append(w.pending, tu.Name)
		w.resultTool[tu.ID] = tu.Name
	case "user.tool_result":
		tr := ev.AsUserToolResult()
		n := 0
		for _, part := range tr.Content {
			n += len(part.Text)
		}
		t.result(w.resultTool[tr.ToolUseID], n)
	case "user.custom_tool_result":
		tr := ev.AsUserCustomToolResult()
		n := 0
		for _, part := range tr.Content {
			n += len(part.Text)
		}
		t.result(w.resultTool[tr.CustomToolUseID], n)
	case "span.model_request_end":
		end := ev.AsSpanModelRequestEnd()
		why := w.cause
		if len(w.pending) > 0 {
			why = "after " + joinCauses(w.pending)
		}
		if why == "" {
			why = "brief"
		}
		w.pending = nil
		w.cause = "continuation"
		t.Requests++
		t.Usage.add(end.ModelUsage)
		cc := t.ByCause[why]
		if cc == nil {
			cc = &CauseCost{}
			t.ByCause[why] = cc
		}
		cc.Requests++
		cc.Usage.add(end.ModelUsage)
		if w.keepRequests {
			var u Tokens
			u.add(end.ModelUsage)
			w.c.Requests = append(w.c.Requests, RequestCost{Thread: t.Agent, At: end.ProcessedAt, Cause: why, Usage: u})
		}
	}
}

func (t *ThreadCost) result(name string, n int) {
	if name == "" {
		name = "?"
	}
	r := t.Results[name]
	if r == nil {
		r = &ResultSize{}
		t.Results[name] = r
	}
	r.Calls++
	r.Bytes += n
}

// joinCauses names a turn's cause by the tools called before it: one
// tool is named; several are "tool×n, other".
func joinCauses(names []string) string {
	count := map[string]int{}
	var order []string
	for _, n := range names {
		if count[n] == 0 {
			order = append(order, n)
		}
		count[n]++
	}
	parts := make([]string, 0, len(order))
	for _, n := range order {
		if count[n] > 1 {
			parts = append(parts, fmt.Sprintf("%s×%d", n, count[n]))
		} else {
			parts = append(parts, n)
		}
	}
	return strings.Join(parts, ", ")
}

// Total sums every thread.
func (c *Cost) Total() (requests int, u Tokens) {
	for _, t := range c.Threads {
		requests += t.Requests
		u.Input += t.Usage.Input
		u.CacheCreation += t.Usage.CacheCreation
		u.CacheRead += t.Usage.CacheRead
		u.Output += t.Usage.Output
	}
	return
}

// Render prints the analysis.
func (c *Cost) Render(w io.Writer) {
	reqs, total := c.Total()
	fmt.Fprintf(w, "session %s\n", c.Session)
	if c.ListCostCents > 0 {
		fmt.Fprintf(w, "list cost $%.2f  active %.0fs  turns %d\n", float64(c.ListCostCents)/100, c.ActiveSeconds, reqs)
	} else {
		fmt.Fprintf(w, "turns %d\n", reqs)
	}
	fmt.Fprintf(w, "tokens: new input %s  cache read %s  output %s  (cache hit %.0f%%)\n",
		k(total.New()), k(total.CacheRead), k(total.Output), hit(total))
	if c.Snapshot != (Tokens{}) && c.Snapshot != total {
		fmt.Fprintf(w, "server snapshot: new input %s  cache read %s  output %s\n", k(c.Snapshot.New()), k(c.Snapshot.CacheRead), k(c.Snapshot.Output))
	}
	fmt.Fprintln(w)

	for _, t := range c.Threads {
		fmt.Fprintf(w, "== %s  turns %d  new %s  cache read %s  output %s  hit %.0f%%  idle %s\n",
			t.Agent, t.Requests, k(t.Usage.New()), k(t.Usage.CacheRead), k(t.Usage.Output), hit(t.Usage), idleText(t.Idle))
		causes := make([]string, 0, len(t.ByCause))
		for name := range t.ByCause {
			causes = append(causes, name)
		}
		sort.Slice(causes, func(i, j int) bool {
			a, b := t.ByCause[causes[i]], t.ByCause[causes[j]]
			return a.Usage.New()+a.Usage.CacheRead > b.Usage.New()+b.Usage.CacheRead
		})
		fmt.Fprintf(w, "   %-44s %5s %9s %10s %8s\n", "turns caused by", "turns", "new in", "cache read", "output")
		for _, name := range causes {
			cc := t.ByCause[name]
			fmt.Fprintf(w, "   %-44s %5d %9s %10s %8s\n", clipTo(name, 44), cc.Requests, k(cc.Usage.New()), k(cc.Usage.CacheRead), k(cc.Usage.Output))
		}
		if len(t.Results) > 0 {
			tools := make([]string, 0, len(t.Results))
			for name := range t.Results {
				tools = append(tools, name)
			}
			sort.Slice(tools, func(i, j int) bool { return t.Results[tools[i]].Bytes > t.Results[tools[j]].Bytes })
			fmt.Fprintf(w, "   %-44s %5s %9s\n", "tool results", "calls", "bytes")
			for _, name := range tools {
				r := t.Results[name]
				fmt.Fprintf(w, "   %-44s %5d %9s\n", name, r.Calls, k(int64(r.Bytes)))
			}
		}
		fmt.Fprintln(w)
	}
	if len(c.Requests) > 0 {
		fmt.Fprintf(w, "%-10s %-8s %9s %10s %8s  %s\n", "thread", "time", "new in", "cache read", "output", "cause")
		start := c.Requests[0].At
		for _, r := range c.Requests {
			fmt.Fprintf(w, "%-10s %-8s %9s %10s %8s  %s\n", clipTo(r.Thread, 10), fmt.Sprintf("+%ds", int(r.At.Sub(start).Seconds())), k(r.Usage.New()), k(r.Usage.CacheRead), k(r.Usage.Output), r.Cause)
		}
	}
}

// JSON is the analysis as data, for the runs directory.
func (c *Cost) JSON() []byte {
	b, _ := json.MarshalIndent(c, "", "  ")
	return b
}

func hit(u Tokens) float64 {
	den := u.Input + u.CacheCreation + u.CacheRead
	if den == 0 {
		return 0
	}
	return 100 * float64(u.CacheRead) / float64(den)
}

func idleText(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func k(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
