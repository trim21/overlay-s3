package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

const defaultRegion = "us-east-1"

// config is the gateway configuration file. Listener and client credentials
// are global; every client-visible bucket declares its own overlay and
// baseline S3 backends.
type config struct {
	Listen     string                  `toml:"listen"`
	AuthKey    string                  `toml:"auth_key"`
	AuthSecret string                  `toml:"auth_secret"`
	Buckets    map[string]bucketConfig `toml:"buckets"`
}

// bucketConfig is one client-visible bucket: writes land on the overlay,
// reads fall back to the baseline.
type bucketConfig struct {
	Overlay  overlayConfig  `toml:"overlay"`
	Baseline baselineConfig `toml:"baseline"`
}

// overlayConfig addresses the write layer. Client bucket b and key k map to
// bucket/prefix/b/k, so several client buckets may share one physical bucket
// as long as their prefixes differ.
type overlayConfig struct {
	Endpoint  string `toml:"endpoint"`
	Region    string `toml:"region"`
	AccessKey string `toml:"access_key"`
	SecretKey string `toml:"secret_key"`
	Bucket    string `toml:"bucket"`
	Prefix    string `toml:"prefix"`
	PathStyle *bool  `toml:"path_style"`
}

// baselineConfig addresses the read-fallback layer. It has no bucket of its
// own: the client bucket is looked up under the same name on the baseline.
type baselineConfig struct {
	Endpoint  string `toml:"endpoint"`
	Region    string `toml:"region"`
	AccessKey string `toml:"access_key"`
	SecretKey string `toml:"secret_key"`
	PathStyle *bool  `toml:"path_style"`
}

// loadConfig reads the TOML file and validates it. A missing path_style
// defaults to true (path-style addressing, as MinIO and most self-hosted
// gateways need) and a missing region to us-east-1.
func loadConfig(path string) (*config, error) {
	var cfg config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	// a typo in a key must be an error, not a silently ignored option
	if keys := md.Undecoded(); len(keys) > 0 {
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.String()
		}
		return nil, fmt.Errorf("config %s: unknown keys: %s", path, strings.Join(names, ", "))
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *config) validate() error {
	if len(c.Buckets) == 0 {
		return errors.New("no buckets configured")
	}
	for name, b := range c.Buckets {
		if err := b.Overlay.validate(); err != nil {
			return fmt.Errorf("bucket %q overlay: %w", name, err)
		}
		if err := b.Baseline.validate(); err != nil {
			return fmt.Errorf("bucket %q baseline: %w", name, err)
		}
	}
	return nil
}

func (o *overlayConfig) validate() error {
	var missing []string
	if o.Endpoint == "" {
		missing = append(missing, "endpoint")
	}
	if o.AccessKey == "" {
		missing = append(missing, "access_key")
	}
	if o.SecretKey == "" {
		missing = append(missing, "secret_key")
	}
	if o.Bucket == "" {
		missing = append(missing, "bucket")
	}
	return missingKeys(missing)
}

func (b *baselineConfig) validate() error {
	var missing []string
	if b.Endpoint == "" {
		missing = append(missing, "endpoint")
	}
	if b.AccessKey == "" {
		missing = append(missing, "access_key")
	}
	if b.SecretKey == "" {
		missing = append(missing, "secret_key")
	}
	return missingKeys(missing)
}

func missingKeys(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing required keys: %s", strings.Join(missing, ", "))
}

// pathStyle reports whether path-style addressing is enabled. Unset means
// enabled: virtual-host addressing needs DNS and TLS wildcard certificates,
// which self-hosted S3 endpoints rarely have.
func pathStyle(setting *bool) bool {
	return setting == nil || *setting
}

func regionOr(region string) string {
	if region == "" {
		return defaultRegion
	}
	return region
}
