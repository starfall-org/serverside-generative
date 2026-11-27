// Package catalog validates and atomically publishes nonsecret YAML catalogs.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/starfall-org/generative-server/internal/compat"
	"go.yaml.in/yaml/v4"
)

var files = []string{"providers.yaml", "models.yaml", "mcp.yaml", "profiles.yaml"}
var ErrConflict = errors.New("catalog revision conflict")
var ErrInvalid = errors.New("invalid catalog")
var gate sync.Mutex

const maxFile = 4 << 20

type Manifest struct {
	SchemaVersion int               `json:"schema_version"`
	Revision      uint64            `json:"revision"`
	Checksums     map[string]string `json:"checksums"`
}
type Bundle struct {
	Manifest Manifest          `json:"manifest"`
	Files    map[string]string `json:"files"`
}
type Store struct{ dir string }

func New(dir string) *Store  { return &Store{dir: dir} }
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func read(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, ErrInvalid
	}
	return b, nil
}
func (s *Store) load() (Bundle, error) {
	data, err := read(filepath.Join(s.dir, "catalog-manifest.json"), 64<<10)
	if err != nil {
		return Bundle{}, err
	}
	var m Manifest
	if json.Unmarshal(data, &m) != nil || m.SchemaVersion != 1 || len(m.Checksums) != len(files) {
		return Bundle{}, ErrInvalid
	}
	b := Bundle{Manifest: m, Files: map[string]string{}}
	for _, name := range files {
		raw, err := read(filepath.Join(s.dir, name), maxFile)
		if errors.Is(err, os.ErrNotExist) {
			return Bundle{}, ErrInvalid
		}
		if err != nil {
			return Bundle{}, err
		}
		if digest(raw) != m.Checksums[name] {
			return Bundle{}, ErrConflict
		}
		b.Files[name] = string(raw)
	}
	if err = Validate(b.Files, m.Revision); err != nil {
		return Bundle{}, err
	}
	return b, nil
}
func (s *Store) Get() (Bundle, error) {
	gate.Lock()
	defer gate.Unlock()
	if err := s.checkDir(false); err != nil {
		return Bundle{}, err
	}
	unlock, err := lockDirectory(s.dir, true)
	if err != nil {
		return Bundle{}, err
	}
	defer unlock()
	if err := s.recover(); err != nil {
		return Bundle{}, err
	}
	return s.load()
}

// Ensure initializes a truly empty catalog and recovers committed transactions.
// An incomplete or corrupted catalog must not be replaced by empty defaults.
func (s *Store) Ensure() (Bundle, error) {
	current, err := s.Get()
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return current, err
	}
	existing, err := s.PreviewInventory()
	if err != nil {
		return Bundle{}, err
	}
	if existing.Files != nil {
		return existing, nil
	}
	docs, err := Empty()
	if err != nil {
		return Bundle{}, err
	}
	current, err = s.Save(0, docs)
	if errors.Is(err, ErrConflict) {
		return s.Get()
	}
	return current, err
}

