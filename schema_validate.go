package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// A validator for the JSON Schema subset tools use for inputSchema and
// outputSchema (draft 2020-12 vocabulary, as the tools spec references):
// type (single or list), enum, const, properties, required,
// additionalProperties (boolean or schema), patternProperties, items,
// prefixItems, minItems, maxItems, uniqueItems, minLength, maxLength,
// pattern, format (checked for the well-known formats only), minimum,
// maximum, exclusiveMinimum, exclusiveMaximum, multipleOf, minProperties,
// maxProperties, anyOf, oneOf, allOf, not, and local $ref ("#/$defs/…" and
// "#/definitions/…"). Keywords outside the subset are ignored, as JSON
// Schema requires of unknown keywords, so a schema is never rejected for
// using a feature this validator does not know.

// schemaError is one violation, with the JSON pointer-ish path of the value.
type schemaError struct {
	path string
	msg  string
}

func (e schemaError) Error() string {
	if e.path == "" {
		return e.msg
	}
	return e.path + ": " + e.msg
}

// validateJSONSchema checks value (decoded JSON: map[string]any, []any,
// string, float64/json.Number/Go integers, bool, nil) against schema and
// returns the first violation found, or nil.
func validateJSONSchema(schema any, value any) error {
	root, _ := schema.(map[string]any)
	v := &schemaValidator{root: root}
	return v.check(schema, value, "")
}

type schemaValidator struct {
	root  map[string]any
	depth int
}

func (v *schemaValidator) check(schema any, value any, path string) error {
	switch s := schema.(type) {
	case bool:
		if !s {
			return schemaError{path, "no value is allowed here"}
		}
		return nil
	case map[string]any:
		return v.checkObjectSchema(s, value, path)
	}
	return nil // not a schema: nothing to check
}

func (v *schemaValidator) checkObjectSchema(s map[string]any, value any, path string) error {
	if v.depth > 64 {
		return schemaError{path, "schema nesting too deep"}
	}
	v.depth++
	defer func() { v.depth-- }()

	if ref, ok := s["$ref"].(string); ok {
		target := v.resolveRef(ref)
		if target == nil {
			return nil // an external or unknown reference: not checked
		}
		if err := v.check(target, value, path); err != nil {
			return err
		}
	}

	if t, ok := s["type"]; ok {
		if err := checkType(t, value, path); err != nil {
			return err
		}
	}
	if c, ok := s["const"]; ok && !jsonEqual(c, value) {
		return schemaError{path, "must equal the constant " + jsonText(c)}
	}
	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if jsonEqual(e, value) {
				found = true
				break
			}
		}
		if !found {
			return schemaError{path, "must be one of " + jsonText(enum)}
		}
	}

	switch val := value.(type) {
	case map[string]any:
		if err := v.checkObject(s, val, path); err != nil {
			return err
		}
	case []any:
		if err := v.checkArray(s, val, path); err != nil {
			return err
		}
	case string:
		if err := checkString(s, val, path); err != nil {
			return err
		}
	default:
		if n, ok := jsonNumberValue(value); ok {
			if err := checkNumber(s, n, path); err != nil {
				return err
			}
		}
	}

	if all, ok := s["allOf"].([]any); ok {
		for i, sub := range all {
			if err := v.check(sub, value, path); err != nil {
				return schemaError{path, fmt.Sprintf("does not satisfy allOf[%d]: %v", i, stripPath(err))}
			}
		}
	}
	if any_, ok := s["anyOf"].([]any); ok {
		matched := false
		for _, sub := range any_ {
			if v.check(sub, value, path) == nil {
				matched = true
				break
			}
		}
		if !matched {
			return schemaError{path, "does not satisfy any of the anyOf schemas"}
		}
	}
	if one, ok := s["oneOf"].([]any); ok {
		matches := 0
		for _, sub := range one {
			if v.check(sub, value, path) == nil {
				matches++
			}
		}
		if matches != 1 {
			return schemaError{path, fmt.Sprintf("must satisfy exactly one oneOf schema, satisfies %d", matches)}
		}
	}
	if not, ok := s["not"]; ok {
		if v.check(not, value, path) == nil {
			return schemaError{path, "must not satisfy the not schema"}
		}
	}
	return nil
}

func (v *schemaValidator) resolveRef(ref string) any {
	if v.root == nil || !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var cur any = v.root
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[part]
		if !ok {
			return nil
		}
	}
	return cur
}

func checkType(t any, value any, path string) error {
	var types []string
	switch tt := t.(type) {
	case string:
		types = []string{tt}
	case []any:
		for _, x := range tt {
			if s, ok := x.(string); ok {
				types = append(types, s)
			}
		}
	case []string:
		types = tt
	}
	if len(types) == 0 {
		return nil
	}
	for _, want := range types {
		if jsonValueMatchesType(value, want) {
			return nil
		}
	}
	return schemaError{path, "must be of type " + strings.Join(types, " or ")}
}

