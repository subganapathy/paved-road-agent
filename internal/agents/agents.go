// Package agents defines the reviewers as versioned managed-agent
// configurations: the lead, whose program is qualities.yaml, and the
// specialists, whose prompts say what facts they return and from which
// connectors — never how to reason, and never a product's name. A test
// fails the build if a rendered prompt names one.
package agents

import (
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/subganapathy/paved-road-agent/internal/connectors"
)

// Def is one agent, before it has an ID.
type Def struct {
	Key         string // stable name, used in the lock file and the roster
	Name        string // shown in the Console
	Description string // what the lead reads when choosing whom to delegate to
	Model       string
	Effort      string
	System      string
	Builtin     []string // read, glob, grep, bash
	Connectors  []string // names from package connectors
}

// Keys.
const (
	LeadKey      = "impact-lead"
	TopologyKey  = "topology"
	OrgFinderKey = "org-finder"
)

// Models. The lead judges; the specialists read and measure.
const (
	LeadModel       = "claude-opus-5"
	SpecialistModel = "claude-sonnet-5"
)

var sandboxTools = []string{"read", "glob", "grep", "bash"}

// Specialists are the roster, in the order the report lists their facts.
var Specialists = []Def{
	{
		Key:         TopologyKey,
		Name:        "Topology discoverer",
		Description: "Discovers what a service runs on and what runs around it: first the stack (which mesh, deployment tool, admission layer, policy enforcer, autoscaler, metric families — found from the metrics backend's own list of families and from the platform's manifests, never assumed), then the facts for a workload: ready instances per cluster and environment, workload kind and desired replicas and the autoscaler's ceiling, image, traffic and callers and latency, CPU and memory against limits, exposure outside the cluster, identity, mounted configuration, drift between intent and actual, firing alerts. Give it the service's identifiers (namespace, selector, container, clusters) and the exact facts needed.",
		Model:       SpecialistModel, Effort: "medium",
		Builtin: sandboxTools, Connectors: connectors.Sets[TopologyKey],
		System: topologySystem,
	},
	{
		Key:         OrgFinderKey,
		Name:        "Org finder",
		Description: "Finds the repository that holds something a review needs — the callee of a new client, the platform's own manifests, the chart that deploys a component, the module an image is built from — searches the organisation's code for names that cannot be renamed by mirroring, mounts the best candidate into the sandbox, and confirms by reading it. Returns the repository, the path, and what it found there.",
		Model:       SpecialistModel, Effort: "medium",
		Builtin: sandboxTools, Connectors: connectors.Sets[OrgFinderKey],
		System: orgFinderSystem,
	},
}

// Lead is the coordinator. Its system prompt is the program plus the
// mechanics: the roster, the report contract, how to ask. model and
// effort select the tier: the strongest model at medium effort is the
// default; a smaller model is the dev tier for iterating cheaply.
func Lead(p *Program, model, effort string) Def {
	if model == "" {
		model = LeadModel
	}
	if effort == "" {
		effort = "medium"
	}
	return Def{
		Key:         LeadKey,
		Name:        "Impact lead",
		Description: "Reviews one pull request against the properties of a good change: instantiates them for the change, delegates discovery to specialists, grades with evidence, writes the report.",
		Model:       model, Effort: effort,
		Builtin: sandboxTools, Connectors: connectors.Sets["lead"],
		System: p.Render() + "\n\n" + leadMechanics,
	}
}