// Save uses a recoverable, fsynced transaction directory and publishes the
// manifest last and serializes catalog writers across processes.
func (s *Store) Save(expected uint64, docs map[string]string) (Bundle, error) {
	gate.Lock()
	defer gate.Unlock()
	if expected == ^uint64(0) {
		return Bundle{}, ErrInvalid
	}
	if err := Validate(docs, expected+1); err != nil {
		return Bundle{}, err
	}
	if err := s.checkDir(true); err != nil {
		return Bundle{}, err
	}
	unlock, err := lockDirectory(s.dir, true)
	if err != nil {
		return Bundle{}, err
	}
	defer unlock()
	if err := s.recover(); err != nil {
		return Bundle{}, err
	}
	old, err := s.load()
	if errors.Is(err, os.ErrNotExist) {
		if expected != 0 {
			return Bundle{}, ErrConflict
		}
		for _, name := range files {
			if _, e := os.Lstat(filepath.Join(s.dir, name)); e == nil || !errors.Is(e, os.ErrNotExist) {
				return Bundle{}, ErrConflict
			}
		}
	} else if err != nil {
		return Bundle{}, err
	} else if old.Manifest.Revision != expected {
		return Bundle{}, ErrConflict
	}

	tx := filepath.Join(s.dir, ".catalog-transaction")
	if err = os.Mkdir(tx, 0700); err != nil {
		return Bundle{}, err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(tx)
		}
	}()
	manifest := Manifest{SchemaVersion: 1, Revision: expected + 1, Checksums: map[string]string{}}
	for _, name := range files {
		data := []byte(docs[name])
		manifest.Checksums[name] = digest(data)
		if err = writeFile(filepath.Join(tx, name), data); err != nil {
			return Bundle{}, err
		}
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	raw = append(raw, '\n')
	if err = writeFile(filepath.Join(tx, "catalog-manifest.json"), raw); err != nil {
		return Bundle{}, err
	}
	if err = syncDir(tx); err != nil {
		return Bundle{}, err
	}
	if err = writeFile(filepath.Join(tx, "READY"), []byte("1\n")); err != nil {
		return Bundle{}, err
	}
	if err = syncDir(tx); err != nil {
		return Bundle{}, err
	}
	if err = syncDir(s.dir); err != nil {
		return Bundle{}, err
	}
	committed = true
	if err = s.recover(); err != nil {
		return Bundle{}, err
	}
	return s.load()
}
func writeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	e := f.Close()
	if err == nil {
		err = e
	}
	return err
}
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	err = d.Sync()
	e := d.Close()
	if err == nil {
		err = e
	}
	return err
}
func (s *Store) recover() error {
	tx := filepath.Join(s.dir, ".catalog-transaction")
	info, err := os.Lstat(tx)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrInvalid
	}
	if _, err = read(filepath.Join(tx, "READY"), 32); errors.Is(err, os.ErrNotExist) {
		return os.RemoveAll(tx)
	} else if err != nil {
		return err
	}
	data, err := read(filepath.Join(tx, "catalog-manifest.json"), 64<<10)
	if err != nil {
		return err
	}
	var m Manifest
	if json.Unmarshal(data, &m) != nil || m.SchemaVersion != 1 || len(m.Checksums) != len(files) {
		return ErrInvalid
	}
	docs := map[string]string{}
	for _, name := range files {
		b, err := read(filepath.Join(tx, name), maxFile)
		if err != nil || digest(b) != m.Checksums[name] {
			return ErrInvalid
		}
		docs[name] = string(b)
	}
	if err = Validate(docs, m.Revision); err != nil {
		return err
	}
	for _, name := range append(append([]string{}, files...), "catalog-manifest.json") {
		raw, err := read(filepath.Join(tx, name), maxFile)
		if err != nil {
			return err
		}
		f, err := os.CreateTemp(s.dir, ".catalog-publish-*")
		if err != nil {
			return err
		}
		temp := f.Name()
		if err = f.Chmod(0600); err == nil {
			_, err = f.Write(raw)
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(temp)
			return err
		}
		if err = os.Rename(temp, filepath.Join(s.dir, name)); err != nil {
			os.Remove(temp)
			return err
		}
	}
	if err = syncDir(s.dir); err != nil {
		return err
	}
	if err = os.RemoveAll(tx); err != nil {
		return err
	}
	return syncDir(s.dir)
}

