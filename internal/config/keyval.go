package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// KeyValue represents a config key and its value
type KeyValue struct {
	Key   string
	Value string
}

// sensitiveKeys is populated at init time by scanning Config and RepoConfig
// struct tags for `sensitive:"true"`, so new sensitive fields are automatically
// detected without maintaining a separate list.
var sensitiveKeys map[string]bool

func init() {
	sensitiveKeys = make(map[string]bool)
	collectSensitiveKeys(reflect.TypeFor[Config](), "", sensitiveKeys)
	collectSensitiveKeys(reflect.TypeFor[RepoConfig](), "", sensitiveKeys)
}

// getTOMLKey extracts the TOML key name from a struct field's tag.
// Returns "" if the field has no toml tag.
func getTOMLKey(field reflect.StructField) string {
	tag := field.Tag.Get("toml")
	if tag == "" {
		return ""
	}
	return strings.Split(tag, ",")[0]
}

func structType(t reflect.Type) (reflect.Type, bool) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice {
		t = t.Elem()
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
	}
	return t, t.Kind() == reflect.Struct
}

// isInlineEmbeddedStructField identifies anonymous embedded structs that are
// represented inline (no TOML tag on the embedding field).
func isInlineEmbeddedStructField(field reflect.StructField) bool {
	if !field.Anonymous || getTOMLKey(field) != "" {
		return false
	}
	_, isStruct := structType(field.Type)
	return isStruct
}

// collectSensitiveKeys walks struct fields and records TOML keys tagged sensitive:"true".
func collectSensitiveKeys(t reflect.Type, prefix string, out map[string]bool) {
	for field := range t.Fields() {
		tagKey := getTOMLKey(field)
		ft, ftIsStruct := structType(field.Type)

		if tagKey == "" {
			if isInlineEmbeddedStructField(field) {
				collectSensitiveKeys(ft, prefix, out)
			}
			continue
		}
		fullKey := tagKey
		if prefix != "" {
			fullKey = prefix + "." + tagKey
		}
		if ftIsStruct {
			collectSensitiveKeys(ft, fullKey, out)
			continue
		}
		if field.Tag.Get("sensitive") == "true" {
			out[fullKey] = true
		}
	}
}

// IsValidKey returns true if the key is recognized by either Config or RepoConfig.
func IsValidKey(key string) bool {
	return hasLeafConfigKey(reflect.ValueOf(Config{}), key) || hasLeafConfigKey(reflect.ValueOf(RepoConfig{}), key)
}

// IsGlobalKey returns true if the key belongs to the global Config struct.
func IsGlobalKey(key string) bool {
	return hasLeafConfigKey(reflect.ValueOf(Config{}), key)
}

// IsSensitiveKey returns true if the key holds a secret that should be masked.
func IsSensitiveKey(key string) bool {
	return sensitiveKeys[key]
}

func hasLeafConfigKey(v reflect.Value, key string) bool {
	field, err := FindFieldByTOMLKey(v, key)
	if err != nil {
		return false
	}
	// Slices and maps are leaf values (e.g., []HookConfig is "hooks"); only
	// direct structs and pointer-to-structs require dot-traversal.
	t := field.Type()
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() != reflect.Struct
}

// MaskValue returns a masked version of a sensitive value, showing only the last 4 chars.
func MaskValue(val string) string {
	if len(val) <= 4 {
		return "****"
	}
	return "****" + val[len(val)-4:]
}

// KeyValueOrigin represents a config key, its value, and where it came from
type KeyValueOrigin struct {
	Key    string
	Value  string
	Origin string // "global", "local", "default"
}

// IsConfigValueSet returns true if the field for key exists and is non-zero.
func IsConfigValueSet(cfg any, key string) bool {
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return false
	}
	field, err := FindFieldByTOMLKey(v, key)
	if err != nil {
		return false
	}
	return !field.IsZero()
}

// LoadRawGlobal loads the global config file as a raw TOML map.
func LoadRawGlobal() (map[string]any, error) {
	return LoadRawTOML(GlobalConfigPath())
}

