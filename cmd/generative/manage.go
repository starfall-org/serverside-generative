package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/starfall-org/generative-server/internal/catalog"
	"go.yaml.in/yaml/v4"
)

var catalogSectionNames = map[string]string{
	"providers": "providers.yaml",
	"models":    "models.yaml",
	"mcp":       "mcp.yaml",
	"profiles":  "profiles.yaml",
}

func sectionFilename(kind string) (string, error) {
	if name, ok := catalogSectionNames[kind]; ok {
		return name, nil
	}
	return "", fmt.Errorf("invalid catalog kind %q (providers, models, mcp, profiles)", kind)
}

func decodeCatalogFile(raw []byte) (*yaml.Node, error) {
	if len(raw) == 0 || len(raw) > 4<<20 {
		return nil, catalog.ErrInvalid
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, catalog.ErrInvalid
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, catalog.ErrInvalid
	}
	return &doc, nil
}

func readCatalogCandidate(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil || len(raw) > 4<<20 {
		return nil, catalog.ErrInvalid
	}
	return raw, nil
}

// applyCatalogFile reuses the same transaction/validation path as all CLI
// management operations; it never implements a second persistence mechanism.
func applyCatalogFile(store *catalog.Store, name string, raw []byte, expected uint64, dry bool) (catalog.Bundle, error) {
	doc, err := decodeCatalogFile(raw)
	if err != nil {
		return catalog.Bundle{}, err
	}
	return store.Update(expected, dry, func(docs map[string]*yaml.Node) error {
		dst := docs[name]
		if dst == nil || len(dst.Content) != 1 {
			return catalog.ErrInvalid
		}
		dst.Content[0] = doc.Content[0]
		return nil
	})
}

func runCatalogFileCommand(args []string) error {
	action := args[0]
	if action != "export" && action != "import" && action != "diff" {
		return fmt.Errorf("unknown config file command")
	}
	fs := flag.NewFlagSet("config "+action, flag.ContinueOnError)
	kind := fs.String("kind", "", "providers, models, mcp or profiles")
	file := fs.String("file", "", "candidate YAML file")
	out := fs.String("out", "", "new file to export")
	dir := fs.String("data-dir", "", "catalog directory")
	revision := fs.String("if-revision", "", "expected manifest revision, required for import")
	dry := fs.Bool("dry-run", false, "validate candidate, do not write catalog")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	name, err := sectionFilename(*kind)
	if err != nil {
		return err
	}
	dataDir, err := dataDirectory(*dir)
	if err != nil {
		return err
	}
	store := catalog.New(dataDir)
	if action == "export" {
		if *out == "" || *file != "" || *dry {
			return fmt.Errorf("config export requires --out and no --file or --dry-run")
		}
		bundle, err := store.Preview()
		if err != nil {
			return err
		}
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, bundle.Files[name])
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		return err
	}
	if *file == "" || *out != "" {
		return fmt.Errorf("config %s requires --file and no --out", action)
	}
	candidate, err := readCatalogCandidate(*file)
	if err != nil {
		return err
	}
	if action == "diff" {
		bundle, err := store.Preview()
		if err != nil {
			return err
		}
		paths, err := changedCatalogPaths([]byte(bundle.Files[name]), candidate)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			fmt.Printf("%s unchanged at revision %d\n", name, bundle.Manifest.Revision)
			return nil
		}
		fmt.Printf("%s: %d paths changed (revision %d)\n", name, len(paths), bundle.Manifest.Revision)
		for i, path := range paths {
			if i == 100 {
				fmt.Printf("... and %d more\n", len(paths)-100)
				break
			}
			fmt.Println(path)
		}
		return nil
	}
	if *revision == "" {
		return fmt.Errorf("config import requires --if-revision")
	}
	expected, err := strconv.ParseUint(*revision, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid revision")
	}
	bundle, err := applyCatalogFile(store, name, candidate, expected, *dry)
	if err != nil {
		return err
	}
	fmt.Printf("Catalog %s validated, revision %d (dry run: %t)\n", name, bundle.Manifest.Revision, *dry)
	return nil
}

// changedCatalogPaths emits only field paths, not secret values. It provides
// a useful, revision-aware dry preview without printing external credentials.
func changedCatalogPaths(original, proposed []byte) ([]string, error) {
	a, err := decodeCatalogFile(original)
	if err != nil {
		return nil, err
	}
	b, err := decodeCatalogFile(proposed)
	if err != nil {
		return nil, err
	}
	var x, y any
	if a.Decode(&x) != nil || b.Decode(&y) != nil {
		return nil, catalog.ErrInvalid
	}
	output := make([]string, 0)
	var walk func(string, any, any)
	walk = func(path string, before, after any) {
		if reflect.DeepEqual(before, after) {
			return
		}
		left, lok := before.(map[string]any)
		right, rok := after.(map[string]any)
		if lok && rok {
			keys := map[string]bool{}
			for key := range left {
				keys[key] = true
			}
			for key := range right {
				keys[key] = true
			}
			sorted := make([]string, 0, len(keys))
			for key := range keys {
				sorted = append(sorted, key)
			}
			sort.Strings(sorted)
			for _, key := range sorted {
				// Dynamic map keys may themselves be secrets, credentials or
				// arbitrary user data. A safe diff only reveals known field
				// labels, never arbitrary key text or any old/new values.
				next := safeCatalogDiffKey(key)
				if path != "" {
					next = path + "." + next
				}
				walk(next, left[key], right[key])
			}
			return
		}
		output = append(output, path)
	}
	walk("catalog", x, y)
	return output, nil
}

