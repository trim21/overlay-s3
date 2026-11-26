package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	path := writeConfig(t, `
listen = "127.0.0.1:9001"
auth_key = "client"
auth_secret = "client-secret"

[buckets.demo.overlay]
endpoint = "http://127.0.0.1:9000"
access_key = "overlay-ak"
secret_key = "overlay-sk"
bucket = "overlay-data"
prefix = "gateway"

[buckets.demo.baseline]
endpoint = "http://127.0.0.1:9100"
access_key = "baseline-ak"
secret_key = "baseline-sk"

[buckets.archive.overlay]
endpoint = "https://s3.example.com"
access_key = "archive-ak"
secret_key = "archive-sk"
bucket = "archive-overlay"
path_style = false

[buckets.archive.baseline]
endpoint = "https://s3.example.com"
access_key = "archive-baseline-ak"
secret_key = "archive-baseline-sk"
path_style = false
`)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9001" || cfg.AuthKey != "client" || cfg.AuthSecret != "client-secret" {
		t.Fatalf("global settings = %+v", cfg)
	}
	if len(cfg.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(cfg.Buckets))
	}

	demo := cfg.Buckets["demo"]
	if demo.Overlay.Bucket != "overlay-data" || demo.Overlay.Prefix != "gateway" {
		t.Fatalf("demo overlay = %+v", demo.Overlay)
	}
	if got := regionOr(demo.Overlay.Region); got != defaultRegion {
		t.Fatalf("demo overlay region = %q, want %q", got, defaultRegion)
	}
	if !pathStyle(demo.Overlay.PathStyle) || !pathStyle(demo.Baseline.PathStyle) {
		t.Fatal("unset path_style must default to true")
	}

	archive := cfg.Buckets["archive"]
	if pathStyle(archive.Overlay.PathStyle) || pathStyle(archive.Baseline.PathStyle) {
		t.Fatal("path_style = false must be honored")
	}
	if archive.Baseline.Endpoint != "https://s3.example.com" {
		t.Fatalf("archive baseline endpoint = %q", archive.Baseline.Endpoint)
	}
}

func TestLoadConfigDefaultsListen(t *testing.T) {
	path := writeConfig(t, validConfig)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Fatalf("listen = %q, want :8080", cfg.Listen)
	}
}

const validConfig = `
[buckets.demo.overlay]
endpoint = "http://127.0.0.1:9000"
access_key = "ak"
secret_key = "sk"
bucket = "overlay-data"

[buckets.demo.baseline]
endpoint = "http://127.0.0.1:9000"
access_key = "ak"
secret_key = "sk"
`

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unknown key",
			body: strings.Replace(validConfig, "bucket = \"overlay-data\"",
				"bucket = \"overlay-data\"\nbucket_name = \"typo\"", 1),
			want: "unknown keys: buckets.demo.overlay.bucket_name",
		},
		{
			name: "missing overlay bucket",
			body: strings.Replace(validConfig, "bucket = \"overlay-data\"", "", 1),
			want: `bucket "demo" overlay: missing required keys: bucket`,
		},
		{
			name: "missing baseline credentials",
			body: `
[buckets.demo.overlay]
endpoint = "http://127.0.0.1:9000"
access_key = "ak"
secret_key = "sk"
bucket = "overlay-data"

[buckets.demo.baseline]
endpoint = "http://127.0.0.1:9000"
access_key = "ak"
`,
			want: `bucket "demo" baseline: missing required keys: secret_key`,
		},
		{
			name: "no buckets",
			body: `listen = ":8080"`,
			want: "no buckets configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, tt.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}
