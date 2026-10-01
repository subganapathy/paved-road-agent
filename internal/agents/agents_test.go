package agents

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/subganapathy/paved-road-agent/internal/classify"
	"github.com/subganapathy/paved-road-agent/internal/tools"
)

// The create request as the API will see it: discriminators present, only
// read-only built-ins enabled, custom tools carrying their schemas, and
// the lead's roster in place.
func TestParamsWire(t *testing.T) {
	reg, err := tools.Registry(tools.Env{})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := tools.Definitions(reg, tools.Sets["lead"])
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Params(Lead, custom, []string{"agent_a", "agent_b"}))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	model := got["model"].(map[string]any)
	if model["id"] != LeadModel || model["effort"] != "high" {
		t.Errorf("model = %v", model)
	}
	ma := got["multiagent"].(map[string]any)
	if ma["type"] != "coordinator" || len(ma["agents"].([]any)) != 2 {
		t.Errorf("multiagent = %v", ma)
	}
	ts := got["tools"].([]any)
	set := ts[0].(map[string]any)
	if set["type"] != "agent_toolset_20260401" {
		t.Errorf("first tool = %v", set)
	}
	if set["default_config"].(map[string]any)["enabled"] != false {
		t.Errorf("built-ins are not disabled by default: %v", set["default_config"])
	}
	var names []string
	for _, c := range set["configs"].([]any) {
		cfg := c.(map[string]any)
		if cfg["enabled"] != true {
			t.Errorf("config %v not enabled", cfg)
		}
		names = append(names, cfg["name"].(string))
	}
	if !slices.Equal(names, []string{"read", "glob", "grep"}) {
		t.Errorf("built-ins = %v", names)
	}
	for _, c := range ts[1:] {
		ct := c.(map[string]any)
		if ct["type"] != "custom" || ct["name"] == "" || ct["description"] == "" {
			t.Errorf("custom tool = %v", ct)
		}
		if _, ok := ct["input_schema"].(map[string]any)["properties"]; !ok {
			t.Errorf("custom tool %v has no properties", ct["name"])
		}
	}
	if len(ts)-1 != len(tools.Sets["lead"]) {
		t.Errorf("lead has %d custom tools, want %d", len(ts)-1, len(tools.Sets["lead"]))
	}
}

// Every classifier aspect has exactly one specialist that owns it, or the
// lead would have nobody to delegate to.
func TestEveryAspectHasOneOwner(t *testing.T) {
	owners := map[string][]string{}
	for _, s := range Specialists {
		for _, a := range s.Aspects {
			owners[a] = append(owners[a], s.Key)
		}
		if _, ok := tools.Sets[s.Key]; !ok {
			t.Errorf("%s has no tool set", s.Key)
		}
	}
	for _, a := range classify.All {
		if n := len(owners[string(a)]); n != 1 {
			t.Errorf("aspect %s has %d owners: %v", a, n, owners[string(a)])
		}
	}
}
