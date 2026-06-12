package redash

import "testing"

func newTestRegistry(t *testing.T) *DataSourceRegistry {
	t.Helper()
	return NewDataSourceRegistry([]DataSource{
		{ID: 1, Name: "Production", Type: "pg"},
		{ID: 2, Name: "Analytics", Type: "mysql"},
		{ID: 3, Name: "Warehouse", Type: "redshift"},
	})
}

func TestLookup(t *testing.T) {
	tests := []struct {
		name   string
		lookup string
		wantID int
		wantOK bool
	}{
		{name: "exact match", lookup: "Production", wantID: 1, wantOK: true},
		{name: "case insensitive lower", lookup: "production", wantID: 1, wantOK: true},
		{name: "case insensitive upper", lookup: "ANALYTICS", wantID: 2, wantOK: true},
		{name: "case insensitive mixed", lookup: "wArEhOuSe", wantID: 3, wantOK: true},
		{name: "not found", lookup: "Staging", wantID: 0, wantOK: false},
		{name: "empty string", lookup: "", wantID: 0, wantOK: false},
	}

	reg := newTestRegistry(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ds, ok := reg.Lookup(tt.lookup)
			if ok != tt.wantOK {
				t.Fatalf("Lookup(%q) ok = %v, want %v", tt.lookup, ok, tt.wantOK)
			}
			if ds.ID != tt.wantID {
				t.Errorf("Lookup(%q) ID = %d, want %d", tt.lookup, ds.ID, tt.wantID)
			}
		})
	}
}

func TestAll(t *testing.T) {
	reg := newTestRegistry(t)

	got := reg.All()
	if len(got) != 3 {
		t.Fatalf("All() returned %d elements, want 3", len(got))
	}

	// Mutate the returned slice and verify the registry is unaffected.
	got[0] = DataSource{ID: 999, Name: "Mutated", Type: "x"}

	fresh := reg.All()
	if len(fresh) != 3 {
		t.Fatalf("after mutation, All() returned %d elements, want 3", len(fresh))
	}
	if fresh[0].ID != 1 {
		t.Errorf("after mutation, first element ID = %d, want 1", fresh[0].ID)
	}
}

func TestAllEmpty(t *testing.T) {
	reg := NewDataSourceRegistry(nil)
	got := reg.All()
	if len(got) != 0 {
		t.Fatalf("All() on empty registry returned %d elements, want 0", len(got))
	}
}

func TestIsPostgresCompatible(t *testing.T) {
	tests := []struct {
		dsType string
		want   bool
	}{
		{"pg", true},
		{"postgres", true},
		{"postgresql", true},
		{"redshift", true},
		{"cockroachdb", true},
		{"PG", true},
		{"Postgres", true},
		{"PostgreSQL", true},
		{"Redshift", true},
		{"CockroachDB", true},
		{"my_postgres_db", true},
		{"mysql", false},
		{"sqlite", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.dsType, func(t *testing.T) {
			if got := IsPostgresCompatible(tt.dsType); got != tt.want {
				t.Errorf("IsPostgresCompatible(%q) = %v, want %v", tt.dsType, got, tt.want)
			}
		})
	}
}

func TestIsMySQLCompatible(t *testing.T) {
	tests := []struct {
		dsType string
		want   bool
	}{
		{"mysql", true},
		{"rds_mysql", true},
		{"aurora_mysql", true},
		{"mariadb", true},
		{"MySQL", true},
		{"RDS_MySQL", true},
		{"Aurora_MySQL", true},
		{"MariaDB", true},
		{"custom_mysql_proxy", true},
		{"pg", false},
		{"postgres", false},
		{"sqlite", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.dsType, func(t *testing.T) {
			if got := IsMySQLCompatible(tt.dsType); got != tt.want {
				t.Errorf("IsMySQLCompatible(%q) = %v, want %v", tt.dsType, got, tt.want)
			}
		})
	}
}

func TestFilterByType(t *testing.T) {
	sources := []DataSource{
		{ID: 1, Name: "PG1", Type: "pg"},
		{ID: 2, Name: "MySQL1", Type: "mysql"},
		{ID: 3, Name: "PG2", Type: "postgres"},
		{ID: 4, Name: "Redshift", Type: "redshift"},
		{ID: 5, Name: "SQLite", Type: "sqlite"},
	}

	t.Run("filter postgres compatible", func(t *testing.T) {
		got := FilterByType(sources, IsPostgresCompatible)
		wantIDs := []int{1, 3, 4}
		assertIDs(t, got, wantIDs)
	})

	t.Run("filter mysql compatible", func(t *testing.T) {
		got := FilterByType(sources, IsMySQLCompatible)
		wantIDs := []int{2}
		assertIDs(t, got, wantIDs)
	})

	t.Run("no matches", func(t *testing.T) {
		got := FilterByType(sources, func(string) bool { return false })
		if len(got) != 0 {
			t.Errorf("expected 0 results, got %d", len(got))
		}
	})

	t.Run("all match", func(t *testing.T) {
		got := FilterByType(sources, func(string) bool { return true })
		if len(got) != len(sources) {
			t.Errorf("expected %d results, got %d", len(sources), len(got))
		}
	})

	t.Run("nil slice", func(t *testing.T) {
		got := FilterByType(nil, func(string) bool { return true })
		if len(got) != 0 {
			t.Errorf("expected 0 results for nil input, got %d", len(got))
		}
	})
}

func assertIDs(t *testing.T, got []DataSource, wantIDs []int) {
	t.Helper()
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d data sources, want %d", len(got), len(wantIDs))
	}
	for i, ds := range got {
		if ds.ID != wantIDs[i] {
			t.Errorf("result[%d].ID = %d, want %d", i, ds.ID, wantIDs[i])
		}
	}
}
