package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/starfall-org/generative-server/internal/catalog"
	"github.com/starfall-org/generative-server/internal/chatstore"
	"github.com/starfall-org/generative-server/internal/httptool"
	"github.com/starfall-org/generative-server/internal/secrets"
	"github.com/starfall-org/generative-server/internal/storedruntime"
	"go.yaml.in/yaml/v4"
)

func TestCatalogManagementCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	docs := map[string]string{"providers.yaml": "schema_version: 1\nrevision: 1\ngroups: []\nproviders: []\n", "models.yaml": "schema_version: 1\nrevision: 1\nproviders: {}\n", "mcp.yaml": "schema_version: 1\nrevision: 1\nservers: []\n", "profiles.yaml": "schema_version: 1\nrevision: 1\nchatProfiles: []\n"}
	if _, err := catalog.New(dir).Save(0, docs); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"provider", "add", "--id", "p", "--name", "P", "--type", "openai", "--data-dir", dir, "--if-revision", "1"}, {"model", "add", "--provider", "p", "--id", "org/model", "--data-dir", dir, "--if-revision", "2"}, {"model", "select", "--provider", "p", "--mode", "explicit", "--id", "org/model", "--data-dir", dir, "--if-revision", "3"}, {"mcp", "add", "--id", "m", "--name", "M", "--protocol", "sse", "--url", "https://example.invalid/mcp", "--data-dir", dir, "--if-revision", "4"}, {"provider", "hide", "p", "--data-dir", dir, "--if-revision", "5"}} {
		if err := run(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	b, err := catalog.New(dir).Get()
	if err != nil || b.Manifest.Revision != 6 || !strings.Contains(b.Files["providers.yaml"], "isHidden: true") || !strings.Contains(b.Files["models.yaml"], "org/model") {
		t.Fatalf("catalog: %+v %v", b, err)
	}
}
func TestChatCommands(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	file := filepath.Join(base, "c.json")
	raw := `{"id":"c","title":"x","created_at":1,"updated_at":2,"messages":[]}`
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"chat", "import", "--id", "c", "--client-id", "owner", "--data-dir", dir, "--file", file, "--if-revision", "0"}
	if err := run(append(append([]string{}, args...), "--dry-run")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote files: %v", err)
	}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	snap, err := chatstore.New(dir).Get("owner", "c")
	if err != nil || snap.Revision != 1 {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
	out := filepath.Join(base, "export.json")
	if err = run([]string{"chat", "export", "--id", "c", "--client-id", "owner", "--data-dir", dir, "--out", out}); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"chat", "delete", "--id", "c", "--client-id", "owner", "--data-dir", dir, "--if-revision", "1"}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderKeyCommandPublishesReferenceWithoutPlaintext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := run([]string{"config", "init", "--data-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"provider", "add", "--id", "p", "--name", "P", "--type", "openai", "--data-dir", dir, "--if-revision", "1"}); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.WriteString("secret-command-sentinel\n"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	old := os.Stdin
	os.Stdin = reader
	defer func() { os.Stdin = old; reader.Close() }()
	if err = run([]string{"provider", "key", "set", "p", "--key-id", "main", "--stdin", "--data-dir", dir, "--if-revision", "2"}); err != nil {
		t.Fatal(err)
	}
	raw, err := storedruntime.Resolve(dir, json.RawMessage(`{"source":"stored","provider_id":"p","model_id":"m"}`))
	if err != nil || !bytes.Contains(raw, []byte("secret-command-sentinel")) {
		t.Fatalf("resolve: %v", err)
	}
	if err = run([]string{"provider", "auth", "p", "--method", "custom_header", "--name", "X-Api-Key", "--data-dir", dir, "--if-revision", "3"}); err != nil {
		t.Fatal(err)
	}
	raw, err = storedruntime.Resolve(dir, json.RawMessage(`{"source":"stored","provider_id":"p","model_id":"m"}`))
	if err != nil || !bytes.Contains(raw, []byte(`"method":"custom_header"`)) || !bytes.Contains(raw, []byte(`"key":"X-Api-Key"`)) || !bytes.Contains(raw, []byte("secret-command-sentinel")) {
		t.Fatalf("auth update failed to preserve key: %v", err)
	}
	if err = run([]string{"provider", "auth", "p", "--method", "custom_header", "--name", "Host", "--data-dir", dir, "--if-revision", "4"}); err == nil {
		t.Fatal("protected authentication header accepted")
	}
	newReader, newWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = newWriter.WriteString("rotated-secret-sentinel\n")
	_ = newWriter.Close()
	os.Stdin = newReader
	if err = run([]string{"provider", "key", "set", "p", "--key-id", "next", "--stdin", "--data-dir", dir, "--if-revision", "4"}); err != nil {
		t.Fatal(err)
	}
	_ = newReader.Close()
	raw, err = storedruntime.Resolve(dir, json.RawMessage(`{"source":"stored","provider_id":"p","model_id":"m"}`))
	if err != nil || !bytes.Contains(raw, []byte("rotated-secret-sentinel")) || !bytes.Contains(raw, []byte(`"key":"X-Api-Key"`)) {
		t.Fatalf("custom-header credential update: %v", err)
	}
	for _, name := range []string{"providers.yaml", "models.yaml", "data.sqlite"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || bytes.Contains(data, []byte("secret-command-sentinel")) {
			t.Fatalf("plaintext in %s: %v", name, err)
		}
	}
}

func TestToolTestRequiresExplicitConfirmation(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	path := filepath.Join(base, "tool.json")
	argsFile := filepath.Join(base, "args.json")
	tool, _ := json.Marshal(map[string]any{"id": "t", "name": "tool", "enabled": true, "url": upstream.URL, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
	if err := os.WriteFile(path, tool, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(argsFile, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"tool", "add", "--file", path, "--data-dir", dir}); err != nil {
		t.Fatal(err)
	}
	baseArgs := []string{"tool", "test", "--id", "t", "--args-file", argsFile, "--data-dir", dir, "--allow-local-api"}
	if err := run(baseArgs); err == nil || calls.Load() != 0 {
		t.Fatalf("test without confirmation: %v", err)
	}
	if err := run(append(baseArgs, "--yes")); err != nil || calls.Load() != 1 {
		t.Fatalf("confirmed test: %v calls=%d", err, calls.Load())
	}
}

func TestToolTestDoesNotPrintEchoedCredential(t *testing.T) {
	const secret = "cli-secret-sentinel"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"echo": r.Header.Get("X-Key")})
	}))
	defer upstream.Close()
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	path := filepath.Join(base, "tool.json")
	argPath := filepath.Join(base, "args.json")
	tool, _ := json.Marshal(map[string]any{"id": "t", "name": "tool", "enabled": true, "url": upstream.URL, "headers": map[string]string{"X-Key": secret}, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
	if err := os.WriteFile(path, tool, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(argPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"tool", "add", "--file", path, "--data-dir", dir}); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = writer, writer
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr; reader.Close() }()
	err = run([]string{"tool", "test", "--id", "t", "--args-file", argPath, "--data-dir", dir, "--allow-local-api", "--yes"})
	writer.Close()
	printed, readErr := io.ReadAll(reader)
	if readErr != nil || !errors.Is(err, httptool.ErrSecretLeak) || bytes.Contains(printed, []byte(secret)) {
		t.Fatalf("credential leaked or not blocked: %v %v", err, readErr)
	}
}