// LoadRawRepo loads the repo config file as a raw TOML map. It applies the
// same linked-worktree fallback as LoadRepoConfig (via RepoConfigPath) so
// explicit-key detection sees the authoritative config even when it lives only
// in the main checkout.
func LoadRawRepo(repoPath string) (map[string]any, error) {
	return LoadRawTOML(RepoConfigPath(repoPath))
}

// LoadRawTOML loads a TOML file as a raw map.
func LoadRawTOML(path string) (map[string]any, error) {
	raw := make(map[string]any)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// IsKeyInTOMLFile checks whether a dot-separated key was explicitly present
// in a raw TOML map (as returned by toml.Decode into map[string]interface{}).
// This correctly detects explicit false/0 values that IsZero would miss.
func IsKeyInTOMLFile(raw map[string]any, key string) bool {
	parts := strings.SplitN(key, ".", 2)
	val, ok := raw[parts[0]]
	if !ok {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	sub, ok := val.(map[string]any)
	if !ok {
		return false
	}
	return IsKeyInTOMLFile(sub, parts[1])
}

// GetConfigValue retrieves a value from a config struct by its TOML key.
// Supports dot-separated keys for nested structs (e.g., "sync.enabled").
func GetConfigValue(cfg any, key string) (string, error) {
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return "", fmt.Errorf("expected struct, got %s", v.Kind())
	}

	field, err := FindFieldByTOMLKey(v, key)
	if err != nil {
		return "", err
	}

	return formatValue(field), nil
}

// SetConfigValue sets a value on a config struct by its TOML key.
// Converts the string value to the appropriate Go type.
func SetConfigValue(cfg any, key string, value string) error {
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("expected pointer to struct, got %s", v.Kind())
	}

	field, err := FindOrCreateFieldByTOMLKey(v, key)
	if err != nil {
		return err
	}

	if !field.CanSet() {
		return fmt.Errorf("cannot set field for key %q", key)
	}

	return setFieldValue(field, value)
}

// ListConfigKeys returns all non-zero values from a config struct as key-value pairs.
func ListConfigKeys(cfg any) []KeyValue {
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}

	return listFields(v, "")
}

// ListExplicitKeys returns key-value pairs only for keys explicitly present
// in the raw TOML map. This avoids showing default values or dropping
// explicit zero/false/empty values.
func ListExplicitKeys(cfg any, raw map[string]any) []KeyValue {
	if raw == nil {
		return nil
	}
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}

	all := listAllFields(v, "")
	var result []KeyValue
	for _, kv := range all {
		if IsKeyInTOMLFile(raw, kv.Key) {
			result = append(result, kv)
		}
	}
	return result
}

// kvMap builds a map from key to formatted value for all fields in a struct.
func kvMap(cfg any) map[string]string {
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	m := make(map[string]string)
	for _, kv := range listAllFields(v, "") {
		m[kv.Key] = kv.Value
	}
	return m
}

// determineOrigin decides the origin label for a global config key.
// It returns ("", false) if the key should be omitted from output.
func determineOrigin(key, value, defaultValue string, rawGlobal map[string]any) (string, bool) {
	isDefault := defaultValue == value
	isEmptyDefault := value == "" || value == "0" || value == "false" || value == "[]"
	explicitInGlobal := IsKeyInTOMLFile(rawGlobal, key)

	if isEmptyDefault && !explicitInGlobal {
		return "", false
	}
	if explicitInGlobal {
		return "global", true
	}
	if isDefault {
		return "default", true
	}
	return "global", true
}

