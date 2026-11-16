package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
)

// ErrMediaProfile denotes invalid mobile image/video/music profile JSON.
var ErrMediaProfile = errors.New("invalid mobile media profile")

// MobileMediaProfile is an immutable, lossless mobile DTO. It preserves unknown
// fields, missing vs null, and the exact byte representation on a round trip.
type MobileMediaProfile struct{ raw json.RawMessage }

func (p MobileMediaProfile) Bytes() json.RawMessage {
	return append(json.RawMessage(nil), p.raw...)
}
func (p MobileMediaProfile) MarshalJSON() ([]byte, error) {
	if len(p.raw) == 0 {
		return nil, ErrMediaProfile
	}
	return p.Bytes(), nil
}

// MediaProfileFields is a read-only typed view using the Kotlin wire names.
// Mutating this view does not change the lossless DTO.
type MediaProfileFields struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Mode                string            `json:"mode"`
	Chain               MediaRequestChain `json:"chain"`
	Options             json.RawMessage   `json:"options"`
	DisabledParams      []string          `json:"disabledParams"`
	ParamsConfigured    bool              `json:"paramsConfigured"`
	EncryptedCredential string            `json:"encryptedCredential"`
	RequiresModel       bool              `json:"requiresModel"`
}
type MediaRequestChain struct {
	Steps         []MediaRequestStep `json:"steps"`
	FinalResponse MediaFinalResponse `json:"finalResponse"`
}
type MediaRequestStep struct {
	Endpoint      string                   `json:"endpoint"`
	Method        string                   `json:"method"`
	Headers       map[string]string        `json:"headers"`
	Mode          string                   `json:"mode"`
	FileTransport string                   `json:"fileTransport"`
	ContentType   string                   `json:"contentType"`
	Body          string                   `json:"body"`
	Observation   MediaResponseObservation `json:"observation"`
}
type MediaResponseObservation struct {
	Enabled       *bool            `json:"enabled"`
	StatusPath    string           `json:"statusPath"`
	Mode          string           `json:"mode"`
	ReadyValues   []string         `json:"readyValues"`
	PendingValues []string         `json:"pendingValues"`
	ErrorValues   []string         `json:"errorValues"`
	Retry         MediaRetryPolicy `json:"retry"`
}
type MediaRetryPolicy struct {
	MaxAttempts   int   `json:"maxAttempts"`
	DelayMillis   int64 `json:"delayMillis"`
	MaxWaitMillis int64 `json:"maxWaitMillis"`
}
type MediaFinalResponse struct {
	Type       string `json:"type"`
	MediaPath  string `json:"mediaPath"`
	ValueKind  string `json:"valueKind"`
	SourceStep *int   `json:"sourceStep"`
}

func mediaDefaults() MediaProfileFields {
	return MediaProfileFields{
		Mode: "custom", Options: json.RawMessage("{}"), DisabledParams: []string{}, RequiresModel: true,
		Chain: MediaRequestChain{
			Steps:         []MediaRequestStep{mediaStepDefaults()},
			FinalResponse: MediaFinalResponse{Type: "json", ValueKind: "link"},
		},
	}
}
func mediaStepDefaults() MediaRequestStep {
	return MediaRequestStep{
		Endpoint: "/", Method: "POST", Headers: map[string]string{}, Mode: "json",
		FileTransport: "base64", Body: "{}",
		Observation: MediaResponseObservation{
			Mode: "value", ReadyValues: []string{"completed", "complete", "succeeded", "success", "done"},
			PendingValues: []string{"queued", "pending", "processing", "running", "in_progress"},
			ErrorValues:   []string{"failed", "failure", "error", "cancelled", "canceled"},
			Retry:         MediaRetryPolicy{MaxAttempts: 1, DelayMillis: 1000, MaxWaitMillis: 60000},
		},
	}
}

// ParseMobileMediaProfile verifies Kotlin's wire schema and keeps the entire
// original nonexecuting JSON DTO for exact private-client round trips.
func ParseMobileMediaProfile(raw []byte) (MobileMediaProfile, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return MobileMediaProfile{}, ErrMediaProfile
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if mediaWalk(d, 0) != nil {
		return MobileMediaProfile{}, ErrMediaProfile
	}
	if _, err := d.Token(); err != io.EOF {
		return MobileMediaProfile{}, ErrMediaProfile
	}
	if _, err := mediaFields(raw); err != nil {
		return MobileMediaProfile{}, err
	}
	return MobileMediaProfile{append(json.RawMessage(nil), raw...)}, nil
}
func (p MobileMediaProfile) Fields() (MediaProfileFields, error) { return mediaFields(p.raw) }

