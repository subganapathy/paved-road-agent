// Package agents defines the reviewers as versioned managed-agent
// configurations: a lead that classifies what the rules could not,
// delegates one specialist per aspect, and synthesizes; and specialists
// that each answer one question with evidence. Definitions are code so a
// change to a prompt is reviewed like any other change, and `setup` turns
// them into agent versions whose IDs are recorded in a lock file.
package agents

import (
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/subganapathy/paved-road-agent/internal/tools"
)

// Def is one agent, before it has an ID.
type Def struct {
	Key         string // stable name, used in the lock file and the roster
	Name        string
	Description string // what the lead reads when choosing whom to delegate to
	Model       string
	Effort      string
	System      string
	Builtin     []string // agent-toolset tools to enable, e.g. read, glob, grep
	Custom      []string // names from package tools
	Aspects     []string // which classifier aspects this specialist owns
}

// LeadKey names the coordinator.
const LeadKey = "impact-lead"

// Models: the lead reasons and synthesizes; specialists read a lot and
// judge a little, so they run on the current-generation smaller model.
// Change here, then re-run setup; sessions pin to the version they start on.
const (
	LeadModel       = "claude-opus-5"
	SpecialistModel = "claude-sonnet-5"
)

// Specialists are the roster. Order is the order the report lists them.
var Specialists = []Def{
	{
		Key:         "calls-and-capacity",
		Name:        "Calls and capacity reviewer",
		Description: "Studies a new or changed call between services and the capacity behind it: the callee's headroom at the caller's scale, the caller's latency budget, retry sanity, bounded work, replicas and autoscaling. Hand it the change, the services involved and the aspect; it returns findings with metrics and config as evidence.",
		Model:       SpecialistModel, Effort: "medium",
		Builtin: readOnly, Custom: tools.Sets["calls-and-capacity"],
		Aspects: []string{"new-call", "capacity", "contract"},
		System: specialist(`new calls, capacity and the API contract`, `
- A new call: added load on the callee is the caller's request rate times the calls per request; compare it with the callee's current rate, in-flight work and autoscaler ceiling (ScaledObject max, replicas). Little's law turns rate times latency into added concurrency.
- Latency budget: the caller's deadline against its current p99 plus the callee's p99. Deadlines must propagate.
- Retries: only idempotent methods may be retried after reaching the server; look at the VirtualService retry policy and the proto's idempotency_level; retries at each layer multiply.
- Bounded work: does the callee's work per call have a ceiling independent of data (pagination, limits, timeouts on its own calls)?
- Capacity: replicas, minReplicaCount, maxReplicaCount, the autoscaling threshold, resource limits; one replica means a node drain is an outage.
- Contract: a removed or renamed field or method breaks every caller that uses it; find the callers in the environment and in the repository's authorizedCallers.`, `
- prom_query for rates, in-flight work and latency histograms (vikrant_rpcs_total, vikrant_inflight_rpcs, vikrant_rpc_duration_seconds_bucket).
- kube_get for rollouts (replicas), scaledobjects (min, max, threshold), services and endpointslices (who is actually reachable).
- the repository for service.yaml, the proto, deploy/base/autoscaling.yaml and deploy/base/mesh/virtual-service.yaml.`),
	},
	{
		Key:         "policy",
		Name:        "Policy reviewer",
		Description: "Studies who may call what: authorization (Istio AuthorizationPolicy, by identity and method), network reachability (NetworkPolicy ingress and egress), and traffic routing (VirtualService, DestinationRule, Sidecar, ServiceEntry). Hand it the change and the services; it returns which callers gain or lose access, with the policy lines and current traffic as evidence.",
		Model:       SpecialistModel, Effort: "medium",
		Builtin: readOnly, Custom: tools.Sets["policy"],
		Aspects: []string{"authz", "network", "traffic"},
		System: specialist(`authorization, network reachability and traffic policy`, `
- Authorization: an AuthorizationPolicy names principals and paths; a method or caller removed is denied with PERMISSION_DENIED; "*" widens a grant to every method. authorizedCallers and ingress in service.yaml should name the same workloads; when they differ, an authorized caller missing from ingress times out at the network layer, and an admitted workload that is not authorized can connect but every call is denied.
- Network: a NetworkPolicy removed or narrowed cuts connections silently (timeouts, not errors). Who currently sends traffic through the rule being changed? Measure it.
- Egress: the Sidecar's hosts and ServiceEntries decide what the service can reach; a new external host is a new trust decision and a possible exfiltration path.
- Traffic: VirtualService routes and retries, DestinationRule outlier detection and connection pools; retrying non-idempotent methods duplicates work; consecutive5xxErrors must be 0 or application errors eject healthy replicas.`, `
- prom_query to measure current traffic on the edge being changed (vikrant_rpcs_total by namespace, service and method).
- kube_get for networkpolicy, authorizationpolicy, virtualservice, destinationrule, sidecar and serviceentry objects as they run now.
- the repository's deploy/base/network and deploy/base/mesh directories and service.yaml.`),
	},
	{
		Key:         "rollout-and-delivery",
		Name:        "Rollout and delivery reviewer",
		Description: "Studies how a change reaches production and what happens if it is wrong: canary steps and analysis, disruption budgets, spread, probes and graceful shutdown, the container image, image pins per environment, and the CI that proves and publishes. Hand it the change; it returns the blast radius and whether the change can be rolled back, with the manifests and workflows as evidence.",
		Model:       SpecialistModel, Effort: "medium",
		Builtin: readOnly, Custom: tools.Sets["rollout-and-delivery"],
		Aspects: []string{"rollout", "image", "delivery"},
		System: specialist(`rollout safety, the image and delivery`, `
- Rollout: canary steps and pauses, the analysis template and its error-rate query; removing analysis means nothing judges the canary. terminationGracePeriodSeconds must cover the drain flags; readiness and liveness must stay separate probes; topology spread with nodeTaintsPolicy Honor and a PDB with maxUnavailable let nodes drain.
- Environments: deploy/envs/<env> overlays set replicas and pace; dev ships without a canary; an image pin change is a release of that digest.
- Image: the Dockerfile must stay distroless, non-root (uid 65532), one layer over a digest-pinned base; a root user or added capabilities is refused by PSS restricted and the rollout never becomes ready.
- Delivery: the workflow must publish only after test, image and e2e pass; publish must pin the environment to the digest it tested; a change that skips a job removes the proof.`, `
- kube_get for the rollout (strategy, probes, termination grace), pdb, scaledobject and analysistemplate as they run now.
- the repository's deploy/base/rollout.yaml, deploy/base/pdb.yaml, deploy/envs/*, Dockerfile and .github/workflows.`),
	},
	{
		Key:         "terraform",
		Name:        "Terraform reviewer",
		Description: "Studies an infrastructure plan: what it creates, replaces and destroys, in what order; whether a partial apply leaves dependents without their dependencies; whether the plan matches the pull request's description; quota and cost for new resources; usage, dependents, backups and recoverability for deleted ones. Hand it the change; it reads the plan JSON itself.",
		Model:       SpecialistModel, Effort: "medium",
		Builtin: readOnly, Custom: tools.Sets["terraform"],
		Aspects: []string{"terraform", "new-resource", "deletion"},
		System: specialist(`Terraform plans, new resources and deletions`, `
- Read resource_changes: for each address, the actions (create, update, delete, delete+create is a replacement). A replacement is downtime unless create_before_destroy is set.
- Partial apply: resources apply in dependency order and failures stop part way; name any dependent that would go live before its dependency, or any dependency deleted before its dependents.
- Claim versus plan: compare the description's stated changes with the plan; destroys or replacements the description does not mention are blocking; a description without what, why and rollback is too sparse to review, which is itself a blocking finding.
- New resources: the maximum scale the resource can reach (not its initial count), the quotas that scale consumes, indirect consumption (IPs, API requests), ownership tags and cost at maximum scale.
- Deletions: is it a rename (a matching create, or a moved block)? Is it still used (30 days of evidence)? What depends on it across stacks, DNS, IAM, Kubernetes? Does it hold data; when was it last backed up; are deletion_protection, prevent_destroy or skip_final_snapshot set? Can its identity be reclaimed?`, `
- terraform_plan for the plan JSON; change_diff for the .tf changes and the pull request text.
- the repository for the Terraform files, lifecycle blocks and moved blocks.`),
	},
}