// MergedConfigWithOrigin returns all effective config values with their origin.
// global is the loaded global config (already has defaults applied by LoadGlobal).
// repo is the loaded repo config (nil if no .roborev.toml).
// rawGlobal and rawRepo are raw TOML maps for detecting explicit presence of
// false/0 values. Pass nil if not available.
func MergedConfigWithOrigin(global *Config, repo *RepoConfig, rawGlobal, rawRepo map[string]any) []KeyValueOrigin {
	if rawGlobal == nil {
		rawGlobal = make(map[string]any)
	}
	if rawRepo == nil {
		rawRepo = make(map[string]any)
	}

	defaultMap := kvMap(DefaultConfig())
	repoValMap := make(map[string]string)
	if repo != nil {
		repoValMap = kvMap(repo)
	}

	globalKVs := listAllFields(reflect.ValueOf(global).Elem(), "")

	var result []KeyValueOrigin
	for _, kv := range globalKVs {
		// Check if repo explicitly sets this key (via raw TOML presence)
		if IsKeyInTOMLFile(rawRepo, kv.Key) {
			result = append(result, KeyValueOrigin{Key: kv.Key, Value: repoValMap[kv.Key], Origin: "local"})
			continue
		}

		if origin, ok := determineOrigin(kv.Key, kv.Value, defaultMap[kv.Key], rawGlobal); ok {
			result = append(result, KeyValueOrigin{Key: kv.Key, Value: kv.Value, Origin: origin})
		}
	}

	// Add repo-only keys not in global (e.g., "agent", "review_guidelines")
	globalKeySet := make(map[string]bool)
	for _, kv := range globalKVs {
		globalKeySet[kv.Key] = true
	}
	var repoOnlyKeys []string
	for key := range repoValMap {
		if !globalKeySet[key] && IsKeyInTOMLFile(rawRepo, key) {
			repoOnlyKeys = append(repoOnlyKeys, key)
		}
	}
	sort.Strings(repoOnlyKeys)
	for _, key := range repoOnlyKeys {
		result = append(result, KeyValueOrigin{Key: key, Value: repoValMap[key], Origin: "local"})
	}

	return result
}

// FindFieldByTOMLKey locates a struct field by its TOML tag, supporting dot notation.
// It never mutates the struct; nil pointer fields are resolved via a throwaway zero struct.
func FindFieldByTOMLKey(v reflect.Value, key string) (reflect.Value, error) {
	return findFieldByTOMLKey(v, key, false)
}

// FindOrCreateFieldByTOMLKey is like FindFieldByTOMLKey but initializes nil pointer
// fields so the returned field is settable. Use this in SetConfigValue.
func FindOrCreateFieldByTOMLKey(v reflect.Value, key string) (reflect.Value, error) {
	return findFieldByTOMLKey(v, key, true)
}

type unknownConfigKeyError struct {
	key string
}

func (e unknownConfigKeyError) Error() string {
	return fmt.Sprintf("unknown config key: %q", e.key)
}

func isUnknownConfigKeyError(err error) bool {
	var u unknownConfigKeyError
	return errors.As(err, &u)
}

// nestedStructValue resolves a field value to an underlying struct value.
// For nil pointers:
// - when initPointers=true, it initializes the pointer (if settable)
// - otherwise, it returns a throwaway zero struct for read-only traversal
func nestedStructValue(fieldVal reflect.Value, initPointers bool) (reflect.Value, bool) {
	if fieldVal.Kind() == reflect.Pointer {
		if fieldVal.IsNil() {
			elemType, isStruct := structType(fieldVal.Type())
			if !isStruct {
				return reflect.Value{}, false
			}
			if initPointers && fieldVal.CanSet() {
				fieldVal.Set(reflect.New(elemType))
			} else {
				return reflect.New(elemType).Elem(), true
			}
		}
		fieldVal = fieldVal.Elem()
	}
	if fieldVal.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	return fieldVal, true
}

func findFieldByTOMLKey(v reflect.Value, key string, initPointers bool) (reflect.Value, error) {
	parts := strings.SplitN(key, ".", 2)
	tagName := parts[0]

	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		fieldVal := v.Field(i)

		// Anonymous embedded struct with no tag is treated inline.
		if isInlineEmbeddedStructField(field) {
			nested, ok := nestedStructValue(fieldVal, initPointers)
			if !ok {
				continue
			}
			found, err := findFieldByTOMLKey(nested, key, initPointers)
			if err == nil {
				return found, nil
			}
			if !isUnknownConfigKeyError(err) {
				return reflect.Value{}, err
			}
			continue
		}

		tagKey := getTOMLKey(field)
		if tagKey == "" || tagKey != tagName {
			continue
		}

		// If there's a remaining dot path, recurse into nested struct
		if len(parts) == 2 {
			nested, ok := nestedStructValue(fieldVal, initPointers)
			if ok {
				return findFieldByTOMLKey(nested, parts[1], initPointers)
			}
			return reflect.Value{}, fmt.Errorf("key %q: %q is not a nested struct", key, tagName)
		}

		return fieldVal, nil
	}

	return reflect.Value{}, unknownConfigKeyError{key: key}
}