func mediaFields(raw []byte) (MediaProfileFields, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil || !mediaPresent(m, "id", "name") ||
		!mediaNonNull(m, "mode", "chain", "options", "disabledParams", "paramsConfigured", "encryptedCredential", "requiresModel") {
		return MediaProfileFields{}, ErrMediaProfile
	}
	f := mediaDefaults()
	if c, ok := m["chain"]; ok {
		var cm map[string]json.RawMessage
		if json.Unmarshal(c, &cm) != nil || cm == nil || !mediaNonNull(cm, "steps", "finalResponse") {
			return MediaProfileFields{}, ErrMediaProfile
		}
		if sr, ok := cm["steps"]; ok {
			var steps []json.RawMessage
			if json.Unmarshal(sr, &steps) != nil || steps == nil || len(steps) > 32 {
				return MediaProfileFields{}, ErrMediaProfile
			}
			f.Chain.Steps = make([]MediaRequestStep, len(steps))
			for i, r := range steps {
				v, err := mediaStep(r)
				if err != nil {
					return MediaProfileFields{}, err
				}
				f.Chain.Steps[i] = v
			}
		}
		if fr, ok := cm["finalResponse"]; ok {
			if !mediaObject(fr, "type", "mediaPath", "valueKind") ||
				json.Unmarshal(fr, &f.Chain.FinalResponse) != nil {
				return MediaProfileFields{}, ErrMediaProfile
			}
		}
	}
	validatedChain := f.Chain
	if json.Unmarshal(raw, &f) != nil || strings.TrimSpace(f.ID) == "" ||
		strings.TrimSpace(f.Name) == "" || len(f.ID) > 128 ||
		!mediaIs(f.Mode, "default", "custom") ||
		(f.EncryptedCredential != "" && !strings.HasPrefix(f.EncryptedCredential, "keystore:v1:")) {
		return MediaProfileFields{}, ErrMediaProfile
	}
	// Encoding/json replaces a slice's elements on a second decode. Restore
	// recursively decoded Kotlin defaults and validated nested values.
	f.Chain = validatedChain
	if !mediaIs(f.Chain.FinalResponse.Type, "json", "image", "video", "audio", "octet-stream") ||
		!mediaIs(f.Chain.FinalResponse.ValueKind, "link", "base64") {
		return MediaProfileFields{}, ErrMediaProfile
	}
	if f.Chain.FinalResponse.SourceStep != nil {
		n := *f.Chain.FinalResponse.SourceStep
		if n < 1 || n > len(f.Chain.Steps) {
			return MediaProfileFields{}, ErrMediaProfile
		}
	}
	var opts map[string]json.RawMessage
	if json.Unmarshal(f.Options, &opts) != nil || opts == nil || f.DisabledParams == nil {
		return MediaProfileFields{}, ErrMediaProfile
	}
	for _, p := range f.DisabledParams {
		if p == "" {
			return MediaProfileFields{}, ErrMediaProfile
		}
	}
	return f, nil
}
func mediaStep(raw []byte) (MediaRequestStep, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil ||
		!mediaNonNull(m, "endpoint", "method", "headers", "mode", "fileTransport", "contentType", "body", "observation") {
		return MediaRequestStep{}, ErrMediaProfile
	}
	s := mediaStepDefaults()
	if r, ok := m["observation"]; ok {
		var obs map[string]json.RawMessage
		if json.Unmarshal(r, &obs) != nil || obs == nil ||
			!mediaNonNull(obs, "statusPath", "mode", "readyValues", "pendingValues", "errorValues", "retry") {
			return MediaRequestStep{}, ErrMediaProfile
		}
		if rr, ok := obs["retry"]; ok {
			if !mediaObject(rr, "maxAttempts", "delayMillis", "maxWaitMillis") ||
				json.Unmarshal(rr, &s.Observation.Retry) != nil {
				return MediaRequestStep{}, ErrMediaProfile
			}
		}
		if json.Unmarshal(r, &s.Observation) != nil {
			return MediaRequestStep{}, ErrMediaProfile
		}
	}
	validatedObservation := s.Observation
	if json.Unmarshal(raw, &s) != nil || !mediaIs(s.Mode, "form", "json", "raw") ||
		!mediaIs(s.FileTransport, "file", "base64", "raw") ||
		!mediaIs(s.Observation.Mode, "path_exists", "value") ||
		s.Headers == nil || s.Observation.ReadyValues == nil || s.Observation.PendingValues == nil ||
		s.Observation.ErrorValues == nil || s.Observation.Retry.MaxAttempts < 1 ||
		s.Observation.Retry.DelayMillis < 0 || s.Observation.Retry.MaxWaitMillis < 0 {
		return MediaRequestStep{}, ErrMediaProfile
	}
	s.Observation = validatedObservation
	return s, nil
}
func mediaPresent(m map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; !ok || string(v) == "null" {
			return false
		}
	}
	return true
}
func mediaNonNull(m map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok && string(v) == "null" {
			return false
		}
	}
	return true
}
func mediaObject(raw []byte, keys ...string) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil && mediaNonNull(m, keys...)
}
func mediaIs(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}

