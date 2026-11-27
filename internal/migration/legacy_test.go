package migration

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/starfall-org/generative-server/internal/catalog"
	"github.com/starfall-org/generative-server/internal/execution"
)

func TestMigrationDryRunBackupAndRetry(t *testing.T) {
	base := t.TempDir()
	identity := filepath.Join(base, "identity")
	data := filepath.Join(base, "data")
	if err := os.Mkdir(identity, 0700); err != nil {
		t.Fatal(err)
	}
	old := legacy{Revision: 1, Context: execution.ClientContext{Instructions: "retain instructions", Skills: []execution.AgentSkill{{ID: "s", Name: "Skill", Content: "body"}}, Providers: []execution.ProviderMetadata{{ID: "p", Name: "Provider", Platform: "openai-chat", Models: []string{"org/model"}}}}}
	raw, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(identity, "shared-config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if r, err := Migrate(identity, data, true); err != nil || r.Providers != 1 || r.Skills != 1 {
		t.Fatalf("preview: %+v %v", r, err)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run wrote data: %v", err)
	}
	if _, err := Migrate(identity, data, false); err != nil {
		t.Fatal(err)
	}
	if r, err := Migrate(identity, data, false); err != nil || !r.AlreadyApplied {
		t.Fatalf("retry: %+v %v", r, err)
	}
	if b, err := os.ReadFile(filepath.Join(identity, "shared-config.json")); err != nil || string(b) != string(raw) {
		t.Fatalf("legacy changed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(data, ".backups"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("backup: %v %v", entries, err)
	}
	if _, err = catalog.New(data).Get(); err != nil {
		t.Fatal(err)
	}
}
