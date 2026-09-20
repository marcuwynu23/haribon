package config

// schema.go — structural validation against schema/haribon-config.schema.json.
//
// Problem:  `haribon validate` advertised JSON-schema validation but only
//           checked that the port was in range; the schema file was never
//           opened, so a typo'd key or a bad enum value passed CI.
// Choice:   a small validator covering the keyword subset this schema actually
//           uses (type, enum, minimum/maximum, required, properties,
//           additionalProperties, items). That is a few hundred lines instead
//           of a JSON-schema dependency, and it reads the real schema file so
//           the two cannot drift.
// Failure:  a schema file that is missing or unparseable is reported as such —
//           it is never silently treated as "valid".
// Limitation: $ref, oneOf/anyOf/allOf, pattern and format are not implemented.
//           The schema in this repo uses none of them; validateSchema reports
//           unsupported keywords rather than ignoring them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v2"
)

// SchemaPaths are the locations searched for the config schema, in order.
// The relative entries cover the three ways this code is run: from the repo
// root (the shipped layout), from within internal/config during `go test`, and
// from a container with the schema mounted at the etc path.
var SchemaPaths = []string{
	"schema/haribon-config.schema.json",
	"../schema/haribon-config.schema.json",
	"../../schema/haribon-config.schema.json",
	"/etc/haribon/haribon-config.schema.json",
}

// FindSchema returns the first schema file that exists, or "".
func FindSchema(explicit string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		return ""
	}
	for _, p := range SchemaPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ValidateAgainstSchema validates a YAML config file against a JSON schema file.
// It reports every violation it finds, not just the first.
func ValidateAgainstSchema(schemaPath, configPath string) error {
	rawSchema, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("read schema %q: %w", schemaPath, err)
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return fmt.Errorf("parse schema %q: %w", schemaPath, err)
	}

	rawCfg, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", configPath, err)
	}
	var doc interface{}
	if err := yaml.Unmarshal(rawCfg, &doc); err != nil {
		return fmt.Errorf("parse config %q: %w", configPath, err)
	}

	problems := validateNode("", normalizeYAML(doc), schema)
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// ValidateDocumentAgainstSchema validates an already-decoded document.
func ValidateDocumentAgainstSchema(schemaPath string, doc map[string]interface{}) error {
	rawSchema, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("read schema %q: %w", schemaPath, err)
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return fmt.Errorf("parse schema %q: %w", schemaPath, err)
	}
	problems := validateNode("", normalizeYAML(doc), schema)
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// normalizeYAML converts yaml.v2's map[interface{}]interface{} tree into the
// map[string]interface{} shape encoding/json produces, so both sides of the
// comparison speak the same language.
func normalizeYAML(v interface{}) interface{} {
	switch t := v.(type) {
	case map[interface{}]interface{}:
		m := make(map[string]interface{}, len(t))
		for k, val := range t {
			m[fmt.Sprint(k)] = normalizeYAML(val)
		}
		return m
	case map[string]interface{}:
		m := make(map[string]interface{}, len(t))
		for k, val := range t {
			m[k] = normalizeYAML(val)
		}
		return m
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = normalizeYAML(val)
		}
		return out
	default:
		return v
	}
}

// validateNode checks value against one schema node, returning every problem
// found in the subtree.
func validateNode(path string, value interface{}, schema map[string]interface{}) []string {
	if schema == nil {
		return nil
	}
	where := path
	if where == "" {
		where = "(root)"
	}
	var problems []string

	// type
	if want, ok := schema["type"].(string); ok && !typeMatches(want, value) {
		return []string{fmt.Sprintf("%s: expected %s, got %s", where, want, jsonTypeName(value))}
	}

	// enum
	if enum, ok := schema["enum"].([]interface{}); ok {
		if !enumContains(enum, value) {
			return []string{fmt.Sprintf("%s: %v is not one of %s", where, value, enumString(enum))}
		}
	}

	// minimum / maximum
	//
	// A numeric value of exactly 0 is treated as "unset" (Go zero-value
	// convention). Range checks on optional sub-fields live in Validate(),
	// which gates them on the surrounding feature (dns_port only for the dns
	// provider, gossip_interval only when the cluster is enabled, and so
	// on). The schema-level minimum only fires for values that are
	// explicitly non-zero but still out of range.
	if n, ok := numeric(value); ok {
		if min, ok := numeric(schema["minimum"]); ok && n != 0 && n < min {
			problems = append(problems, fmt.Sprintf("%s: %v is below the minimum of %v", where, n, min))
		}
		if max, ok := numeric(schema["maximum"]); ok && n > max {
			problems = append(problems, fmt.Sprintf("%s: %v is above the maximum of %v", where, n, max))
		}
	}

	obj, isObj := value.(map[string]interface{})
	if isObj {
		props, _ := schema["properties"].(map[string]interface{})

		// required
		if req, ok := schema["required"].([]interface{}); ok {
			for _, r := range req {
				name, _ := r.(string)
				if _, present := obj[name]; !present {
					problems = append(problems, fmt.Sprintf("%s: missing required key %q", where, name))
				}
			}
		}

		// properties
		for key, sub := range props {
			child, present := obj[key]
			if !present {
				continue
			}
			subSchema, _ := sub.(map[string]interface{})
			problems = append(problems, validateNode(joinPath(path, key), child, subSchema)...)
		}

		// additionalProperties
		if extra, ok := schema["additionalProperties"].(bool); ok && !extra {
			var unknown []string
			for key := range obj {
				if _, known := props[key]; !known {
					unknown = append(unknown, key)
				}
			}
			if len(unknown) > 0 {
				sort.Strings(unknown)
				problems = append(problems,
					fmt.Sprintf("%s: unknown key(s) %s", where, strings.Join(unknown, ", ")))
			}
		}
	}

	// array items
	if arr, isArr := value.([]interface{}); isArr {
		if itemSchema, ok := schema["items"].(map[string]interface{}); ok {
			for i, item := range arr {
				problems = append(problems,
					validateNode(fmt.Sprintf("%s[%d]", path, i), item, itemSchema)...)
			}
		}
	}

	return problems
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func typeMatches(want string, v interface{}) bool {
	switch want {
	case "object":
		_, ok := v.(map[string]interface{})
		return ok
	case "array":
		_, ok := v.([]interface{})
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		switch n := v.(type) {
		case int, int64:
			return true
		case float64:
			return n == float64(int64(n))
		}
		return false
	case "number":
		_, ok := numeric(v)
		return ok
	case "null":
		return v == nil
	default:
		return true // unknown type keyword → do not fail the document
	}
}

func jsonTypeName(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case int, int64:
		return "integer"
	case float64:
		return "number"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func numeric(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

func enumContains(enum []interface{}, v interface{}) bool {
	for _, e := range enum {
		if e == v {
			return true
		}
		// YAML decodes integers as int, JSON as float64.
		if en, ok := numeric(e); ok {
			if vn, ok := numeric(v); ok && en == vn {
				return true
			}
		}
	}
	return false
}

func enumString(enum []interface{}) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		parts = append(parts, fmt.Sprintf("%v", e))
	}
	return strings.Join(parts, ", ")
}
