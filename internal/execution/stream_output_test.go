package execution

import (
	"encoding/json"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/openai/openai-go/v3/responses"
	"google.golang.org/genai"
)

func TestTypedOutputFromProviderAdapters(t *testing.T) {
	var events []OutputEvent
	emit := func(event OutputEvent) { events = append(events, event) }
	var reasoning responses.ResponseStreamEventUnion
	if err := json.Unmarshal([]byte(`{"type":"response.reasoning_summary_text.delta","delta":"Responses thinking"}`), &reasoning); err != nil {
		t.Fatal(err)
	}
	emitResponsesOutput(reasoning, emit)

	var tool responses.ResponseStreamEventUnion
	if err := json.Unmarshal([]byte(`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","name":"lookup","arguments":""}}`), &tool); err != nil {
		t.Fatal(err)
	}
	emitResponsesOutput(tool, emit)
	emitResponsesOutput(responses.ResponseStreamEventUnion{
		Type: "response.function_call_arguments.delta", OutputIndex: 2, Delta: `{"query":"test"}`,
	}, emit)

	var thinking anthropic.MessageStreamEventUnion
	if err := json.Unmarshal([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Anthropic thinking"}}`), &thinking); err != nil {
		t.Fatal(err)
	}
	emitAnthropicOutput(thinking, make(map[string]string), emit)

	nextTool := 0
	emitGeminiOutput(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
		Content: &genai.Content{Parts: []*genai.Part{
			{Text: "Gemini thinking", Thought: true},
			{FunctionCall: &genai.FunctionCall{ID: "call", Name: "lookup", Args: map[string]any{"key": "test"}}},
			{Text: "Answer"},
		}},
	}}}, &nextTool, emit)

	want := []OutputEvent{
		{Kind: OutputReasoning, Text: "Responses thinking"},
		{Kind: OutputTool, Tool: ToolOutput{Key: "2", Name: "lookup"}},
		{Kind: OutputTool, Tool: ToolOutput{Key: "2", Arguments: `{"query":"test"}`}},
		{Kind: OutputReasoning, Text: "Anthropic thinking"},
		{Kind: OutputReasoning, Text: "Gemini thinking"},
		{Kind: OutputTool, Tool: ToolOutput{Key: "call", Name: "lookup", Arguments: `{"key":"test"}`, ReplaceArguments: true}},
		{Kind: OutputText, Text: "Answer"},
	}
	if len(events) != len(want) {
		t.Fatalf("events=%+v, want %+v", events, want)
	}
	for i, event := range events {
		if event != want[i] {
			t.Errorf("event %d=%+v, want %+v", i, event, want[i])
		}
	}
}
