package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/starfall-org/serverside-generative/internal/execution"
)

// SharedConfig is global for this personal single-user server, not per client.
// It contains only explicitly whitelisted, non-secret metadata.
type SharedConfig struct {
	Revision uint64                  `json:"revision"`
	Context  execution.ClientContext `json:"context"`
}
type syncedContext struct {
	revision uint64
	digest   string
	value    execution.ClientContext
}

func (n *Node) sharedPath() string { return filepath.Join(n.Config.IdentityDir, "shared-config.json") }
func canonicalContext(snapshot execution.ClientContext) ([]byte, error) {
	return json.Marshal(snapshot)
}
func parseContext(raw json.RawMessage) (execution.ClientContext, error) {
	var snapshot execution.ClientContext
	if len(raw) == 0 || len(raw) > execution.MaxContextSize {
		return snapshot, errors.New("missing or oversized context snapshot")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snapshot); err != nil {
		return snapshot, errors.New("invalid context snapshot")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return snapshot, errors.New("context must contain one JSON object")
	}
	if err := snapshot.Validate(); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}
func digestContext(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (n *Node) loadSharedConfig() error {
	path := n.sharedPath()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		empty := execution.ClientContext{Instructions: "", Skills: []execution.AgentSkill{}, Providers: []execution.ProviderMetadata{}}
		data, _ := canonicalContext(empty)
		n.shared = syncedContext{revision: 0, digest: digestContext(data), value: empty}
		return nil
	}
	if err != nil {
		return err
	}
	var stored SharedConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&stored); err != nil {
		return errors.New("corrupted shared configuration; restore or remove shared-config.json")
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return errors.New("corrupted shared configuration: trailing data")
	}
	data, err := canonicalContext(stored.Context)
	if err != nil {
		return err
	}
	verified, err := parseContext(data)
	if err != nil {
		return errors.New("corrupted shared configuration: invalid fields")
	}
	if stored.Revision == 0 {
		return errors.New("corrupted shared configuration: invalid revision")
	}
	n.shared = syncedContext{revision: stored.Revision, digest: digestContext(data), value: verified.Clone()}
	return nil
}
func (n *Node) beginClientConnection(owner string) uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nextConnectionEpoch++
	n.activeConnection[owner] = n.nextConnectionEpoch
	return n.nextConnectionEpoch
}
func (n *Node) connectionCurrent(owner string, epoch uint64) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, paired := n.pairs[owner]
	return paired && n.activeConnection[owner] == epoch
}
func (n *Node) contextSummary() (uint64, string, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.shared.revision, n.shared.digest, len(n.shared.value.Skills)
}
func (n *Node) clientContext(_ string) execution.ClientContext {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.shared.value.Clone()
}
func (n *Node) contextSnapshot() SharedConfig {
	n.mu.Lock()
	defer n.mu.Unlock()
	return SharedConfig{Revision: n.shared.revision, Context: n.shared.value.Clone()}
}
func (n *Node) confirmContext(owner string, epoch, revision uint64, digest string) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.pairs[owner]; !ok || n.activeConnection[owner] != epoch {
		return 0, errors.New("client no longer authenticated")
	}
	if revision != n.shared.revision || digest != n.shared.digest {
		return 0, errors.New("stale shared context: fetch latest revision")
	}
	return len(n.shared.value.Skills), nil
}

// applyContext performs an optimistic concurrency check against the GLOBAL
// revision so a reconnecting stale device cannot overwrite other clients.
// The data is written atomically before modifying RAM.
func (n *Node) applyContext(owner string, epoch, expected uint64, raw json.RawMessage) (uint64, string, int, error) {
	snapshot, err := parseContext(raw)
	if err != nil {
		return 0, "", 0, err
	}
	encoded, err := canonicalContext(snapshot)
	if err != nil {
		return 0, "", 0, errors.New("invalid configuration")
	}
	if len(encoded) > execution.MaxContextSize {
		return 0, "", 0, errors.New("configuration exceeds size limit")
	}
	digest := digestContext(encoded)
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.pairs[owner]; !ok || n.activeConnection[owner] != epoch {
		return 0, "", 0, errors.New("client no longer authenticated")
	}
	if expected != n.shared.revision {
		return n.shared.revision, n.shared.digest, 0, errors.New("shared context revision conflict; fetch latest revision")
	}
	if digest == n.shared.digest {
		return n.shared.revision, n.shared.digest, len(n.shared.value.Skills), nil
	}
	next := n.shared.revision + 1
	if next == 0 {
		return 0, "", 0, errors.New("shared context revision exhausted")
	}
	data, err := json.Marshal(SharedConfig{Revision: next, Context: snapshot})
	if err != nil {
		return 0, "", 0, errors.New("configuration serialization failed")
	}
	if err = atomicPrivateWrite(n.sharedPath(), data); err != nil {
		return 0, "", 0, errors.New("shared context storage write failed")
	}
	n.shared = syncedContext{revision: next, digest: digest, value: snapshot.Clone()}
	return next, digest, len(snapshot.Skills), nil
}
