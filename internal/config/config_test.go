package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var allEnvKeys = []string{
	EnvListenAddr, EnvDSN, EnvTimezone,
	EnvTeamusersURL, EnvTeamusersIssuer, EnvTeamusersAud,
	EnvTeamusersClientID, EnvTeamusersClientSecret, EnvTeamusersTimeout,
	EnvWebcamURL, EnvWebcamTimeout, EnvSchedTick, EnvPrestart,
	EnvPoststopGrace, EnvStartRetryMax, EnvMissGrace,
	EnvPhotoPollInterval, EnvPhotoPollTTL, EnvMaxConcurrentCommands,
	EnvMaxImportRows, EnvDev, EnvLogLevel, EnvFile,
}

// clearEnv blanks every DISPATCH_* variable so FromEnv sees defaults. Setting
// a variable to the empty string is equivalent to unsetting it for every
// reader in this package.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range allEnvKeys {
		t.Setenv(key, "")
	}
}

// unset removes a variable until the test ends, so loadEnvFile can set it.
func unset(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "") // registers the original value for restoration
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

func TestFromEnvDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvDSN, "  postgres://dispatch:secret@db:5432/dispatch  ")
	t.Setenv(EnvTeamusersURL, "http://iam:8080")
	t.Setenv(EnvTeamusersClientID, "dispatchub-svc")
	t.Setenv(EnvTeamusersClientSecret, "client-secret")
	t.Setenv(EnvWebcamURL, "http://webcam:8080")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v, want nil", err)
	}
	if cfg.DSN != "postgres://dispatch:secret@db:5432/dispatch" {
		t.Fatalf("DSN = %q, want the trimmed value", cfg.DSN)
	}
	if cfg.ListenAddr != ":8081" {
		t.Fatalf("ListenAddr = %q, want :8081", cfg.ListenAddr)
	}
	if cfg.Timezone != "Asia/Shanghai" {
		t.Fatalf("Timezone = %q, want Asia/Shanghai", cfg.Timezone)
	}
	if cfg.Location == nil || cfg.Location.String() != "Asia/Shanghai" {
		t.Fatalf("Location = %v, want Asia/Shanghai", cfg.Location)
	}
	if cfg.TeamusersIssuer != "teamusers" || cfg.TeamusersAudience != "teamusers" {
		t.Fatalf("issuer/audience = %q/%q, want teamusers/teamusers", cfg.TeamusersIssuer, cfg.TeamusersAudience)
	}
	durations := map[string]struct{ got, want time.Duration }{
		"TeamusersTimeout":  {cfg.TeamusersTimeout, 5 * time.Second},
		"WebcamTimeout":     {cfg.WebcamTimeout, 10 * time.Second},
		"SchedTick":         {cfg.SchedTick, 30 * time.Second},
		"Prestart":          {cfg.Prestart, 2 * time.Minute},
		"PoststopGrace":     {cfg.PoststopGrace, 0},
		"MissGrace":         {cfg.MissGrace, 10 * time.Minute},
		"PhotoPollInterval": {cfg.PhotoPollInterval, 2 * time.Second},
		"PhotoPollTTL":      {cfg.PhotoPollTTL, 15 * time.Minute},
	}
	for name, pair := range durations {
		if pair.got != pair.want {
			t.Fatalf("%s = %s, want %s", name, pair.got, pair.want)
		}
	}
	if cfg.StartRetryMax != 3 || cfg.MaxConcurrentCommands != 8 || cfg.MaxImportRows != 5000 {
		t.Fatalf("counts = %d/%d/%d, want 3/8/5000",
			cfg.StartRetryMax, cfg.MaxConcurrentCommands, cfg.MaxImportRows)
	}
	if cfg.LogLevel != "info" || cfg.Level() != slog.LevelInfo {
		t.Fatalf("LogLevel/Level = %q/%s, want info/INFO", cfg.LogLevel, cfg.Level())
	}
	if cfg.Dev {
		t.Fatalf("Dev = true, want false in production configuration")
	}
}

// TestFromEnvRequiresProductionSettings checks that a non-dev configuration
// reports every missing required value at once.
func TestFromEnvRequiresProductionSettings(t *testing.T) {
	clearEnv(t)

	_, err := FromEnv()
	if err == nil {
		t.Fatalf("FromEnv() error = nil, want the missing required values")
	}
	for _, key := range []string{EnvDSN, EnvTeamusersURL, EnvTeamusersClientID, EnvTeamusersClientSecret, EnvWebcamURL} {
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("FromEnv() error = %q, want it to name %s", err, key)
		}
	}
	if !strings.Contains(err.Error(), EnvDev+"=true") {
		t.Fatalf("FromEnv() error = %q, want it to mention the dev bypass", err)
	}
}