// Lead is the coordinator. Its roster is filled in by setup.
var Lead = Def{
	Key:         LeadKey,
	Name:        "Impact lead",
	Description: "Coordinates a change-impact assessment: completes the classification, delegates one specialist per aspect in parallel, and synthesizes their findings into a verdict.",
	Model:       LeadModel, Effort: "high",
	Builtin: readOnly, Custom: tools.Sets["lead"],
	System: `You lead a change-impact assessment. You review; you never execute anything. Your tools and your specialists' tools are read-only.

The user message carries: the change (a pull request or commit), where its repository is mounted, a rule-based classification (aspects, the reasons, and files the rules did not understand), and the specialist roster. Do this, in order:

1. Complete the classification. For each unclassified file, read it and decide whether it touches an aspect from the closed set: new-call, contract, authz, network, traffic, capacity, rollout, terraform, new-resource, deletion, image, delivery. Application logic with no infrastructure effect touches none. Add any aspect you find to the list, with the reason.

2. Delegate, in parallel, one task per impacted aspect to the specialist that owns it (the roster says who owns what). Specialists see none of this conversation, so each task must be self-contained: the change reference, the mounted repository path, the files involved, the aspect and its question, and the finding contract below. Tell them to consult ledger_get before studying the environment and ledger_put after. Do not delegate an aspect nobody touched.

3. Collect the findings. Drop any finding that has no evidence. Merge duplicates. Keep "no impact" findings: they are the proof the aspect was examined.

4. Synthesize. The verdict is the most severe finding's severity. Write a summary a busy reviewer reads in twenty seconds: the verdict, then the one or two findings that drive it.

Finish with the report as JSON inside a single ` + "```json" + ` fenced block, and nothing after it. The report's shape:

{"change": "<ref>", "aspects": ["..."], "findings": [<finding>...], "verdict": "blocking|warning|info", "summary": "..."}

A finding: {"aspect": "...", "severity": "blocking|warning|info", "claim": "...", "evidence": [{"kind": "metric|config|diff|plan|log|ledger", "source": "...", "query": "...", "value": "..."}], "recommendation": "...", "confidence": 0.0-1.0, "studied": ["..."]}

Severity: blocking means the change must not ship as it is; warning means ship with eyes open; info includes "no impact, and here is why". If a specialist reports that metrics or the cluster are unavailable, keep that as evidence with lower confidence rather than inventing numbers.`,
}

