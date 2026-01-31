package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk action registry.
// Supports YAML (recommended) or JSON.
//
// Key safety properties:
//   - Actions resolve to an argv array (no sh -c).
//   - Params are strongly validated (types, bounds, regex/enums).
//   - Templates are substituted per-argument, never concatenated into a shell string.
type Config struct {
	Version  int               `json:"version"  yaml:"version"`
	Defaults Defaults          `json:"defaults" yaml:"defaults"`
	Actions  map[string]Action `json:"actions"  yaml:"actions"`
}

type Defaults struct {
	TimeoutMS      int               `json:"timeout_ms" yaml:"timeout_ms"`
	MaxStdoutBytes int               `json:"max_stdout_bytes" yaml:"max_stdout_bytes"`
	MaxStderrBytes int               `json:"max_stderr_bytes" yaml:"max_stderr_bytes"`
	WorkDir        string            `json:"workdir" yaml:"workdir"`
	Env            map[string]string `json:"env" yaml:"env"`
	MaxConcurrency int               `json:"max_concurrency" yaml:"max_concurrency"`
	CooldownMS     int               `json:"cooldown_ms" yaml:"cooldown_ms"`
	AllowNetwork   *bool             `json:"allow_network" yaml:"allow_network"` // default false
}

type Action struct {
	Description string `json:"description" yaml:"description"`
	// Exec is argv, e.g. ["/usr/bin/sudo","-n","/bin/systemctl","restart","{name}"]
	Exec []string `json:"exec" yaml:"exec"`

	// Requires (optional) - intended to be enforced by the daemon primarily.
	Requires Requires `json:"requires" yaml:"requires"`

	Policy Policy `json:"policy" yaml:"policy"`

	Params map[string]ParamSpec `json:"params" yaml:"params"`
}

type Requires struct {
	Arm     bool `json:"arm" yaml:"arm"`
	Approve bool `json:"approve" yaml:"approve"`
}

type Policy struct {
	TimeoutMS      int               `json:"timeout_ms" yaml:"timeout_ms"`
	MaxStdoutBytes int               `json:"max_stdout_bytes" yaml:"max_stdout_bytes"`
	MaxStderrBytes int               `json:"max_stderr_bytes" yaml:"max_stderr_bytes"`
	WorkDir        string            `json:"workdir" yaml:"workdir"`
	Env            map[string]string `json:"env" yaml:"env"`
	MaxConcurrency int               `json:"max_concurrency" yaml:"max_concurrency"`
	CooldownMS     int               `json:"cooldown_ms" yaml:"cooldown_ms"`
	DevicesAllowlist []string        `json:"devices_allowlist" yaml:"devices_allowlist"`
	AllowNetwork   *bool             `json:"allow_network" yaml:"allow_network"`
}

type ParamType string

const (
	ParamString ParamType = "string"
	ParamInt    ParamType = "int"
	ParamBool   ParamType = "bool"
)

type ParamSpec struct {
	Type   ParamType `json:"type" yaml:"type"`
	// String constraints
	Regex  string   `json:"regex" yaml:"regex"`
	Enum   []string `json:"enum" yaml:"enum"`
	MaxLen int      `json:"max_len" yaml:"max_len"`
	MinLen int      `json:"min_len" yaml:"min_len"`
	// Int constraints
	Min *int `json:"min" yaml:"min"`
	Max *int `json:"max" yaml:"max"`
	// Redaction for logs
	Redact bool `json:"redact" yaml:"redact"`
}

// Load reads a YAML/JSON registry from path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, err
		}
	} else {
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return nil, err
		}
	}
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if cfg.Actions == nil {
		cfg.Actions = map[string]Action{}
	}
	applyDefaults(&cfg)
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Defaults.TimeoutMS == 0 {
		cfg.Defaults.TimeoutMS = 15000
	}
	if cfg.Defaults.MaxStdoutBytes == 0 {
		cfg.Defaults.MaxStdoutBytes = 65536
	}
	if cfg.Defaults.MaxStderrBytes == 0 {
		cfg.Defaults.MaxStderrBytes = 65536
	}
	if cfg.Defaults.WorkDir == "" {
		cfg.Defaults.WorkDir = "."
	}
	if cfg.Defaults.Env == nil {
		cfg.Defaults.Env = map[string]string{"PATH": "/usr/sbin:/usr/bin:/sbin:/bin"}
	}
	if cfg.Defaults.MaxConcurrency == 0 {
		cfg.Defaults.MaxConcurrency = 1
	}
	if cfg.Defaults.AllowNetwork == nil {
		v := false
		cfg.Defaults.AllowNetwork = &v
	}

	for k, a := range cfg.Actions {
		if a.Policy.TimeoutMS == 0 {
			a.Policy.TimeoutMS = cfg.Defaults.TimeoutMS
		}
		if a.Policy.MaxStdoutBytes == 0 {
			a.Policy.MaxStdoutBytes = cfg.Defaults.MaxStdoutBytes
		}
		if a.Policy.MaxStderrBytes == 0 {
			a.Policy.MaxStderrBytes = cfg.Defaults.MaxStderrBytes
		}
		if a.Policy.WorkDir == "" {
			a.Policy.WorkDir = cfg.Defaults.WorkDir
		}
		if a.Policy.Env == nil {
			a.Policy.Env = cfg.Defaults.Env
		}
		if a.Policy.MaxConcurrency == 0 {
			a.Policy.MaxConcurrency = cfg.Defaults.MaxConcurrency
		}
		if a.Policy.AllowNetwork == nil {
			a.Policy.AllowNetwork = cfg.Defaults.AllowNetwork
		}
		cfg.Actions[k] = a
	}
}

