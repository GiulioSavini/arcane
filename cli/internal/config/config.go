// Package config loads and persists CLI configuration in ~/.config/arcanecli.yml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samber/hot"
	"go.getarcane.app/acfs/atomic"
	"go.yaml.in/yaml/v4"

	"github.com/getarcaneapp/arcane/cli/v2/internal/types"
)

// Version and build information - set via ldflags at build time
var (
	Version          = "dev"
	Revision         = "unknown"
	CLIStableBaseURL = "https://github.com/getarcaneapp/arcane/releases/download"
	CLINextBaseURL   = "https://bucket.getarcane.app/bin/cli-next"
)

const (
	configFileName             = "arcanecli.yml"
	defaultPaginationInitLimit = 20
)

var customConfigPath string

var configCache = hot.NewHotCache[string, *types.Config](hot.LRU, 4).
	WithCopyOnRead((*types.Config).Clone).
	WithCopyOnWrite((*types.Config).Clone).
	Build()

func normalizeConfig(cfg *types.Config) *types.Config {
	if cfg == nil {
		return DefaultConfig()
	}
	normalized := cfg.Clone()
	if normalized == nil {
		return DefaultConfig()
	}

	if normalized.Pagination.Resources == nil {
		normalized.Pagination.Resources = make(map[string]types.PaginationResourceConfig)
	}

	return normalized
}

func invalidateCache() {
	configCache.Purge()
}

// DefaultConfig uses localhost:3552, local environment "0", and info logging.
func DefaultConfig() *types.Config {
	return &types.Config{
		ServerURL:          "http://localhost:3552",
		DefaultEnvironment: "0",
		LogLevel:           "info",
	}
}

// ConfigPath resolves the active config path, defaulting to ~/.config/arcanecli.yml.
func ConfigPath() (string, error) {
	if customConfigPath != "" {
		return customConfigPath, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(home, ".config", configFileName), nil
}

// SetConfigPath overrides the default configuration file location.
// Accepts absolute or relative paths and expands a leading ~ to the home directory.
func SetConfigPath(path string) error {
	if strings.TrimSpace(path) == "" {
		customConfigPath = ""
		invalidateCache()
		return nil
	}

	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to expand config path: %w", err)
		}
		rel := strings.TrimPrefix(path, "~")
		path = filepath.Join(home, strings.TrimPrefix(rel, string(os.PathSeparator)))
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("failed to resolve config path: %w", err)
	}

	customConfigPath = absPath
	invalidateCache()
	return nil
}

// Load reads configuration, using defaults when the file does not exist.
func Load() (*types.Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}

	if cfg, ok, _ := configCache.Get(path); ok {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			def := DefaultConfig()
			configCache.Set(path, def)
			return def, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	cfg := DefaultConfig()
	if parseErr := yaml.Unmarshal(data, cfg); parseErr != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", parseErr)
	}
	normalized := normalizeConfig(cfg)

	configCache.Set(path, normalized)

	return normalized, nil
}

// Save writes configuration with mode 0600, creating its directory when needed.
func Save(c *types.Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}

	cfg := normalizeConfig(c)
	cfg.Pagination.Default.Limit = max(0, cfg.Pagination.Default.Limit)
	resources := cfg.Pagination.Resources
	cfg.Pagination.Resources = make(map[string]types.PaginationResourceConfig, len(resources))
	for resource, rc := range resources {
		if rc.Limit > 0 {
			cfg.SetResourceLimit(resource, rc.Limit)
		}
	}

	if writeErr := writeConfig(path, cfg, false); writeErr != nil {
		return writeErr
	}

	configCache.Set(path, cfg)

	return nil
}

// InitDefaultFile creates a template with credential placeholders and returns false if the file already exists.
func InitDefaultFile() (bool, error) {
	path, err := ConfigPath()
	if err != nil {
		return false, err
	}

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return false, fmt.Errorf("config path is a directory: %s", path)
		}
		return false, nil
	case os.IsNotExist(err):
		// Continue and create the file below.
	default:
		return false, fmt.Errorf("failed to stat config path: %w", err)
	}

	cfg := DefaultConfig()
	cfg.SetDefaultLimit(defaultPaginationInitLimit)
	for _, resource := range types.KnownPaginatedResources {
		cfg.SetResourceLimit(resource, defaultPaginationInitLimit)
	}
	var template yaml.Node
	if encodeErr := template.Encode(cfg); encodeErr != nil {
		return false, fmt.Errorf("failed to encode config template: %w", encodeErr)
	}
	// Show empty authentication fields even though normal saves omit them.
	for _, key := range []string{"api_key", "jwt_token", "refresh_token", "federated_audience"} {
		template.Content = append(template.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""},
		)
	}

	if writeErr := writeConfig(path, &template, true); writeErr != nil {
		if errors.Is(writeErr, os.ErrExist) {
			return false, nil
		}
		return false, writeErr
	}

	invalidateCache()
	return true, nil
}

func writeConfig(path string, cfg any, exclusive bool) (err error) {
	data, marshalErr := yaml.Marshal(cfg)
	if marshalErr != nil {
		return fmt.Errorf("failed to marshal config: %w", marshalErr)
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o700); mkdirErr != nil {
		return fmt.Errorf("failed to create config directory: %w", mkdirErr)
	}
	writePath := path
	if exclusive {
		stagingDir, tempErr := os.MkdirTemp(filepath.Dir(path), ".arcanecli-*")
		if tempErr != nil {
			return fmt.Errorf("failed to create config staging directory: %w", tempErr)
		}
		defer func() {
			if cleanupErr := os.RemoveAll(stagingDir); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("failed to remove config staging directory: %w", cleanupErr))
			}
		}()
		writePath = filepath.Join(stagingDir, configFileName)
	}
	if writeErr := atomic.WriteFile(writePath, data, 0o600); writeErr != nil {
		return fmt.Errorf("failed to write config file: %w", writeErr)
	}
	if exclusive {
		// Linking publishes the complete file without replacing an existing path.
		if linkErr := os.Link(writePath, path); linkErr != nil {
			return fmt.Errorf("failed to create config file: %w", linkErr)
		}
	}
	return nil
}

// BackupFile moves the active config to .bak, returning moved=false when no config exists.
func BackupFile() (backupPath string, moved bool, err error) {
	path, err := ConfigPath()
	if err != nil {
		return "", false, err
	}
	backupPath = path + ".bak"

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return "", false, fmt.Errorf("config path is a directory: %s", path)
		}
	case os.IsNotExist(err):
		return backupPath, false, nil
	default:
		return "", false, fmt.Errorf("failed to stat config path: %w", err)
	}

	if existingBackup, backupErr := os.Stat(backupPath); backupErr == nil {
		if existingBackup.IsDir() {
			return "", false, fmt.Errorf("backup path is a directory: %s", backupPath)
		}
		rotatedPath := fmt.Sprintf("%s.%s", backupPath, time.Now().UTC().Format("20060102150405"))
		if renameErr := os.Rename(backupPath, rotatedPath); renameErr != nil {
			return "", false, fmt.Errorf("failed to rotate existing backup %s: %w", backupPath, renameErr)
		}
	} else if !os.IsNotExist(backupErr) {
		return "", false, fmt.Errorf("failed to stat backup path: %w", backupErr)
	}

	if backupConfigErr := os.Rename(path, backupPath); backupConfigErr != nil {
		return "", false, fmt.Errorf("failed to move config to backup: %w", backupConfigErr)
	}

	invalidateCache()
	return backupPath, true, nil
}
