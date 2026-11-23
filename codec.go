package main

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

type fieldInfo struct {
	jsonKey   string
	fieldName string
	fieldType reflect.Type
}

var (
	structCacheMu sync.Mutex
	structCache   = map[reflect.Type]map[string]fieldInfo{}
)

// tolerantInto lets one decoder accept every spelling CPA and operators
// actually use on the wire:
//
//   - StorageJSON serializes as base64 by Go's []byte rules, but seeds written
//     by hand (and older plugin samples) carry a raw object.
//   - Field names may arrive as PascalCase, camelCase, snake_case, or kebab-case,
//     whether or not the target SDK struct carries explicit json tags.
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
	fields := structFields(elementType(target))
	if len(fields) == 0 {
		return
	}
	for key, raw := range source {
		info, ok := fields[canonicalLookup(key)]
		if !ok || !isByteSlice(info.fieldType) {
			continue
		}
		bytes := storageBytes(raw)
		if bytes == nil {
			continue
		}
		field := value.FieldByName(info.fieldName)
		if field.IsValid() && field.CanSet() && field.Kind() == reflect.Slice {
			field.SetBytes(bytes)
		}
	}
}

// normalizeKeys maps snake_case, kebab-case, camelCase, and PascalCase
// spellings onto the exact JSON key each SDK struct field expects (honouring
// explicit json tags as well as untagged Go field names). Map-shaped fields
// (http.Header, url.Values, metadata maps) pass through untouched; byte slices
// go through storageBytes, which accepts base64 or a raw object.
func normalizeKeys(in map[string]any, target any) map[string]any {
	fields := structFields(elementType(target))
	out := make(map[string]any, len(in))
	for key, value := range in {
		info, known := fields[canonicalLookup(key)]
		if known {
			if info.fieldType.Kind() == reflect.Map {
				out[info.jsonKey] = value
				continue
			}
			if isByteSlice(info.fieldType) {
				if raw := storageBytes(value); raw != nil {
					out[info.jsonKey] = base64.StdEncoding.EncodeToString(raw)
				}
				continue
			}
			out[info.jsonKey] = normalizeValue(value, info.fieldType)
			continue
		}
		out[pascal(key)] = value
	}
	return out
}

func normalizeValue(value any, target reflect.Type) any {
	switch typed := value.(type) {
	case map[string]any:
		if target == nil {
			return typed
		}
		elem := elementType(target)
		if elem == nil || elem.Kind() != reflect.Struct {
			return typed
		}
		return normalizeKeys(typed, elem)
	case []any:
		elem := elemTypeOf(target)
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = normalizeValue(item, elem)
		}
		return out
	default:
		return value
	}
}

// pascal turns any common key spelling into a PascalCase Go field name.
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

// storageBytes accepts base64 (Go's []byte wire form), a raw JSON object, a
// JSON string holding either, or a numeric byte array.
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
		if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
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
			n, ok := numeric(item)
			if !ok || n < 0 || n > 255 {
				return nil
			}
			bytes = append(bytes, byte(n))
		}
		return bytes
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

func canonicalLookup(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.TrimSpace(key) {
		if r == '_' || r == '-' || r == ' ' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func elementType(target any) reflect.Type {
	t, ok := target.(reflect.Type)
	if !ok {
		t = reflect.TypeOf(target)
	}
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func structFields(t reflect.Type) map[string]fieldInfo {
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	structCacheMu.Lock()
	if cached, ok := structCache[t]; ok {
		structCacheMu.Unlock()
		return cached
	}
	fields := make(map[string]fieldInfo, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		jsonKey := field.Name
		if tag := field.Tag.Get("json"); tag != "" {
			name := strings.Split(tag, ",")[0]
			if name == "-" {
				continue
			}
			if name != "" {
				jsonKey = name
			}
		}
		info := fieldInfo{
			jsonKey:   jsonKey,
			fieldName: field.Name,
			fieldType: field.Type,
		}
		fields[canonicalLookup(field.Name)] = info
		fields[canonicalLookup(jsonKey)] = info
	}
	structCache[t] = fields
	structCacheMu.Unlock()
	return fields
}

func isByteSlice(t reflect.Type) bool {
	return t != nil && t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8
}

func elemTypeOf(t reflect.Type) reflect.Type {
	if t == nil {
		return nil
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Pointer:
		return t.Elem()
	}
	return nil
}
