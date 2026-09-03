package testutil

import (
	"context"
	"strings"

	"github.com/lhpalacio/redash-wire/internal/redash"
)

// MockRedashAPI implements redash.RedashAPI for testing.
type MockRedashAPI struct {
	ExecuteQueryFunc    func(ctx context.Context, sql string, dataSourceID int) (*redash.QueryResult, error)
	GetSchemaFunc       func(ctx context.Context, dataSourceID int) ([]redash.SchemaTable, error)
	ListDataSourcesFunc func(ctx context.Context) ([]redash.DataSource, error)
}

func (m *MockRedashAPI) ExecuteQuery(ctx context.Context, sql string, dataSourceID int) (*redash.QueryResult, error) {
	if m.ExecuteQueryFunc != nil {
		return m.ExecuteQueryFunc(ctx, sql, dataSourceID)
	}
	return &redash.QueryResult{}, nil
}

func (m *MockRedashAPI) GetSchema(ctx context.Context, dataSourceID int) ([]redash.SchemaTable, error) {
	if m.GetSchemaFunc != nil {
		return m.GetSchemaFunc(ctx, dataSourceID)
	}
	return nil, nil
}

func (m *MockRedashAPI) ListDataSources(ctx context.Context) ([]redash.DataSource, error) {
	if m.ListDataSourcesFunc != nil {
		return m.ListDataSourcesFunc(ctx)
	}
	return nil, nil
}

// MockSourceRegistry implements redash.SourceRegistry for testing.
type MockSourceRegistry struct {
	sources map[string]redash.DataSource
	allList []redash.DataSource
}

func NewMockSourceRegistry(sources []redash.DataSource) *MockSourceRegistry {
	m := &MockSourceRegistry{
		sources: make(map[string]redash.DataSource, len(sources)),
		allList: sources,
	}
	for _, ds := range sources {
		m.sources[strings.ToLower(ds.Name)] = ds
	}
	return m
}

func (m *MockSourceRegistry) Lookup(name string) (redash.DataSource, bool) {
	ds, ok := m.sources[strings.ToLower(name)]
	return ds, ok
}

func (m *MockSourceRegistry) All() []redash.DataSource {
	return append([]redash.DataSource(nil), m.allList...)
}

func SampleDataSources() []redash.DataSource {
	return []redash.DataSource{
		{ID: 1, Name: "Production PG", Type: "pg"},
		{ID: 2, Name: "Analytics MySQL", Type: "mysql"},
		{ID: 3, Name: "Redshift DW", Type: "redshift"},
	}
}

func SampleSchema() []redash.SchemaTable {
	return []redash.SchemaTable{
		{Name: "users", Columns: []redash.SchemaColumn{{Name: "id", Type: "int"}, {Name: "name", Type: "varchar"}, {Name: "email", Type: "varchar"}}},
		{Name: "orders", Columns: []redash.SchemaColumn{{Name: "id", Type: "int"}, {Name: "user_id", Type: "int"}, {Name: "total", Type: "decimal"}}},
	}
}
