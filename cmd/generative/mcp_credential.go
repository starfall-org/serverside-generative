package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/starfall-org/serverside-generative/internal/catalog"
	"github.com/starfall-org/serverside-generative/internal/node"
	"github.com/starfall-org/serverside-generative/internal/secrets"
	"go.yaml.in/yaml/v4"
)

func setMCPCredential(serverID string, args []string) error {
	fs := flag.NewFlagSet("mcp credential set", flag.ContinueOnError)
	method := fs.String("method", "bearer", "bearer or custom-header")
	header := fs.String("header", "", "header name for custom-header")
	stdin := fs.Bool("stdin", false, "read credential from stdin")
	dir := fs.String("data-dir", "", "catalog data directory")
	revision := fs.String("if-revision", "", "current catalog revision")
	dry := fs.Bool("dry-run", false, "validate without writing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if serverID == "" || !*stdin || *revision == "" || fs.NArg() != 0 {
		return fmt.Errorf("server ID, --stdin and --if-revision are required")
	}
	authMethod := ""
	switch *method {
	case "bearer":
		if *header != "" {
			return fmt.Errorf("--header requires custom-header")
		}
		authMethod = "BEARER_TOKEN"
	case "custom-header":
		authMethod = "CUSTOM_HEADER"
	default:
		return fmt.Errorf("unsupported MCP authentication method")
	}
	expected, err := strconv.ParseUint(*revision, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid revision")
	}
	value, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20+1))
	if err != nil {
		return err
	}
	if len(value) > 1<<20 {
		return fmt.Errorf("credential too large")
	}
	value = []byte(strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r"))
	if len(value) == 0 {
		return fmt.Errorf("invalid credential")
	}
	for _, b := range value {
		if b < 32 || b == 127 {
			return fmt.Errorf("invalid credential")
		}
	}
	path, err := dataDirectory(*dir)
	if err != nil {
		return err
	}
	secretID, err := node.RandomToken(18)
	if err != nil {
		return err
	}
	ref := "mcp/" + serverID + "/" + secretID
	edit := func(d map[string]*yaml.Node) error {
		return catalog.SetMCPCredential(d, serverID, authMethod, *header, ref)
	}
	cat := catalog.New(path)
	if _, err = cat.Update(expected, true, edit); err != nil {
		return err
	}
	if *dry {
		fmt.Println("MCP credential update is valid")
		return nil
	}
	store, err := secrets.Open(path)
	if err != nil {
		return err
	}
	if err = store.Put("mcp", serverID, secretID, "", "auth", value); err != nil {
		_ = store.Close()
		return err
	}
	if err = store.Close(); err != nil {
		return err
	}
	if _, err = cat.Update(expected, false, edit); err != nil {
		return fmt.Errorf("catalog update failed; encrypted staged credential may require cleanup: %w", err)
	}
	fmt.Println("MCP credential stored and catalog updated")
	return nil
}