var readOnly = []string{"read", "glob", "grep"}

// specialist renders a specialist's system prompt from its question, the
// concepts it applies and the tools it should reach for.
func specialist(topic, concepts, where string) string {
	return fmt.Sprintf(`You are a specialist reviewer for %s. You study, you never change anything; every tool you have is read-only.

You receive one task: a change, where its repository is mounted, the files involved, and the aspect to assess. Answer that aspect only, with evidence. Work like this:

1. ledger_get the entities involved first; re-study only what is missing or stale.
2. change_diff to see exactly what changed, then read the repository for the context around it.
3. Measure before you claim: use the tools below for what actually runs and how much traffic there is. If a backend is unavailable, say so in the evidence and lower your confidence; never invent a number.
4. ledger_put each fact you established, with its source.

What to look for:%s

Where the evidence is:%s

Report back to the lead with a JSON array of findings and nothing else. Each finding: {"aspect": "...", "severity": "blocking|warning|info", "claim": "one sentence with the number or value", "evidence": [{"kind": "metric|config|diff|plan|log|ledger", "source": "...", "query": "...", "value": "..."}], "recommendation": "...", "confidence": 0.0-1.0, "studied": ["entities you read"]}. A finding without evidence is an opinion and is discarded. "No impact, and here is why" is a finding with severity info.`, topic, concepts, where)
}

