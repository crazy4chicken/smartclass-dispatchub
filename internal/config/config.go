// Package config loads the dispatchub configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables read by FromEnv and printed by Redacted.
const (
	EnvListenAddr            = "DISPATCH_LISTEN_ADDR"
	EnvDSN                   = "DISPATCH_DSN"
	EnvTimezone              = "DISPATCH_TIMEZONE"
	EnvTeamusersURL          = "DISPATCH_TEAMUSERS_URL"
	EnvTeamusersIssuer       = "DISPATCH_TEAMUSERS_ISSUER"
	EnvTeamusersAud          = "DISPATCH_TEAMUSERS_AUD"
	EnvTeamusersClientID     = "DISPATCH_TEAMUSERS_CLIENT_ID"
	EnvTeamusersClientSecret = "DISPATCH_TEAMUSERS_CLIENT_SECRET"
	EnvTeamusersTimeout      = "DISPATCH_TEAMUSERS_TIMEOUT"
	EnvWebcamURL             = "DISPATCH_WEBCAM_URL"
	EnvWebcamTimeout         = "DISPATCH_WEBCAM_TIMEOUT"
	EnvSchedTick             = "DISPATCH_SCHED_TICK"
	EnvPrestart              = "DISPATCH_PRESTART"
	EnvPoststopGrace         = "DISPATCH_POSTSTOP_GRACE"
	EnvStartRetryMax         = "DISPATCH_START_RETRY_MAX"
	EnvMissGrace             = "DISPATCH_MISS_GRACE"
	EnvPhotoPollInterval     = "DISPATCH_PHOTO_POLL_INTERVAL"
	EnvPhotoPollTTL          = "DISPATCH_PHOTO_POLL_TTL"
	EnvMaxConcurrentCommands = "DISPATCH_MAX_CONCURRENT_COMMANDS"
	EnvMaxImportRows         = "DISPATCH_MAX_IMPORT_ROWS"
	EnvDev                   = "DISPATCH_DEV"
	EnvLogLevel              = "DISPATCH_LOG_LEVEL"
	EnvFile                  = "DISPATCH_ENV_FILE"
)

// Defaults applied by FromEnv when a variable is unset or empty.
const (
	defaultListenAddr            = ":8081"
	defaultTimezone              = "Asia/Shanghai"
	defaultTeamusersIssuer       = "teamusers"
	defaultTeamusersAudience     = "teamusers"
	defaultTeamusersTimeout      = 5 * time.Second
	defaultWebcamTimeout         = 10 * time.Second
	defaultSchedTick             = 30 * time.Second
	defaultPrestart              = 2 * time.Minute
	defaultPoststopGrace         = time.Duration(0)
	defaultStartRetryMax         = 3
	defaultMissGrace             = 10 * time.Minute
	defaultPhotoPollInterval     = 2 * time.Second
	defaultPhotoPollTTL          = 15 * time.Minute
	defaultMaxConcurrentCommands = 8
	defaultMaxImportRows         = 5000
	defaultLogLevel              = "info"
)

// Config holds all dispatchub configuration.
type Config struct {
	ListenAddr            string         // DISPATCH_LISTEN_ADDR, default ":8081"
	DSN                   string         // DISPATCH_DSN (required)
	Timezone              string         // DISPATCH_TIMEZONE, default "Asia/Shanghai"
	Location              *time.Location // resolved from Timezone by FromEnv
	TeamusersURL          string         // DISPATCH_TEAMUSERS_URL (required unless Dev)
	TeamusersIssuer       string         // DISPATCH_TEAMUSERS_ISSUER, default "teamusers"
	TeamusersAudience     string         // DISPATCH_TEAMUSERS_AUD, default "teamusers"
	TeamusersClientID     string         // DISPATCH_TEAMUSERS_CLIENT_ID (required unless Dev)
	TeamusersClientSecret string         // DISPATCH_TEAMUSERS_CLIENT_SECRET (required unless Dev)
	TeamusersTimeout      time.Duration  // DISPATCH_TEAMUSERS_TIMEOUT, default 5s
	WebcamURL             string         // DISPATCH_WEBCAM_URL (required unless Dev)
	WebcamTimeout         time.Duration  // DISPATCH_WEBCAM_TIMEOUT, default 10s
	SchedTick             time.Duration  // DISPATCH_SCHED_TICK, default 30s
	Prestart              time.Duration  // DISPATCH_PRESTART, default 2m
	PoststopGrace         time.Duration  // DISPATCH_POSTSTOP_GRACE, default 0s
	StartRetryMax         int            // DISPATCH_START_RETRY_MAX, default 3
	MissGrace             time.Duration  // DISPATCH_MISS_GRACE, default 10m
	PhotoPollInterval     time.Duration  // DISPATCH_PHOTO_POLL_INTERVAL, default 2s
	PhotoPollTTL          time.Duration  // DISPATCH_PHOTO_POLL_TTL, default 15m
	MaxConcurrentCommands int            // DISPATCH_MAX_CONCURRENT_COMMANDS, default 8
	MaxImportRows         int            // DISPATCH_MAX_IMPORT_ROWS, default 5000
	Dev                   bool           // DISPATCH_DEV, default false
	LogLevel              string         // DISPATCH_LOG_LEVEL, default "info"
}

