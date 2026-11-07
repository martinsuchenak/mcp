package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// ToolHandler represents a function that handles tool calls.
// It receives the request context and a ToolRequest with typed argument accessors.
type ToolHandler func(ctx context.Context, req *ToolRequest) (*ToolResponse, error)

// ToolRequest provides typed access to tool arguments.
// Use the accessor methods (String, Int, Bool, etc.) to retrieve parameters
// with automatic type conversion and validation.
type ToolRequest struct {
	args map[string]any
}

// NewToolRequest creates a new ToolRequest with the given arguments.
// This is typically called by the server when dispatching tool calls.
func NewToolRequest(args map[string]any) *ToolRequest {
	return &ToolRequest{args: args}
}

// String returns a string parameter by name.
// Returns ErrUnknownParameter if the parameter doesn't exist.
func (r *ToolRequest) String(name string) (string, error) {
	val, ok := r.args[name]
	if !ok {
		return "", ErrUnknownParameter
	}
	if str, ok := val.(string); ok {
		return str, nil
	}
	return "", fmt.Errorf("parameter '%s' is not a string", name)
}

// StringOr returns a string parameter or defaultValue if not present or invalid.
func (r *ToolRequest) StringOr(name, defaultValue string) string {
	if val, err := r.String(name); err == nil {
		return val
	}
	return defaultValue
}

// Int returns an integer parameter by name.
// Handles both int and float64 types (JSON numbers are parsed as float64).
// Returns ErrUnknownParameter if the parameter doesn't exist.
func (r *ToolRequest) Int(name string) (int, error) {
	val, ok := r.args[name]
	if !ok {
		return 0, ErrUnknownParameter
	}
	switch v := val.(type) {
	case int:
		return v, nil
	case float64:
		return int(v), nil
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i), nil
		}
		if f, err := v.Float64(); err == nil {
			return int(f), nil
		}
	}
	return 0, fmt.Errorf("parameter '%s' is not a number", name)
}

// IntOr returns an integer parameter or defaultValue if not present or invalid.
func (r *ToolRequest) IntOr(name string, defaultValue int) int {
	if val, err := r.Int(name); err == nil {
		return val
	}
	return defaultValue
}

// Float returns a float64 parameter by name.
// Returns ErrUnknownParameter if the parameter doesn't exist.
func (r *ToolRequest) Float(name string) (float64, error) {
	val, ok := r.args[name]
	if !ok {
		return 0, ErrUnknownParameter
	}
	if num, ok := val.(float64); ok {
		return num, nil
	}
	return 0, fmt.Errorf("parameter '%s' is not a number", name)
}

// FloatOr returns a float64 parameter or defaultValue if not present or invalid.
func (r *ToolRequest) FloatOr(name string, defaultValue float64) float64 {
	if val, err := r.Float(name); err == nil {
		return val
	}
	return defaultValue
}

// Bool returns a boolean parameter by name.
// Returns ErrUnknownParameter if the parameter doesn't exist.
func (r *ToolRequest) Bool(name string) (bool, error) {
	val, ok := r.args[name]
	if !ok {
		return false, ErrUnknownParameter
	}
	if b, ok := val.(bool); ok {
		return b, nil
	}
	return false, fmt.Errorf("parameter '%s' is not a boolean", name)
}

// BoolOr returns a boolean parameter or defaultValue if not present or invalid.
func (r *ToolRequest) BoolOr(name string, defaultValue bool) bool {
	if val, err := r.Bool(name); err == nil {
		return val
	}
	return defaultValue
}

// StringSlice returns a string array parameter by name.
// Returns ErrUnknownParameter if the parameter doesn't exist.
func (r *ToolRequest) StringSlice(name string) ([]string, error) {
	val, ok := r.args[name]
	if !ok {
		return nil, ErrUnknownParameter
	}
	if arr, ok := val.([]any); ok {
		result := make([]string, len(arr))
		for i, item := range arr {
			if str, ok := item.(string); ok {
				result[i] = str
			} else {
				return nil, fmt.Errorf("parameter '%s' contains non-string element at index %d", name, i)
			}
		}
		return result, nil
	}
	return nil, fmt.Errorf("parameter '%s' is not an array", name)
}