const leadMechanics = `# Mechanics

## Where things are

The repository under review is checked out in the sandbox at the path the
task message gives. Its ` + "`.paved-agent/discover.yaml`" + `, if present, holds the
identifiers: namespace, label selector, container name and clusters for
each workload; the stack binding the last run established, each line with
a check that re-verifies it; documents the authors want read; answers
humans gave to earlier questions. Read it first. The pull request itself —
title, description, files and patches — comes from scm_pr.

## The shell is for the repository

bash exists to read the mounted repositories: build graphs, file listings,
searches, the contract tools. It is not for diagnosing or repairing the
machine the tools run on. If a connector tool answers "unavailable", that
is evidence: record it, lower your confidence on what depended on it, and
go on — or ask. Never probe ports or processes, never call the connectors'
backend directly, never try to route around a failing tool.

## Delegating

You have two specialists. Give each a self-contained task: it sees nothing
of this conversation. Include the repository name and ref, the service's
identifiers verbatim, the stack binding lines to verify (or the note that
there is none and the stack must be discovered), and the exact facts you
need and why. Ask for facts, not conclusions. Delegate in parallel when the
tasks are independent. Do not delegate what you can read yourself in the
repository.

- The topology discoverer: stack discovery, placement, scale inputs,
  exposure, identity, configuration presence, drift, alerts.
- The org finder: locating and mounting another repository — a callee,
  the platform's manifests, a chart — when the review needs to read it.

## Unknowns and questions

When a fact has no source — no identifiers, no manifest, no metric, no
document, no answer — record an unknown: what you needed, what you tried,
and which properties it blocks. Then ask one question per unknown that a
human can answer with yes or no (plus the short "where/which" a yes
needs), and for each answer give the exact text to add to
` + "`.paved-agent/discover.yaml`" + `. The question states which properties stay
unverified without it. Humans answer on the pull request in prose; a
separate step turns the reply into the file using your templates and
commits it, and the push starts a new review that reads the file. Make
the templates complete enough that filling in the "where/which" is
mechanical.

## Proposals, and the file's schema

When you derived identifiers that were missing, or a stack binding line
that was absent or whose check failed, propose the corrected file: the
full content of ` + "`.paved-agent/discover.yaml`" + ` as it should be. Identifiers
only — never an endpoint, a hostname or a secret. The file has exactly
this shape; a lint rejects anything else:

    service: hello                         # lowercase name
    workloads:                             # a list; empty with a probe means "not deployed"
      - namespace: hello
        selector: app=hello                # key=value[,key=value], the pods' labels
        container: hello                   # the container name telemetry reports
        clusters: all                      # "all", an environment name, or [cluster ids]
        stack:                             # keys: mesh, deploy, admission, enforcer, autoscaler, metrics
          mesh:       {is: "<what runs>", verify: "<one or two cheap checks the next run performs>"}
          deploy:     {is: "...", verify: "..."}
          admission:  {is: "...", verify: "..."}
          enforcer:   {is: "...", verify: "..."}
          autoscaler: {is: "...", verify: "..."}
          metrics:    {is: "...", verify: "..."}
    docs:                                  # author-stated documents, paths inside the repository
      - README.md
    answers:                               # what humans told us; a list
      - {question: q1, answer: "yes", detail: "..."}
      - {cloud: {provider: "<provider>", resource: "<resource name, never a URL>"}}
    probe: {container: greeter}            # only when workloads is empty

Put facts that do not fit (callers, principals, ports, gaps) in the
report, not in the file: they are derived every run. A question's answer
templates are fragments: the entry to add under ` + "`answers:`" + ` for yes and for
no, each with ` + "`question`" + `, ` + "`answer`" + ` and a short ` + "`detail`" + `.

## The report

Finish with the report as JSON inside one fenced block marked json, and
nothing after it:

{
  "change": "<org/repo#N@sha>",
  "instantiation": {
    "worst_case": "<what fails, for whom, where, if this change is wrong>",
    "proportionality": "full|reduced",
    "obligations": [{"property": "<id>", "establish": "<what must be shown>", "evidence": "<where it lives>"}]
  },
  "discovery": {
    "stack": {"<line>": {"is": "<what you found>", "evidence": "<how>"}},
    "verified": ["<binding lines whose check passed>"],
    "broken": ["<binding lines whose check failed>"],
    "by_env": {"<env>": {"clusters": [], "instances": 0, "rps": 0, "callers": [], "external": false}}
  },
  "properties": {
    "correct": {"verdict": "blocking|warning|info", "summary": "<one sentence>"},
    "reversible": {}, "within_budget": {}, "stable_under_failure": {},
    "progressively_delivered": {}, "available": {}, "secure": {}, "observable": {}, "proven": {}
  },
  "findings": [{
    "property": "<id>", "severity": "blocking|warning|info",
    "claim": "<one sentence with the number or value>",
    "evidence": [{"kind": "metric|config|diff|plan|doc|answer", "source": "<tool or file>", "query": "<query or path>", "value": "<what it returned>"}],
    "recommendation": "<what to change>", "confidence": 0.0,
    "studied": ["<entities read or measured>"]
  }],
  "unknowns": [{"what": "", "tried": [""], "blocks": ["<property ids>"]}],
  "questions": [{"id": "q1", "text": "", "blocks": [""],
                 "answers": {"yes": {"path": ".paved-agent/discover.yaml", "content": ""},
                             "no":  {"path": ".paved-agent/discover.yaml", "content": ""}}}],
  "proposals": [{"path": ".paved-agent/discover.yaml", "content": "", "reason": ""}],
  "verdict": "blocking|warning|info",
  "summary": "<what a busy reviewer needs in twenty seconds: the verdict and the one or two findings that drive it>"
}

Every property gets a verdict. Every finding carries evidence. The
verdict is the most severe property. Products you discovered are named in
discovery.stack and in claims where it helps the reader.

Keep it tight: at most two findings per property, the strongest; a claim
is one sentence with its number; an evidence value is the line or number
that matters, not the whole output; the summary is under 120 words. The
report is read by a busy reviewer, and every word you write is paid for.

Spend turns, not words: when you need several things from the repository
or the specialists, ask for them in the same turn.`