// FromEnv loads the configuration: DISPATCH_ENV_FILE, when set, is read first,
// then real environment variables win over the file, and defaults fill the
// rest. The result is validated, so a nil error means a usable Config.
func FromEnv() (Config, error) {
	if path := strings.TrimSpace(os.Getenv(EnvFile)); path != "" {
		if err := loadEnvFile(path); err != nil {
			return Config{}, err
		}
	}
	cfg := Config{
		ListenAddr:            envOr(EnvListenAddr, defaultListenAddr),
		DSN:                   strings.TrimSpace(os.Getenv(EnvDSN)),
		Timezone:              envOr(EnvTimezone, defaultTimezone),
		TeamusersURL:          envOr(EnvTeamusersURL, ""),
		TeamusersIssuer:       envOr(EnvTeamusersIssuer, defaultTeamusersIssuer),
		TeamusersAudience:     envOr(EnvTeamusersAud, defaultTeamusersAudience),
		TeamusersClientID:     os.Getenv(EnvTeamusersClientID),
		TeamusersClientSecret: os.Getenv(EnvTeamusersClientSecret),
		WebcamURL:             envOr(EnvWebcamURL, ""),
		LogLevel:              envOr(EnvLogLevel, defaultLogLevel),
	}
	var err error
	if cfg.TeamusersTimeout, err = envDuration(EnvTeamusersTimeout, defaultTeamusersTimeout); err != nil {
		return Config{}, err
	}
	if cfg.WebcamTimeout, err = envDuration(EnvWebcamTimeout, defaultWebcamTimeout); err != nil {
		return Config{}, err
	}
	if cfg.SchedTick, err = envDuration(EnvSchedTick, defaultSchedTick); err != nil {
		return Config{}, err
	}
	if cfg.Prestart, err = envDuration(EnvPrestart, defaultPrestart); err != nil {
		return Config{}, err
	}
	if cfg.PoststopGrace, err = envDuration(EnvPoststopGrace, defaultPoststopGrace); err != nil {
		return Config{}, err
	}
	if cfg.MissGrace, err = envDuration(EnvMissGrace, defaultMissGrace); err != nil {
		return Config{}, err
	}
	if cfg.PhotoPollInterval, err = envDuration(EnvPhotoPollInterval, defaultPhotoPollInterval); err != nil {
		return Config{}, err
	}
	if cfg.PhotoPollTTL, err = envDuration(EnvPhotoPollTTL, defaultPhotoPollTTL); err != nil {
		return Config{}, err
	}
	if cfg.StartRetryMax, err = envInt(EnvStartRetryMax, defaultStartRetryMax); err != nil {
		return Config{}, err
	}
	if cfg.MaxConcurrentCommands, err = envInt(EnvMaxConcurrentCommands, defaultMaxConcurrentCommands); err != nil {
		return Config{}, err
	}
	if cfg.MaxImportRows, err = envInt(EnvMaxImportRows, defaultMaxImportRows); err != nil {
		return Config{}, err
	}
	if cfg.Dev, err = envBool(EnvDev, false); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", EnvTimezone, err)
	}
	cfg.Location = location
	return cfg, nil
}

