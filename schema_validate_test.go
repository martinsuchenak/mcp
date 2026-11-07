package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func schemaOf(t *testing.T, text string) any {
	t.Helper()
	var s any
	if err := json.Unmarshal([]byte(text), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func valueOf(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestValidateJSONSchema(t *testing.T) {
	schema := schemaOf(t, `{
	  "type": "object",
	  "properties": {
	    "name": {"type": "string", "minLength": 1, "maxLength": 10, "pattern": "^[a-z]+$"},
	    "age": {"type": "integer", "minimum": 0, "maximum": 150},
	    "ratio": {"type": "number", "exclusiveMinimum": 0, "multipleOf": 0.5},
	    "tags": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 3, "uniqueItems": true},
	    "kind": {"enum": ["a", "b"]},
	    "fixed": {"const": 7},
	    "addr": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"], "additionalProperties": false},
	    "when": {"type": "string", "format": "date"},
	    "either": {"oneOf": [{"type": "string"}, {"type": "integer"}]},
	    "any": {"anyOf": [{"type": "string"}, {"type": "null"}]},
	    "notbool": {"not": {"type": "boolean"}},
	    "nullable": {"type": ["string", "null"]},
	    "deep": {"$ref": "#/$defs/point"},
	    "pair": {"type": "array", "prefixItems": [{"type": "string"}, {"type": "integer"}]}
	  },
	  "required": ["name"],
	  "additionalProperties": false,
	  "$defs": {"point": {"type": "object", "properties": {"x": {"type": "number"}}, "required": ["x"]}}
	}`)
	good := []string{
		`{"name":"bob"}`,
		`{"name":"bob","age":42,"ratio":1.5,"tags":["x","y"],"kind":"a","fixed":7,"addr":{"city":"Perth"},"when":"2026-11-07","either":"s","any":null,"notbool":3,"nullable":null,"deep":{"x":1},"pair":["a",1]}`,
	}
	for _, g := range good {
		if err := validateJSONSchema(schema, valueOf(t, g)); err != nil {
			t.Errorf("%s: unexpected %v", g, err)
		}
	}
	bad := map[string]string{
		`{}`:                                         "missing required property name",
		`{"name":5}`:                                 "name: must be of type string",
		`{"name":""}`:                                "name: must be at least 1",
		`{"name":"Bob"}`:                             "name: must match pattern",
		`{"name":"bob","age":1.5}`:                   "age: must be of type integer",
		`{"name":"bob","age":200}`:                   "age: must be at most 150",
		`{"name":"bob","ratio":0}`:                   "ratio: must be greater than 0",
		`{"name":"bob","ratio":0.3}`:                 "ratio: must be a multiple of 0.5",
		`{"name":"bob","tags":[]}`:                   "tags: must have at least 1",
		`{"name":"bob","tags":["a","a"]}`:            "tags[1]: items must be unique",
		`{"name":"bob","tags":[1]}`:                  "tags[0]: must be of type string",
		`{"name":"bob","kind":"c"}`:                  "kind: must be one of",
		`{"name":"bob","fixed":8}`:                   "fixed: must equal the constant 7",
		`{"name":"bob","addr":{}}`:                   "addr: missing required property city",
		`{"name":"bob","addr":{"city":"P","zip":1}}`: "addr.zip: unexpected property",
		`{"name":"bob","when":"today"}`:              "when: must be a valid date",
		`{"name":"bob","either":true}`:               "either: must satisfy exactly one",
		`{"name":"bob","any":1}`:                     "any: does not satisfy any",
		`{"name":"bob","notbool":true}`:              "notbool: must not satisfy",
		`{"name":"bob","nullable":1}`:                "nullable: must be of type string or null",
		`{"name":"bob","deep":{}}`:                   "deep: missing required property x",
		`{"name":"bob","pair":["a","b"]}`:            "pair[1]: must be of type integer",
		`{"name":"bob","extra":1}`:                   "extra: unexpected property",
	}
	for input, want := range bad {
		err := validateJSONSchema(schema, valueOf(t, input))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", input, err, want)
		}
	}
	// Unknown keywords and an unresolvable $ref are ignored; boolean schemas work.
	if err := validateJSONSchema(schemaOf(t, `{"type":"object","x-vendor":1,"properties":{"a":{"$ref":"https://example/x"}}}`), valueOf(t, `{"a":1}`)); err != nil {
		t.Errorf("unknown keyword/ref: %v", err)
	}
	if err := validateJSONSchema(schemaOf(t, `{"type":"object","additionalProperties":{"type":"string"}}`), valueOf(t, `{"a":1}`)); err == nil {
		t.Error("additionalProperties schema not applied")
	}
	if err := validateJSONSchema(false, 1); err == nil {
		t.Error("false schema accepted a value")
	}
}