// safeCatalogDiffKey deliberately recognizes only schema-controlled field
// names. User-supplied map keys are never echoed into CLI output.
func safeCatalogDiffKey(key string) string {
	switch key {
	case "schema_version", "revision", "groups", "providers", "models", "mcp",
		"profiles", "id", "name", "type", "baseUrl", "config", "auth",
		"icon", "groupId", "isHidden", "sortOrder", "modelIds",
		"modelConfigs", "catalog", "cachedTools", "protocol",
		"servers", "chatProfiles", "mediaProfiles", "toolSettings",
		"quickMcp", "mcpTools", "mcpToolParameters", "system",
		"mode", "requiresModel", "steps", "finalResponse",
		"input_capabilities", "output_capabilities", "model_tools",
		"disabledParams", "paramsConfigured", "responseMapping",
		"mediaPath", "sourceStep", "valueKind":
		return key
	default:
		return "<entry>"
	}
}

// runManage implements a small built-in terminal editor. Editing an entire
// nonsecret catalog document exposes all mobile-compatible advanced fields
// without duplicating the YAML validator or any credential storage path.
func runManage(args []string, input io.Reader, output io.Writer) error {
	fs := flag.NewFlagSet("manage", flag.ContinueOnError)
	dir := fs.String("data-dir", "", "catalog directory")
	editor := fs.String("editor", os.Getenv("EDITOR"), "terminal editor executable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected manage arguments")
	}
	path, err := dataDirectory(*dir)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(input)
	store := catalog.New(path)
	for {
		fmt.Fprint(output, "\nGenerative server management\n"+
			"  1. Providers     2. Models      3. MCP servers\n"+
			"  4. Profiles      5. Validate    6. Quit\n"+
			"Select: ")
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		choice := strings.TrimSpace(line)
		if choice == "6" || strings.EqualFold(choice, "q") || (err == io.EOF && choice == "") {
			return nil
		}
		if choice == "5" {
			bundle, e := store.Preview()
			if e != nil {
				fmt.Fprintf(output, "Validation error: %v\n", e)
			} else {
				fmt.Fprintf(output, "Catalog valid at revision %d\n", bundle.Manifest.Revision)
			}
			continue
		}
		names := map[string]string{"1": "providers.yaml", "2": "models.yaml", "3": "mcp.yaml", "4": "profiles.yaml"}
		name := names[choice]
		if name == "" {
			fmt.Fprintln(output, "Invalid selection")
			continue
		}
		if *editor == "" {
			return fmt.Errorf("set EDITOR or --editor to edit catalog files")
		}
		bundle, e := store.Preview()
		if e != nil {
			return e
		}
		f, e := os.CreateTemp("", "serverside-"+strings.TrimSuffix(name, ".yaml")+"-*.yaml")
		if e != nil {
			return e
		}
		tempName := f.Name()
		_ = f.Chmod(0600)
		_, e = f.WriteString(bundle.Files[name])
		if e == nil {
			e = f.Close()
		} else {
			_ = f.Close()
		}
		if e != nil {
			_ = os.Remove(tempName)
			return e
		}
		parts := strings.Fields(*editor)
		if len(parts) == 0 {
			_ = os.Remove(tempName)
			return fmt.Errorf("invalid editor")
		}
		cmd := exec.Command(parts[0], append(parts[1:], tempName)...)
		cmd.Stdin = input
		cmd.Stdout = output
		cmd.Stderr = os.Stderr
		e = cmd.Run()
		if e != nil {
			_ = os.Remove(tempName)
			return e
		}
		raw, e := readCatalogCandidate(tempName)
		_ = os.Remove(tempName)
		if e != nil {
			return e
		}
		paths, e := changedCatalogPaths([]byte(bundle.Files[name]), raw)
		if e != nil {
			return e
		}
		if len(paths) == 0 {
			fmt.Fprintln(output, "No changes")
			continue
		}
		if _, e = applyCatalogFile(store, name, raw, bundle.Manifest.Revision, true); e != nil {
			fmt.Fprintf(output, "Invalid or conflicting edit: %v\n", e)
			continue
		}
		fmt.Fprintf(output, "%d paths changed. Save at revision %d? [y/N]: ", len(paths), bundle.Manifest.Revision)
		line, e = reader.ReadString('\n')
		if e != nil && e != io.EOF {
			return e
		}
		if strings.EqualFold(strings.TrimSpace(line), "y") {
			result, e := applyCatalogFile(store, name, raw, bundle.Manifest.Revision, false)
			if e != nil {
				fmt.Fprintf(output, "Save rejected: %v\n", e)
			} else {
				fmt.Fprintf(output, "Saved revision %d\n", result.Manifest.Revision)
			}
		} else {
			fmt.Fprintln(output, "Cancelled")
		}
	}
}
