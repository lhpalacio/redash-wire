package redash

import "sync"

// maxSchemaFetchAttempts bounds how many times a session will try to fetch a
// schema that keeps failing, so a persistently unhealthy /schema endpoint is not
// re-hammered on every catalog query while a transient failure can still recover.
const maxSchemaFetchAttempts = 3

// SchemaCache memoizes a data source's schema for the lifetime of a session.
//
// Policy (shared by both the PostgreSQL and MySQL sessions so they behave
// identically): a successful fetch is cached for the session; a failed fetch is
// not cached, so it can be retried on the next catalog query, but only up to
// maxSchemaFetchAttempts times, after which the cache returns an empty schema
// without contacting Redash again.
type SchemaCache struct {
	mu       sync.Mutex
	ready    bool
	attempts int
	schema   []SchemaTable
}

func NewSchemaCache() *SchemaCache {
	return &SchemaCache{}
}

// Get returns the cached schema, invoking fetch on first use (and on retry after a
// prior failure). The mutex is held across fetch; a single client connection only
// runs one query at a time, so this never serializes unrelated work.
func (c *SchemaCache) Get(fetch func() ([]SchemaTable, error)) ([]SchemaTable, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ready {
		return c.schema, nil
	}
	if c.attempts >= maxSchemaFetchAttempts {
		return nil, nil
	}
	c.attempts++

	schema, err := fetch()
	if err != nil {
		return nil, err
	}
	c.schema = schema
	c.ready = true
	return schema, nil
}

// Reset clears the cache so the next Get re-fetches (used when a session switches
// data source via USE).
func (c *SchemaCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
	c.attempts = 0
	c.schema = nil
}
