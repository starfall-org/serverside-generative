package httptool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/starfall-org/serverside-generative/internal/execution"
)

var ErrDenied = errors.New("tool permission denied")
var ErrUnsupported = errors.New("unsupported tool feature")
var ErrSecretLeak = errors.New("tool output contained runtime credentials")

// Policy must be granted by the caller; the tool record alone never grants execution.
type Policy struct {
	Allowed       bool
	AllowLoopback bool
}
type Result struct {
	Status int `json:"status"`
	Result any `json:"result"`
}
type executable struct {
	Enabled         bool            `json:"enabled"`
	Method          string          `json:"method"`
	URL             string          `json:"url"`
	Parameters      json.RawMessage `json:"parameters"`
	Query           map[string]any  `json:"query"`
	Headers         map[string]any  `json:"headers"`
	Body            string          `json:"body"`
	BodyFormat      string          `json:"bodyFormat"`
	ResponseMode    string          `json:"responseMode"`
	ResponseMapping any             `json:"responseMapping"`
}

func Execute(ctx context.Context, raw, argsRaw json.RawMessage, policy Policy) (Result, error) {
	if !policy.Allowed {
		return Result{}, ErrDenied
	}
	if _, err := Validate(raw); err != nil {
		return Result{}, err
	}
	t := executable{Method: "GET", BodyFormat: "JSON", ResponseMode: "AUTO", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}
	if json.Unmarshal(raw, &t) != nil || !t.Enabled {
		return Result{}, ErrDenied
	}
	var args map[string]any
	if len(argsRaw) > 64<<10 || json.Unmarshal(argsRaw, &args) != nil || args == nil {
		return Result{}, ErrInvalid
	}
	var schema jsonschema.Schema
	if json.Unmarshal(t.Parameters, &schema) != nil {
		return Result{}, ErrInvalid
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return Result{}, ErrInvalid
	}
	// Match mobile: keep declared arguments and supply each property's default.
	clean := map[string]any{}
	var definition struct {
		Properties map[string]struct {
			Default json.RawMessage `json:"default"`
		} `json:"properties"`
	}
	if json.Unmarshal(t.Parameters, &definition) != nil {
		return Result{}, ErrInvalid
	}
	for key, p := range definition.Properties {
		if value, ok := args[key]; ok {
			clean[key] = value
		} else if len(p.Default) > 0 {
			var value any
			if json.Unmarshal(p.Default, &value) != nil {
				return Result{}, ErrInvalid
			}
			clean[key] = value
		}
	}
	if resolved.Validate(clean) != nil {
		return Result{}, ErrInvalid
	}
	endpoint, err := url.Parse(t.URL)
	if err != nil {
		return Result{}, ErrInvalid
	}
	if endpoint.Scheme != "https" {
		host := strings.ToLower(endpoint.Hostname())
		ipLocal := host == "localhost" || host == "127.0.0.1" || host == "::1"
		if endpoint.Scheme != "http" || !policy.AllowLoopback || !ipLocal {
			return Result{}, ErrDenied
		}
	}
	query := endpoint.Query()
	for key, template := range t.Query {
		value, present, err := render(template, clean)
		if err != nil {
			return Result{}, ErrInvalid
		}
		if !present {
			continue
		}
		query.Del(key)
		if values, ok := value.([]any); ok {
			for _, v := range values {
				query.Add(key, textValue(v))
			}
		} else {
			query.Add(key, textValue(value))
		}
	}
	endpoint.RawQuery = query.Encode()
	var body []byte
	if strings.TrimSpace(t.Body) != "" {
		if t.BodyFormat == "JSON" {
			var template any
			if json.Unmarshal([]byte(t.Body), &template) != nil {
				return Result{}, ErrInvalid
			}
			value, _, err := render(template, clean)
			if err != nil {
				return Result{}, ErrInvalid
			}
			body, err = json.Marshal(value)
			if err != nil {
				return Result{}, ErrInvalid
			}
		} else {
			value, err := renderText(t.Body, clean)
			if err != nil {
				return Result{}, ErrInvalid
			}
			body = []byte(value)
		}
	}
	if len(body) > 1<<20 {
		return Result{}, ErrInvalid
	}
	request, err := http.NewRequestWithContext(ctx, t.Method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Result{}, ErrInvalid
	}
	request.Header.Set("Accept", "*/*")
	for key, template := range t.Headers {
		value, present, err := render(template, clean)
		if err != nil {
			return Result{}, ErrInvalid
		}
		if !present {
			continue
		}
		switch value.(type) {
		case string, float64, bool:
		default:
			return Result{}, ErrInvalid
		}
		valueText := textValue(value)
		if strings.ContainsAny(valueText, "\r\n") || strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Transfer-Encoding") || strings.EqualFold(key, "Proxy-Authorization") {
			return Result{}, ErrInvalid
		}
		request.Header.Set(key, valueText)
	}
	if len(body) > 0 && request.Header.Get("Content-Type") == "" {
		if t.BodyFormat == "JSON" {
			request.Header.Set("Content-Type", "application/json")
		} else {
			request.Header.Set("Content-Type", "text/plain")
		}
	}
	client := execution.SafeHTTPClient(policy.AllowLoopback)
	client.Timeout = 45 * time.Second
	response, err := client.Do(request)
	if err != nil {
		return Result{}, errors.New("tool request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Result{}, errors.New("tool request failed")
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	mode := t.ResponseMode
	if mode == "AUTO" {
		switch {
		case strings.HasPrefix(mime, "image/"), strings.HasPrefix(mime, "audio/"), strings.HasPrefix(mime, "video/"), mime == "application/octet-stream":
			mode = "MEDIA"
		case strings.Contains(mime, "json"):
			mode = "JSON"
		default:
			mode = "RAW"
		}
	}
	if mode == "MEDIA" {
		return Result{}, ErrUnsupported
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return Result{}, errors.New("tool response too large")
	}
	var result any = string(data)
	if mode == "JSON" {
		if json.Unmarshal(data, &result) != nil {
			return Result{}, errors.New("invalid tool response")
		}
	}
	if shape, ok := t.ResponseMapping.(map[string]any); ok && len(shape) > 0 {
		result, err = mapResponse(result, shape)
		if err != nil {
			return Result{}, errors.New("tool response mapping failed")
		}
	}
	secrets := StaticSecrets(raw)
	for key := range t.Headers {
		value := request.Header.Get(key)
		if value != "" {
			secrets = append(secrets, value)
			if len(value) > 7 && strings.EqualFold(value[:7], "Bearer ") {
				secrets = append(secrets, value[7:])
			}
		}
	}
	if original, err := url.Parse(t.URL); err == nil {
		for _, values := range original.Query() {
			for _, value := range values {
				if value != "" {
					secrets = append(secrets, value, url.QueryEscape(value), url.PathEscape(value))
				}
			}
		}
	}
	if containsCredential(result, secrets) {
		return Result{}, ErrSecretLeak
	}
	return Result{Status: response.StatusCode, Result: result}, nil
}
func textValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func renderText(s string, args map[string]any) (string, error) {
	missing := false
	out := placeholders.ReplaceAllStringFunc(s, func(match string) string {
		parts := placeholders.FindStringSubmatch(match)
		value, ok := args[strings.TrimSpace(parts[1])]
		if !ok {
			missing = true
			return ""
		}
		return textValue(value)
	})
	if missing {
		return "", ErrInvalid
	}
	return out, nil
}
func render(v any, args map[string]any) (any, bool, error) {
	switch x := v.(type) {
	case string:
		m := placeholders.FindStringSubmatchIndex(x)
		if m != nil && m[0] == 0 && m[1] == len(x) {
			key := strings.TrimSpace(x[m[2]:m[3]])
			value, ok := args[key]
			return value, ok, nil
		}
		value, err := renderText(x, args)
		return value, true, err
	case []any:
		out := []any{}
		for _, item := range x {
			value, ok, err := render(item, args)
			if err != nil {
				return nil, false, err
			}
			if ok {
				out = append(out, value)
			}
		}
		return out, true, nil
	case map[string]any:
		out := map[string]any{}
		for key, item := range x {
			value, ok, err := render(item, args)
			if err != nil {
				return nil, false, err
			}
			if ok {
				out[key] = value
			}
		}
		return out, true, nil
	default:
		return v, true, nil
	}
}
func mapResponse(root, shape any) (any, error) {
	switch x := shape.(type) {
	case string:
		if x == "" {
			return root, nil
		}
		if !strings.HasPrefix(x, "/") {
			return nil, ErrInvalid
		}
		value := root
		for _, part := range strings.Split(x[1:], "/") {
			key := strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			switch data := value.(type) {
			case map[string]any:
				var ok bool
				value, ok = data[key]
				if !ok {
					return nil, ErrInvalid
				}
			case []any:
				i, err := strconv.Atoi(key)
				if err != nil || i < 0 || i >= len(data) {
					return nil, ErrInvalid
				}
				value = data[i]
			default:
				return nil, ErrInvalid
			}
		}
		return value, nil
	case []any:
		values := make([]any, len(x))
		for i, v := range x {
			mapped, err := mapResponse(root, v)
			if err != nil {
				return nil, err
			}
			values[i] = mapped
		}
		return values, nil
	case map[string]any:
		values := map[string]any{}
		for k, v := range x {
			mapped, err := mapResponse(root, v)
			if err != nil {
				return nil, err
			}
			values[k] = mapped
		}
		return values, nil
	default:
		return nil, ErrInvalid
	}
}

func containsCredential(value any, secrets []string) bool {
	switch v := value.(type) {
	case string:
		for _, secret := range secrets {
			if secret != "" && strings.Contains(v, secret) {
				return true
			}
		}
	case map[string]any:
		for k, x := range v {
			if containsCredential(k, secrets) || containsCredential(x, secrets) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if containsCredential(x, secrets) {
				return true
			}
		}
	case float64, bool, json.Number:
		b, _ := json.Marshal(v)
		return containsCredential(string(b), secrets)
	}
	return false
}
