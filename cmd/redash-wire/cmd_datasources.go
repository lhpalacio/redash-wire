package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/lhpalacio/redash-wire/internal/redash"
)

// A listing this slow is a hung endpoint, not a slow query, so it does not use
// the much longer poll_timeout.
const listTimeout = 30 * time.Second

// The published shape lives in the redash package so that `datasources -json`
// and the datasources_refreshed event the menu bar app reads cannot drift apart.
type dataSourcePayload = redash.DataSourceView

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

func buildDataSourcesPayload(sources []redash.DataSource) []dataSourcePayload {
	return redash.NewDataSourceViews(sources)
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
