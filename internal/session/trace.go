package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Trace prints a session's events in order, one line per event, enough
// to see what each agent did and said. Tool inputs and results are
// truncated; nothing is omitted.
func Trace(ctx context.Context, client anthropic.Client, id string, w io.Writer, width int) error {
	if width <= 0 {
		width = 300
	}
	clip := func(s string) string {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > width {
			return s[:width-1] + "…"
		}
		return s
	}
	pager := client.Beta.Sessions.Events.ListAutoPaging(ctx, id, anthropic.BetaSessionEventListParams{
		Order: anthropic.BetaSessionEventListParamsOrderAsc,
		Limit: anthropic.Int(100),
	})
	for pager.Next() {
		ev := pager.Current()
		who := "lead"
		if ev.SessionThreadID != "" {
			t := ev.SessionThreadID
			if len(t) > 8 {
				t = t[len(t)-8:]
			}
			who = "thread " + t
		}
		line := ""
		switch ev.Type {
		case "agent.message":
			var sb strings.Builder
			for _, c := range ev.AsAgentMessage().Content {
				sb.WriteString(c.Text)
			}
			line = "MESSAGE " + clip(sb.String())
		case "agent.thinking":
			line = "thinking"
		case "agent.tool_use":
			tu := ev.AsAgentToolUse()
			b, _ := json.Marshal(tu.Input)
			line = "TOOL " + tu.Name + " " + clip(string(b))
		case "agent.custom_tool_use":
			tu := ev.AsAgentCustomToolUse()
			b, _ := json.Marshal(tu.Input)
			line = "TOOL " + tu.Name + " " + clip(string(b))
		case "user.tool_result":
			tr := ev.AsUserToolResult()
			var sb strings.Builder
			for _, c := range tr.Content {
				sb.WriteString(c.Text)
			}
			line = "  result " + clip(sb.String())
		case "user.custom_tool_result":
			tr := ev.AsUserCustomToolResult()
			var sb strings.Builder
			for _, c := range tr.Content {
				sb.WriteString(c.Text)
			}
			line = "  result " + clip(sb.String())
		case "user.message":
			var sb strings.Builder
			for _, c := range ev.AsUserMessage().Content {
				sb.WriteString(c.Text)
			}
			line = "USER " + clip(sb.String())
		case "agent.thread_message_sent", "agent.thread_message_received", "session.thread_created":
			line = ev.Type
		case "session.status_idle":
			line = "IDLE " + ev.AsSessionStatusIdle().StopReason.Type
		case "session.error":
			line = "ERROR " + clip(ev.AsSessionError().Error.Message)
		default:
			line = ev.Type
		}
		fmt.Fprintf(w, "%-16s %s\n", who, line)
	}
	return pager.Err()
}
