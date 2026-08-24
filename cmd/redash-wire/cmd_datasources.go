package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lhpalacio/redash-wire/internal/redash"
)

// A listing this slow is a hung endpoint, not a slow query, so it does not use
// the much longer poll_timeout.
const listTimeout = 30 * time.Second

type dataSourcePayload struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// "postgres", "mysql", or "" when the proxy cannot serve the source.
	Wire string `json:"wire"`
}

func cmdDataSources(args []string) int {
	fs := flag.NewFlagSet("datasources", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file to use (default: ./config.yaml, then ~/.redash-wire/config.yaml)")
	profile := fs.String("profile", "", "profile to load (overrides default_profile)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	cfg, cerr := loadProfile(*configPath, *profile)
	if cerr != nil {
		return reportError(cerr, *asJSON)
	}

	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()

	sources, err := redash.NewClient(cfg.RedashURL, cfg.APIKey).ListDataSources(ctx)
	if err != nil {
		return reportError(classifyAPIError(err), *asJSON)
	}

	payload := buildDataSourcesPayload(sources)

	if *asJSON {
		if cerr := emitJSON(payload); cerr != nil {
			return reportError(cerr, false)
		}
		return exitOK
	}

	printDataSources(payload)
	return exitOK
}

// Redash returns sources in no particular order, so sort them for stable output.
func buildDataSourcesPayload(sources []redash.DataSource) []dataSourcePayload {
	payload := make([]dataSourcePayload, 0, len(sources))
	for _, ds := range sources {
		payload = append(payload, dataSourcePayload{
			ID:   ds.ID,
			Name: ds.Name,
			Type: ds.Type,
			Wire: wireProtocol(ds.Type),
		})
	}

	sort.Slice(payload, func(i, j int) bool {
		li, lj := strings.ToLower(payload[i].Name), strings.ToLower(payload[j].Name)
		if li != lj {
			return li < lj
		}
		return payload[i].ID < payload[j].ID
	})

	return payload
}

// Mirrors the proxy's own dispatch, in the same order.
func wireProtocol(dsType string) string {
	switch {
	case redash.IsPostgresCompatible(dsType):
		return "postgres"
	case redash.IsMySQLCompatible(dsType):
		return "mysql"
	default:
		return ""
	}
}

func printDataSources(sources []dataSourcePayload) {
	if len(sources) == 0 {
		fmt.Println("no data sources")
		return
	}
	width := 0
	for _, ds := range sources {
		if len(ds.Name) > width {
			width = len(ds.Name)
		}
	}
	for _, ds := range sources {
		wire := ds.Wire
		if wire == "" {
			wire = "unsupported"
		}
		fmt.Printf("%-*s  %-12s %s\n", width, ds.Name, ds.Type, wire)
	}
}
