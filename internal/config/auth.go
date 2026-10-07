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

// ClientAuth is the credential and transport security a client uses to
// contact the daemon.
type ClientAuth struct {
	Key string
	TLS DaemonTLSConfig
}

// LoadGlobalClientAuth reads only auth_key and [daemon_tls] from the global
// TOML config. Clients need these to contact a running daemon even when
// another setting has a semantic error; malformed TOML and invalid values
// still fail.
func LoadGlobalClientAuth() (ClientAuth, error) {
	path := GlobalConfigPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return ClientAuth{}, nil
	} else if err != nil {
		return ClientAuth{}, err
	}

	var values struct {
		AuthKey   any             `toml:"auth_key"`
		DaemonTLS DaemonTLSConfig `toml:"daemon_tls"`
	}
	if _, err := toml.DecodeFile(path, &values); err != nil {
		return ClientAuth{}, safeGlobalConfigError(path, err)
	}
	if err := values.DaemonTLS.Validate(); err != nil {
		return ClientAuth{}, err
	}
	result := ClientAuth{TLS: values.DaemonTLS}
	if values.AuthKey == nil {
		return result, nil
	}
	key, ok := values.AuthKey.(string)
	if !ok {
		return ClientAuth{}, errors.New("auth_key must be a string")
	}
	if err := ValidateAuthKey(key); err != nil {
		return ClientAuth{}, err
	}
	result.Key = key
	return result, nil
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