// StoredMetadataJSON strips Android's Keystore-bound credential and refuses
// other credential-bearing values rather than silently persisting them in YAML.
func (p MobileMediaProfile) StoredMetadataJSON() ([]byte, error) {
	if _, err := p.Fields(); err != nil {
		return nil, err
	}
	var data map[string]any
	dec := json.NewDecoder(bytes.NewReader(p.raw))
	dec.UseNumber() // Keep opaque Kotlin extension integers above 2^53 exactly.
	if dec.Decode(&data) != nil || data == nil {
		return nil, ErrMediaProfile
	}
	delete(data, "encryptedCredential")
	if mediaPrivate(data, "") {
		return nil, ErrMediaProfile
	}
	return json.Marshal(data)
}
func mediaPrivate(v any, parent string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			norm := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(k))
			if parent == "headers" {
				t, ok := item.(string)
				if !ok {
					return true
				}
				if mediaSensitive(norm) {
					if strings.TrimSpace(t) != "{{api_token}}" && strings.TrimSpace(t) != "Bearer {{api_token}}" {
						return true
					}
				} else if norm != "accept" && norm != "contenttype" && norm != "useragent" && norm != "xclientversion" && t != "" {
					return true
				}
			} else if mediaSensitive(norm) {
				return true
			}
			if mediaPrivate(item, norm) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if mediaPrivate(item, parent) {
				return true
			}
		}
	case string:
		if parent == "body" {
			trimmed := strings.TrimSpace(x)
			if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
				dec := json.NewDecoder(strings.NewReader(trimmed))
				dec.UseNumber()
				var body any
				if dec.Decode(&body) != nil || mediaPrivate(body, "body") {
					return true
				}
			}
		}
		s := strings.ToLower(x)
		if (strings.Contains(s, "bearer ") && !strings.Contains(s, "bearer {{api_token}}")) ||
			strings.Contains(s, "sk-") || strings.Contains(s, "api_key=") ||
			strings.Contains(s, "access_token=") {
			return true
		}
		if parent == "endpoint" || parent == "url" ||
			strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
			u, err := url.Parse(x)
			if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return true
			}
		}
	}
	return false
}
func mediaSensitive(key string) bool {
	return strings.Contains(key, "secret") || strings.Contains(key, "password") ||
		strings.Contains(key, "token") || strings.Contains(key, "apikey") ||
		strings.Contains(key, "credential") || strings.Contains(key, "privatekey") ||
		strings.Contains(key, "accesskey") ||
		key == "authorization" || key == "cookie" || key == "credentials" || key == "auth"
}

// mediaWalk detects duplicate keys inside nested opaque extension objects.
func mediaWalk(d *json.Decoder, depth int) error {
	if depth > 64 {
		return ErrMediaProfile
	}
	v, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := v.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return ErrMediaProfile
			}
			seen[s] = true
			if err = mediaWalk(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return ErrMediaProfile
		}
	case '[':
		for d.More() {
			if err := mediaWalk(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return ErrMediaProfile
		}
	default:
		return ErrMediaProfile
	}
	return nil
}
