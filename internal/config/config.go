package config

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

const DefaultConfigPath = "~/.config/screenote/config.toml"

type Values struct {
	Token   string            `toml:"token" json:"token,omitempty"`
	BaseURL string            `toml:"base_url" json:"base_url,omitempty"`
	Project string            `toml:"project" json:"project,omitempty"`
	Login   *LoginCredentials `toml:"login,omitempty" json:"-"`
}

type LoginCredentials struct {
	AccessToken  string    `toml:"access_token"`
	RefreshToken string    `toml:"refresh_token,omitempty"`
	ExpiresAt    time.Time `toml:"expires_at,omitempty"`
	ClientID     string    `toml:"client_id"`
	BaseURL      string    `toml:"base_url"`
	Issuer       string    `toml:"issuer,omitempty"`
}

type Sources struct {
	Token   string `json:"token,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	Project string `json:"project,omitempty"`
}

type Resolved struct {
	Values
	Sources Sources `json:"sources"`
}

type Options struct {
	ConfigPath string
	Flags      Values
	Env        map[string]string
}

func Resolve(options Options) (Resolved, error) {
	env := func(key string) string { return os.Getenv(key) }
	if options.Env != nil {
		env = func(key string) string { return options.Env[key] }
	}

	path, err := ExpandPath(options.ConfigPath)
	if err != nil {
		return Resolved{}, err
	}
	if path == "" {
		path, err = ExpandPath(DefaultConfigPath)
		if err != nil {
			return Resolved{}, err
		}
	}

	fileValues, err := Load(path)
	if err != nil {
		return Resolved{}, err
	}

	resolved := Resolved{Values: fileValues}
	if fileValues.Token != "" {
		resolved.Sources.Token = "config"
	}
	if fileValues.BaseURL != "" {
		resolved.Sources.BaseURL = "config"
	}
	if fileValues.Project != "" {
		resolved.Sources.Project = "config"
	}

	apply := func(value, source string, target *string, sourceTarget *string) {
		if value == "" {
			return
		}
		*target = value
		*sourceTarget = source
	}

	apply(env("SCREENOTE_TOKEN"), "env", &resolved.Token, &resolved.Sources.Token)
	apply(env("SCREENOTE_BASE_URL"), "env", &resolved.BaseURL, &resolved.Sources.BaseURL)
	apply(env("SCREENOTE_PROJECT"), "env", &resolved.Project, &resolved.Sources.Project)
	apply(options.Flags.Token, "flag", &resolved.Token, &resolved.Sources.Token)
	apply(options.Flags.BaseURL, "flag", &resolved.BaseURL, &resolved.Sources.BaseURL)
	apply(options.Flags.Project, "flag", &resolved.Project, &resolved.Sources.Project)

	return resolved, nil
}

func Load(path string) (Values, error) {
	if path == "" {
		return Values{}, nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Values{}, nil
	}
	if err != nil {
		return Values{}, err
	}

	var values Values
	if _, err := toml.Decode(string(data), &values); err != nil {
		return Values{}, err
	}
	return values, nil
}

func LoadExpanded(path string) (Values, error) {
	expanded, err := ExpandPath(path)
	if err != nil {
		return Values{}, err
	}
	return Load(expanded)
}

func Save(path string, values Values) error {
	expanded, err := ExpandPath(path)
	if err != nil {
		return err
	}
	if expanded == "" {
		expanded, err = ExpandPath(DefaultConfigPath)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(expanded), 0o700); err != nil {
		return err
	}

	f, err := os.OpenFile(expanded, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// OpenFile's permission is only applied on creation. Tighten an existing
	// hand-authored config before writing OAuth access/refresh credentials.
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}

	return toml.NewEncoder(f).Encode(values)
}

func ExpandPath(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	if len(path) > 1 && os.IsPathSeparator(path[1]) {
		return filepath.Join(home, path[2:]), nil
	}
	return "", errors.New("unsupported config path")
}
