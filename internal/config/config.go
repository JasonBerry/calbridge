// Package config loads and validates calbridge's YAML configuration, applies
// CALBRIDGE_<SECTION>_<KEY> environment overrides, and refuses inline secrets.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config mirrors deploy/config.example.yaml (PLAN.md §10).
type Config struct {
	Google     Google     `yaml:"google"`
	ICloud     ICloud     `yaml:"icloud"`
	Sync       Sync       `yaml:"sync"`
	Store      Store      `yaml:"store"`
	Server     Server     `yaml:"server"`
	Monitoring Monitoring `yaml:"monitoring"`
	Log        Log        `yaml:"log"`
}

type Google struct {
	ClientFile  string `yaml:"client_file"`
	TokenFile   string `yaml:"token_file"`
	CalendarID  string `yaml:"calendar_id"`
	SelfEmail   string `yaml:"self_email"`
	SendUpdates string `yaml:"send_updates"`
	Scope       string `yaml:"scope"`
}

type ICloud struct {
	Username        string  `yaml:"username"`
	AppPasswordFile string  `yaml:"app_password_file"`
	Calendar        string  `yaml:"calendar"`
	CalendarHref    string  `yaml:"calendar_href"`
	AttendeeEmail   string  `yaml:"attendee_email"`
	ScheduleAgent   string  `yaml:"schedule_agent"`
	DeleteMode      string  `yaml:"delete_mode"`
	SyncMethod      string  `yaml:"sync_method"`
	EmitVTimezone   bool    `yaml:"emit_vtimezone"`
	RateLimitRPS    float64 `yaml:"rate_limit_rps"`
}

type Sync struct {
	Interval               Duration   `yaml:"interval"`
	Lookback               Duration   `yaml:"lookback"`
	IncludeOrganizedByMe   bool       `yaml:"include_organized_by_me"`
	Declined               string     `yaml:"declined"`
	Privacy                string     `yaml:"privacy"`
	IncludeGuestList       bool       `yaml:"include_guest_list"`
	Alarms                 []Duration `yaml:"alarms"`
	PushGoogleRSVPToICloud bool       `yaml:"push_google_rsvp_to_icloud"`
	RecreateOnReschedule   bool       `yaml:"recreate_on_reschedule"`
}

type Store struct {
	Path      string `yaml:"path"`
	BackupDir string `yaml:"backup_dir"`
}

type Server struct {
	Listen string `yaml:"listen"`
}

type Monitoring struct {
	PingURL string `yaml:"ping_url"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Scopes accepted for google.scope. Changing this list needs the user's
// agreement (CLAUDE.md rule 4).
const (
	ScopeEvents      = "https://www.googleapis.com/auth/calendar.events"
	ScopeEventsOwned = "https://www.googleapis.com/auth/calendar.events.owned"
)

// MinInterval is the smallest allowed sync.interval (PLAN.md §9).
const MinInterval = 30 * time.Second

// Default returns the configuration defaults from PLAN.md §10.
func Default() Config {
	return Config{
		Google: Google{ // #nosec G101 -- file paths, not credentials
			ClientFile:  "/etc/calbridge/google-client.json",
			TokenFile:   "/var/lib/calbridge/google-token.json",
			CalendarID:  "primary",
			SendUpdates: "all",
			Scope:       ScopeEvents,
		},
		ICloud: ICloud{ // #nosec G101 -- file paths, not credentials
			AppPasswordFile: "/etc/calbridge/icloud-app-password",
			DeleteMode:      "plain",
			SyncMethod:      "auto",
			EmitVTimezone:   true,
			RateLimitRPS:    5,
		},
		Sync: Sync{
			Interval:               Duration(2 * time.Minute),
			Lookback:               Duration(720 * time.Hour),
			Declined:               "keep",
			Privacy:                "honor_private",
			PushGoogleRSVPToICloud: true,
		},
		Store: Store{
			Path:      "/var/lib/calbridge/calbridge.db",
			BackupDir: "/var/lib/calbridge/backups",
		},
		Server: Server{Listen: "127.0.0.1:8089"},
		Log:    Log{Level: "info", Format: "json"},
	}
}

// Load reads the YAML file at path on top of the defaults, applies environment
// overrides from environ (normally os.Environ()), and validates the result.
func Load(path string, environ []string) (Config, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the operator-supplied config file
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw, environ)
}

// Parse is Load without the file read.
func Parse(raw []byte, environ []string) (Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := rejectInlineSecrets(&root, ""); err != nil {
		return Config{}, err
	}
	cfg := Default()
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
	}
	if err := applyEnv(&cfg, environ); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ErrInlineSecret is returned when the YAML appears to contain a secret value.
var ErrInlineSecret = errors.New("config: secrets must not be stored inline; use a *_file path, an environment variable or a systemd credential")

var (
	secretKey = regexp.MustCompile(`(?i)(password|secret|token|credential|api_?key)`)
	// Apple app-specific passwords look like abcd-efgh-ijkl-mnop.
	appPassword = regexp.MustCompile(`^[a-z]{4}-[a-z]{4}-[a-z]{4}-[a-z]{4}$`)
	// Google OAuth refresh tokens start with "1//"; client secrets with "GOCSPX-";
	// access tokens with "ya29.".
	googleSecret = regexp.MustCompile(`^(1//|GOCSPX-|ya29\.)`)
)

func rejectInlineSecrets(n *yaml.Node, path string) error {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if err := rejectInlineSecrets(c, path); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := k.Value
			if path != "" {
				p = path + "." + k.Value
			}
			if v.Kind == yaml.ScalarNode && v.Value != "" &&
				secretKey.MatchString(k.Value) && !strings.HasSuffix(k.Value, "_file") {
				return fmt.Errorf("%w (key %s)", ErrInlineSecret, p)
			}
			if err := rejectInlineSecrets(v, p); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		v := strings.TrimSpace(n.Value)
		if appPassword.MatchString(v) || googleSecret.MatchString(v) {
			return fmt.Errorf("%w (value at %s)", ErrInlineSecret, path)
		}
	}
	return nil
}