// formatValue converts a reflect.Value to its string representation
func formatValue(v reflect.Value) string {
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Int, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String {
			strs := make([]string, v.Len())
			for i := 0; i < v.Len(); i++ {
				strs[i] = v.Index(i).String()
			}
			return strings.Join(strs, ",")
		}
		return fmt.Sprintf("%v", v.Interface())
	case reflect.Map:
		return formatMap(v)
	case reflect.Pointer:
		if v.IsNil() {
			return ""
		}
		return formatValue(v.Elem())
	default:
		return fmt.Sprintf("%v", v.Interface())
	}
}

// mapEntry pairs a stringified key with its original reflect.Value for
// collision-safe sorting. The val field stores the map value captured
// during iteration so we don't need to re-lookup via MapIndex (which
// fails for NaN keys since NaN != NaN).
type mapEntry struct {
	str string
	key reflect.Value
	val reflect.Value
}

// formatMap returns a deterministic string representation of a map by sorting keys.
func formatMap(v reflect.Value) string {
	entries := make([]mapEntry, 0, v.Len())
	iter := v.MapRange()
	for iter.Next() {
		entries = append(entries, mapEntry{
			str: fmt.Sprintf("%v", iter.Key().Interface()),
			key: iter.Key(),
			val: iter.Value(),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].str != entries[j].str {
			return entries[i].str < entries[j].str
		}
		return compareKeys(entries[i].key, entries[j].key) < 0
	})
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.str+":"+fmt.Sprintf("%v", e.val.Interface()))
	}
	return strings.Join(parts, ",")
}

// compareKeys returns -1, 0, or 1 comparing two reflect.Values structurally.
// It handles all valid map-key kinds without relying on formatted strings,
// guaranteeing deterministic ordering even for types with colliding String()
// or GoString() output.
func compareKeys(a, b reflect.Value) int {
	// Dereference interfaces to their underlying values.
	// Nil interfaces have no underlying value; sort them before non-nil.
	if a.Kind() == reflect.Interface || b.Kind() == reflect.Interface {
		aNil := a.Kind() == reflect.Interface && a.IsNil()
		bNil := b.Kind() == reflect.Interface && b.IsNil()
		if aNil || bNil {
			if aNil && bNil {
				return 0
			}
			if aNil {
				return -1
			}
			return 1
		}
		if a.Kind() == reflect.Interface {
			a = a.Elem()
		}
		if b.Kind() == reflect.Interface {
			b = b.Elem()
		}
	}

	// Different concrete types: order by type string.
	if a.Type() != b.Type() {
		ta, tb := a.Type().String(), b.Type().String()
		if ta < tb {
			return -1
		}
		return 1
	}

	switch a.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		ai, bi := a.Int(), b.Int()
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
		return 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		au, bu := a.Uint(), b.Uint()
		if au < bu {
			return -1
		}
		if au > bu {
			return 1
		}
		return 0
	case reflect.Float32, reflect.Float64:
		af, bf := a.Float(), b.Float()
		if af < bf {
			return -1
		}
		if af > bf {
			return 1
		}
		// Neither < nor > was true. This happens for equal values and for
		// NaN comparisons. Fall back to bit-pattern ordering so distinct
		// NaN payloads (or ±0) get a deterministic order.
		ab, bb := math.Float64bits(af), math.Float64bits(bf)
		if ab < bb {
			return -1
		}
		if ab > bb {
			return 1
		}
		return 0
	case reflect.String:
		as, bs := a.String(), b.String()
		if as < bs {
			return -1
		}
		if as > bs {
			return 1
		}
		return 0
	case reflect.Bool:
		ab, bb := a.Bool(), b.Bool()
		if ab == bb {
			return 0
		}
		if !ab {
			return -1
		}
		return 1
	case reflect.Pointer:
		ap, bp := a.Pointer(), b.Pointer()
		if ap < bp {
			return -1
		}
		if ap > bp {
			return 1
		}
		return 0
	case reflect.Array:
		for i := 0; i < a.Len(); i++ {
			if c := compareKeys(a.Index(i), b.Index(i)); c != 0 {
				return c
			}
		}
		return 0
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			if c := compareKeys(a.Field(i), b.Field(i)); c != 0 {
				return c
			}
		}
		return 0
	default:
		// Fallback for exotic key types: use %#v formatting.
		fa := fmt.Sprintf("%#v", a.Interface())
		fb := fmt.Sprintf("%#v", b.Interface())
		if fa < fb {
			return -1
		}
		if fa > fb {
			return 1
		}
		return 0
	}
}