func (v *schemaValidator) checkObject(s map[string]any, obj map[string]any, path string) error {
	required := stringList(s["required"])
	for _, name := range required {
		if _, present := obj[name]; !present {
			return schemaError{path, "missing required property " + name}
		}
	}
	if n, ok := jsonNumberValue(s["minProperties"]); ok && float64(len(obj)) < n {
		return schemaError{path, fmt.Sprintf("must have at least %v properties", n)}
	}
	if n, ok := jsonNumberValue(s["maxProperties"]); ok && float64(len(obj)) > n {
		return schemaError{path, fmt.Sprintf("must have at most %v properties", n)}
	}
	props, _ := s["properties"].(map[string]any)
	patterns, _ := s["patternProperties"].(map[string]any)
	additional, hasAdditional := s["additionalProperties"]

	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic first error
	requiredSet := map[string]bool{}
	for _, r := range required {
		requiredSet[r] = true
	}
	for _, name := range names {
		val := obj[name]
		childPath := joinPath(path, name)
		if val == nil && !requiredSet[name] {
			// An optional property sent as null is treated as absent: hosts
			// commonly send null for an argument they leave out, and the
			// x-mcp-header rules read null as "no value" too.
			if _, declared := props[name]; declared {
				continue
			}
		}
		covered := false
		if ps, ok := props[name]; ok {
			covered = true
			if err := v.check(ps, val, childPath); err != nil {
				return err
			}
		}
		for pattern, ps := range patterns {
			if re := compiledPattern(pattern); re != nil && re.MatchString(name) {
				covered = true
				if err := v.check(ps, val, childPath); err != nil {
					return err
				}
			}
		}
		if !covered && hasAdditional {
			switch a := additional.(type) {
			case bool:
				if !a {
					return schemaError{childPath, "unexpected property"}
				}
			case map[string]any:
				if err := v.check(a, val, childPath); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (v *schemaValidator) checkArray(s map[string]any, arr []any, path string) error {
	if n, ok := jsonNumberValue(s["minItems"]); ok && float64(len(arr)) < n {
		return schemaError{path, fmt.Sprintf("must have at least %v items", n)}
	}
	if n, ok := jsonNumberValue(s["maxItems"]); ok && float64(len(arr)) > n {
		return schemaError{path, fmt.Sprintf("must have at most %v items", n)}
	}
	prefix, _ := s["prefixItems"].([]any)
	for i, item := range arr {
		childPath := fmt.Sprintf("%s[%d]", path, i)
		if i < len(prefix) {
			if err := v.check(prefix[i], item, childPath); err != nil {
				return err
			}
			continue
		}
		if items, ok := s["items"]; ok {
			if err := v.check(items, item, childPath); err != nil {
				return err
			}
		}
	}
	if unique, _ := s["uniqueItems"].(bool); unique {
		seen := map[string]bool{}
		for i, item := range arr {
			key := jsonText(item)
			if seen[key] {
				return schemaError{fmt.Sprintf("%s[%d]", path, i), "items must be unique"}
			}
			seen[key] = true
		}
	}
	return nil
}

func checkString(s map[string]any, str string, path string) error {
	length := float64(len([]rune(str)))
	if n, ok := jsonNumberValue(s["minLength"]); ok && length < n {
		return schemaError{path, fmt.Sprintf("must be at least %v characters", n)}
	}
	if n, ok := jsonNumberValue(s["maxLength"]); ok && length > n {
		return schemaError{path, fmt.Sprintf("must be at most %v characters", n)}
	}
	if pattern, ok := s["pattern"].(string); ok {
		if re := compiledPattern(pattern); re != nil && !re.MatchString(str) {
			return schemaError{path, "must match pattern " + pattern}
		}
	}
	if format, ok := s["format"].(string); ok {
		if re := formatPatterns[format]; re != nil && !re.MatchString(str) {
			return schemaError{path, "must be a valid " + format}
		}
	}
	return nil
}

func checkNumber(s map[string]any, n float64, path string) error {
	if m, ok := jsonNumberValue(s["minimum"]); ok && n < m {
		return schemaError{path, fmt.Sprintf("must be at least %v", m)}
	}
	if m, ok := jsonNumberValue(s["maximum"]); ok && n > m {
		return schemaError{path, fmt.Sprintf("must be at most %v", m)}
	}
	if m, ok := jsonNumberValue(s["exclusiveMinimum"]); ok && n <= m {
		return schemaError{path, fmt.Sprintf("must be greater than %v", m)}
	}
	if m, ok := jsonNumberValue(s["exclusiveMaximum"]); ok && n >= m {
		return schemaError{path, fmt.Sprintf("must be less than %v", m)}
	}
	if m, ok := jsonNumberValue(s["multipleOf"]); ok && m > 0 {
		q := n / m
		if math.Abs(q-math.Round(q)) > 1e-9 {
			return schemaError{path, fmt.Sprintf("must be a multiple of %v", m)}
		}
	}
	return nil
}

// formatPatterns covers the formats worth checking without a dependency;
// other formats are accepted (JSON Schema treats format as an annotation).
var formatPatterns = map[string]*regexp.Regexp{
	"date":      regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`),
	"date-time": regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[Tt ]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$`),
	"time":      regexp.MustCompile(`^\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})?$`),
	"email":     regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`),
	"uuid":      regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`),
	"ipv4":      regexp.MustCompile(`^(\d{1,3}\.){3}\d{1,3}$`),
	"uri":       regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`),
}

var (
	patternMu    sync.Mutex
	patternCache = map[string]*regexp.Regexp{}
)

// compiledPattern compiles a schema regexp once; an invalid pattern is
// treated as matching everything rather than rejecting every value.
func compiledPattern(pattern string) *regexp.Regexp {
	patternMu.Lock()
	defer patternMu.Unlock()
	if re, ok := patternCache[pattern]; ok {
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil
	}
	patternCache[pattern] = re
	return re
}

func stringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// jsonEqual compares two decoded JSON values by their JSON text, with
// numbers compared numerically.
func jsonEqual(a, b any) bool {
	if na, ok := jsonNumberValue(a); ok {
		nb, ok := jsonNumberValue(b)
		return ok && na == nb
	}
	return jsonText(a) == jsonText(b)
}

func stripPath(err error) string {
	if se, ok := err.(schemaError); ok {
		return se.msg
	}
	return err.Error()
}