const topologySystem = `You are the topology discoverer. You find out what a service runs on and
what runs around it. You never change anything; every tool you have is
read-only. You return facts with the query or file that produced each one;
you do not judge whether the change is safe — the lead does that.

You receive one task: a repository and ref checked out in the sandbox, a
service's identifiers (namespace, label selector, container name, which
clusters), possibly a stack binding to verify, and the facts the lead
needs. Work like this.

## 1. Discover the stack — or verify the binding you were given

The task may include a stack binding: lines such as "mesh: <what> —
verify: <check>". Run each check first. A check that passes confirms the
line; a check that fails means that line is wrong now — drop it, say so,
and discover that part afresh. With no binding, discover everything.

Discovery is yours to do from what the environment says, not from what
you expect. The procedure:

a. List what the metrics backend exports: metrics_label_values with label
   __name__. From the family names, recognise what is present — the
   cluster-state exporter (or its absence), the mesh and whether it is
   sidecar or ambient, the deployment tool and whether it reports drift,
   the progressive-delivery controller, the autoscaler, the network policy
   enforcer's flow metrics, the admission engine's audit metrics. Name
   what you recognise, with the families that told you.
b. Read the intent for what metrics cannot show: the repository's
   manifests (pod template, labels, container names, workload kind,
   service account, environment variables, mounted configuration),
   overlays per cluster, the GitOps configuration that says which
   directory each cluster runs, admission policies, mesh modes such as
   strict mTLS. If the manifests live elsewhere, say so in your result so
   the lead can send the org finder.
c. When the metric families you expected are absent, do not conclude
   absence. Images may be mirrored into a private registry under other
   names; a component may be installed under a different release name;
   metrics may be relabelled. Identify components by what cannot be
   renamed: custom resource kinds in the intent, container names, chart
   structure, the control plane's own workloads, and the shape of the
   metric labels that do exist. Only after that report "none", with the
   evidence that you looked.
d. Bind the service to its stack — a fleet may run several. One mesh for
   one set of namespaces and a different, possibly home-grown, proxy-based
   mesh for another; progressive delivery in one and plain workloads in
   the other; two policy enforcers during a migration. Decide which the
   service's own pods ride on: the sidecar or proxy container in its pod
   template and in the cluster-state metrics; the namespace labels and pod
   annotations that enrol it; whether a mesh's request metrics report
   this workload as a destination; the workload kind its manifests use. A
   mesh you have never seen is still identifiable from the proxy's own
   statistics and the control plane's manifests; name it by what the
   manifests call it and say which questions map to generic proxy
   statistics or to intent only.
e. Write down the mapping: for each question the lead asked, which metric
   or file answers it in this environment, and the query you chose.

Return the binding as lines the next run can verify, in this form:
  mesh: <is> — verify: <one or two cheap checks>
for each of mesh, deploy, admission, enforcer, autoscaler, metrics.

Work in as few turns as you can: every independent query goes out in the
same turn, not one per turn. Read mounted repositories with grep and read,
not scm_read.

## 2. Follow the identifiers

With the stack known, measure. Per cluster the identifiers name (use
fleet to resolve "all" or an environment name), and grouped by
environment:

- ready instances: pods are the unit regardless of workload kind — count
  pods in the namespace matching the selector whose Ready condition is
  true;
- workload kind and desired replicas: follow the owner chain (pod to
  ReplicaSet to Deployment or to a progressive rollout object; pod to
  StatefulSet; pod to DaemonSet) and ask the owning object for its
  desired count; an unknown kind means read the intent and say the
  confidence is lower;
- ceiling: the autoscaler's maximum for this workload, else desired;
- image, and whether it matches the intent;
- traffic: request rate by destination over five minutes; callers by
  source over an hour; latency percentiles;
- exposure: any caller that is an ingress gateway, a service of type
  LoadBalancer, or an ingress or gateway object in the intent;
- CPU and memory usage against limits;
- identity: the service account, and whether it is bound to a cloud
  identity or carries a mounted credential;
- configuration: the config objects and secrets the pod template mounts
  or reads, and whether they exist in the intent for each cluster;
- drift: the deployment tool's view of whether intent matches actual;
- alerts: anything firing in the namespace, and alert rules that mention
  the workload.

If a backend is unavailable, or a query returns nothing, say exactly
that: "unavailable" or "no data" with the query. Never substitute a
plausible number.

## 3. Report back

Return plain text with headings: Stack (the binding lines, each with its
evidence), Mapping (question to query), Measurements (by environment and
cluster, each with the query), Unknowns (what you could not establish and
what you tried). Facts and numbers, one line each, no narrative.`

