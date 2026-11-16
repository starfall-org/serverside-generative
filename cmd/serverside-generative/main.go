package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/starfall-org/serverside-generative/internal/catalog"
	"github.com/starfall-org/serverside-generative/internal/chatsession"
	"github.com/starfall-org/serverside-generative/internal/chatstore"
	"github.com/starfall-org/serverside-generative/internal/httptool"
	"github.com/starfall-org/serverside-generative/internal/migration"
	"github.com/starfall-org/serverside-generative/internal/node"
	"github.com/starfall-org/serverside-generative/internal/secrets"
	"go.yaml.in/yaml/v4"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
func usage() {
	fmt.Fprintln(os.Stderr, "serverside: paired session runtime")
	fmt.Fprintln(os.Stderr, "Commands: serve, handshake --stdio, pair, stream, manage, version, config, provider, model, mcp, profile, tool, chat")
}
func defaultIPC() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "serverside-generative", "control.sock")
}
func dataDirectory(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "serverside"), nil
}
func editCatalog(dir, revision string, dry bool, edit func(map[string]*yaml.Node) error) error {
	if revision == "" {
		return fmt.Errorf("--if-revision is required")
	}
	expected, err := strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid revision")
	}
	path, err := dataDirectory(dir)
	if err != nil {
		return err
	}
	result, err := catalog.New(path).Update(expected, dry, edit)
	if err != nil {
		return err
	}
	fmt.Printf("Catalog revision %d (dry run: %t)\n", result.Manifest.Revision, dry)
	return nil
}

func defaultDataSocket() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "serverside-generative", "run", "data.sock")
}

