package execution

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go/v2"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"google.golang.org/genai"
)

type OutputKind int

const (
	OutputText OutputKind = iota
	OutputReasoning
	OutputTool
)

// OutputEvent describes provider output only. It does not authorize tool execution.
type OutputEvent struct {
	Kind OutputKind
	Text string
	Tool ToolOutput
}

type ToolOutput struct {
	Key              string
	Name             string
	Arguments        string
	ReplaceArguments bool
	Result           string
	Status           string
}

func emitText(emit func(OutputEvent), kind OutputKind, text string) {
	if text != "" {
		emit(OutputEvent{Kind: kind, Text: text})
	}
}

func emitTool(emit func(OutputEvent), tool ToolOutput) {
	emit(OutputEvent{Kind: OutputTool, Tool: tool})
}

func emitResponsesOutput(event responses.ResponseStreamEventUnion, emit func(OutputEvent)) {
	key := strconv.FormatInt(event.OutputIndex, 10)
	switch event.Type {
	case "response.output_text.delta":
		emitText(emit, OutputText, event.Delta)
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		emitText(emit, OutputReasoning, event.Delta)
	case "response.output_item.added", "response.output_item.done":
		switch event.Item.Type {
		case "function_call", "mcp_call", "custom_tool_call":
		default:
			return
		}
		var item struct {
			Arguments string `json:"arguments"`
			Input     string `json:"input"`
			Output    string `json:"output"`
			Error     string `json:"error"`
		}
		_ = json.Unmarshal([]byte(event.Item.RawJSON()), &item)
		arguments := item.Arguments
		if event.Item.Type == "custom_tool_call" {
			arguments = item.Input
		}
		tool := ToolOutput{Key: key, Name: event.Item.Name, Arguments: arguments, ReplaceArguments: arguments != ""}
		if event.Item.Type == "mcp_call" && event.Type == "response.output_item.done" {
			switch event.Item.Status {
			case "completed":
				tool.Status, tool.Result = "success", item.Output
			case "failed", "incomplete":
				tool.Status, tool.Result = "error", item.Error
			}
		}
		emitTool(emit, tool)
	case "response.function_call_arguments.delta", "response.mcp_call_arguments.delta", "response.custom_tool_call_input.delta":
		emitTool(emit, ToolOutput{Key: key, Arguments: event.Delta})
	case "response.function_call_arguments.done", "response.mcp_call_arguments.done":
		emitTool(emit, ToolOutput{Key: key, Arguments: event.Arguments, ReplaceArguments: true})
	}
}

func emitChatOutput(choice openai.ChatCompletionChunkChoice, emit func(OutputEvent)) {
	// OpenAI-compatible servers use these nonstandard fields for visible reasoning.
	var extras struct {
		ReasoningContent string `json:"reasoning_content"`
		Reasoning        string `json:"reasoning"`
	}
	_ = json.Unmarshal([]byte(choice.Delta.RawJSON()), &extras)
	if extras.ReasoningContent == "" {
		extras.ReasoningContent = extras.Reasoning
	}
	emitText(emit, OutputReasoning, extras.ReasoningContent)
	emitText(emit, OutputText, choice.Delta.Content)
	for _, call := range choice.Delta.ToolCalls {
		emitTool(emit, ToolOutput{Key: fmt.Sprintf("%d:%d", choice.Index, call.Index),
			Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
}

func emitAnthropicOutput(event anthropic.MessageStreamEventUnion, toolKeys map[string]string, emit func(OutputEvent)) {
	key := strconv.FormatInt(event.Index, 10)
	switch event.Type {
	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta":
			emitText(emit, OutputText, event.Delta.Text)
		case "thinking_delta":
			emitText(emit, OutputReasoning, event.Delta.Thinking)
		case "input_json_delta":
			emitTool(emit, ToolOutput{Key: key, Arguments: event.Delta.PartialJSON})
		}
	case "content_block_start":
		block := event.ContentBlock
		switch block.Type {
		case "text":
			emitText(emit, OutputText, block.Text)
		case "thinking":
			emitText(emit, OutputReasoning, block.Thinking)
		case "tool_use", "server_tool_use":
			toolKeys[block.ID] = key
			args := outputJSON(block.Input)
			if args == "{}" {
				args = ""
			}
			emitTool(emit, ToolOutput{Key: key, Name: block.Name, Arguments: args})
		default:
			if !strings.HasSuffix(block.Type, "_tool_result") {
				return
			}
			var result struct {
				ID      string          `json:"tool_use_id"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal([]byte(block.RawJSON()), &result) != nil {
				return
			}
			if toolKey, exists := toolKeys[result.ID]; exists {
				status := "success"
				var detail struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(result.Content, &detail) == nil && strings.HasSuffix(detail.Type, "_error") {
					status = "error"
				}
				emitTool(emit, ToolOutput{Key: toolKey, Status: status, Result: string(result.Content)})
			}
		}
	}
}

func emitGeminiOutput(response *genai.GenerateContentResponse, nextTool *int, emit func(OutputEvent)) {
	if len(response.Candidates) == 0 || response.Candidates[0] == nil || response.Candidates[0].Content == nil {
		return
	}
	for _, part := range response.Candidates[0].Content.Parts {
		if part == nil {
			continue
		}
		kind := OutputText
		if part.Thought {
			kind = OutputReasoning
		}
		emitText(emit, kind, part.Text)
		if call := part.FunctionCall; call != nil {
			key := call.ID
			if key == "" {
				key = "anonymous:" + strconv.Itoa(*nextTool)
				*nextTool++
			}
			emitTool(emit, ToolOutput{Key: key, Name: call.Name, Arguments: outputJSON(call.Args), ReplaceArguments: true})
		}
		if result := part.FunctionResponse; result != nil && result.ID != "" {
			emitTool(emit, ToolOutput{Key: result.ID, Name: result.Name, Result: outputJSON(result.Response), Status: "success"})
		}
	}
}

func outputJSON(value any) string {
	if value == nil {
		return ""
	}
	data, _ := json.Marshal(value)
	return string(data)
}
