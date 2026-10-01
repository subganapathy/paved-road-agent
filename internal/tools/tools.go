// Package tools is what the agent may ask our side to do. Every tool is
// read-only and runs here, with our credentials, never in the sandbox: the
// agent sees results, not secrets. The same definitions register the tools
// on the agents and serve them in a session.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"github.com/subganapathy/paved-road-agent/internal/change"
	"github.com/subganapathy/paved-road-agent/internal/ledger"
)

// Env is what the tools need from the environment. Everything optional:
// a tool whose backend is not configured says so in its result, which is
// itself evidence ("metrics unavailable").
type Env struct {
	Change        change.Change
	Ledger        *ledger.Store
	PrometheusURL string // e.g. http://localhost:9090
	Kubeconfig    string // path; empty uses kubectl's default
	KubeContext   string
	HTTP          *http.Client
}

// Names of the tools, as the agents refer to them.
const (
	ChangeDiff    = "change_diff"
	TerraformPlan = "terraform_plan"
	PromQuery     = "prom_query"
	KubeGet       = "kube_get"
	LedgerGet     = "ledger_get"
	LedgerPut     = "ledger_put"
)

// Sets are which tools each kind of agent gets.
var Sets = map[string][]string{
	"lead":                 {ChangeDiff, TerraformPlan, LedgerGet, LedgerPut},
	"calls-and-capacity":   {ChangeDiff, PromQuery, KubeGet, LedgerGet, LedgerPut},
	"policy":               {ChangeDiff, PromQuery, KubeGet, LedgerGet, LedgerPut},
	"rollout-and-delivery": {ChangeDiff, KubeGet, LedgerGet, LedgerPut},
	"terraform":            {ChangeDiff, TerraformPlan, LedgerGet, LedgerPut},
}

type (
	emptyIn struct{}
	promIn  struct {
		Query string `json:"query" jsonschema:"required" jsonschema_description:"A PromQL instant query, e.g. sum(rate(vikrant_rpcs_total{namespace=\"echo\"}[5m]))"`
	}
	kubeIn struct {
		Kind      string `json:"kind" jsonschema:"required" jsonschema_description:"One of: rollout, service, networkpolicy, authorizationpolicy, virtualservice, destinationrule, sidecar, serviceentry, scaledobject, pdb, pod, namespace, endpointslice"`
		Namespace string `json:"namespace" jsonschema:"required"`
		Name      string `json:"name,omitempty" jsonschema_description:"Omit to list"`
	}
	ledgerGetIn struct {
		Entity string `json:"entity" jsonschema:"required" jsonschema_description:"A service name, cluster or resource address"`
		Aspect string `json:"aspect,omitempty" jsonschema_description:"Omit for every aspect"`
	}
	ledgerPutIn struct {
		Entity     string `json:"entity" jsonschema:"required"`
		Aspect     string `json:"aspect" jsonschema:"required"`
		Fact       string `json:"fact" jsonschema:"required" jsonschema_description:"One statement with its number or value"`
		Source     string `json:"source" jsonschema:"required" jsonschema_description:"The query, file or plan it came from"`
		ValidHours int    `json:"valid_hours,omitempty" jsonschema_description:"How long before it should be re-studied; 0 means until the entity changes"`
	}
)

var kubeKinds = []string{"rollout", "service", "networkpolicy", "authorizationpolicy", "virtualservice", "destinationrule", "sidecar", "serviceentry", "scaledobject", "pdb", "pod", "namespace", "endpointslice", "analysistemplate"}

