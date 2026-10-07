package application

import "testing"

func TestVerifyMySQLVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		{name: "minimum version", version: "8.0.16"},
		{name: "newer patch", version: "8.0.43"},
		{name: "suffixed release", version: "8.4.0-commercial"},
		{name: "below minimum", version: "8.0.15", wantErr: true},
		{name: "older major version", version: "5.7.44", wantErr: true},
		{name: "MariaDB", version: "10.11.9-MariaDB", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifyMySQLVersion(test.version)
			if test.wantErr && err == nil {
				t.Fatal("expected unsupported database version to be rejected")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("verify supported database version: %v", err)
			}
		})
	}
}
