package main

import (
	"flag"
	"fmt"

	"github.com/starfall-org/generative-server/internal/catalog"
	"go.yaml.in/yaml/v4"
)

// runProviderGroup handles the provider-folder relationships in one catalog
// CAS transaction. No secret or model selection is touched.
func runProviderGroup(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use provider group add|edit|assign|clear|delete")
	}
	action := args[0]
	switch action {
	case "add", "edit", "assign", "clear", "delete":
	default:
		return fmt.Errorf("unsupported provider group command")
	}
	fs := flag.NewFlagSet("provider group "+action, flag.ContinueOnError)
	id := fs.String("id", "", "provider group ID")
	name := fs.String("name", "", "group display name")
	sortOrder := fs.Int("sort-order", 0, "group sort order")
	provider := fs.String("provider-id", "", "provider to assign or ungroup")
	group := fs.String("group-id", "", "target provider group ID")
	dir := fs.String("data-dir", "", "catalog directory")
	revision := fs.String("if-revision", "", "expected catalog revision")
	dryRun := fs.Bool("dry-run", false, "validate without writing")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected provider group arguments")
	}
	var hasName, hasSortOrder bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "name" {
			hasName = true
		}
		if f.Name == "sort-order" {
			hasSortOrder = true
		}
	})
	var update func(map[string]*yaml.Node) error
	switch action {
	case "add":
		update = func(d map[string]*yaml.Node) error { return catalog.AddProviderGroup(d, *id, *name, *sortOrder) }
	case "edit":
		var newName *string
		var newOrder *int
		if hasName {
			newName = name
		}
		if hasSortOrder {
			newOrder = sortOrder
		}
		update = func(d map[string]*yaml.Node) error { return catalog.EditProviderGroup(d, *id, newName, newOrder) }
	case "assign":
		if *group == "" {
			return fmt.Errorf("--group-id required")
		}
		update = func(d map[string]*yaml.Node) error { return catalog.SetProviderGroup(d, *provider, *group) }
	case "clear":
		update = func(d map[string]*yaml.Node) error { return catalog.SetProviderGroup(d, *provider, "") }
	case "delete":
		update = func(d map[string]*yaml.Node) error { return catalog.DeleteProviderGroup(d, *id) }
	}
	return editCatalog(*dir, *revision, *dryRun, update)
}