func TestMCPCredentialCommandEncryptsAndPublishesReference(t *testing.T) {
	const secret = "mcp-credential-sentinel"
	dir := filepath.Join(t.TempDir(), "data")
	if err := run([]string{"config", "init", "--data-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"mcp", "add", "--id", "s", "--name", "Server", "--url", "https://example.com/mcp", "--data-dir", dir, "--if-revision", "1"}); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.WriteString(secret + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	old := os.Stdin
	os.Stdin = reader
	defer func() { os.Stdin = old; _ = reader.Close() }()
	if err = run([]string{"mcp", "credential", "set", "s", "--method", "custom-header", "--header", "X-Token", "--stdin", "--data-dir", dir, "--if-revision", "2"}); err != nil {
		t.Fatal(err)
	}
	bundle, err := catalog.New(dir).Get()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers []struct {
			Auth struct {
				Method string `yaml:"method"`
				Key    string `yaml:"header_name"`
				Ref    string `yaml:"credential_ref"`
			} `yaml:"auth"`
		} `yaml:"servers"`
	}
	if err = yaml.Unmarshal([]byte(bundle.Files["mcp.yaml"]), &doc); err != nil || len(doc.Servers) != 1 {
		t.Fatalf("catalog: %v", err)
	}
	auth := doc.Servers[0].Auth
	if auth.Method != "CUSTOM_HEADER" || auth.Key != "X-Token" || !strings.HasPrefix(auth.Ref, "mcp/s/") {
		t.Fatalf("auth: %+v", auth)
	}
	store, err := secrets.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Get("mcp", "s", strings.TrimPrefix(auth.Ref, "mcp/s/"))
	_ = store.Close()
	if err != nil || string(value) != secret {
		t.Fatalf("credential: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w2.WriteString("another-secret\n")
	_ = w2.Close()
	os.Stdin = r2
	if err = run([]string{"mcp", "credential", "set", "s", "--stdin", "--data-dir", dir, "--if-revision", "2"}); err == nil {
		t.Fatal("stale credential update accepted")
	}
	_ = r2.Close()
	after, err := os.ReadFile(filepath.Join(dir, "data.sqlite"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("conflicting update changed credential database")
	}
	for _, name := range []string{"mcp.yaml", "data.sqlite", "master.key"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("plaintext in %s", name)
		}
	}
}

func TestValidateAllChatsDetectsCorruptionWithoutWriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	store := chatstore.New(dir)
	for _, id := range []string{"first", "second"} {
		raw := json.RawMessage(fmt.Sprintf(`{"id":%q,"title":"x","created_at":1,"updated_at":2,"messages":[]}`, id))
		if _, err := store.Save(id, "owner", "create-"+id, 0, raw); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"chat", "validate-all", "--client-id", "owner", "--data-dir", dir}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("second"))
	path := filepath.Join(dir, "chats", fmt.Sprintf("%x.json", hash))
	broken := []byte(`{"broken":`)
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(args); err == nil {
		t.Fatal("accepted corrupt snapshot")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, broken) {
		t.Fatalf("validation modified file: %q %v", after, err)
	}
}

func TestChatReloadValidatesAndBumpsRevision(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	store := chatstore.New(dir)
	raw := json.RawMessage(`{"id":"c","title":"original","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err := store.Save("c", "owner", "create", 0, raw); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("c"))
	path := filepath.Join(dir, "chats", fmt.Sprintf("%x.json", hash))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte("original"), []byte("hand edit"), 1)
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"chat", "reload", "--id", "c", "--client-id", "owner", "--data-dir", dir, "--if-revision", "1"}
	if err = run(append(append([]string{}, args...), "--dry-run")); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get("owner", "c")
	if err != nil || before.Revision != 1 {
		t.Fatalf("dry run wrote: %+v %v", before, err)
	}
	if _, err = os.Stat(filepath.Join(dir, ".backups")); !os.IsNotExist(err) {
		t.Fatalf("dry run created backup: %v", err)
	}
	if err = run(args); err != nil {
		t.Fatal(err)
	}
	if err = run(args); !errors.Is(err, chatstore.ErrConflict) {
		t.Fatalf("stale reload: %v", err)
	}
	after, err := store.Get("owner", "c")
	if err != nil || after.Revision != 2 || !bytes.Contains(after.Conversation, []byte("hand edit")) {
		t.Fatalf("reload: %+v %v", after, err)
	}
	if err = os.WriteFile(path, []byte(`{"bad":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"chat", "reload", "--id", "c", "--client-id", "owner", "--data-dir", dir, "--if-revision", "2"}); err == nil {
		t.Fatal("reloaded corrupt JSON")
	}
}

func TestChatMutationsRespectActiveJobLease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	store := chatstore.New(dir)
	raw := json.RawMessage(`{"id":"c","title":"original","created_at":1,"updated_at":2,"messages":[]}`)
	if _, err := store.Save("c", "owner", "create", 0, raw); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "conversation.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.TryJobLease("c")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	base := []string{"--id", "c", "--client-id", "owner", "--data-dir", dir, "--if-revision", "1"}
	for _, action := range []string{"reload", "import", "delete"} {
		args := append([]string{"chat", action}, base...)
		if action == "import" {
			args = append(args, "--file", file)
		}
		if err := run(args); !errors.Is(err, chatstore.ErrJobBusy) {
			t.Fatalf("%s bypassed job lease: %v", action, err)
		}
	}
	snap, err := store.Get("owner", "c")
	if err != nil || snap.Revision != 1 || !chatstore.Equal(snap.Conversation, raw) {
		t.Fatalf("busy mutation changed chat: %+v %v", snap, err)
	}
}
