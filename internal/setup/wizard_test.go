package setup

import "testing"

func TestValidateURL(t *testing.T) {
	tests := []struct {
		url     string
		wantErr bool
	}{
		{"https://redash.example.com", false},
		{"http://localhost:5000", false},
		{"", true},
		{"redash.example.com", true},
		{"ftp://redash.example.com", true},
		{"https://", true},
	}
	for _, tt := range tests {
		if err := ValidateURL(tt.url); (err != nil) != tt.wantErr {
			t.Errorf("ValidateURL(%q) = %v, wantErr %v", tt.url, err, tt.wantErr)
		}
	}
}
