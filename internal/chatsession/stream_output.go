package chatsession

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/starfall-org/generative-server/internal/execution"
)

type runToolActivity struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	WireName        string   `json:"wireName"`
	Status          string   `json:"status"`
	Arguments       string   `json:"arguments"`
	Response        string   `json:"response"`
	Files           []string `json:"files"`
	ContentOffset   int      `json:"contentOffset"`
	ReasoningOffset int      `json:"reasoningOffset"`
}

type runTool struct {
	activity  runToolActivity
	arguments strings.Builder
}

// Access is protected by the manager lock. Only checked output reaches storage and events.
type runOutput struct {
	id              string
	secrets         []string
	answer          strings.Builder
	reasoning       strings.Builder
	answerGuard     *streamGuard
	reasoningGuard  *streamGuard
	reasoningOffset int
	reasoningBegun  bool
	tools           map[string]*runTool
	toolOrder       []*runTool
	changes         int
	leaked          bool
}

func newRunOutput(id string, secrets []string) *runOutput {
	return &runOutput{
		id: id, secrets: secrets, answerGuard: newStreamGuard(secrets),
		reasoningGuard: newStreamGuard(secrets), tools: make(map[string]*runTool),
	}
}

func (output *runOutput) apply(event execution.OutputEvent, emit func(string, any)) {
	switch event.Kind {
	case execution.OutputText:
		output.writeText(false, output.answerGuard.push(event.Text, false), emit)
	case execution.OutputReasoning:
		if !output.reasoningBegun {
			output.reasoningBegun = true
			output.reasoningOffset = utf16Length(output.answer.String()) + utf16Length(output.answerGuard.pending)
		}
		output.writeText(true, output.reasoningGuard.push(event.Text, false), emit)
	case execution.OutputTool:
		output.writeTool(event.Tool, emit)
	}
	output.leaked = output.leaked || output.answerGuard.leaked || output.reasoningGuard.leaked
}

func (output *runOutput) writeText(reasoning bool, text string, emit func(string, any)) {
	if text == "" {
		return
	}
	output.changes++
	if reasoning {
		output.reasoning.WriteString(text)
		emit("reasoning_delta", map[string]any{"text": text, "content_offset": output.reasoningOffset})
	} else {
		output.answer.WriteString(text)
		emit("delta", map[string]string{"text": text})
	}
}

func (output *runOutput) writeTool(update execution.ToolOutput, emit func(string, any)) {
	if hasSecret(update.Name, output.secrets) || output.payloadHasSecret(update.Result) {
		output.leaked = true
		return
	}
	tool := output.tools[update.Key]
	created := tool == nil
	if created {
		if update.Name == "" {
			return
		}
		tool = &runTool{activity: runToolActivity{
			ID: output.id + "-tool-" + strconv.Itoa(len(output.toolOrder)), Name: update.Name,
			WireName: update.Name, Status: "running", Files: []string{},
			ContentOffset:   utf16Length(output.answer.String()) + utf16Length(output.answerGuard.pending),
			ReasoningOffset: utf16Length(output.reasoning.String()) + utf16Length(output.reasoningGuard.pending),
		}}
		output.tools[update.Key] = tool
		output.toolOrder = append(output.toolOrder, tool)
	}
	if update.ReplaceArguments {
		tool.arguments.Reset()
	}
	tool.arguments.WriteString(update.Arguments)
	if output.payloadHasSecret(tool.arguments.String()) {
		tool.arguments.Reset()
		output.leaked = true
		return
	}
	if update.ReplaceArguments || update.Status != "" {
		tool.activity.Arguments = tool.arguments.String()
	}
	if update.Status != "" {
		tool.activity.Status = update.Status
		tool.activity.Response = update.Result
	}
	if created || update.ReplaceArguments || update.Status != "" {
		output.changes++
		emit("tool_activity", tool.activity)
	}
}

func (output *runOutput) payloadHasSecret(payload string) bool {
	if hasSecret(payload, output.secrets) {
		return true
	}
	var decoded any
	return json.Unmarshal([]byte(payload), &decoded) == nil && hasSecret(decoded, output.secrets)
}

func (output *runOutput) finish(emit func(string, any)) {
	output.writeText(false, output.answerGuard.push("", true), emit)
	output.writeText(true, output.reasoningGuard.push("", true), emit)
	for _, tool := range output.toolOrder {
		tool.activity.Arguments = tool.arguments.String()
		if tool.activity.Status == "running" {
			// This runner did not execute a provider-supplied call without a result.
			tool.activity.Status = "error"
		}
		emit("tool_activity", tool.activity)
	}
}

func (output *runOutput) applyToVersion(version map[string]any) {
	version["content"] = output.answer.String()
	if output.reasoning.Len() > 0 {
		version["reasoning_content"] = output.reasoning.String()
		version["reasoning_content_offset"] = output.reasoningOffset
	}
	if len(output.toolOrder) > 0 {
		tools := make([]runToolActivity, 0, len(output.toolOrder))
		for _, tool := range output.toolOrder {
			tools = append(tools, tool.activity)
		}
		version["toolActivity"] = tools
	}
}

func utf16Length(text string) int {
	length := 0
	for _, char := range text {
		length += utf16.RuneLen(char)
	}
	return length
}
