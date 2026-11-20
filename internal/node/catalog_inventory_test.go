package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/starfall-org/serverside-generative/internal/rpc"
)

func TestInventoryOnUninitializedServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	n, err := New(Config{IdentityDir: t.TempDir(), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	checkEmpty := func() {
		t.Helper()
		for op, want := range map[string]string{
			"provider.catalog": `{"version":1,"revision":0,"providers":[]}`,
			"mcp.catalog":      `[]`,
		} {
			reply := n.rpc.Dispatch("owner", rpc.Request{ID: "inventory", Op: op})
			data, err := json.Marshal(reply.Data)
			if !reply.OK || err != nil || string(data) != want {
				t.Fatalf("%s: reply=%+v, data=%s, marshal error=%v", op, reply, data, err)
			}
		}
	}
	checkEmpty()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inventory created the data directory: %v", err)
	}

	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	checkEmpty()
	if err := os.WriteFile(filepath.Join(dir, "providers.yaml"), []byte("providers: []"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"provider.catalog", "mcp.catalog"} {
		reply := n.rpc.Dispatch("owner", rpc.Request{ID: "inventory", Op: op})
		if reply.OK || reply.Error == nil || reply.Error.Code != "invalid_request" {
			t.Fatalf("%s accepted an incomplete catalog: %+v", op, reply)
		}
	}
}
