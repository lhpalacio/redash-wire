package redash

import (
	"strings"
)

type DataSourceRegistry struct {
	byName map[string]DataSource
	all    []DataSource
}

func NewDataSourceRegistry(sources []DataSource) *DataSourceRegistry {
	r := &DataSourceRegistry{
		byName: make(map[string]DataSource, len(sources)),
		all:    sources,
	}
	for _, ds := range sources {
		r.byName[strings.ToLower(ds.Name)] = ds
	}
	return r
}

func (r *DataSourceRegistry) Lookup(name string) (DataSource, bool) {
	ds, ok := r.byName[strings.ToLower(name)]
	return ds, ok
}

func (r *DataSourceRegistry) All() []DataSource {
	return append([]DataSource(nil), r.all...)
}

func IsPostgresCompatible(dsType string) bool {
	t := strings.ToLower(dsType)
	return t == "pg" || t == "postgres" || t == "postgresql" || t == "redshift" || t == "cockroachdb" || strings.Contains(t, "postgres")
}

func IsMySQLCompatible(dsType string) bool {
	t := strings.ToLower(dsType)
	return t == "mysql" || t == "rds_mysql" || t == "aurora_mysql" || t == "mariadb" || strings.Contains(t, "mysql")
}

func FilterByType(sources []DataSource, pred func(string) bool) []DataSource {
	var filtered []DataSource
	for _, ds := range sources {
		if pred(ds.Type) {
			filtered = append(filtered, ds)
		}
	}
	return filtered
}
