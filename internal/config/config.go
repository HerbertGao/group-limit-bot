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
	// Guest-bot summoner punishment policy.
	GuestMuteThreshold int           // violation count at which the summoner is muted (>= 2)
	GuestMuteDuration  time.Duration // how long restrictChatMember mutes the summoner
	GuestBanThreshold  int           // violation count at which the summoner is banned (> GuestMuteThreshold)
}

type rawConfig struct {
	BotToken            *string  `yaml:"bot_token"`
	DBPath              *string  `yaml:"db_path"`
	CacheTTL            *string  `yaml:"cache_ttl"`
	LogLevel            *string  `yaml:"log_level"`
	Allowlist           *[]int64 `yaml:"allowlist"`
	BotAllowlist        *[]int64 `yaml:"bot_allowlist"` // deprecated, fallback only
	AllowAnonymousAdmin *bool    `yaml:"allow_anonymous_admin"`
	GuestMuteThreshold  *int     `yaml:"guest_mute_threshold"`
	GuestMuteDuration   *string  `yaml:"guest_mute_duration"`
	GuestBanThreshold   *int     `yaml:"guest_ban_threshold"`
}

func defaults() *Config {
	return &Config{
		DBPath:              "./bot.db",
		CacheTTL:            30 * time.Minute,
		LogLevel:            "info",
		AllowAnonymousAdmin: true,
		GuestMuteThreshold:  2,
		GuestMuteDuration:   24 * time.Hour,
		GuestBanThreshold:   4,
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
			if raw.GuestMuteThreshold != nil {
				c.GuestMuteThreshold = *raw.GuestMuteThreshold
			}
			if raw.GuestMuteDuration != nil && *raw.GuestMuteDuration != "" {
				d, err := time.ParseDuration(*raw.GuestMuteDuration)
				if err != nil {
					return nil, fmt.Errorf("parse guest_mute_duration: %w", err)
				}
				c.GuestMuteDuration = d
			}
			if raw.GuestBanThreshold != nil {
				c.GuestBanThreshold = *raw.GuestBanThreshold
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
	if v := getenv("BOT_GUEST_MUTE_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("parse BOT_GUEST_MUTE_THRESHOLD: %w", err)
		}
		c.GuestMuteThreshold = n
	}
	if v := getenv("BOT_GUEST_MUTE_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("parse BOT_GUEST_MUTE_DURATION: %w", err)
		}
		c.GuestMuteDuration = d
	}
	if v := getenv("BOT_GUEST_BAN_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("parse BOT_GUEST_BAN_THRESHOLD: %w", err)
		}
		c.GuestBanThreshold = n
	}

	if c.BotToken == "" {
		return nil, errors.New("bot_token is required (config.yaml or env BOT_TOKEN)")
	}
	// Punishment thresholds must guarantee "first violation is never punished":
	// the mute threshold must be at least 2, and ban must escalate strictly above mute.
	if c.GuestMuteThreshold < 2 {
		return nil, fmt.Errorf("guest_mute_threshold must be >= 2, got %d", c.GuestMuteThreshold)
	}
	if c.GuestBanThreshold <= c.GuestMuteThreshold {
		return nil, fmt.Errorf("guest_ban_threshold (%d) must be greater than guest_mute_threshold (%d)",
			c.GuestBanThreshold, c.GuestMuteThreshold)
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
