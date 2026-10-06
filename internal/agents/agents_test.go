package agents

import (
	"encoding/json"
	"testing"

	"github.com/subganapathy/paved-road-agent/internal/connectors"
)

// No rendered prompt names a product: the model discovers them.
func TestPromptsNameNoProduct(t *testing.T) {
	p, err := LoadProgram()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range append([]Def{Lead(p, "", ""), Instantiator(p)}, Specialists...) {
		if hits := FindProductNames(d.System + " " + d.Description); len(hits) > 0 {
			t.Errorf("%s: product names in prompt: %v", d.Key, hits)
		}
	}
}

// The create request as the API will see it.
func TestParamsWire(t *testing.T) {
	p, err := LoadProgram()
	if err != nil {
		t.Fatal(err)
	}
	reg, err := connectors.Tools(&connectors.Client{Base: "http://proxy", Session: "s"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lead := Lead(p, "", "")
	custom, err := connectors.Definitions(reg, lead.Connectors)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Params(lead, custom, []string{"agent_a", "agent_b"}))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"].(map[string]any)["id"] != LeadModel {
		t.Errorf("model = %v", got["model"])
	}
	ma := got["multiagent"].(map[string]any)
	if ma["type"] != "coordinator" || len(ma["agents"].([]any)) != 2 {
		t.Errorf("multiagent = %v", ma)
	}
	tools := got["tools"].([]any)
	set := tools[0].(map[string]any)
	if set["type"] != "agent_toolset_20260401" || set["default_config"].(map[string]any)["enabled"] != false {
		t.Errorf("toolset = %v", set)
	}
	var enabled []string
	for _, c := range set["configs"].([]any) {
		enabled = append(enabled, c.(map[string]any)["name"].(string))
	}
	if len(enabled) != 4 {
		t.Errorf("built-ins = %v", enabled)
	}
	if len(tools)-1 != len(lead.Connectors) {
		t.Errorf("lead has %d custom tools, want %d", len(tools)-1, len(lead.Connectors))
	}
	for _, c := range tools[1:] {
		ct := c.(map[string]any)
		if ct["type"] != "custom" || ct["name"] == "" {
			t.Errorf("custom tool = %v", ct)
		}
	}
	// Specialists have connector sets that exist.
	for _, s := range Specialists {
		if _, err := connectors.Definitions(reg, s.Connectors); err != nil {
			t.Errorf("%s: %v", s.Key, err)
		}
	}
}