// applyEnv overrides scalar fields from CALBRIDGE_<SECTION>_<KEY> variables,
// where SECTION and KEY are the upper-cased YAML names.
func applyEnv(cfg *Config, environ []string) error {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "CALBRIDGE_") {
			env[k] = v
		}
	}
	root := reflect.ValueOf(cfg).Elem()
	for i := 0; i < root.NumField(); i++ {
		section := root.Field(i)
		sname := yamlName(root.Type().Field(i))
		for j := 0; j < section.NumField(); j++ {
			fname := yamlName(section.Type().Field(j))
			key := "CALBRIDGE_" + strings.ToUpper(sname+"_"+fname)
			val, ok := env[key]
			if !ok {
				continue
			}
			if err := setFromString(section.Field(j), val); err != nil {
				return fmt.Errorf("config: env %s: %w", key, err)
			}
		}
	}
	return nil
}

func yamlName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	return name
}

var durationType = reflect.TypeFor[Duration]()

func setFromString(f reflect.Value, s string) error {
	if f.Type() == durationType {
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		f.Set(reflect.ValueOf(Duration(d)))
		return nil
	}
	switch f.Kind() {
	case reflect.String:
		f.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		f.SetBool(b)
	case reflect.Float64:
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		f.SetFloat(x)
	case reflect.Slice:
		if f.Type().Elem() != durationType {
			return fmt.Errorf("unsupported list type %s", f.Type())
		}
		var out []Duration
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			d, err := time.ParseDuration(part)
			if err != nil {
				return err
			}
			out = append(out, Duration(d))
		}
		f.Set(reflect.ValueOf(out))
	default:
		return fmt.Errorf("unsupported type %s", f.Type())
	}
	return nil
}

// Validate checks enumerations and required fields.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf("config: "+format, a...)) }
	oneOf := func(field, v string, allowed ...string) {
		for _, a := range allowed {
			if v == a {
				return
			}
		}
		bad("%s = %q, want one of %q", field, v, allowed)
	}
	email := func(field, v string) {
		if v == "" {
			bad("%s is required", field)
			return
		}
		if a, err := mail.ParseAddress(v); err != nil || a.Address != v {
			bad("%s = %q is not a plain email address", field, v)
		}
	}

	if c.Google.ClientFile == "" {
		bad("google.client_file is required")
	}
	if c.Google.TokenFile == "" {
		bad("google.token_file is required")
	}
	if c.Google.CalendarID == "" {
		bad("google.calendar_id is required")
	}
	email("google.self_email", c.Google.SelfEmail)
	oneOf("google.send_updates", c.Google.SendUpdates, "all", "externalOnly", "none")
	oneOf("google.scope", c.Google.Scope, ScopeEvents, ScopeEventsOwned)

	email("icloud.username", c.ICloud.Username)
	email("icloud.attendee_email", c.ICloud.AttendeeEmail)
	if c.ICloud.Calendar == "" && c.ICloud.CalendarHref == "" {
		bad("one of icloud.calendar or icloud.calendar_href is required")
	}
	oneOf("icloud.schedule_agent", c.ICloud.ScheduleAgent, "", "CLIENT", "NONE")
	oneOf("icloud.delete_mode", c.ICloud.DeleteMode, "plain", "schedule_reply_f", "neutralize_then_delete")
	oneOf("icloud.sync_method", c.ICloud.SyncMethod, "auto", "sync-collection", "ctag")
	if c.ICloud.RateLimitRPS <= 0 {
		bad("icloud.rate_limit_rps must be > 0")
	}

	if c.Sync.Interval.D() < MinInterval {
		bad("sync.interval = %s, minimum is %s", c.Sync.Interval.D(), MinInterval)
	}
	if c.Sync.Lookback.D() < 0 {
		bad("sync.lookback must not be negative")
	}
	oneOf("sync.declined", c.Sync.Declined, "keep", "delete")
	oneOf("sync.privacy", c.Sync.Privacy, "full", "honor_private", "busy_only")
	for _, a := range c.Sync.Alarms {
		if a.D() <= 0 {
			bad("sync.alarms entries must be positive durations")
		}
	}

	if c.Store.Path == "" {
		bad("store.path is required")
	}
	oneOf("log.level", c.Log.Level, "debug", "info", "warn", "error")
	oneOf("log.format", c.Log.Format, "json", "text")
	return errors.Join(errs...)
}

// Duration is a time.Duration that (un)marshals as a Go duration string ("2m").
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: duration must be a string like \"2m\"", n.Line)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