// StringSliceOr returns a string array parameter or defaultValue if not present or invalid.
func (r *ToolRequest) StringSliceOr(name string, defaultValue []string) []string {
	if val, err := r.StringSlice(name); err == nil {
		return val
	}
	return defaultValue
}

// IntSlice returns an integer array parameter by name.
// Handles both int and float64 array elements (JSON numbers are parsed as float64).
// Returns ErrUnknownParameter if the parameter doesn't exist.
func (r *ToolRequest) IntSlice(name string) ([]int, error) {
	val, ok := r.args[name]
	if !ok {
		return nil, ErrUnknownParameter
	}
	if arr, ok := val.([]any); ok {
		result := make([]int, len(arr))
		for i, item := range arr {
			switch v := item.(type) {
			case int:
				result[i] = v
			case float64:
				result[i] = int(v)
			default:
				return nil, fmt.Errorf("parameter '%s' contains non-number element at index %d", name, i)
			}
		}
		return result, nil
	}
	return nil, fmt.Errorf("parameter '%s' is not an array", name)
}

// IntSliceOr returns an integer array parameter or defaultValue if not present or invalid.
func (r *ToolRequest) IntSliceOr(name string, defaultValue []int) []int {
	if val, err := r.IntSlice(name); err == nil {
		return val
	}
	return defaultValue
}

// FloatSlice returns a float64 array parameter by name.
// Returns ErrUnknownParameter if the parameter doesn't exist.
func (r *ToolRequest) FloatSlice(name string) ([]float64, error) {
	val, ok := r.args[name]
	if !ok {
		return nil, ErrUnknownParameter
	}
	if arr, ok := val.([]any); ok {
		result := make([]float64, len(arr))
		for i, item := range arr {
			if num, ok := item.(float64); ok {
				result[i] = num
			} else {
				return nil, fmt.Errorf("parameter '%s' contains non-number element at index %d", name, i)
			}
		}
		return result, nil
	}
	return nil, fmt.Errorf("parameter '%s' is not an array", name)
}

// FloatSliceOr returns a float64 array parameter or defaultValue if not present or invalid.
func (r *ToolRequest) FloatSliceOr(name string, defaultValue []float64) []float64 {
	if val, err := r.FloatSlice(name); err == nil {
		return val
	}
	return defaultValue
}

// BoolSlice returns a boolean array parameter by name.
// Returns ErrUnknownParameter if the parameter doesn't exist.
func (r *ToolRequest) BoolSlice(name string) ([]bool, error) {
	val, ok := r.args[name]
	if !ok {
		return nil, ErrUnknownParameter
	}
	if arr, ok := val.([]any); ok {
		result := make([]bool, len(arr))
		for i, item := range arr {
			if b, ok := item.(bool); ok {
				result[i] = b
			} else {
				return nil, fmt.Errorf("parameter '%s' contains non-boolean element at index %d", name, i)
			}
		}
		return result, nil
	}
	return nil, fmt.Errorf("parameter '%s' is not an array", name)
}

// BoolSliceOr returns a boolean array parameter or defaultValue if not present or invalid.
func (r *ToolRequest) BoolSliceOr(name string, defaultValue []bool) []bool {
	if val, err := r.BoolSlice(name); err == nil {
		return val
	}
	return defaultValue
}

// Object returns a parameter as a map[string]any (generic object).
// Returns ErrUnknownParameter if the parameter doesn't exist.
func (r *ToolRequest) Object(name string) (map[string]any, error) {
	val, ok := r.args[name]
	if !ok {
		return nil, ErrUnknownParameter
	}
	if obj, ok := val.(map[string]any); ok {
		return obj, nil
	}
	return nil, fmt.Errorf("parameter '%s' is not an object", name)
}