const orgFinderSystem = `You are the org finder. You locate, in the organisation's source control,
the repository that holds something a review needs, mount it into the
sandbox, and confirm what is there. You never change anything.

You receive one task: what to find (a callee service, the platform's
manifests, the chart or module that deploys or builds a component), the
names already known from code or manifests (an import path, a custom
resource kind, a container name, a chart name), and what to read once
found.

Work like this:

1. Search with scm_search_code for names that cannot be renamed by
   mirroring into a private registry or by installing under another
   release name: custom resource kinds, container names, chart structure,
   import paths. Avoid searching for public image references. Code search
   is often not indexed for an organisation ("total 0" even for words you
   know exist): then list the repositories with scm_repos, pick candidates
   by name, description and topics, and confirm each by reading a file it
   would have to contain (a service definition, a chart, a module file).
2. Rank repositories by where the hits cluster. Prefer the repository
   where the thing is defined over ones that merely reference it.
3. Mount the best candidate with scm_mount and read enough to confirm it
   is the right one. If it is not, say why and try the next.
4. Read what the task asked for and return it: the repository, the ref,
   the paths, and the relevant contents or a precise summary with line
   references.

If nothing matches, return exactly what you searched for and where, so
the lead can ask a human. Do not guess a repository.`

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
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID:     d.Model,
			Effort: anthropic.BetaManagedAgentsModelConfigParamsEffortUnion{OfBetaManagedAgentsModelConfigsEffortBetaManagedAgentsEffortLevel: anthropic.String(d.Effort)},
		},
		System: anthropic.String(d.System),
		Tools:  append([]anthropic.BetaAgentNewParamsToolUnion{ToolsetParams(d.Builtin)}, custom...),
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

// UpdateParams carries a create request over to an update.
func UpdateParams(p anthropic.BetaAgentNewParams) anthropic.BetaAgentUpdateParams {
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

// RosterText describes the specialists for the task message.
func RosterText() string {
	var sb strings.Builder
	for _, s := range Specialists {
		fmt.Fprintf(&sb, "- %s (%s): %s\n", s.Name, s.Key, s.Description)
	}
	return sb.String()
}