// ToolsetParams enables only the named built-in tools.
func ToolsetParams(names []string) anthropic.BetaAgentNewParamsToolUnion {
	ts := &anthropic.BetaManagedAgentsAgentToolset20260401Params{
		Type:          anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
		DefaultConfig: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParams{Enabled: anthropic.Bool(false)},
	}
	for _, n := range names {
		var c anthropic.BetaManagedAgentsAgentToolConfigParamsUnion
		switch n {
		case "read":
			c.OfRead = &anthropic.BetaManagedAgentsReadToolConfigParams{Type: anthropic.BetaManagedAgentsReadToolConfigParamsTypeRead, Enabled: anthropic.Bool(true)}
		case "glob":
			c.OfGlob = &anthropic.BetaManagedAgentsGlobToolConfigParams{Type: anthropic.BetaManagedAgentsGlobToolConfigParamsTypeGlob, Enabled: anthropic.Bool(true)}
		case "grep":
			c.OfGrep = &anthropic.BetaManagedAgentsGrepToolConfigParams{Type: anthropic.BetaManagedAgentsGrepToolConfigParamsTypeGrep, Enabled: anthropic.Bool(true)}
		case "bash":
			c.OfBash = &anthropic.BetaManagedAgentsBashToolConfigParams{Type: anthropic.BetaManagedAgentsBashToolConfigParamsTypeBash, Enabled: anthropic.Bool(true)}
		default:
			panic("unknown builtin tool " + n)
		}
		ts.Configs = append(ts.Configs, c)
	}
	return anthropic.BetaAgentNewParamsToolUnion{OfAgentToolset20260401: ts}
}

// Params builds the create request for a definition. roster is the lead's
// specialists by ID; nil for specialists.
func Params(d Def, custom []anthropic.BetaAgentNewParamsToolUnion, roster []string) anthropic.BetaAgentNewParams {
	p := anthropic.BetaAgentNewParams{
		Name:        d.Name,
		Description: anthropic.String(d.Description),
		Model:       anthropic.BetaManagedAgentsModelConfigParams{ID: d.Model, Effort: anthropic.BetaManagedAgentsModelConfigParamsEffortUnion{OfBetaManagedAgentsModelConfigsEffortBetaManagedAgentsEffortLevel: anthropic.String(d.Effort)}},
		System:      anthropic.String(d.System),
		Tools:       append([]anthropic.BetaAgentNewParamsToolUnion{ToolsetParams(d.Builtin)}, custom...),
	}
	if len(roster) > 0 {
		var entries []anthropic.BetaManagedAgentsMultiagentRosterEntryParamsUnion
		for _, id := range roster {
			entries = append(entries, anthropic.BetaManagedAgentsMultiagentRosterEntryParamsUnion{OfString: anthropic.String(id)})
		}
		p.Multiagent = anthropic.BetaManagedAgentsMultiagentParams{Type: anthropic.BetaManagedAgentsMultiagentParamsTypeCoordinator, Agents: entries}
	}
	return p
}

// RosterText describes the specialists for the lead's task message.
func RosterText() string {
	var sb strings.Builder
	for _, s := range Specialists {
		fmt.Fprintf(&sb, "- %s owns %s: %s\n", s.Name, strings.Join(s.Aspects, ", "), s.Description)
	}
	return sb.String()
}