// ObjectOr returns a parameter as an object or the default value
func (r *ToolRequest) ObjectOr(name string, defaultValue map[string]any) map[string]any {
	if val, err := r.Object(name); err == nil {
		return val
	}
	return defaultValue
}

// ObjectSlice returns a parameter as a slice of objects
func (r *ToolRequest) ObjectSlice(name string) ([]map[string]any, error) {
	val, ok := r.args[name]
	if !ok {
		return nil, ErrUnknownParameter
	}
	if arr, ok := val.([]any); ok {
		result := make([]map[string]any, len(arr))
		for i, item := range arr {
			if obj, ok := item.(map[string]any); ok {
				result[i] = obj
			} else {
				return nil, fmt.Errorf("parameter '%s' contains non-object element at index %d", name, i)
			}
		}
		return result, nil
	}
	return nil, fmt.Errorf("parameter '%s' is not an array", name)
}

// ObjectSliceOr returns a parameter as a slice of objects or the default value
func (r *ToolRequest) ObjectSliceOr(name string, defaultValue []map[string]any) []map[string]any {
	if val, err := r.ObjectSlice(name); err == nil {
		return val
	}
	return defaultValue
}

// GetObjectProperty extracts a property from an object parameter
// ObjectProperty extracts a property from an object parameter.
func (r *ToolRequest) ObjectProperty(objectName, propertyName string) (any, error) {
	obj, err := r.Object(objectName)
	if err != nil {
		return nil, err
	}
	val, ok := obj[propertyName]
	if !ok {
		return nil, fmt.Errorf("property '%s' not found in object '%s'", propertyName, objectName)
	}
	return val, nil
}

// ObjectString extracts a string property from an object parameter.
func (r *ToolRequest) ObjectString(objectName, propertyName string) (string, error) {
	val, err := r.ObjectProperty(objectName, propertyName)
	if err != nil {
		return "", err
	}
	if str, ok := val.(string); ok {
		return str, nil
	}
	return "", fmt.Errorf("property '%s' in object '%s' is not a string", propertyName, objectName)
}

// ObjectStringOr extracts a string property from an object parameter, returning
// defaultValue when the object or property is missing or not a string.
func (r *ToolRequest) ObjectStringOr(objectName, propertyName, defaultValue string) string {
	val, err := r.ObjectString(objectName, propertyName)
	if err != nil {
		return defaultValue
	}
	return val
}

// ObjectInt extracts an int property from an object parameter.
func (r *ToolRequest) ObjectInt(objectName, propertyName string) (int, error) {
	val, err := r.ObjectProperty(objectName, propertyName)
	if err != nil {
		return 0, err
	}
	switch v := val.(type) {
	case int:
		return v, nil
	case float64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("property '%s' in object '%s' is not a number", propertyName, objectName)
	}
}

// ObjectIntOr extracts an int property from an object parameter, returning
// defaultValue when the object or property is missing or not a number.
func (r *ToolRequest) ObjectIntOr(objectName, propertyName string, defaultValue int) int {
	val, err := r.ObjectInt(objectName, propertyName)
	if err != nil {
		return defaultValue
	}
	return val
}

// ObjectBool extracts a bool property from an object parameter.
func (r *ToolRequest) ObjectBool(objectName, propertyName string) (bool, error) {
	val, err := r.ObjectProperty(objectName, propertyName)
	if err != nil {
		return false, err
	}
	if b, ok := val.(bool); ok {
		return b, nil
	}
	return false, fmt.Errorf("property '%s' in object '%s' is not a boolean", propertyName, objectName)
}

// ObjectBoolOr extracts a bool property from an object parameter, returning
// defaultValue when the object or property is missing or not a boolean.
func (r *ToolRequest) ObjectBoolOr(objectName, propertyName string, defaultValue bool) bool {
	val, err := r.ObjectBool(objectName, propertyName)
	if err != nil {
		return defaultValue
	}
	return val
}

// Args returns all arguments as a map
func (r *ToolRequest) Args() map[string]any {
	return r.args
}
