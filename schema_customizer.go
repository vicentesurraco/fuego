package fuego

import (
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/getkin/kin-openapi/openapi3"
)

// EnumValuer is implemented by types that want to expose their enum values for OpenAPI.
// Types implementing this interface will automatically have their enum values included
// in the generated OpenAPI schema.
type EnumValuer interface {
	EnumValues() []any
}

// parseValidate parses the values of the validate tag
// It adds the following struct tags (tag => OpenAPI schema field):
// - validate:
//   - min=1 => min=1 (for integers)
//   - min=1 => minLength=1 (for strings)
//   - max=100 => max=100 (for integers)
//   - max=100 => maxLength=100 (for strings)
//   - oneof=A B C => enum: [A, B, C]
func parseValidate(tag reflect.StructTag, schema *openapi3.Schema) {
	validateTag, ok := tag.Lookup("validate")
	if !ok {
		return
	}

	validateTags := strings.SplitSeq(validateTag, ",")
	for validateTag := range validateTags {
		if strings.HasPrefix(validateTag, "min=") {
			minValue, err := strconv.Atoi(strings.Split(validateTag, "=")[1])
			if err != nil {
				slog.Warn("Min might be incorrect (should be integer)", "error", err)
			}

			if schema.Type.Is(openapi3.TypeInteger) {
				minPtr := float64(minValue)
				schema.Min = &minPtr
			} else if schema.Type.Is(openapi3.TypeString) {
				//nolint:gosec // disable G115
				schema.MinLength = uint64(minValue)
			}
		}
		if strings.HasPrefix(validateTag, "max=") {
			maxValue, err := strconv.Atoi(strings.Split(validateTag, "=")[1])
			if err != nil {
				slog.Warn("Max might be incorrect (should be integer)", "error", err)
			}
			if schema.Type.Is(openapi3.TypeInteger) {
				maxPtr := float64(maxValue)
				schema.Max = &maxPtr
			} else if schema.Type.Is(openapi3.TypeString) {
				//nolint:gosec // disable G115
				maxPtr := uint64(maxValue)
				schema.MaxLength = &maxPtr
			}
		}
		// Parse oneof validation tag for enum values
		if strings.HasPrefix(validateTag, "oneof=") {
			enumValues := strings.Split(strings.TrimPrefix(validateTag, "oneof="), " ")
			for _, v := range enumValues {
				if v != "" {
					schema.Enum = append(schema.Enum, v)
				}
			}
		}
	}
}

// determineRequired takes a reflect.Type and a schema,
// and determines which fields should be marked as required.
// It checks for fields that either:
// - Don't have the `omitempty` JSON tag
// - Have the `required` validation tag
func determineRequired(t reflect.Type, schema *openapi3.Schema) {
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := f.Name
		// skip if it's a private field
		firstRune, _ := utf8.DecodeRuneInString(name)
		if unicode.IsLower(firstRune) {
			continue
		}

		jsonTag := f.Tag.Get("json")
		parts := strings.Split(jsonTag, ",")
		if parts[0] != "" {
			name = parts[0]
		}
		if name == "-" {
			continue
		}

		if !strings.Contains(jsonTag, ",omitempty") || slices.Contains(strings.Split(f.Tag.Get("validate"), ","), "required") {
			schema.Required = append(schema.Required, name)
		}
	}
	sort.Strings(schema.Required)
}

// parseExample parses the "example" tag and sets the schema example.
// If the example type does not match the type of the property, it will log a warning.
func parseExample(tag reflect.StructTag, schema *openapi3.Schema) {
	example, ok := tag.Lookup("example")
	if !ok {
		return
	}

	switch {
	case schema.Type.Is(openapi3.TypeInteger):
		exNum, err := strconv.Atoi(example)
		if err != nil {
			slog.Warn("Example might be incorrect (should be integer)", "error", err)
		}
		schema.Example = exNum
	case schema.Type.Is(openapi3.TypeNumber):
		exNum, err := strconv.ParseFloat(example, 64)
		if err != nil {
			slog.Warn("Example might be incorrect (should be floating point number)", "error", err)
		}
		schema.Example = exNum
	case schema.Type.Is(openapi3.TypeBoolean):
		exBool, err := strconv.ParseBool(example)
		if err != nil {
			slog.Warn("Example might be incorrect (should be boolean)", "error", err)
		}
		schema.Example = exBool
	default:
		schema.Example = example
	}
}

// parseDescriptions parses the "description" tag and adds it to the schema description.
func parseDescription(tag reflect.StructTag, schema *openapi3.Schema) {
	description, ok := tag.Lookup("description")
	if ok {
		schema.Description = description
	}
}

// parseEnum parses the "enum" tag and sets the schema enum values.
// Format: enum:"VALUE1,VALUE2,VALUE3"
func parseEnum(tag reflect.StructTag, schema *openapi3.Schema) {
	enumTag, ok := tag.Lookup("enum")
	if !ok {
		return
	}

	enumValues := strings.Split(enumTag, ",")
	for _, v := range enumValues {
		v = strings.TrimSpace(v)
		if v != "" {
			schema.Enum = append(schema.Enum, v)
		}
	}
}

// SchemaCustomizer parses struct tags and modifies the schema using kin-openapi3gen's
// schema customization functionality.
// It adds the following struct tags (tag => OpenAPI schema field):
// - description => description
// - example => example
// - enum => enum (comma-separated values)
// - json => nullable (if contains omitempty)
// - validate:
//   - required => required
//   - min=1 => min=1 (for integers)
//   - min=1 => minLength=1 (for strings)
//   - max=100 => max=100 (for integers)
//   - max=100 => maxLength=100 (for strings)
//   - oneof=A B C => enum: [A, B, C]
//
// Additionally, if a type implements EnumValuer, its EnumValues() method will be called
// to automatically populate the enum values in the schema.
func SchemaCustomizer(name string, t reflect.Type, tag reflect.StructTag, schema *openapi3.Schema) error {
	// Example
	parseExample(tag, schema)

	// Validation (includes oneof enum parsing)
	parseValidate(tag, schema)

	// Description
	parseDescription(tag, schema)

	// Enum (explicit enum tag, only if not already set by validate oneof)
	if len(schema.Enum) == 0 {
		parseEnum(tag, schema)
	}

	// Check if the type implements EnumValuer (only if enum not already set)
	if len(schema.Enum) == 0 {
		parseEnumValuer(t, schema)
	}

	// After we are done parsing tags, get the required tags
	determineRequired(t, schema)

	return nil
}

// parseEnumValuer checks if the type implements EnumValuer and populates enum values.
func parseEnumValuer(t reflect.Type, schema *openapi3.Schema) {
	enumType := t
	if enumType.Kind() == reflect.Ptr {
		enumType = enumType.Elem()
	}

	enumValuerType := reflect.TypeOf((*EnumValuer)(nil)).Elem()

	// Check if the type or pointer to type implements EnumValuer
	if reflect.PointerTo(enumType).Implements(enumValuerType) {
		instance := reflect.New(enumType).Interface().(EnumValuer)
		schema.Enum = instance.EnumValues()
	} else if enumType.Implements(enumValuerType) {
		instance := reflect.Zero(enumType).Interface().(EnumValuer)
		schema.Enum = instance.EnumValues()
	}
}
