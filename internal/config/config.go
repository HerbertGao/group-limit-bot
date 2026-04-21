package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	BotToken            string
	DBPath              string
	CacheTTL            time.Duration
	LogLevel            string
	BotAllowlist        []int64
	AllowAnonymousAdmin bool
}

type rawConfig struct {
	BotToken            *string  `yaml:"bot_token"`
	DBPath              *string  `yaml:"db_path"`
	CacheTTL            *string  `yaml:"cache_ttl"`
	LogLevel            *string  `yaml:"log_level"`
	Allowlist           *[]int64 `yaml:"allowlist"`
	BotAllowlist        *[]int64 `yaml:"bot_allowlist"` // deprecated, fallback only
	AllowAnonymousAdmin *bool    `yaml:"allow_anonymous_admin"`
}

func defaults() *Config {
	return &Config{
		DBPath:              "./bot.db",
		CacheTTL:            30 * time.Minute,
		LogLevel:            "info",
		AllowAnonymousAdmin: true,
	}
}

// Load reads yaml at path (missing file is OK), overlays env vars,
// and validates required fields.
func Load(path string) (*Config, error) {
	return loadWithGetenv(path, os.Getenv, os.ReadFile)
}

func loadWithGetenv(path string, getenv func(string) string, readFile func(string) ([]byte, error)) (*Config, error) {
	c := defaults()

	if path != "" {
		data, err := readFile(path)
		switch {
		case err == nil:
			var raw rawConfig
			if err := yaml.Unmarshal(data, &raw); err != nil {
				return nil, fmt.Errorf("parse yaml: %w", err)
			}
			if raw.BotToken != nil {
				c.BotToken = *raw.BotToken
			}
			if raw.DBPath != nil && *raw.DBPath != "" {
				c.DBPath = *raw.DBPath
			}
			if raw.CacheTTL != nil && *raw.CacheTTL != "" {
				d, err := time.ParseDuration(*raw.CacheTTL)
				if err != nil {
					return nil, fmt.Errorf("parse cache_ttl: %w", err)
				}
				c.CacheTTL = d
			}
			if raw.LogLevel != nil && *raw.LogLevel != "" {
				c.LogLevel = *raw.LogLevel
			}
			switch {
			case raw.Allowlist != nil:
				c.BotAllowlist = *raw.Allowlist
			case raw.BotAllowlist != nil:
				c.BotAllowlist = *raw.BotAllowlist
			}
			if raw.AllowAnonymousAdmin != nil {
				c.AllowAnonymousAdmin = *raw.AllowAnonymousAdmin
			}
		case errors.Is(err, os.ErrNotExist):
			// ok — env may still supply everything
		default:
			return nil, fmt.Errorf("read config file: %w", err)
		}
	}

	if v := getenv("BOT_TOKEN"); v != "" {
		c.BotToken = v
	}
	if v := getenv("BOT_DB_PATH"); v != "" {
		c.DBPath = v
	}
	if v := getenv("BOT_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parse BOT_CACHE_TTL: %w", err)
		}
		c.CacheTTL = d
	}
	if v := getenv("BOT_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := getenv("BOT_ALLOWLIST"); v != "" {
		list, err := parseIntList(v)
		if err != nil {
			return nil, fmt.Errorf("parse BOT_ALLOWLIST: %w", err)
		}
		c.BotAllowlist = list
	} else if v := getenv("BOT_BOT_ALLOWLIST"); v != "" {
		list, err := parseIntList(v)
		if err != nil {
			return nil, fmt.Errorf("parse BOT_BOT_ALLOWLIST: %w", err)
		}
		c.BotAllowlist = list
	}
	if v := getenv("BOT_ALLOW_ANONYMOUS_ADMIN"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("parse BOT_ALLOW_ANONYMOUS_ADMIN: %w", err)
		}
		c.AllowAnonymousAdmin = b
	}

	if c.BotToken == "" {
		return nil, errors.New("bot_token is required (config.yaml or env BOT_TOKEN)")
	}
	return c, nil
}

func parseIntList(s string) ([]int64, error) {
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