// TestFromEnvDevBypass covers the webcam-server dev policy: endpoints and
// credentials are optional when DISPATCH_DEV is set.
func TestFromEnvDevBypass(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvDSN, "postgres://dispatch@db/dispatch")
	t.Setenv(EnvDev, "1")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v, want the dev bypass to accept an incomplete configuration", err)
	}
	if !cfg.Dev {
		t.Fatalf("Dev = false, want true")
	}
	if cfg.TeamusersURL != "" || cfg.TeamusersClientID != "" || cfg.TeamusersClientSecret != "" || cfg.WebcamURL != "" {
		t.Fatalf("dev config = %+v, want the optional endpoints and credentials left empty", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() on the dev config = %v, want nil", err)
	}
}

func TestFromEnvCustomValues(t *testing.T) {
	clearEnv(t)
	values := map[string]string{
		EnvListenAddr:            "127.0.0.1:9000",
		EnvDSN:                   "postgres://dispatch@db/dispatch",
		EnvTimezone:              "UTC",
		EnvTeamusersURL:          "http://iam:8080",
		EnvTeamusersIssuer:       "issuer",
		EnvTeamusersAud:          "nekostick",
		EnvTeamusersClientID:     "dispatchub-svc",
		EnvTeamusersClientSecret: "client-secret",
		EnvTeamusersTimeout:      "3s",
		EnvWebcamURL:             "http://webcam:8080",
		EnvWebcamTimeout:         "7s",
		EnvSchedTick:             "45s",
		EnvPrestart:              "90s",
		EnvPoststopGrace:         "15s",
		EnvStartRetryMax:         "5",
		EnvMissGrace:             "1m",
		EnvPhotoPollInterval:     "500ms",
		EnvPhotoPollTTL:          "20m",
		EnvMaxConcurrentCommands: "4",
		EnvMaxImportRows:         "100",
		EnvDev:                   "true",
		EnvLogLevel:              "debug",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v, want nil", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" || cfg.DSN != values[EnvDSN] || cfg.Timezone != "UTC" {
		t.Fatalf("basic fields = %+v", cfg)
	}
	if cfg.TeamusersURL != "http://iam:8080" || cfg.TeamusersIssuer != "issuer" ||
		cfg.TeamusersAudience != "nekostick" || cfg.TeamusersClientID != "dispatchub-svc" ||
		cfg.TeamusersClientSecret != "client-secret" || cfg.WebcamURL != "http://webcam:8080" {
		t.Fatalf("endpoint fields = %+v", cfg)
	}
	if cfg.TeamusersTimeout != 3*time.Second || cfg.WebcamTimeout != 7*time.Second ||
		cfg.SchedTick != 45*time.Second || cfg.Prestart != 90*time.Second ||
		cfg.PoststopGrace != 15*time.Second || cfg.MissGrace != time.Minute ||
		cfg.PhotoPollInterval != 500*time.Millisecond || cfg.PhotoPollTTL != 20*time.Minute {
		t.Fatalf("duration fields = %+v", cfg)
	}
	if cfg.StartRetryMax != 5 || cfg.MaxConcurrentCommands != 4 || cfg.MaxImportRows != 100 {
		t.Fatalf("count fields = %+v", cfg)
	}
	if cfg.LogLevel != "debug" || cfg.Level() != slog.LevelDebug {
		t.Fatalf("LogLevel/Level = %q/%s, want debug/DEBUG", cfg.LogLevel, cfg.Level())
	}
	if cfg.Location == nil || cfg.Location.String() != "UTC" {
		t.Fatalf("Location = %v, want UTC", cfg.Location)
	}
}

func TestFromEnvRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{EnvWebcamTimeout, "abc"},
		{EnvTeamusersTimeout, "5 seconds"},
		{EnvStartRetryMax, "many"},
		{EnvDev, "maybe"},
		{EnvTimezone, "Not/AZone"},
		{EnvLogLevel, "verbose"},
		{EnvMissGrace, "-1s"},
		{EnvPrestart, "-1s"},
		{EnvPoststopGrace, "-1s"},
		{EnvSchedTick, "0s"},
		{EnvPhotoPollInterval, "0s"},
		{EnvPhotoPollTTL, "0s"},
		{EnvMaxConcurrentCommands, "-2"},
		{EnvMaxImportRows, "0"},
		{EnvStartRetryMax, "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(EnvDSN, "postgres://dispatch@db/dispatch")
			t.Setenv(EnvDev, "true")
			t.Setenv(tt.key, tt.value)

			_, err := FromEnv()
			if err == nil {
				t.Fatalf("FromEnv() error = nil for %s=%q, want an error", tt.key, tt.value)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("FromEnv() error = %q, want it to name %s", err, tt.key)
			}
		})
	}
}