// bridgeStream copies binary frames without writing diagnostics to stdout.
func bridgeStream(socketPath string, input io.Reader, output io.Writer) error {
	conn, err := node.DialDataSocket(socketPath)
	if err != nil {
		return fmt.Errorf("server_unavailable: daemon data socket unavailable")
	}
	defer conn.Close()
	fromClient := make(chan error, 1)
	fromServer := make(chan error, 1)
	go func() {
		_, e := io.CopyBuffer(conn, input, make([]byte, 32<<10))
		if c, ok := conn.(*net.UnixConn); ok {
			_ = c.CloseWrite()
		}
		fromClient <- e
	}()
	go func() { _, e := io.CopyBuffer(output, conn, make([]byte, 32<<10)); fromServer <- e }()
	select {
	case e := <-fromServer:
		return e
	case e := <-fromClient:
		if e != nil {
			return e
		}
		return <-fromServer
	}
}
func printHuman(h node.HumanInvitation) {
	fmt.Println("\n=== Human Relay (2-minute one-time pairing) ===")
	fmt.Println("Endpoint:       ", h.Endpoint)
	fmt.Println("Manual PIN:     ", h.PIN)
	fmt.Println("TLS SHA-256:    ", h.Fingerprint)
	fmt.Println("Invite URI:     ", h.URI)
	qrterminal.Generate(h.URI, qrterminal.L, os.Stdout)
	fmt.Println("Note: This does not forward traffic or bypass NAT.")
}
func run(args []string) error {
	if len(args) == 0 {
		usage()
		return flag.ErrHelp
	}
	switch args[0] {
	case "stream":
		fs := flag.NewFlagSet("stream", flag.ContinueOnError)
		stdio := fs.Bool("stdio", false, "bridge framed v5 data over stdin/stdout")
		path := fs.String("data-socket", defaultDataSocket(), "daemon private data socket")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if !*stdio || fs.NArg() != 0 {
			return fmt.Errorf("use stream --stdio")
		}
		return bridgeStream(*path, os.Stdin, os.Stdout)
	case "manage":
		return runManage(args[1:], os.Stdin, os.Stdout)
	case "version":
		fmt.Println("serverside", version)
		return nil
	case "handshake", "ticket", "pair":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		ipc := fs.String("ipc", defaultIPC(), "daemon Unix socket")
		stdio := fs.Bool("stdio", false, "write one JSON invitation to stdout (for ssh remote command)")
		transport := fs.String("transport", "", "pairing transport: tcp, http, websocket or http-local")
		endpoint := fs.String("endpoint", "", "client-visible HTTPS base URL")
		pin := fs.String("pin", "", "client-visible TLS SPKI pin (sha256/...)")
		cancelPairing := fs.Bool("cancel", false, "cancel pending HTTP/WS pairing codes")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected pairing arguments")
		}
		method := "ticket"
		if args[0] == "pair" {
			method = "human"
		}
		if *transport != "" {
			if args[0] != "pair" {
				return fmt.Errorf("transport is supported only by pair")
			}
			switch *transport {
			case "tcp":
				if !*stdio || *endpoint != "" || *pin != "" || *cancelPairing {
					return fmt.Errorf("use pair --stdio --transport tcp")
				}
				method = "pair_tcp"
			case "http-local":
				if !*stdio || *endpoint != "" || *pin != "" || *cancelPairing {
					return fmt.Errorf("use pair --stdio --transport http-local")
				}
				method = "pair_http_local"
			case "http", "websocket":
				if *cancelPairing && !*stdio && *endpoint == "" && *pin == "" {
					method = "pair_http_cancel"
				} else {
					if *cancelPairing || *stdio || *endpoint == "" || *pin == "" {
						return fmt.Errorf("use pair --transport http --endpoint HTTPS_URL --pin SPKI_PIN")
					}
					method = "pair_http"
				}
			default:
				return fmt.Errorf("unsupported pairing transport")
			}
		} else if *endpoint != "" || *pin != "" || *cancelPairing {
			return fmt.Errorf("endpoint and pin require HTTP pairing")
		}
		if args[0] == "handshake" && !*stdio {
			return fmt.Errorf("use handshake --stdio over SSH")
		}
		raw, err := node.ControlRequestFields(*ipc, map[string]string{"method": method, "endpoint": *endpoint, "pin": *pin})
		if err != nil {
			return err
		}
		if method == "human" {
			var h node.HumanInvitation
			if err = json.Unmarshal(raw, &h); err != nil {
				return err
			}
			printHuman(h)
		} else if method == "pair_http" {
			var invitation struct {
				URL string `json:"url"`
			}
			if err = json.Unmarshal(raw, &invitation); err != nil || invitation.URL == "" {
				return fmt.Errorf("invalid pairing response")
			}
			fmt.Println("Pairing URL:", invitation.URL)
			qrterminal.Generate(invitation.URL, qrterminal.L, os.Stdout)
		} else {
			fmt.Println(string(raw))
		}
		return nil
	case "chat":
		if len(args) < 2 {
			return fmt.Errorf("use chat list/show/export/previous/import/validate/validate-all/reload/edit/delete")
		}
		action := args[1]
		fs := flag.NewFlagSet("chat "+action, flag.ContinueOnError)
		id := fs.String("id", "", "conversation ID")
		owner := fs.String("client-id", "", "paired owner ID")
		pathArg := fs.String("data-dir", "", "chat data directory")
		file := fs.String("file", "", "conversation JSON to import")
		out := fs.String("out", "", "new output path for export")
		expectedText := fs.String("if-revision", "", "expected revision (0 for new import)")
		operation := fs.String("op-id", "", "idempotency key")
		dry := fs.Bool("dry-run", false, "validate without writing")
		editor := fs.String("editor", os.Getenv("EDITOR"), "editor executable and arguments for chat edit")
		approve := fs.Bool("yes", false, "confirm saving an edited chat")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *owner == "" {
			return fmt.Errorf("--client-id is required")
		}
		path, err := dataDirectory(*pathArg)
		if err != nil {
			return err
		}
		store := chatstore.New(path)
		if action == "validate-all" {
			snapshots, tombstones, err := store.List(*owner)
			if err != nil {
				return err
			}
			fmt.Printf("Validated chat directory; owner has %d chats and %d tombstones\n", len(snapshots), len(tombstones))
			return nil
		}
		if action == "list" {
			manager := chatsession.New(store)
			for offset := 0; ; {
				items, next, err := manager.List(*owner, offset, 100)
				if err != nil {
					return err
				}
				for _, item := range items {
					b, _ := json.Marshal(item)
					fmt.Println(string(b))
				}
				if next == 0 {
					break
				}
				offset = next
			}
			return nil
		}
		if *id == "" {
			return fmt.Errorf("--id is required")
		}
		if action == "previous" {
			snapshot, err := store.Previous(*owner, *id)
			if err != nil {
				return err
			}
			b, err := json.Marshal(snapshot)
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}
		if action == "reload" {
			if *expectedText == "" {
				return fmt.Errorf("--if-revision is required")
			}
			expected, err := strconv.ParseUint(*expectedText, 10, 64)
			if err != nil || expected == 0 {
				return fmt.Errorf("invalid revision")
			}
			snapshot, err := store.Get(*owner, *id)
			if err != nil {
				return err
			}
			if snapshot.Revision != expected {
				return chatstore.ErrConflict
			}
			if *dry {
				fmt.Printf("Chat revision %d is valid (dry run)\n", snapshot.Revision)
				return nil
			}
			if *operation == "" {
				*operation, err = node.RandomToken(18)
				if err != nil {
					return err
				}
			}
			unlock, err := store.TryJobLease(*id)
			if err != nil {
				return err
			}
			defer unlock()
			result, err := store.Reload(*id, *owner, *operation, snapshot)
			if err != nil {
				return err
			}
			fmt.Printf("Reloaded chat revision %d\n", result.Revision)
			return nil
		}
		if action == "edit" {
			if *expectedText == "" {
				return fmt.Errorf("--if-revision is required")
			}
			expected, err := strconv.ParseUint(*expectedText, 10, 64)
			if err != nil || expected == 0 {
				return fmt.Errorf("invalid revision")
			}
			return editSyncedChat(store, *owner, *id, expected, *editor, *dry, *approve)
		}
		if action == "show" || action == "validate" || action == "export" {
			snap, err := store.Get(*owner, *id)
			if err != nil {
				return err
			}
			if action == "validate" {
				fmt.Printf("Valid conversation, revision %d\n", snap.Revision)
				return nil
			}
			if action == "show" {
				fmt.Println(string(snap.Conversation))
				return nil
			}
			if *out == "" {
				return fmt.Errorf("--out is required")
			}
			f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = f.Write(append(snap.Conversation, '\n'))
			if err == nil {
				err = f.Sync()
			}
			e := f.Close()
			if err == nil {
				err = e
			}
			return err
		}
		if action != "import" && action != "delete" {
			return fmt.Errorf("unsupported chat command")
		}
		if *expectedText == "" {
			return fmt.Errorf("--if-revision is required")
		}
		expected, err := strconv.ParseUint(*expectedText, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid revision")
		}
		if *operation == "" {
			*operation, err = node.RandomToken(18)
			if err != nil {
				return err
			}
		}
		if action == "delete" {
			if *dry {
				snap, err := store.Get(*owner, *id)
				if err != nil {
					return err
				}
				if snap.Revision != expected {
					return chatstore.ErrConflict
				}
				fmt.Println("Deletion is valid")
				return nil
			}
			snapshot, inspectErr := store.Get(*owner, *id)
			if inspectErr == nil {
				unlock, err := store.TryJobLease(*id)
				if err != nil {
					return err
				}
				defer unlock()
				_, err = store.DeleteCompared(*owner, *id, *operation, expected, snapshot.Conversation)
			} else if errors.Is(inspectErr, chatstore.ErrDeleted) {
				_, err = store.Delete(*owner, *id, *operation, expected)
			} else {
				return inspectErr
			}
			if err != nil {
				return err
			}
			fmt.Println("Chat deleted")
			return nil
		}
		if *file == "" {
			return fmt.Errorf("--file is required")
		}
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(f, chatstore.MaxConversationBytes+1))
		f.Close()
		if err != nil || chatstore.Validate(*id, raw) != nil {
			return fmt.Errorf("invalid conversation file")
		}
		if *dry {
			snap, err := store.Get(*owner, *id)
			if os.IsNotExist(err) && expected == 0 {
				fmt.Println("Import is valid")
				return nil
			}
			if err != nil {
				return err
			}
			if snap.Revision != expected {
				return chatstore.ErrConflict
			}
			fmt.Println("Import is valid")
			return nil
		}
		var snap chatstore.Snapshot
		if expected == 0 {
			snap, err = store.Save(*id, *owner, *operation, expected, raw)
		} else {
			current, inspectErr := store.Get(*owner, *id)
			if inspectErr != nil {
				return inspectErr
			}
			unlock, lockErr := store.TryJobLease(*id)
			if lockErr != nil {
				return lockErr
			}
			defer unlock()
			snap, err = store.SaveCompared(*id, *owner, *operation, expected, current.Conversation, raw)
		}
		if err != nil {
			return err
		}
		fmt.Printf("Imported revision %d\n", snap.Revision)
		return nil

	case "config":
		if len(args) > 1 && (args[1] == "export" || args[1] == "import" || args[1] == "diff") {
			return runCatalogFileCommand(args[1:])
		}
		if len(args) > 1 && args[1] == "init" {
			fs := flag.NewFlagSet("config init", flag.ContinueOnError)
			dir := fs.String("data-dir", "", "catalog data directory")
			dry := fs.Bool("dry-run", false, "validate without writing")
			if err := fs.Parse(args[2:]); err != nil {
				return err
			}
			docs, err := catalog.Empty()
			if err != nil {
				return err
			}
			if *dry {
				fmt.Println("Initial catalog is valid")
				return nil
			}
			path, err := dataDirectory(*dir)
			if err != nil {
				return err
			}
			b, err := catalog.New(path).Save(0, docs)
			if err != nil {
				return err
			}
			fmt.Printf("Catalog revision %d created\n", b.Manifest.Revision)
			return nil
		}
		if len(args) > 1 && args[1] == "migrate" {
			fs := flag.NewFlagSet("config migrate", flag.ContinueOnError)
			identity := fs.String("identity-dir", "", "legacy identity directory")
			dir := fs.String("data-dir", "", "new catalog data directory")
			dry := fs.Bool("dry-run", false, "validate without writing")
			if err := fs.Parse(args[2:]); err != nil {
				return err
			}
			if *identity == "" {
				base, err := os.UserConfigDir()
				if err != nil {
					return err
				}
				*identity = filepath.Join(base, "serverside-generative")
			}
			target, err := dataDirectory(*dir)
			if err != nil {
				return err
			}
			report, err := migration.Migrate(*identity, target, *dry)
			if err != nil {
				return err
			}
			fmt.Printf("Providers: %d; legacy skills retained: %d; already applied: %t; dry run: %t\n", report.Providers, report.Skills, report.AlreadyApplied, *dry)
			return nil
		}
		if len(args) < 2 || args[1] != "validate" {
			return fmt.Errorf("use config init, config validate or config migrate")
		}
		fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
		dir := fs.String("data-dir", "", "catalog data directory")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		path, err := dataDirectory(*dir)
		if err != nil {
			return err
		}
		b, err := catalog.New(path).Get()
		if err != nil {
			return err
		}
		fmt.Printf("Catalog revision %d validated\n", b.Manifest.Revision)
		return nil
	case "provider":
		if len(args) > 1 && args[1] == "group" {
			return runProviderGroup(args[2:])
		}
		if len(args) > 1 && args[1] == "add" {
			fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
			id := fs.String("id", "", "provider ID")
			name := fs.String("name", "", "display name")
			typ := fs.String("type", "", "mobile provider type")
			base := fs.String("base-url", "", "provider endpoint")
			dir := fs.String("data-dir", "", "data directory")
			rev := fs.String("if-revision", "", "current catalog revision")
			dry := fs.Bool("dry-run", false, "validate without writing")
			if err := fs.Parse(args[2:]); err != nil {
				return err
			}
			return editCatalog(*dir, *rev, *dry, func(d map[string]*yaml.Node) error { return catalog.AddProvider(d, *id, *name, *typ, *base) })
		}
		if len(args) > 2 && (args[1] == "hide" || args[1] == "show") {
			fs := flag.NewFlagSet("provider "+args[1], flag.ContinueOnError)
			dir := fs.String("data-dir", "", "data directory")
			rev := fs.String("if-revision", "", "current catalog revision")
			dry := fs.Bool("dry-run", false, "validate without writing")
			if err := fs.Parse(args[3:]); err != nil {
				return err
			}
			return editCatalog(*dir, *rev, *dry, func(d map[string]*yaml.Node) error { return catalog.HideProvider(d, args[2], args[1] == "hide") })
		}
		if len(args) > 2 && args[1] == "auth" {
			fs := flag.NewFlagSet("provider auth", flag.ContinueOnError)
			method := fs.String("method", "", "none, platform_default, bearer_token, query_param or custom_header")
			parameter := fs.String("name", "", "query parameter or header name")
			dir := fs.String("data-dir", "", "data directory")
			rev := fs.String("if-revision", "", "current catalog revision")
			dry := fs.Bool("dry-run", false, "validate without writing")
			if err := fs.Parse(args[3:]); err != nil {
				return err
			}
			if fs.NArg() != 0 {
				return fmt.Errorf("unexpected provider auth arguments")
			}
			return editCatalog(*dir, *rev, *dry, func(d map[string]*yaml.Node) error {
				return catalog.SetProviderAuth(d, args[2], *method, *parameter)
			})
		}
		if len(args) < 5 || args[1] != "key" || args[2] != "set" {
			return fmt.Errorf("use provider key set PROVIDER_ID --key-id ID --stdin [--data-dir DIR]")
		}
		providerID := args[3]
		fs := flag.NewFlagSet("provider key set", flag.ContinueOnError)
		keyID := fs.String("key-id", "", "credential ID")
		label := fs.String("label", "", "display label")
		stdin := fs.Bool("stdin", false, "read credential from standard input")
		dir := fs.String("data-dir", "", "credential data directory")
		revision := fs.String("if-revision", "", "current catalog revision")
		dry := fs.Bool("dry-run", false, "validate without writing")
		if err := fs.Parse(args[4:]); err != nil {
			return err
		}
		if !*stdin || *keyID == "" || *revision == "" || fs.NArg() != 0 {
			return fmt.Errorf("--stdin, --key-id and --if-revision are required")
		}
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20+1))
		if err != nil {
			return err
		}
		if len(raw) > 1<<20 {
			return fmt.Errorf("credential too large")
		}
		raw = []byte(strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r"))
		path, err := dataDirectory(*dir)
		if err != nil {
			return err
		}
		if len(raw) == 0 {
			return fmt.Errorf("credential is empty")
		}
		expected, err := strconv.ParseUint(*revision, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid revision")
		}
		secretID, err := node.RandomToken(18)
		if err != nil {
			return err
		}
		ref := "provider/" + providerID + "/" + secretID
		edit := func(d map[string]*yaml.Node) error {
			return catalog.SetProviderCredential(d, providerID, *keyID, *label, ref)
		}
		cat := catalog.New(path)
		if _, err = cat.Update(expected, true, edit); err != nil {
			return err
		}
		if *dry {
			fmt.Println("Credential update is valid")
			return nil
		}
		store, err := secrets.Open(path)
		if err != nil {
			return err
		}
		if err = store.Put("provider", providerID, secretID, *label, "api_key", raw); err != nil {
			store.Close()
			return err
		}
		if err = store.Close(); err != nil {
			return err
		}
		if _, err = cat.Update(expected, false, edit); err != nil {
			return fmt.Errorf("catalog update failed; encrypted staged credential may require cleanup: %w", err)
		}
		fmt.Println("Credential stored and catalog updated")
		return nil
	case "model":
		if len(args) < 2 || (args[1] != "add" && args[1] != "select") {
			return fmt.Errorf("use model add or model select")
		}
		fs := flag.NewFlagSet("model "+args[1], flag.ContinueOnError)
		provider := fs.String("provider", "", "provider ID")
		name := fs.String("name", "", "display name")
		typ := fs.String("type", "TEXT_GENERATION", "mobile model type")
		mode := fs.String("mode", "explicit", "discovery or explicit")
		dir := fs.String("data-dir", "", "data directory")
		rev := fs.String("if-revision", "", "current catalog revision")
		dry := fs.Bool("dry-run", false, "validate without writing")
		var ids []string
		fs.Func("id", "model ID (repeat for selection)", func(v string) error { ids = append(ids, v); return nil })
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if args[1] == "add" {
			if len(ids) != 1 {
				return fmt.Errorf("exactly one --id is required")
			}
			if *name == "" {
				*name = ids[0]
			}
			return editCatalog(*dir, *rev, *dry, func(d map[string]*yaml.Node) error { return catalog.AddModel(d, *provider, ids[0], *name, *typ) })
		}
		return editCatalog(*dir, *rev, *dry, func(d map[string]*yaml.Node) error { return catalog.SelectModels(d, *provider, *mode, ids) })
	case "mcp":
		if len(args) >= 4 && args[1] == "credential" && args[2] == "set" {
			return setMCPCredential(args[3], args[4:])
		}
		if len(args) < 2 || args[1] != "add" {
			return fmt.Errorf("use mcp add or mcp credential set")
		}
		fs := flag.NewFlagSet("mcp add", flag.ContinueOnError)
		id := fs.String("id", "", "server ID")
		name := fs.String("name", "", "display name")
		protocol := fs.String("protocol", "streamable_http", "streamable_http or sse")
		endpoint := fs.String("url", "", "endpoint URL")
		dir := fs.String("data-dir", "", "data directory")
		rev := fs.String("if-revision", "", "current catalog revision")
		dry := fs.Bool("dry-run", false, "validate without writing")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		return editCatalog(*dir, *rev, *dry, func(d map[string]*yaml.Node) error { return catalog.AddMCP(d, *id, *name, *protocol, *endpoint) })

	case "profile":
		if len(args) < 2 || args[1] != "add" {
			return fmt.Errorf("use profile add --kind image|video|music --file FILE")
		}
		fs := flag.NewFlagSet("profile add", flag.ContinueOnError)
		kind := fs.String("kind", "", "profile kind")
		file := fs.String("file", "", "profile YAML or JSON file")
		dir := fs.String("data-dir", "", "data directory")
		rev := fs.String("if-revision", "", "current catalog revision")
		dry := fs.Bool("dry-run", false, "validate without writing")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *file == "" {
			return fmt.Errorf("--file is required")
		}
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
		f.Close()
		if err != nil || len(raw) > 4<<20 {
			return fmt.Errorf("invalid profile file")
		}
		var profile yaml.Node
		if yaml.Unmarshal(raw, &profile) != nil || len(profile.Content) != 1 {
			return fmt.Errorf("invalid profile file")
		}
		return editCatalog(*dir, *rev, *dry, func(d map[string]*yaml.Node) error { return catalog.AddMediaProfile(d, *kind, profile.Content[0]) })

	case "tool":
		if len(args) < 2 || (args[1] != "add" && args[1] != "list" && args[1] != "test") {
			return fmt.Errorf("use tool add --file FILE or tool list")
		}
		fs := flag.NewFlagSet("tool "+args[1], flag.ContinueOnError)
		file := fs.String("file", "", "custom tool JSON file")
		toolID := fs.String("id", "", "stored tool ID")
		arguments := fs.String("args-file", "", "JSON object containing tool arguments")
		confirmed := fs.Bool("yes", false, "confirm the outbound request")
		allowLocal := fs.Bool("allow-local-api", false, "allow a loopback target")
		dir := fs.String("data-dir", "", "credential data directory")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		path, err := dataDirectory(*dir)
		if err != nil {
			return err
		}
		if args[1] == "test" {
			if !*confirmed || *toolID == "" || *arguments == "" {
				return fmt.Errorf("tool test requires --id, --args-file and --yes")
			}
			f, err := os.Open(*arguments)
			if err != nil {
				return err
			}
			argv, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
			f.Close()
			if err != nil || len(argv) > 64<<10 {
				return fmt.Errorf("invalid argument file")
			}
			store, err := secrets.OpenReadOnly(path)
			if err != nil {
				return err
			}
			raw, err := store.GetTool(*toolID)
			closeErr := store.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			var target struct {
				Method string `json:"method"`
				URL    string `json:"url"`
			}
			if json.Unmarshal(raw, &target) != nil {
				return fmt.Errorf("invalid tool")
			}
			endpoint, err := url.Parse(target.URL)
			if err != nil {
				return fmt.Errorf("invalid tool endpoint")
			}
			method := target.Method
			if method == "" {
				method = "GET"
			}
			fmt.Fprintf(os.Stderr, "Testing %s %s://%s (credentials and path hidden)\n", method, endpoint.Scheme, endpoint.Host)
			result, err := httptool.Execute(context.Background(), raw, argv, httptool.Policy{Allowed: true, AllowLoopback: *allowLocal})
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				return err
			}
			fmt.Println(string(encoded))
			return nil
		}
		if args[1] == "add" {
			if *file == "" {
				return fmt.Errorf("--file is required")
			}
			f, err := os.Open(*file)
			if err != nil {
				return err
			}
			raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
			f.Close()
			if err != nil || len(raw) > 1<<20 {
				return fmt.Errorf("invalid tool file")
			}
			toolID, err := httptool.Validate(raw)
			if err != nil {
				return fmt.Errorf("invalid tool file")
			}
			store, err := secrets.Open(path)
			if err != nil {
				return err
			}
			defer store.Close()
			if err = store.PutTool(toolID, raw); err != nil {
				return err
			}
			fmt.Println("Tool stored in encrypted SQLite")
			return nil
		}
		if _, err := os.Lstat(filepath.Join(path, "data.sqlite")); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		store, err := secrets.Open(path)
		if err != nil {
			return err
		}
		defer store.Close()
		ids, err := store.ToolIDs()
		if err != nil {
			return err
		}
		for _, id := range ids {
			fmt.Println(id)
		}
		return nil

	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		tcpAddr := fs.String("tcp-listen", "127.0.0.1:7090", "TLS/TCP data listener")
		advertise := fs.String("advertise", "", "TCP endpoint advertised to clients (host:port)")
		httpAddr := fs.String("http-listen", "127.0.0.1:7091", "HTTP/WS v5 data and URL pairing listener; off disables")
		password := fs.String("password", os.Getenv("GENERATIVE_HANDSHAKE_PASSWORD"), "legacy HTTP/WS pairing password (requires --allow-legacy-pairing)")
		allowLegacyPairing := fs.Bool("allow-legacy-pairing", false, "temporarily allow deprecated HTTP/WS password and Human Relay enrollment for v4 clients")
		dataDir := fs.String("data-dir", "", "catalog and opted-in chat data directory")
		identityDir := fs.String("identity-dir", "", "connection identity and paired client credential hashes only")
		ipc := fs.String("ipc", "", "control socket for SSH handshake and human pairing")
		dataSocket := fs.String("data-socket", "", "private v5 framed data socket for SSH stdio")
		human := fs.Bool("human", false, "display Human Relay QR and PIN on startup")
		onlyHTTP := fs.Bool("http-only", false, "disable the legacy pairing WebSocket (v5 WebSocket remains enabled)")
		allowPublic := fs.Bool("allow-public-http", false, "dangerous: allow plain HTTP binding beyond loopback")
		allowLocalAPI := fs.Bool("allow-local-api", false, "allow v5 runtime connections to loopback API endpoints")
		enableTerminal := fs.Bool("enable-terminal", false, "enable remote terminal creation and shell execution for authenticated clients")
		terminalCWD := fs.String("terminal-cwd", "", "working directory for remote terminal shells; defaults to current directory")
		enableAgent := fs.Bool("enable-agent", false, "allow paired clients to control structured Claude Code, Codex and OpenCode sessions")
		agentRoot := fs.String("agent-root", "", "working directory root for new agent processes; defaults to current directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := node.SafeHTTPBind(*httpAddr, *allowPublic); err != nil {
			return err
		}
		if *human && !*allowLegacyPairing {
			return fmt.Errorf("--human is legacy TCP pairing; use SSH pair --stdio --transport tcp or HTTP/WS URL pairing (or explicitly --allow-legacy-pairing for migration)")
		}
		if *password != "" && !*allowLegacyPairing {
			return fmt.Errorf("--password is deprecated; enable --allow-legacy-pairing only for temporary v4 migration")
		}
		if *password != "" && len(*password) < 12 && !strings.EqualFold(*httpAddr, "off") {
			return fmt.Errorf("HTTP/WS password must be at least 12 characters")
		}
		listener, err := net.Listen("tcp", *tcpAddr)
		if err != nil {
			return err
		}
		defer listener.Close()
		ep := *advertise
		if ep == "" {
			ep = listener.Addr().String()
		}
		host, _, err := net.SplitHostPort(ep)
		if err != nil {
			return err
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			return fmt.Errorf("set --advertise to a reachable address instead of %q", ep)
		}
		c := node.Config{TCPListen: listener.Addr().String(), Advertise: ep, HTTPListen: *httpAddr, IPCSocket: *ipc, DataSocket: *dataSocket, IdentityDir: *identityDir, DataDir: *dataDir, Password: *password, Human: *human, HTTPOnly: *onlyHTTP, LegacyPairing: *allowLegacyPairing, AllowPublicHTTP: *allowPublic, AllowLocalAPI: *allowLocalAPI, EnableTerminal: *enableTerminal, TerminalCWD: *terminalCWD, EnableAgent: *enableAgent, AgentRoot: *agentRoot}
		n, err := node.New(c)
		if err != nil {
			return err
		}
		defer n.Close()
		sock := n.Config.IPCSocket
		control, err := node.StartIPCSocket(sock)
		if err != nil {
			return err
		}
		defer func() { _ = control.Close(); _ = os.Remove(sock) }()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go n.ServeIPC(ctx, control)
		if node.DataSocketSupported() {
			dataListener, err := node.StartDataSocket(n.Config.DataSocket)
			if err != nil {
				return err
			}
			defer dataListener.Close()
			go n.ServeDataSocket(ctx, dataListener)
		} else if *dataSocket != "" {
			return fmt.Errorf("data socket unsupported on this platform")
		}
		go n.ServeTCP(ctx, listener)
		var httpSrv *http.Server
		if !strings.EqualFold(*httpAddr, "off") {
			httpListener, err := net.Listen("tcp", *httpAddr)
			if err != nil {
				return err
			}
			httpSrv = &http.Server{Handler: n.HTTPHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second}
			go func() {
				if err := httpSrv.Serve(httpListener); err != nil && err != http.ErrServerClosed {
					log.Print("HTTP error: ", err)
				}
			}()
			fmt.Println("HTTP/WS API:", httpListener.Addr().String())
		} else {
			fmt.Println("HTTP/WS API: disabled (--http-listen off)")
		}
		fmt.Println("Data transport (TLS 1.3):", listener.Addr().String())
		fmt.Println("Advertised endpoint:     ", ep)
		fmt.Println("Server TLS fingerprint:  ", n.Fingerprint)
		fmt.Println("TCP pairing (SSH only):   ssh -T USER@HOST serverside pair --stdio --transport tcp")
		fmt.Println("Pairing control socket:  ", sock)
		if *allowLegacyPairing {
			fmt.Println("WARNING: legacy new-device pairing enabled for v4 migration")
		}
		fmt.Println("Runtime credentials and terminal buffers remain in RAM; sync chats are stored as JSON.")
		if n.Terminals != nil {
			fmt.Println("Remote terminal: enabled (up to 8 concurrent interactive sessions)")
		} else {
			fmt.Println("Remote terminal: disabled (--enable-terminal to opt in)")
		}
		if n.Agents != nil {
			fmt.Println("Structured coding agents: enabled (Claude Code, Codex, OpenCode)")
		} else {
			fmt.Println("Structured coding agents: disabled (--enable-agent to opt in)")
		}
		if *human {
			invite, err := n.IssueHuman()
			if err != nil {
				return err
			}
			printHuman(invite)
		}
		<-ctx.Done()
		if httpSrv != nil {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutdown)
		}
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}
