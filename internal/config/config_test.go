package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const minimal = `
google:
  self_email: user@example.com
icloud:
  username: apple@example.com
  attendee_email: user@example.com
  calendar: calbridge-test
`

func TestExampleConfigLoads(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	example := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "config.example.yaml")
	raw, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	// The example uses placeholders; substitute plain addresses so it validates.
	s := strings.NewReplacer("user@YOUR_DOMAIN", "user@example.com", "APPLE_ID_EMAIL", "apple@example.com").Replace(string(raw))
	cfg, err := Parse([]byte(s), nil)
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if cfg.Sync.Interval.D() != 2*time.Minute || cfg.Sync.Lookback.D() != 720*time.Hour {
		t.Errorf("durations = %s/%s", cfg.Sync.Interval.D(), cfg.Sync.Lookback.D())
	}
}

func TestDefaultsApplied(t *testing.T) {
	cfg, err := Parse([]byte(minimal), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Google.CalendarID != "primary" || cfg.Google.Scope != ScopeEvents || cfg.Sync.Privacy != "honor_private" ||
		!cfg.ICloud.EmitVTimezone || !cfg.Sync.PushGoogleRSVPToICloud || cfg.Sync.Declined != "keep" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestEnvOverrides(t *testing.T) {
	env := []string{
		"CALBRIDGE_SYNC_INTERVAL=45s",
		"CALBRIDGE_ICLOUD_EMIT_VTIMEZONE=false",
		"CALBRIDGE_ICLOUD_RATE_LIMIT_RPS=2.5",
		"CALBRIDGE_SYNC_ALARMS=10m, 1h",
		"CALBRIDGE_GOOGLE_SEND_UPDATES=none",
		"UNRELATED=1",
	}
	cfg, err := Parse([]byte(minimal), env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sync.Interval.D() != 45*time.Second {
		t.Errorf("interval = %s", cfg.Sync.Interval.D())
	}
	if cfg.ICloud.EmitVTimezone {
		t.Error("emit_vtimezone not overridden")
	}
	if cfg.ICloud.RateLimitRPS != 2.5 {
		t.Errorf("rps = %v", cfg.ICloud.RateLimitRPS)
	}
	if len(cfg.Sync.Alarms) != 2 || cfg.Sync.Alarms[1].D() != time.Hour {
		t.Errorf("alarms = %v", cfg.Sync.Alarms)
	}
	if cfg.Google.SendUpdates != "none" {
		t.Errorf("send_updates = %q", cfg.Google.SendUpdates)
	}
}

func TestEnvOverrideBadValue(t *testing.T) {
	_, err := Parse([]byte(minimal), []string{"CALBRIDGE_SYNC_INTERVAL=soon"})
	if err == nil || !strings.Contains(err.Error(), "CALBRIDGE_SYNC_INTERVAL") {
		t.Fatalf("err = %v", err)
	}
}

func TestRejectsInlineSecrets(t *testing.T) {
	cases := map[string]string{
		"password key":       "icloud:\n  app_password: hunter2\n",
		"token key":          "google:\n  refresh_token: abc\n",
		"apple app password": "icloud:\n  calendar: abcd-efgh-ijkl-mnop\n",
		"google refresh":     "monitoring:\n  ping_url: \"1//0gabc\"\n",
		"client secret":      "log:\n  level: GOCSPX-abc\n",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(minimal+y), nil)
			if !errors.Is(err, ErrInlineSecret) {
				t.Fatalf("err = %v, want ErrInlineSecret", err)
			}
		})
	}
}

func TestSecretFilePathsAllowed(t *testing.T) {
	y := minimal + "  app_password_file: /run/x\n"
	if _, err := Parse([]byte(y), nil); err != nil {
		t.Fatal(err)
	}
}

func TestValidation(t *testing.T) {
	envCases := map[string]string{
		"short interval": "CALBRIDGE_SYNC_INTERVAL=10s",
		"bad privacy":    "CALBRIDGE_SYNC_PRIVACY=open",
		"bad agent":      "CALBRIDGE_ICLOUD_SCHEDULE_AGENT=SERVER",
		"bad scope":      "CALBRIDGE_GOOGLE_SCOPE=https://www.googleapis.com/auth/calendar",
		"bad delete":     "CALBRIDGE_ICLOUD_DELETE_MODE=nuke",
		"no calendar":    "CALBRIDGE_ICLOUD_CALENDAR=",
		"zero rps":       "CALBRIDGE_ICLOUD_RATE_LIMIT_RPS=0",
	}
	for name, kv := range envCases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(minimal), []string{kv})
			if err == nil || !strings.Contains(err.Error(), "config: ") {
				t.Fatalf("err = %v, want validation error", err)
			}
		})
	}
	yamlCases := map[string]string{
		"unknown key":  "sync:\n  intervall: 2m\n",
		"bad duration": "sync:\n  lookback: forever\n",
	}
	for name, y := range yamlCases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(minimal+y), nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if _, err := Parse([]byte("google:\n  calendar_id: primary\n"), nil); err == nil ||
		!strings.Contains(err.Error(), "self_email is required") {
		t.Fatalf("missing required fields: err = %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(minimal, "user@example.com", "User <user@example.com>", 1)), nil); err == nil {
		t.Fatal("display-name address accepted")
	}
}

func TestLoadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p+".missing", nil); err == nil {
		t.Fatal("missing file accepted")
	}
}