// Validate reports every missing required value, unparseable timezone or level
// and nonsensical duration or count. Required endpoints and credentials are
// enforced unless Dev is set, mirroring the webcam-server dev bypass.
func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.ListenAddr) == "" {
		errs = append(errs, fmt.Errorf("%s must not be empty", EnvListenAddr))
	}
	if strings.TrimSpace(c.DSN) == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvDSN))
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvTimezone, err))
	}
	if strings.TrimSpace(c.TeamusersIssuer) == "" {
		errs = append(errs, fmt.Errorf("%s must not be empty", EnvTeamusersIssuer))
	}
	if strings.TrimSpace(c.TeamusersAudience) == "" {
		errs = append(errs, fmt.Errorf("%s must not be empty", EnvTeamusersAud))
	}
	if !c.Dev {
		if strings.TrimSpace(c.TeamusersURL) == "" {
			errs = append(errs, fmt.Errorf("%s is required (or set %s=true)", EnvTeamusersURL, EnvDev))
		}
		if strings.TrimSpace(c.TeamusersClientID) == "" {
			errs = append(errs, fmt.Errorf("%s is required (or set %s=true)", EnvTeamusersClientID, EnvDev))
		}
		if strings.TrimSpace(c.TeamusersClientSecret) == "" {
			errs = append(errs, fmt.Errorf("%s is required (or set %s=true)", EnvTeamusersClientSecret, EnvDev))
		}
		if strings.TrimSpace(c.WebcamURL) == "" {
			errs = append(errs, fmt.Errorf("%s is required (or set %s=true)", EnvWebcamURL, EnvDev))
		}
	}
	if c.TeamusersTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvTeamusersTimeout))
	}
	if c.WebcamTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvWebcamTimeout))
	}
	if c.SchedTick <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvSchedTick))
	}
	if c.Prestart < 0 {
		errs = append(errs, fmt.Errorf("%s must not be negative", EnvPrestart))
	}
	if c.PoststopGrace < 0 {
		errs = append(errs, fmt.Errorf("%s must not be negative", EnvPoststopGrace))
	}
	if c.MissGrace <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvMissGrace))
	}
	if c.PhotoPollInterval <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvPhotoPollInterval))
	}
	if c.PhotoPollTTL <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvPhotoPollTTL))
	}
	if c.StartRetryMax < 0 {
		errs = append(errs, fmt.Errorf("%s must not be negative", EnvStartRetryMax))
	}
	if c.MaxConcurrentCommands <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvMaxConcurrentCommands))
	}
	if c.MaxImportRows <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive", EnvMaxImportRows))
	}
	if _, ok := parseLevel(c.LogLevel); !ok {
		errs = append(errs, fmt.Errorf("%s: unknown level %q", EnvLogLevel, c.LogLevel))
	}
	return errors.Join(errs...)
}

// Redacted renders the effective configuration as KEY=value lines with every
// secret masked. It is safe to print.
func (c Config) Redacted() []string {
	return []string{
		EnvListenAddr + "=" + c.ListenAddr,
		EnvDSN + "=" + redactDSN(c.DSN),
		EnvTimezone + "=" + c.Timezone,
		EnvTeamusersURL + "=" + c.TeamusersURL,
		EnvTeamusersIssuer + "=" + c.TeamusersIssuer,
		EnvTeamusersAud + "=" + c.TeamusersAudience,
		EnvTeamusersClientID + "=" + c.TeamusersClientID,
		EnvTeamusersClientSecret + "=" + maskSecret(c.TeamusersClientSecret),
		EnvTeamusersTimeout + "=" + c.TeamusersTimeout.String(),
		EnvWebcamURL + "=" + c.WebcamURL,
		EnvWebcamTimeout + "=" + c.WebcamTimeout.String(),
		EnvSchedTick + "=" + c.SchedTick.String(),
		EnvPrestart + "=" + c.Prestart.String(),
		EnvPoststopGrace + "=" + c.PoststopGrace.String(),
		EnvStartRetryMax + "=" + strconv.Itoa(c.StartRetryMax),
		EnvMissGrace + "=" + c.MissGrace.String(),
		EnvPhotoPollInterval + "=" + c.PhotoPollInterval.String(),
		EnvPhotoPollTTL + "=" + c.PhotoPollTTL.String(),
		EnvMaxConcurrentCommands + "=" + strconv.Itoa(c.MaxConcurrentCommands),
		EnvMaxImportRows + "=" + strconv.Itoa(c.MaxImportRows),
		EnvDev + "=" + strconv.FormatBool(c.Dev),
		EnvLogLevel + "=" + c.LogLevel,
	}
}

// Level returns the slog level for LogLevel, defaulting to info.
func (c Config) Level() slog.Level {
	level, _ := parseLevel(c.LogLevel)
	return level
}

func parseLevel(raw string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// loadEnvFile reads KEY=VALUE assignments from path into the process
// environment. Real environment variables always win, so values injected by
// systemd or compose are never overwritten by the file.
func loadEnvFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", EnvFile, err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		key, value, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf("%s: line %d: missing '='", path, i+1)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s: line %d: missing variable name", path, i+1)
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s: line %d: %w", path, i+1, err)
		}
	}
	return nil
}

// maskSecret hides a non-empty secret, and keeps an unset one visibly empty.
func maskSecret(value string) string {
	if value == "" {
		return ""
	}
	return "<redacted>"
}

// redactDSN masks the password of a PostgreSQL connection string in either URL
// or keyword/value form.
func redactDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "<redacted>"
		}
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(parsed.User.Username(), "<redacted>")
			// url.URL escapes the mask; render it verbatim.
			return strings.ReplaceAll(parsed.String(), "%3Credacted%3E", "<redacted>")
		}
		return parsed.String()
	}
	fields := strings.Fields(dsn)
	for i, field := range fields {
		if strings.HasPrefix(strings.ToLower(field), "password=") {
			fields[i] = "password=<redacted>"
		}
	}
	return strings.Join(fields, " ")
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", key, raw)
	}
	return value, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q", key, raw)
	}
	return value, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q", key, raw)
	}
	return value, nil
}