// Registry builds every tool bound to env. The session runner serves them;
// Definitions turns them into agent configuration.
func Registry(env Env) ([]anthropic.BetaTool, error) {
	if env.HTTP == nil {
		env.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	text := func(s string) (anthropic.BetaToolResultBlockParamContentUnion, error) {
		return anthropic.BetaToolResultBlockParamContentUnion{OfText: &anthropic.BetaTextBlockParam{Text: s}}, nil
	}
	var reg []anthropic.BetaTool
	add := func(t anthropic.BetaTool, err error) error {
		if err != nil {
			return err
		}
		reg = append(reg, t)
		return nil
	}

	if err := add(toolrunner.NewBetaToolFromJSONSchema(ChangeDiff,
		"The change under review: every changed file with its unified diff, and both sides of small configuration files. Call it before reading the repository.",
		func(ctx context.Context, _ emptyIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			b, _ := json.MarshalIndent(trimmed(env.Change), "", " ")
			return text(string(b))
		})); err != nil {
		return nil, err
	}
	if err := add(toolrunner.NewBetaToolFromJSONSchema(TerraformPlan,
		"The Terraform plan for the change as `terraform show -json` prints it, or 'no plan' when the change has none.",
		func(ctx context.Context, _ emptyIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if len(env.Change.PlanJSON) == 0 {
				return text("no plan: the change carries no Terraform plan")
			}
			return text(string(env.Change.PlanJSON))
		})); err != nil {
		return nil, err
	}
	if err := add(toolrunner.NewBetaToolFromJSONSchema(PromQuery,
		"Run a PromQL instant query against the environment's Prometheus. Metrics the platform's services expose include vikrant_rpcs_total, vikrant_inflight_rpcs, vikrant_rpc_duration_seconds_bucket, labelled by namespace, service and method. Returns the raw result.",
		func(ctx context.Context, in promIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if env.PrometheusURL == "" {
				return text("metrics unavailable: no Prometheus is configured for this environment (PROMETHEUS_URL). Say so in your finding instead of guessing a number.")
			}
			u := strings.TrimSuffix(env.PrometheusURL, "/") + "/api/v1/query?query=" + url.QueryEscape(in.Query)
			req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
			resp, err := env.HTTP.Do(req)
			if err != nil {
				return text("metrics unavailable: " + err.Error())
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 200<<10))
			return text(fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, b))
		})); err != nil {
		return nil, err
	}
	if err := add(toolrunner.NewBetaToolFromJSONSchema(KubeGet,
		"Read one Kubernetes object, or list a kind in a namespace, as JSON. Read-only: get and list are the only verbs. Use it to see what actually runs, not what the repo says should run.",
		func(ctx context.Context, in kubeIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			kind := strings.ToLower(in.Kind)
			if !slices.Contains(kubeKinds, kind) {
				return text("refused: kind must be one of " + strings.Join(kubeKinds, ", "))
			}
			if in.Namespace == "" || strings.ContainsAny(in.Namespace+in.Name, " ;|&$`") {
				return text("refused: namespace is required and names must be plain")
			}
			args := []string{"get", kind, "-n", in.Namespace, "-o", "json"}
			if in.Name != "" {
				args = append(args, in.Name)
			}
			if env.Kubeconfig != "" {
				args = append(args, "--kubeconfig", env.Kubeconfig)
			}
			if env.KubeContext != "" {
				args = append(args, "--context", env.KubeContext)
			}
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			var out, errb bytes.Buffer
			cmd := exec.CommandContext(cctx, "kubectl", args...)
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Run(); err != nil {
				return text("cluster unavailable or object missing: " + strings.TrimSpace(errb.String()))
			}
			return text(clip(out.String(), 100<<10))
		})); err != nil {
		return nil, err
	}
	if err := add(toolrunner.NewBetaToolFromJSONSchema(LedgerGet,
		"What earlier assessments learned about an entity: facts with their source and validity. Consult it before studying the environment; re-study only what is missing or stale.",
		func(ctx context.Context, in ledgerGetIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if env.Ledger == nil {
				return text("no ledger configured")
			}
			facts, err := env.Ledger.Get(in.Entity, in.Aspect, time.Now())
			if err != nil {
				return text("ledger error: " + err.Error())
			}
			if len(facts) == 0 {
				return text("nothing recorded yet about " + in.Entity)
			}
			var sb strings.Builder
			for _, f := range facts {
				state := "current"
				if f.Stale(time.Now()) {
					state = "STALE, re-study"
				}
				fmt.Fprintf(&sb, "- [%s] %s: %s (source: %s; recorded %s)\n", f.Aspect, state, f.Fact, f.Source, f.RecordedAt.Format(time.RFC3339))
			}
			return text(sb.String())
		})); err != nil {
		return nil, err
	}
	if err := add(toolrunner.NewBetaToolFromJSONSchema(LedgerPut,
		"Record one fact you established about an entity, with its source, so the next assessment need not study it again.",
		func(ctx context.Context, in ledgerPutIn) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if env.Ledger == nil {
				return text("no ledger configured")
			}
			f := ledger.Fact{Entity: in.Entity, Aspect: in.Aspect, Fact: in.Fact, Source: in.Source, Change: env.Change.Ref}
			if in.ValidHours > 0 {
				f.ValidUntil = time.Now().Add(time.Duration(in.ValidHours) * time.Hour)
			}
			if err := env.Ledger.Put(f); err != nil {
				return text("ledger error: " + err.Error())
			}
			return text("recorded")
		})); err != nil {
		return nil, err
	}
	return reg, nil
}

// Definitions renders the named tools as agent configuration, from the same
// registry the session serves, so the two can't drift.
func Definitions(reg []anthropic.BetaTool, names []string) ([]anthropic.BetaAgentNewParamsToolUnion, error) {
	var out []anthropic.BetaAgentNewParamsToolUnion
	for _, n := range names {
		i := slices.IndexFunc(reg, func(t anthropic.BetaTool) bool { return t.Name() == n })
		if i < 0 {
			return nil, fmt.Errorf("no tool named %s", n)
		}
		schema := reg[i].InputSchema()
		props := map[string]any{}
		if schema.Properties != nil {
			b, _ := json.Marshal(schema.Properties)
			_ = json.Unmarshal(b, &props)
		}
		out = append(out, anthropic.BetaAgentNewParamsToolUnion{OfCustom: &anthropic.BetaManagedAgentsCustomToolParams{
			Name:        n,
			Description: reg[i].Description(),
			InputSchema: anthropic.BetaManagedAgentsCustomToolInputSchemaParam{Properties: props, Required: schema.Required},
			Type:        anthropic.BetaManagedAgentsCustomToolParamsTypeCustom,
		}})
	}
	return out, nil
}

// trimmed bounds what the diff tool returns so one huge file can't fill a
// specialist's context: the agent can read the mounted repository for the rest.
func trimmed(c change.Change) change.Change {
	out := c
	out.Files = nil
	out.PlanJSON = nil
	for _, f := range c.Files {
		f.Patch = clip(f.Patch, 12<<10)
		f.Before = clip(f.Before, 8<<10)
		f.After = clip(f.After, 8<<10)
		out.Files = append(out.Files, f)
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n… (%d more bytes; read the file in the repository for the rest)", len(s)-n)
}

// FromEnv reads the tool backends from the process environment.
func FromEnv(c change.Change, l *ledger.Store) Env {
	return Env{Change: c, Ledger: l, PrometheusURL: os.Getenv("PROMETHEUS_URL"), Kubeconfig: os.Getenv("KUBECONFIG"), KubeContext: os.Getenv("KUBE_CONTEXT")}
}
