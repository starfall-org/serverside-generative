package chatsession

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/starfall-org/generative-server/internal/execution"
)

func TestTypedOutputDoesNotEmitSecretsFromNewChannels(t *testing.T) {
	for _, test := range []struct {
		name   string
		events []execution.OutputEvent
	}{
		{
			name: "split thinking",
			events: []execution.OutputEvent{
				{Kind: execution.OutputReasoning, Text: "reasoning: key-"},
				{Kind: execution.OutputReasoning, Text: "sentinel"},
			},
		},
		{
			name: "escaped arguments",
			events: []execution.OutputEvent{
				{Kind: execution.OutputTool, Tool: execution.ToolOutput{Key: "tool", Name: "lookup", Arguments: `{"key":"\u006bey-`}},
				{Kind: execution.OutputTool, Tool: execution.ToolOutput{Key: "tool", Arguments: `sentinel"}`}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := newRunOutput("reply", []string{"key-sentinel"})
			emit := func(kind string, data any) {
				encoded, err := json.Marshal(data)
				if err != nil || strings.Contains(string(encoded), "sentinel") {
					t.Fatalf("%s included unsafe data: %s (%v)", kind, encoded, err)
				}
			}
			for _, event := range test.events {
				output.apply(event, emit)
			}
			output.finish(emit)
			if !output.leaked {
				t.Fatal("provider secret was not detected")
			}
			version := map[string]any{}
			output.applyToVersion(version)
			encoded, _ := json.Marshal(version)
			if strings.Contains(string(encoded), "sentinel") {
				t.Fatalf("unsafe transcript: %s", encoded)
			}
		})
	}
}
