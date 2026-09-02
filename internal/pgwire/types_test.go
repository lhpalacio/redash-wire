package pgwire

import "testing"

func TestRedashTypeToPgOID(t *testing.T) {
	tests := []struct {
		name       string
		redashType string
		want       uint32
	}{
		{name: "string", redashType: "string", want: OidText},
		{name: "integer", redashType: "integer", want: OidInt8},
		{name: "float", redashType: "float", want: OidFloat8},
		{name: "boolean", redashType: "boolean", want: OidBool},
		{name: "datetime is naive until the values say otherwise", redashType: "datetime", want: OidTimestamp},
		{name: "date", redashType: "date", want: OidDate},
		{name: "json", redashType: "json", want: OidJSONB},
		{name: "jsonb", redashType: "jsonb", want: OidJSONB},
		{name: "unknown falls back to text", redashType: "binary", want: OidText},
		{name: "empty falls back to text", redashType: "", want: OidText},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedashTypeToPgOID(tt.redashType)
			if got != tt.want {
				t.Errorf("RedashTypeToPgOID(%q) = %d, want %d", tt.redashType, got, tt.want)
			}
		})
	}
}

func TestRedashTypeToPgSize(t *testing.T) {
	tests := []struct {
		name       string
		redashType string
		want       int16
	}{
		{name: "boolean", redashType: "boolean", want: 1},
		{name: "integer", redashType: "integer", want: 8},
		{name: "float", redashType: "float", want: 8},
		{name: "date", redashType: "date", want: 4},
		{name: "datetime", redashType: "datetime", want: 8},
		{name: "string is variable length", redashType: "string", want: -1},
		{name: "json is variable length", redashType: "json", want: -1},
		{name: "unknown is variable length", redashType: "unknown", want: -1},
		{name: "empty is variable length", redashType: "", want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedashTypeToPgSize(tt.redashType)
			if got != tt.want {
				t.Errorf("RedashTypeToPgSize(%q) = %d, want %d", tt.redashType, got, tt.want)
			}
		})
	}
}
