package setup

import (
	"context"
	"fmt"
	"time"

	"github.com/lhpalacio/redash-wire/internal/redash"
)

func ValidateConnection(redashURL, apiKey string) (*redash.SessionInfo, []redash.DataSource, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := redash.NewClient(redashURL, apiKey)
	session, err := client.GetSession(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("connection failed: %w", err)
	}

	sources, err := client.ListDataSources(ctx)
	if err != nil {
		// The key authenticated but cannot list data sources (e.g. a restricted
		// user). Surface this instead of silently reporting success, since startup
		// will fail the same way.
		return session, nil, fmt.Errorf("listing data sources failed: %w", err)
	}

	return session, sources, nil
}
