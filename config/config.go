package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// InstanceConfig holds connection details for a single Immich instance.
type InstanceConfig struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
}

// SyncRuleEndpoint identifies one side (source or destination) of a sync rule.
type SyncRuleEndpoint struct {
	Instance  string `json:"instance"`
	AlbumName string `json:"album_name"`
}

// SyncRule describes a single album-to-album transfer between two instances.
type SyncRule struct {
	Name             string           `json:"name"`
	Source           SyncRuleEndpoint `json:"source"`
	Destination      SyncRuleEndpoint `json:"destination"`
	DeleteFromSource *bool            `json:"delete_from_source,omitempty"`
	AutoDiscovered   bool             `json:"-"`
}

// ShouldDeleteFromSource returns the effective value of DeleteFromSource,
// defaulting to true when the field is not set.
func (r SyncRule) ShouldDeleteFromSource() bool {
	if r.DeleteFromSource == nil {
		return true
	}
	return *r.DeleteFromSource
}

// Config is the top-level configuration loaded from config.json.
type Config struct {
	Instances    map[string]InstanceConfig `json:"instances"`
	SyncRules    []SyncRule               `json:"sync_rules,omitempty"`
	PollInterval string                    `json:"poll_interval"`
	LogLevel     string                    `json:"log_level"`
}

// Load reads and validates a config file from disk.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if len(c.Instances) == 0 {
		return fmt.Errorf("no instances defined")
	}

	for name, inst := range c.Instances {
		if inst.URL == "" {
			return fmt.Errorf("instance %q: url is required", name)
		}
		if inst.APIKey == "" {
			return fmt.Errorf("instance %q: api_key is required", name)
		}
	}

	for i := range c.SyncRules {
		rule := &c.SyncRules[i]
		if rule.Source.Instance == "" {
			return fmt.Errorf("sync_rules[%d]: source.instance is required", i)
		}
		if rule.Source.AlbumName == "" {
			return fmt.Errorf("sync_rules[%d]: source.album_name is required", i)
		}
		if rule.Destination.Instance == "" {
			return fmt.Errorf("sync_rules[%d]: destination.instance is required", i)
		}
		if rule.Destination.AlbumName == "" {
			return fmt.Errorf("sync_rules[%d]: destination.album_name is required", i)
		}
		if _, ok := c.Instances[rule.Source.Instance]; !ok {
			return fmt.Errorf("sync_rules[%d]: source instance %q not found in instances", i, rule.Source.Instance)
		}
		if _, ok := c.Instances[rule.Destination.Instance]; !ok {
			return fmt.Errorf("sync_rules[%d]: destination instance %q not found in instances", i, rule.Destination.Instance)
		}
		// Auto-generate name if not provided
		if rule.Name == "" {
			rule.Name = fmt.Sprintf("explicit:%s/%s→%s/%s",
				rule.Source.Instance, rule.Source.AlbumName,
				rule.Destination.Instance, rule.Destination.AlbumName)
		}
	}

	if c.PollInterval != "" {
		if _, err := time.ParseDuration(c.PollInterval); err != nil {
			return fmt.Errorf("invalid poll_interval %q: %w", c.PollInterval, err)
		}
	}

	return nil
}

// GetPollDuration returns the parsed poll interval, defaulting to 5m.
func (c *Config) GetPollDuration() time.Duration {
	if c.PollInterval == "" {
		return 5 * time.Minute
	}
	d, _ := time.ParseDuration(c.PollInterval)
	return d
}
