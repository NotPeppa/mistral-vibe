package main

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

var (
	structCacheMu sync.Mutex
	structCache   = map[reflect.Type]map[string]reflect.Type{}
)

// tolerantIntoTarget lets one decoder accept every spelling CPA and operators
// actually use on the wire:
//
//   - StorageJSON serializes as base64 by Go's []byte rules, but seeds written
//     by hand (and older plugin samples) carry a raw object.
//   - Auth field names are PascalCase here, snake_case in raw auth files.
//   - Numeric strings appear wherever a panel round-tripped a form value.
//
// It re-encodes a normalized map and unmarshals that into the target. Fields
// the target does not know are ignored, so this never loses data.
func tolerantInto(raw []byte, target any) error {
	if len(raw) == 0 {
		return nil
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return err
	}
	normalized := normalizeKeys(generic, target)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return err
	}
	setByteFields(target, generic)
	return nil
}

// setByteFields copies every []byte field the caller supplied outright.
// encoding/json needs base64 for []byte, and CPA's request structs embed
// anonymous members that can swallow the tagged field, so the bytes are
// assigned reflectively from the source map instead.
func setByteFields(target any, source map[string]any) {
	value := reflect.ValueOf(target)
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return
	}
	kinds := byteFieldsOf(target)
	if len(kinds) == 0 {
		return
	}
	for key, raw := range source {
		name := pascal(key)
		if _, ok := kinds[name]; !ok {
			continue
		}
		bytes := storageBytes(raw)
		if bytes == nil {
			continue
		}
		field := value.FieldByName(name)
		if field.IsValid() && field.CanSet() && field.Kind() == reflect.Slice {
			field.SetBytes(bytes)
		}
	}
}

// normalizeKeys maps snake_case, kebab-case and camelCase spellings onto the
// PascalCase field names the SDK structs use. Map-shaped fields (http.Header,
// url.Values) and raw JSON pass through untouched; byte slices go through
// storageBytes, which accepts base64 or a raw object.
func normalizeKeys(in map[string]any, target any) map[string]any {
	bytesFields := byteFieldsOf(target)
	opaque := opaqueFieldsOf(target)
	out := make(map[string]any, len(in))
	for key, value := range in {
		name := pascal(key)
		if _, isOpaque := opaque[name]; isOpaque {
			out[name] = value
			continue
		}
		if _, isBytes := bytesFields[name]; isBytes {
			if raw := storageBytes(value); raw != nil {
				out[name] = base64.StdEncoding.EncodeToString(raw)
			}
			continue
		}
		out[name] = normalizeValue(value, fieldTypeOf(target, name))
	}
	return out
}

// opaqueFieldsOf lists map-typed fields, which carry user data (header names,
// query keys) whose own casing must never be rewritten.
func opaqueFieldsOf(target any) map[string]struct{} {
	out := map[string]struct{}{}
	for name, fieldType := range structFields(elementType(target)) {
		if fieldType.Kind() == reflect.Map {
			out[name] = struct{}{}
		}
	}
	return out
}

func normalizeValue(value any, target reflect.Type) any {
	switch typed := value.(type) {
	case map[string]any:
		if target == nil {
			return typed
		}
		return normalizeKeys(typed, target)
	case []any:
		for i, item := range typed {
			typed[i] = normalizeValue(item, elemTypeOf(target))
		}
		return typed
	default:
		return value
	}
}

// pascal turns any common key spelling into the SDK's Go field name.
func pascal(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return key
	}
	replacer := strings.NewReplacer("_", " ", "-", " ")
	parts := strings.Fields(replacer.Replace(key))
	if len(parts) == 0 {
		return key
	}
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		upper := strings.ToUpper(part[:1]) + part[1:]
		if upper == "Json" {
			upper = "JSON"
		}
		b.WriteString(upper)
	}
	out := b.String()
	if out == "StorageJson" {
		return "StorageJSON"
	}
	return out
}

// storageBytes accepts base64 (Go's []byte wire form), a raw JSON object, or a
// JSON string holding either.
func storageBytes(value any) []byte {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			return []byte(trimmed)
		}
		if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil && json.Valid(decoded) {
			return decoded
		}
		return []byte(trimmed)
	case map[string]any:
		if raw, err := json.Marshal(typed); err == nil {
			return raw
		}
	case []any:
		bytes := make([]byte, 0, len(typed))
		for _, item := range typed {
			if n, ok := numeric(item); ok && n >= 0 && n <= 255 {
				bytes = append(bytes, byte(n))
			}
		}
		if json.Valid(bytes) {
			return bytes
		}
	}
	return nil
}

func numeric(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case json.Number:
		f, err := typed.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return f, err == nil
	}
	return 0, false
}

// -- reflection helpers ------------------------------------------------------

func elementType(target any) reflect.Type {
	t := reflect.TypeOf(target)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func structFields(t reflect.Type) map[string]reflect.Type {
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	structCacheMu.Lock()
	if cached, ok := structCache[t]; ok {
		structCacheMu.Unlock()
		return cached
	}
	fields := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		fields[field.Name] = field.Type
	}
	structCache[t] = fields
	structCacheMu.Unlock()
	return fields
}

func fieldTypeOf(target any, name string) reflect.Type {
	return structFields(elementType(target))[name]
}

func byteFieldsOf(target any) map[string]struct{} {
	out := map[string]struct{}{}
	for name, fieldType := range structFields(elementType(target)) {
		if fieldType.Kind() == reflect.Slice && fieldType.Elem().Kind() == reflect.Uint8 {
			out[name] = struct{}{}
		}
	}
	return out
}

func elemTypeOf(t reflect.Type) reflect.Type {
	if t == nil {
		return nil
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return t.Elem()
	case reflect.Pointer:
		return t.Elem()
	}
	return nil
}
