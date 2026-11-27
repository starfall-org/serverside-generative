// Package migration provides explicit, backup-first legacy metadata import.
package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/starfall-org/generative-server/internal/catalog"
	"github.com/starfall-org/generative-server/internal/execution"
	"go.yaml.in/yaml/v4"
)

type Report struct {
	Providers      int
	Skills         int
	AlreadyApplied bool
}
type legacy struct {
	Revision uint64                  `json:"revision"`
	Context  execution.ClientContext `json:"context"`
}

func Migrate(identityDir, dataDir string, dryRun bool) (Report, error) {
	raw, err := os.ReadFile(filepath.Join(identityDir, "shared-config.json"))
	if err != nil {
		return Report{}, err
	}
	if len(raw) > execution.MaxContextSize+4096 {
		return Report{}, errors.New("legacy configuration too large")
	}
	var old legacy
	if json.Unmarshal(raw, &old) != nil || old.Revision == 0 || old.Context.Validate() != nil {
		return Report{}, errors.New("invalid legacy configuration")
	}
	report := Report{Providers: len(old.Context.Providers), Skills: len(old.Context.Skills)}
	providers := make([]map[string]any, 0, len(old.Context.Providers))
	models := map[string]any{}
	aliases := map[string]string{"openai-chat": "openai", "openai-responses": "openai_responses", "anthropic": "anthropic", "gemini": "google"}
	for _, p := range old.Context.Providers {
		typ := aliases[p.Platform]
		if typ == "" {
			typ = p.Platform
		}
		providers = append(providers, map[string]any{"id": p.ID, "name": p.Name, "type": typ, "baseUrl": p.BaseURL, "auth": map[string]any{"method": "platform_default"}, "config": map[string]any{}, "sortOrder": len(providers), "isHidden": false})
		models[p.ID] = map[string]any{"modelIds": p.Models, "modelConfigs": map[string]any{}, "catalog": []any{}}
	}
	values := map[string]any{"providers.yaml": map[string]any{"schema_version": 1, "revision": 1, "groups": []any{}, "providers": providers}, "models.yaml": map[string]any{"schema_version": 1, "revision": 1, "providers": models}, "mcp.yaml": map[string]any{"schema_version": 1, "revision": 1, "servers": []any{}}, "profiles.yaml": map[string]any{"schema_version": 1, "revision": 1, "chatProfiles": []any{}, "mediaProfiles": map[string]any{"image": map[string]any{}, "video": map[string]any{}, "music": map[string]any{}}, "toolSettings": map[string]any{}}}
	docs := map[string]string{}
	for name, value := range values {
		b, err := yaml.Marshal(value)
		if err != nil {
			return Report{}, err
		}
		docs[name] = string(b)
	}
	if err = catalog.Validate(docs, 1); err != nil {
		return Report{}, err
	}
	store := catalog.New(dataDir)
	var existing catalog.Bundle
	if dryRun {
		existing, err = store.Preview()
	} else {
		existing, err = store.Get()
	}
	if err == nil {
		if existing.Manifest.Revision != 1 || len(existing.Files) != len(docs) {
			return Report{}, catalog.ErrConflict
		}
		for k, v := range docs {
			if existing.Files[k] != v {
				return Report{}, catalog.ErrConflict
			}
		}
		report.AlreadyApplied = true
		return report, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Report{}, err
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Report{}, err
	}
	for _, e := range entries {
		if e.Name() != ".backups" && !(e.Name() == "identity" && e.IsDir() && filepath.Clean(identityDir) == filepath.Join(dataDir, "identity")) {
			return Report{}, errors.New("data directory is not empty; migration requires a backup of existing data")
		}
	}
	if dryRun {
		return report, nil
	}
	if err = os.MkdirAll(dataDir, 0700); err != nil {
		return Report{}, err
	}
	root, err := os.Lstat(dataDir)
	if err != nil || !root.IsDir() || root.Mode().Perm()&0077 != 0 {
		return Report{}, errors.New("unsafe data directory")
	}
	backupDir := filepath.Join(dataDir, ".backups")
	if err = os.Mkdir(backupDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return Report{}, err
	}
	st, err := os.Lstat(backupDir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return Report{}, errors.New("unsafe backup directory")
	}
	sum := sha256.Sum256(raw)
	backup := filepath.Join(backupDir, "shared-config-"+hex.EncodeToString(sum[:])+".json")
	f, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		info, e := os.Lstat(backup)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return Report{}, errors.New("unsafe backup")
		}
		b, e := os.ReadFile(backup)
		if e != nil || sha256.Sum256(b) != sum {
			return Report{}, errors.New("backup mismatch")
		}
	} else if err != nil {
		return Report{}, err
	} else {
		if _, err = f.Write(raw); err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return Report{}, err
		}
		d, e := os.Open(backupDir)
		if e != nil {
			return Report{}, e
		}
		e = d.Sync()
		d.Close()
		if e != nil {
			return Report{}, e
		}
	}
	_, err = store.Save(0, docs)
	return report, err
}
