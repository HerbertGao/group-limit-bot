package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTempYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return path
}

func loadFromFakeEnv(t *testing.T, path string, env map[string]string) (*Config, error) {
	t.Helper()
	getenv := func(k string) string { return env[k] }
	return loadWithGetenv(path, getenv, os.ReadFile)
}

func TestLoad_YAMLOnly(t *testing.T) {
	path := writeTempYAML(t, `
bot_token: yaml-token
db_path: /tmp/yaml.db
cache_ttl: 5m
log_level: debug
bot_allowlist: [123, 456]
allow_anonymous_admin: false
`)
	c, err := loadFromFakeEnv(t, path, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.BotToken != "yaml-token" {
		t.Errorf("BotToken = %q", c.BotToken)
	}
	if c.DBPath != "/tmp/yaml.db" {
		t.Errorf("DBPath = %q", c.DBPath)
	}
	if c.CacheTTL != 5*time.Minute {
		t.Errorf("CacheTTL = %v", c.CacheTTL)
	}
	if c.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
	if len(c.BotAllowlist) != 2 || c.BotAllowlist[0] != 123 || c.BotAllowlist[1] != 456 {
		t.Errorf("BotAllowlist = %v", c.BotAllowlist)
	}
	if c.AllowAnonymousAdmin {
		t.Error("AllowAnonymousAdmin should be false")
	}
}

func TestLoad_EnvOnly(t *testing.T) {
	env := map[string]string{
		"BOT_TOKEN":                 "env-token",
		"BOT_DB_PATH":               "/tmp/env.db",
		"BOT_CACHE_TTL":             "10m",
		"BOT_LOG_LEVEL":             "warn",
		"BOT_BOT_ALLOWLIST":         "1,2, 3",
		"BOT_ALLOW_ANONYMOUS_ADMIN": "false",
	}
	c, err := loadFromFakeEnv(t, "", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.BotToken != "env-token" {
		t.Errorf("BotToken = %q", c.BotToken)
	}
	if c.DBPath != "/tmp/env.db" {
		t.Errorf("DBPath = %q", c.DBPath)
	}
	if c.CacheTTL != 10*time.Minute {
		t.Errorf("CacheTTL = %v", c.CacheTTL)
	}
	if c.LogLevel != "warn" {
		t.Errorf("LogLevel = %q", c.LogLevel)
	}
	if len(c.BotAllowlist) != 3 {
		t.Errorf("BotAllowlist = %v", c.BotAllowlist)
	}
	if c.AllowAnonymousAdmin {
		t.Error("AllowAnonymousAdmin should be false")
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	path := writeTempYAML(t, `
bot_token: yaml-token
db_path: /tmp/yaml.db
`)
	env := map[string]string{"BOT_TOKEN": "env-token"}
	c, err := loadFromFakeEnv(t, path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.BotToken != "env-token" {
		t.Errorf("env should override yaml: got %q", c.BotToken)
	}
	if c.DBPath != "/tmp/yaml.db" {
		t.Errorf("yaml DBPath should survive: got %q", c.DBPath)
	}
}

func TestLoad_MissingToken(t *testing.T) {
	_, err := loadFromFakeEnv(t, "", nil)
	if err == nil {
		t.Fatal("expected error when token missing")
	}
}

func TestLoad_MissingFileIsOK(t *testing.T) {
	env := map[string]string{"BOT_TOKEN": "env-token"}
	c, err := loadFromFakeEnv(t, "/nonexistent/path.yaml", env)
	if err != nil {
		t.Fatalf("should accept missing file when env provides token: %v", err)
	}
	if c.BotToken != "env-token" {
		t.Errorf("BotToken = %q", c.BotToken)
	}
}

func TestLoad_NewAllowlistEnvName(t *testing.T) {
	env := map[string]string{
		"BOT_TOKEN":     "env-token",
		"BOT_ALLOWLIST": "1,2,3",
	}
	c, err := loadFromFakeEnv(t, "", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.BotAllowlist) != 3 || c.BotAllowlist[0] != 1 || c.BotAllowlist[1] != 2 || c.BotAllowlist[2] != 3 {
		t.Errorf("BotAllowlist = %v, want [1 2 3]", c.BotAllowlist)
	}
}

func TestLoad_NewAllowlistYAMLName(t *testing.T) {
	path := writeTempYAML(t, `
bot_token: yaml-token
allowlist: [4, 5]
`)
	c, err := loadFromFakeEnv(t, path, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.BotAllowlist) != 2 || c.BotAllowlist[0] != 4 || c.BotAllowlist[1] != 5 {
		t.Errorf("BotAllowlist = %v, want [4 5]", c.BotAllowlist)
	}
}

func TestLoad_NewNameOverridesOldName(t *testing.T) {
	path := writeTempYAML(t, `
bot_token: yaml-token
bot_allowlist: [1]
allowlist: [9]
`)
	c, err := loadFromFakeEnv(t, path, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.BotAllowlist) != 1 || c.BotAllowlist[0] != 9 {
		t.Errorf("BotAllowlist = %v, want [9]", c.BotAllowlist)
	}
}