// setFieldValue sets a reflect.Value from a string, handling type conversion
func setFieldValue(field reflect.Value, value string) error {
	switch field.Kind() {
	case reflect.String:
		field.SetString(value)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid integer value: %q", value)
		}
		field.SetInt(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("invalid boolean value: %q", value)
		}
		field.SetBool(b)
	case reflect.Slice:
		if field.Type().Elem().Kind() == reflect.String {
			if value == "" {
				field.Set(reflect.MakeSlice(field.Type(), 0, 0))
			} else {
				parts := strings.Split(value, ",")
				for i := range parts {
					parts[i] = strings.TrimSpace(parts[i])
				}
				field.Set(reflect.ValueOf(parts))
			}
		} else {
			return fmt.Errorf("unsupported slice type for key")
		}
	case reflect.Pointer:
		// Handle *bool
		if field.Type().Elem().Kind() == reflect.Bool {
			b, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid boolean value: %q", value)
			}
			field.Set(reflect.ValueOf(&b))
		} else {
			return fmt.Errorf("unsupported pointer type for key")
		}
	default:
		return fmt.Errorf("unsupported field type: %s", field.Kind())
	}
	return nil
}

// listFields returns key-value pairs for all non-zero fields in a struct.
func listFields(v reflect.Value, prefix string) []KeyValue {
	return flattenStruct(v, prefix, false)
}

// listAllFields returns key-value pairs for ALL fields (including zero) in a struct.
// Used for merged config comparison.
func listAllFields(v reflect.Value, prefix string) []KeyValue {
	return flattenStruct(v, prefix, true)
}

// flattenStruct walks a struct's fields recursively, building dot-separated keys
// from TOML tags. When includeZero is false, zero-valued leaf fields are skipped.
func flattenStruct(v reflect.Value, prefix string, includeZero bool) []KeyValue {
	var result []KeyValue
	t := v.Type()

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		fieldVal := v.Field(i)
		tagKey := getTOMLKey(field)

		if tagKey == "" {
			if isInlineEmbeddedStructField(field) {
				if fieldVal.Kind() == reflect.Pointer {
					if fieldVal.IsNil() {
						continue
					}
					fieldVal = fieldVal.Elem()
				}
				if fieldVal.Kind() == reflect.Struct {
					result = append(result, flattenStruct(fieldVal, prefix, includeZero)...)
				}
			}
			continue
		}

		fullKey := tagKey
		if prefix != "" {
			fullKey = prefix + "." + tagKey
		}

		// Recurse into nested structs
		if fieldVal.Kind() == reflect.Pointer && !fieldVal.IsNil() && fieldVal.Elem().Kind() == reflect.Struct {
			result = append(result, flattenStruct(fieldVal.Elem(), fullKey, includeZero)...)
			continue
		}
		if fieldVal.Kind() == reflect.Struct {
			result = append(result, flattenStruct(fieldVal, fullKey, includeZero)...)
			continue
		}

		if !includeZero && fieldVal.IsZero() {
			continue
		}

		result = append(result, KeyValue{Key: fullKey, Value: formatValue(fieldVal)})
	}

	return result
}