// Validate rejects ambiguous YAML, credential-bearing keys and private URLs.
// It conservatively rejects header values because they may contain secrets.
func Validate(docs map[string]string, revision uint64) error {
	if len(docs) != len(files) {
		return ErrInvalid
	}
	for _, name := range files {
		raw, ok := docs[name]
		if !ok || len(raw) == 0 || len(raw) > maxFile {
			return ErrInvalid
		}
		dec := yaml.NewDecoder(strings.NewReader(raw))
		var root yaml.Node
		if dec.Decode(&root) != nil {
			return ErrInvalid
		}
		var extra yaml.Node
		if err := dec.Decode(&extra); err != io.EOF {
			return ErrInvalid
		}
		if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode || validateNode(root.Content[0], 0, "") != nil {
			return ErrInvalid
		}
		var hdr struct {
			SchemaVersion int    `yaml:"schema_version"`
			Revision      uint64 `yaml:"revision"`
		}
		if root.Decode(&hdr) != nil || hdr.SchemaVersion != 1 || hdr.Revision != revision {
			return ErrInvalid
		}
	}
	return validateRelations(docs)
}
func validateNode(n *yaml.Node, depth int, parent string) error {
	if depth > 64 || n.Kind == yaml.AliasNode || n.Anchor != "" || strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
		return ErrInvalid
	}
	switch n.Kind {
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || seen[k.Value] || k.Value == "<<" {
				return ErrInvalid
			}
			seen[k.Value] = true
			key := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k.Value))
			switch key {
			case "apikey", "accesstoken", "refreshtoken", "clientsecret", "password", "authorization", "encryptedcredential", "oauthclientsecret", "marker", "secret", "token", "credential", "credentials", "bearertoken", "runtime":
				if v.Tag != "!!null" && !(v.Kind == yaml.ScalarNode && v.Value == "") {
					return ErrInvalid
				}
			}
			if key == "headers" && len(v.Content) > 0 {
				return ErrInvalid
			}
			// Request body templates may contain literal credentials; until
			// encrypted body references exist, accept only empty bodies.
			if key == "body" && v.Kind == yaml.ScalarNode && v.Tag != "!!null" && v.Value != "" && v.Value != "{}" && v.Value != "[]" {
				return ErrInvalid
			}
			looksURL := v.Kind == yaml.ScalarNode && (strings.HasPrefix(strings.ToLower(v.Value), "http://") || strings.HasPrefix(strings.ToLower(v.Value), "https://"))
			if (looksURL || strings.HasSuffix(key, "url") || key == "endpoint" || key == "httpproxy" || key == "socksproxy") && v.Kind == yaml.ScalarNode && v.Tag != "!!null" && v.Value != "" {
				u, err := url.Parse(v.Value)
				if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && key != "socksproxy") {
					return ErrInvalid
				}
			}
			if (parent == "auth" || parent == "apikeys") && (key == "value" || key == "key" || key == "marker") && v.Tag != "!!null" && v.Value != "" {
				return ErrInvalid
			}
			if err := validateNode(v, depth+1, key); err != nil {
				return err
			}
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, v := range n.Content {
			if err := validateNode(v, depth+1, parent); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if len(n.Value) > maxFile {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// validateRelations checks the catalog's principal ID and provider links.
func validateRelations(docs map[string]string) error {
	var providers struct {
		Groups []struct {
			ID string `yaml:"id"`
		} `yaml:"groups"`
		Providers []struct {
			ID      string  `yaml:"id"`
			GroupID *string `yaml:"groupId"`
		} `yaml:"providers"`
	}
	if yaml.Unmarshal([]byte(docs["providers.yaml"]), &providers) != nil {
		return ErrInvalid
	}
	groups := map[string]bool{}
	for _, g := range providers.Groups {
		if g.ID == "" || groups[g.ID] {
			return ErrInvalid
		}
		groups[g.ID] = true
	}
	ids := map[string]bool{}
	for _, p := range providers.Providers {
		if p.ID == "" || ids[p.ID] || (p.GroupID != nil && *p.GroupID != "" && !groups[*p.GroupID]) {
			return ErrInvalid
		}
		ids[p.ID] = true
	}
	var models struct {
		Providers map[string]any `yaml:"providers"`
	}
	if yaml.Unmarshal([]byte(docs["models.yaml"]), &models) != nil {
		return ErrInvalid
	}
	for id := range models.Providers {
		if !ids[id] {
			return ErrInvalid
		}
	}
	var mcp struct {
		Servers []struct {
			ID       string `yaml:"id"`
			Protocol string `yaml:"protocol"`
		} `yaml:"servers"`
	}
	if yaml.Unmarshal([]byte(docs["mcp.yaml"]), &mcp) != nil {
		return ErrInvalid
	}
	servers := map[string]bool{}
	for _, v := range mcp.Servers {
		if v.ID == "" || servers[v.ID] || (v.Protocol != "streamable_http" && v.Protocol != "sse") {
			return ErrInvalid
		}
		servers[v.ID] = true
	}
	return validateMobileCatalogMetadata(docs)
}

// validateMobileCatalogMetadata reuses Kotlin-compatible validation for YAML
// imports; a catalog write cannot silently introduce invalid mobile profiles.
func validateMobileCatalogMetadata(docs map[string]string) error {
	var providers struct {
		Groups    []map[string]any `yaml:"groups"`
		Providers []map[string]any `yaml:"providers"`
	}
	if yaml.Unmarshal([]byte(docs["providers.yaml"]), &providers) != nil {
		return ErrInvalid
	}
	for _, group := range providers.Groups {
		raw, err := json.Marshal(group)
		if err != nil {
			return ErrInvalid
		}
		if _, err = compat.ParseMobileProviderGroup(raw); err != nil {
			return ErrInvalid
		}
	}
	for _, provider := range providers.Providers {
		// Legacy YAML did not require this Kotlin field. Validate a
		// projection with an empty URL, without rewriting existing data.
		if _, ok := provider["baseUrl"]; !ok {
			provider["baseUrl"] = ""
		}
		raw, err := json.Marshal(provider)
		if err != nil {
			return ErrInvalid
		}
		if _, err = compat.ParseMobileProvider(raw); err != nil {
			return ErrInvalid
		}
	}
	var models struct {
		Providers map[string]struct {
			ModelConfigs map[string]map[string]any `yaml:"modelConfigs"`
			Catalog      []map[string]any          `yaml:"catalog"`
		} `yaml:"providers"`
	}
	if yaml.Unmarshal([]byte(docs["models.yaml"]), &models) != nil {
		return ErrInvalid
	}
	for providerID, group := range models.Providers {
		for id, config := range group.ModelConfigs {
			if id == "" {
				return ErrInvalid
			}
			raw, err := json.Marshal(config)
			if err != nil {
				return ErrInvalid
			}
			if _, err = compat.ParseMobileModelConfiguration(raw); err != nil {
				return ErrInvalid
			}
		}
		for _, model := range group.Catalog {
			raw, err := json.Marshal(model)
			if err != nil {
				return ErrInvalid
			}
			parsed, err := compat.ParseMobileLanguageModel(raw)
			if err != nil {
				return ErrInvalid
			}
			fields, _ := parsed.Fields()
			if fields.ProviderID != providerID {
				return ErrInvalid
			}
		}
	}
	var mcp struct {
		Servers []map[string]any `yaml:"servers"`
	}
	if yaml.Unmarshal([]byte(docs["mcp.yaml"]), &mcp) != nil {
		return ErrInvalid
	}
	for _, server := range mcp.Servers {
		raw, err := json.Marshal(server)
		if err != nil {
			return ErrInvalid
		}
		if _, err = compat.ParseMobileMCP(raw); err != nil {
			return ErrInvalid
		}
	}
	var profiles struct {
		ChatProfiles  []map[string]any                     `yaml:"chatProfiles"`
		MediaProfiles map[string]map[string]map[string]any `yaml:"mediaProfiles"`
		ToolSettings  map[string]any                       `yaml:"toolSettings"`
	}
	if yaml.Unmarshal([]byte(docs["profiles.yaml"]), &profiles) != nil {
		return ErrInvalid
	}
	if profiles.ToolSettings != nil {
		raw, err := json.Marshal(profiles.ToolSettings)
		if err != nil {
			return ErrInvalid
		}
		settings, err := compat.ParseMobileToolSettings(raw)
		if err != nil {
			return ErrInvalid
		}
		if _, err = settings.StoredMetadataJSON(); err != nil {
			return ErrInvalid
		}
	}
	seenChat := map[string]bool{}
	for _, entry := range profiles.ChatProfiles {
		raw, err := json.Marshal(entry)
		if err != nil {
			return ErrInvalid
		}
		profile, err := compat.ParseMobileChatProfile(raw)
		if err != nil {
			return ErrInvalid
		}
		f, _ := profile.Fields()
		if seenChat[f.ID] {
			return ErrInvalid
		}
		seenChat[f.ID] = true
	}
	for kind, items := range profiles.MediaProfiles {
		if kind != "image" && kind != "video" && kind != "music" {
			return ErrInvalid
		}
		for id, entry := range items {
			raw, err := json.Marshal(entry)
			if err != nil {
				return ErrInvalid
			}
			profile, err := compat.ParseMobileMediaProfile(raw)
			if err != nil {
				return ErrInvalid
			}
			f, _ := profile.Fields()
			if f.ID != id || (kind == "music" && f.Mode != "custom") ||
				(f.Mode == "default" && (kind == "music" || id != kind+"gen-default")) {
				return ErrInvalid
			}
			if kind == "music" {
				if len(f.Chain.Steps) == 0 || len(f.Chain.Steps) > 20 ||
					(f.Chain.FinalResponse.Type != "audio" && f.Chain.FinalResponse.Type != "json") ||
					(f.Chain.FinalResponse.Type == "json" && f.Chain.FinalResponse.MediaPath == "") {
					return ErrInvalid
				}
				for _, step := range f.Chain.Steps {
					u, err := url.Parse(step.Endpoint)
					if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
						return ErrInvalid
					}
				}
			}
			if f.EncryptedCredential != "" {
				return ErrInvalid
			}
			if _, err = profile.StoredMetadataJSON(); err != nil {
				return ErrInvalid
			}
		}
	}
	return nil
}

// Preview reads a committed catalog without attempting crash recovery or writes.
func (s *Store) Preview() (Bundle, error) {
	return s.preview(false)
}

// PreviewInventory returns a zero bundle for a catalog that has never been initialized.
func (s *Store) PreviewInventory() (Bundle, error) {
	return s.preview(true)
}

func (s *Store) preview(allowEmpty bool) (Bundle, error) {
	gate.Lock()
	defer gate.Unlock()
	if err := s.checkDir(false); err != nil {
		if allowEmpty && errors.Is(err, os.ErrNotExist) {
			return Bundle{}, nil
		}
		return Bundle{}, err
	}
	unlock, err := lockDirectory(s.dir, false)
	if err != nil {
		return Bundle{}, err
	}
	defer unlock()
	if _, err := os.Lstat(filepath.Join(s.dir, ".catalog-transaction")); err == nil {
		return Bundle{}, ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return Bundle{}, err
	}
	bundle, err := s.load()
	if !allowEmpty || !errors.Is(err, os.ErrNotExist) {
		return bundle, err
	}
	// A published manifest or a document without its manifest is an incomplete catalog.
	for _, name := range append([]string{"catalog-manifest.json"}, files...) {
		if _, err := os.Lstat(filepath.Join(s.dir, name)); err == nil {
			return Bundle{}, ErrInvalid
		} else if !errors.Is(err, os.ErrNotExist) {
			return Bundle{}, err
		}
	}
	return Bundle{}, nil
}

func (s *Store) checkDir(create bool) error {
	if create {
		if err := os.MkdirAll(s.dir, 0700); err != nil {
			return err
		}
	}
	info, err := os.Lstat(s.dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrInvalid
	}
	return nil
}
