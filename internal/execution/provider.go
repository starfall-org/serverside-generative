package execution

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go/v2"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/v2/option"
	openai "github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"google.golang.org/genai"
)

// Configuration is supplied by the paired Android client during handoff.
// Never write this structure to a file, environment variable, or log.
type State struct {
	Provider     Provider    `json:"provider"`
	Conversation []Message   `json:"conversation"`
	MCP          []MCPServer `json:"mcp,omitempty"`
}
type Provider struct {
	Platform  string `json:"platform"`
	APIKey    string `json:"api_key"`
	Model     string `json:"model"`
	BaseURL   string `json:"base_url,omitempty"`
	MaxTokens int64  `json:"max_tokens,omitempty"`
}
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type MCPServer struct {
	ID      string            `json:"id"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

func validateRemoteEndpoint(value string) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid endpoint URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return errors.New("endpoint must use HTTPS")
	}
	// Plain HTTP is allowed only for services explicitly bound to localhost.
	// DNS aliases are not trusted: resolve no hostnames for this exception.
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("public endpoints require HTTPS")
	}
	return nil
}

func (r State) ValidateModel() error {
	p := r.Provider
	switch p.Platform {
	case "openai-responses", "openai-chat", "anthropic", "gemini":
	default:
		return errors.New("unsupported model platform")
	}
	if p.APIKey == "" || len(p.APIKey) > 16*1024 {
		return errors.New("missing or invalid provider API key")
	}
	if strings.TrimSpace(p.Model) == "" || len(p.Model) > 256 {
		return errors.New("missing or invalid model")
	}
	if p.MaxTokens < 0 || p.MaxTokens > 131072 {
		return errors.New("invalid max_tokens")
	}
	if err := validateRemoteEndpoint(p.BaseURL); err != nil {
		return err
	}
	if len(r.Conversation) == 0 || len(r.Conversation) > 256 {
		return errors.New("conversation must contain 1-256 messages")
	}
	for _, m := range r.Conversation {
		switch m.Role {
		case "system", "developer", "user", "assistant":
		default:
			return errors.New("invalid conversation role")
		}
		if len(m.Content) > 512*1024 {
			return errors.New("message too large")
		}
	}
	return nil
}

func Stream(ctx context.Context, state State, onDelta func(string)) error {
	if err := state.ValidateModel(); err != nil {
		return err
	}
	switch state.Provider.Platform {
	case "openai-responses":
		return streamOpenAIResponses(ctx, state, onDelta)
	case "openai-chat":
		return streamOpenAIChat(ctx, state, onDelta)
	case "anthropic":
		return streamAnthropic(ctx, state, onDelta)
	case "gemini":
		return streamGemini(ctx, state, onDelta)
	}
	return errors.New("unsupported provider")
}

func streamOpenAIResponses(ctx context.Context, s State, emit func(string)) error {
	options := []openaioption.RequestOption{openaioption.WithAPIKey(s.Provider.APIKey)}
	if s.Provider.BaseURL != "" {
		options = append(options, openaioption.WithBaseURL(s.Provider.BaseURL))
	}
	client := openai.NewClient(options...)
	input := make(responses.ResponseInputParam, 0, len(s.Conversation))
	for _, m := range s.Conversation {
		input = append(input, responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRole(m.Role)))
	}
	params := responses.ResponseNewParams{
		Model: openai.ChatModel(s.Provider.Model),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: input},
	}
	if s.Provider.MaxTokens > 0 {
		params.MaxOutputTokens = openai.Int(s.Provider.MaxTokens)
	}
	stream := client.Responses.NewStreaming(ctx, params)
	defer stream.Close()
	for stream.Next() {
		event := stream.Current()
		if event.Type == "response.output_text.delta" && event.Delta != "" {
			emit(event.Delta)
		}
		if event.Type == "response.failed" {
			return errors.New("model response failed")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("OpenAI Responses stream failed: %w", err)
	}
	return nil
}

func streamOpenAIChat(ctx context.Context, s State, emit func(string)) error {
	options := []openaioption.RequestOption{openaioption.WithAPIKey(s.Provider.APIKey)}
	if s.Provider.BaseURL != "" {
		options = append(options, openaioption.WithBaseURL(s.Provider.BaseURL))
	}
	client := openai.NewClient(options...)
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(s.Conversation))
	for _, m := range s.Conversation {
		switch m.Role {
		case "system":
			messages = append(messages, openai.SystemMessage(m.Content))
		case "developer":
			messages = append(messages, openai.DeveloperMessage(m.Content))
		case "assistant":
			messages = append(messages, openai.AssistantMessage(m.Content))
		case "user":
			messages = append(messages, openai.UserMessage(m.Content))
		}
	}
	params := openai.ChatCompletionNewParams{Model: openai.ChatModel(s.Provider.Model), Messages: messages}
	if s.Provider.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(s.Provider.MaxTokens)
	}
	stream := client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()
	for stream.Next() {
		for _, choice := range stream.Current().Choices {
			if choice.Delta.Content != "" {
				emit(choice.Delta.Content)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("OpenAI Chat stream failed: %w", err)
	}
	return nil
}

func streamAnthropic(ctx context.Context, s State, emit func(string)) error {
	options := []anthropicoption.RequestOption{anthropicoption.WithAPIKey(s.Provider.APIKey)}
	if s.Provider.BaseURL != "" {
		options = append(options, anthropicoption.WithBaseURL(s.Provider.BaseURL))
	}
	client := anthropic.NewClient(options...)
	messages := make([]anthropic.MessageParam, 0, len(s.Conversation))
	system := make([]anthropic.TextBlockParam, 0, 2)
	for _, m := range s.Conversation {
		switch m.Role {
		case "system", "developer":
			system = append(system, anthropic.TextBlockParam{Text: m.Content})
		case "user":
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case "assistant":
			messages = append(messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		}
	}
	if len(messages) == 0 {
		return errors.New("Anthropic requires at least one user/assistant message")
	}
	max := s.Provider.MaxTokens
	if max == 0 {
		max = 4096
	}
	params := anthropic.MessageNewParams{Model: anthropic.Model(s.Provider.Model), MaxTokens: max, Messages: messages, System: system}
	stream := client.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	for stream.Next() {
		event := stream.Current()
		if event.Type == "content_block_delta" && event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			emit(event.Delta.Text)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("Anthropic stream failed: %w", err)
	}
	return nil
}

func streamGemini(ctx context.Context, s State, emit func(string)) error {
	cfg := &genai.ClientConfig{Backend: genai.BackendGeminiAPI, APIKey: s.Provider.APIKey}
	if s.Provider.BaseURL != "" {
		cfg.HTTPOptions = genai.HTTPOptions{BaseURL: s.Provider.BaseURL}
	}
	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return err
	}
	contents := make([]*genai.Content, 0, len(s.Conversation))
	var system []string
	for _, m := range s.Conversation {
		switch m.Role {
		case "system", "developer":
			system = append(system, m.Content)
		case "user":
			contents = append(contents, genai.NewContentFromText(m.Content, genai.RoleUser))
		case "assistant":
			contents = append(contents, genai.NewContentFromText(m.Content, genai.RoleModel))
		}
	}
	if len(contents) == 0 {
		return errors.New("Gemini requires at least one user/assistant message")
	}
	config := &genai.GenerateContentConfig{}
	if len(system) > 0 {
		config.SystemInstruction = genai.NewContentFromText(strings.Join(system, "\n"), genai.RoleUser)
	}
	if s.Provider.MaxTokens > 0 {
		config.MaxOutputTokens = int32(s.Provider.MaxTokens)
	}
	for result, err := range client.Models.GenerateContentStream(ctx, s.Provider.Model, contents, config) {
		if err != nil {
			return fmt.Errorf("Gemini stream failed: %w", err)
		}
		if result != nil {
			if delta := result.Text(); delta != "" {
				emit(delta)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}