// ValidateParams validates and normalizes params against the action's ParamSpec.
// Returns a string map of normalized values suitable for template substitution.
func (a Action) ValidateParams(params map[string]any) (map[string]string, error) {
	out := map[string]string{}
	for k := range params {
		if _, ok := a.Params[k]; !ok {
			return nil, fmt.Errorf("unknown param: %s", k)
		}
	}
	for name, spec := range a.Params {
		v, ok := params[name]
		if !ok {
			return nil, fmt.Errorf("missing param: %s", name)
		}
		switch spec.Type {
		case ParamString:
			s, err := asString(v)
			if err != nil {
				return nil, fmt.Errorf("param %s: %w", name, err)
			}
			if spec.MinLen > 0 && len(s) < spec.MinLen {
				return nil, fmt.Errorf("param %s: too short", name)
			}
			if spec.MaxLen > 0 && len(s) > spec.MaxLen {
				return nil, fmt.Errorf("param %s: too long", name)
			}
			if len(spec.Enum) > 0 {
				found := false
				for _, e := range spec.Enum {
					if s == e {
						found = true
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("param %s: not in enum", name)
				}
			}
			if spec.Regex != "" {
				re, err := regexp.Compile(spec.Regex)
				if err != nil {
					return nil, fmt.Errorf("param %s: bad regex in registry", name)
				}
				if !re.MatchString(s) {
					return nil, fmt.Errorf("param %s: regex mismatch", name)
				}
			}
			out[name] = s

		case ParamInt:
			n, err := asInt(v)
			if err != nil {
				return nil, fmt.Errorf("param %s: %w", name, err)
			}
			if spec.Min != nil && n < *spec.Min {
				return nil, fmt.Errorf("param %s: below min", name)
			}
			if spec.Max != nil && n > *spec.Max {
				return nil, fmt.Errorf("param %s: above max", name)
			}
			out[name] = strconv.Itoa(n)

		case ParamBool:
			b, err := asBool(v)
			if err != nil {
				return nil, fmt.Errorf("param %s: %w", name, err)
			}
			if b {
				out[name] = "true"
			} else {
				out[name] = "false"
			}

		default:
			return nil, fmt.Errorf("param %s: unsupported type %q", name, spec.Type)
		}
	}
	return out, nil
}

// SubstituteArg substitutes {param} tokens within a single argv element.
// No nested templating, no shell expansion.
func SubstituteArg(arg string, vals map[string]string) (string, error) {
	if !strings.Contains(arg, "{") {
		return arg, nil
	}
	out := arg
	for k, v := range vals {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	if strings.Contains(out, "{") || strings.Contains(out, "}") {
		return "", errors.New("unresolved template token")
	}
	if strings.ContainsAny(out, "\x00\r\n") {
		return "", errors.New("invalid characters in arg")
	}
	return out, nil
}

func (a Action) EffectiveTimeout() time.Duration {
	ms := a.Policy.TimeoutMS
	if ms <= 0 {
		ms = 15000
	}
	return time.Duration(ms) * time.Millisecond
}

func asString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	default:
		return "", fmt.Errorf("expected string")
	}
}

func asInt(v any) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case int32:
		return int(t), nil
	case int64:
		return int(t), nil
	case float64:
		return int(t), nil
	case json.Number:
		i64, err := t.Int64()
		return int(i64), err
	default:
		return 0, fmt.Errorf("expected int")
	}
}

func asBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return false, fmt.Errorf("expected bool")
	default:
		return false, fmt.Errorf("expected bool")
	}
}
