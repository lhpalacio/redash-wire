package redash

import "context"

type QueryExecutor interface {
	ExecuteQuery(ctx context.Context, sql string, dataSourceID int) (*QueryResult, error)
}

type SchemaFetcher interface {
	GetSchema(ctx context.Context, dataSourceID int) ([]SchemaTable, error)
}

type DataSourceLister interface {
	ListDataSources(ctx context.Context) ([]DataSource, error)
}

type RedashAPI interface {
	QueryExecutor
	SchemaFetcher
	DataSourceLister
}

type SourceRegistry interface {
	Lookup(name string) (DataSource, bool)
	All() []DataSource
}