// TestRedactedMasksSecrets checks that no credential reaches the printed
// configuration, in both DSN forms.
func TestRedactedMasksSecrets(t *testing.T) {
	base := Config{
		ListenAddr:            ":8081",
		DSN:                   "postgres://dispatch:s3cret@db:5432/dispatch?sslmode=disable",
		Timezone:              "Asia/Shanghai",
		TeamusersURL:          "http://iam:8080",
		TeamusersIssuer:       "teamusers",
		TeamusersAudience:     "teamusers",
		TeamusersClientID:     "dispatchub-svc",
		TeamusersClientSecret: "topsecret",
		TeamusersTimeout:      5 * time.Second,
		WebcamURL:             "http://webcam:8080",
		WebcamTimeout:         10 * time.Second,
		SchedTick:             30 * time.Second,
		Prestart:              2 * time.Minute,
		StartRetryMax:         3,
		MissGrace:             10 * time.Minute,
		PhotoPollInterval:     2 * time.Second,
		PhotoPollTTL:          15 * time.Minute,
		MaxConcurrentCommands: 8,
		MaxImportRows:         5000,
		LogLevel:              "info",
	}

	lines := base.Redacted()
	joined := strings.Join(lines, "\n")
	for _, key := range allEnvKeys {
		if key == EnvFile {
			continue
		}
		if !strings.Contains(joined+"\n", key+"=") {
			t.Fatalf("Redacted() does not print %s:\n%s", key, joined)
		}
	}
	if strings.Contains(joined, "s3cret") || strings.Contains(joined, "topsecret") {
		t.Fatalf("Redacted() leaked a secret:\n%s", joined)
	}
	if !strings.Contains(joined, EnvTeamusersClientSecret+"=<redacted>") {
		t.Fatalf("Redacted() = \n%s\nwant the client secret masked", joined)
	}
	if !strings.Contains(joined, "dispatch:<redacted>@db:5432/dispatch") {
		t.Fatalf("Redacted() DSN = %q, want the URL password masked", joined)
	}
	if !strings.Contains(joined, EnvTeamusersClientID+"=dispatchub-svc") {
		t.Fatalf("Redacted() masked the client id, which is not a secret:\n%s", joined)
	}

	keyword := base
	keyword.DSN = "host=db user=dispatch password=s3cret dbname=dispatch"
	if line := strings.Join(keyword.Redacted(), "\n"); !strings.Contains(line, "password=<redacted>") || strings.Contains(line, "password=s3cret") {
		t.Fatalf("Redacted() with a keyword DSN = \n%s\nwant the password masked", line)
	}

	empty := base
	empty.TeamusersClientSecret = ""
	if line := strings.Join(empty.Redacted(), "\n"); !strings.Contains(line, EnvTeamusersClientSecret+"=\n") {
		t.Fatalf("Redacted() = \n%s\nwant an unset secret to stay visibly empty", line)
	}

	passwordless := base
	passwordless.DSN = "postgres://dispatch@db:5432/dispatch"
	if line := strings.Join(passwordless.Redacted(), "\n"); !strings.Contains(line, "dispatch@db:5432/dispatch") {
		t.Fatalf("Redacted() mangled a passwordless DSN:\n%s", line)
	}
}

func TestConfigLevel(t *testing.T) {
	tests := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"bogus", slog.LevelInfo},
	}
	for _, tt := range tests {
		if got := (Config{LogLevel: tt.level}).Level(); got != tt.want {
			t.Fatalf("Level(%q) = %s, want %s", tt.level, got, tt.want)
		}
	}
}

func TestFromEnvLoadsEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch.env")
	content := strings.Join([]string{
		"# dispatchub local configuration",
		"export DISPATCH_LISTEN_ADDR=:9999",
		"DISPATCH_LOG_LEVEL=debug",
		"DISPATCH_TEAMUSERS_CLIENT_ID='from-file'",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	clearEnv(t)
	unset(t, EnvListenAddr, EnvLogLevel, EnvTeamusersClientID)
	t.Setenv(EnvFile, path)
	t.Setenv(EnvDSN, "postgres://dispatch@db/dispatch")
	t.Setenv(EnvDev, "true")
	// A real environment variable must win over the file.
	t.Setenv(EnvTeamusersClientID, "from-env")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv() error = %v, want nil", err)
	}
	if cfg.ListenAddr != ":9999" {
		t.Fatalf("ListenAddr = %q, want the env-file value :9999", cfg.ListenAddr)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want the env-file value debug", cfg.LogLevel)
	}
	if cfg.TeamusersClientID != "from-env" {
		t.Fatalf("TeamusersClientID = %q, want the real environment to win", cfg.TeamusersClientID)
	}
}

func TestFromEnvEnvFileErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		clearEnv(t)
		t.Setenv(EnvFile, filepath.Join(t.TempDir(), "absent.env"))
		if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), EnvFile) {
			t.Fatalf("FromEnv() error = %v, want it to name %s", err, EnvFile)
		}
	})

	t.Run("missing equals sign", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.env")
		if err := os.WriteFile(path, []byte("DISPATCH_DSN\n"), 0o600); err != nil {
			t.Fatalf("write env file: %v", err)
		}
		clearEnv(t)
		t.Setenv(EnvFile, path)
		if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "line 1") {
			t.Fatalf("FromEnv() error = %v, want a line-1 parse error", err)
		}
	})
}
