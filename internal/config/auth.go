package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"

	"go.kenn.io/roborev/internal/auth"
)

// ValidateAuthKey checks the shared key format without including the key in
// errors. Empty keys preserve unauthenticated local-daemon behavior.
func ValidateAuthKey(key string) error {
	return auth.ValidateKey(key)
}

// LoadGlobalAuthKey reads only auth_key from the global TOML config. Clients
// need the credential to contact a running daemon even when another setting
// has a semantic error; malformed TOML and invalid auth keys still fail.
func LoadGlobalAuthKey() (string, error) {
	path := GlobalConfigPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}

	var values map[string]any
	if _, err := toml.DecodeFile(path, &values); err != nil {
		return "", safeGlobalConfigError(path, err)
	}
	raw, ok := values["auth_key"]
	if !ok {
		return "", nil
	}
	key, ok := raw.(string)
	if !ok {
		return "", errors.New("auth_key must be a string")
	}
	if err := ValidateAuthKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// safeGlobalConfigError prevents a TOML parser's source snippets from exposing
// credentials in CLI errors and daemon reload logs. Non-parser errors retain
// their existing diagnostics (including legacy-config migration guidance).
func safeGlobalConfigError(path string, err error) error {
	if parseErr, ok := errors.AsType[toml.ParseError](err); ok {
		location := fmt.Sprintf("invalid global config TOML in %s at line %d", path, parseErr.Position.Line)
		// Only print recognized config keys, never arbitrary parser input.
		if IsGlobalKey(parseErr.LastKey) {
			location += fmt.Sprintf(" (last key %q)", parseErr.LastKey)
		}
		return errors.New(location)
	}
	return err
}
