package fuego

import (
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
)

func NewOpenAPI() *OpenAPI {
	desc := NewOpenApiSpec()
	openAPI := &OpenAPI{
		description:            &desc,
		globalOpenAPIResponses: []openAPIResponse{},
		Config:                 defaultOpenAPIConfig,
		enumRegistry:           make(map[string]string),
	}
	// Create generator with schema customizer that has access to enumRegistry
	openAPI.generator = openapi3gen.NewGenerator(
		openapi3gen.SchemaCustomizer(openAPI.schemaCustomizerWithEnumRegistry),
		openapi3gen.CreateComponentSchemas(openapi3gen.ExportComponentSchemasOptions{
			ExportComponentSchemas: true,
			ExportTopLevelSchema:   false,
		}),
	)
	return openAPI
}

// OpenAPI holds the OpenAPI OpenAPIDescription (OAD) and OpenAPI capabilities.
type OpenAPI struct {
	description            *openapi3.T
	generator              *openapi3gen.Generator
	globalOpenAPIResponses []openAPIResponse
	Config                 OpenAPIConfig
	// enumRegistry maps enum value signatures to their type names for post-processing
	enumRegistry map[string]string
}

func (openAPI *OpenAPI) Description() *openapi3.T {
	return openAPI.description
}

func (openAPI *OpenAPI) Generator() *openapi3gen.Generator {
	return openAPI.generator
}

// Sets the openapi generator with a custom schema customizer function.
func (openAPI *OpenAPI) SetGeneratorSchemaCustomizer(sc openapi3gen.SchemaCustomizerFn) {
	// Create a function with the default schema customizer, and one with the provided, thereby merging the two.
	customizerFn := func(name string, t reflect.Type, tag reflect.StructTag, schema *openapi3.Schema) error {
		if err := SchemaCustomizer(name, t, tag, schema); err != nil {
			return err
		}
		return sc(name, t, tag, schema)
	}
	openAPI.generator = openapi3gen.NewGenerator(
		openapi3gen.SchemaCustomizer(customizerFn),
		openapi3gen.CreateComponentSchemas(openapi3gen.ExportComponentSchemasOptions{
			ExportComponentSchemas: true,
			ExportTopLevelSchema:   false,
		}),
	)
}

func (openAPI *OpenAPI) mergeInfo(info *openapi3.Info) {
	if info.Title == "" {
		info.Title = openAPI.description.Info.Title
	}
	if info.Description == "" {
		info.Description = openAPI.description.Info.Description
	}
	if info.Version == "" {
		info.Version = openAPI.description.Info.Version
	}
	openAPI.description.Info = info
}

// Compute the tags to declare at the root of the OpenAPI spec from the tags declared in the operations.
func (openAPI *OpenAPI) computeTags() {
	for _, pathItem := range openAPI.Description().Paths.Map() {
		for _, op := range pathItem.Operations() {
			for _, tag := range op.Tags {
				if openAPI.Description().Tags.Get(tag) == nil {
					openAPI.Description().Tags = append(openAPI.Description().Tags, &openapi3.Tag{
						Name: tag,
					})
				}
			}
		}
	}

	// Make sure tags are sorted
	slices.SortFunc(openAPI.Description().Tags, func(a, b *openapi3.Tag) int {
		return strings.Compare(a.Name, b.Name)
	})
}

func NewOpenApiSpec() openapi3.T {
	return openapi3.T{
		OpenAPI:  "3.1.0",
		Info:     defaultOpenAPIConfig.Info,
		Paths:    &openapi3.Paths{},
		Servers:  []*openapi3.Server{},
		Security: openapi3.SecurityRequirements{},
		Components: &openapi3.Components{
			Schemas:       make(map[string]*openapi3.SchemaRef),
			RequestBodies: make(map[string]*openapi3.RequestBodyRef),
			Responses:     make(map[string]*openapi3.ResponseRef),
		},
	}
}

type OpenAPIServable interface {
	SpecHandler(e *Engine)
	UIHandler(e *Engine)
}

func (e *Engine) RegisterOpenAPIRoutes(o OpenAPIServable) {
	if e.OpenAPI.Config.Disabled {
		return
	}
	o.SpecHandler(e)

	if e.OpenAPI.Config.DisableSwaggerUI {
		return
	}
	o.UIHandler(e)
}

// Hide prevents the routes in this server or group from being included in the OpenAPI spec.
//
// Deprecated: Please use [OptionHide] with [WithRouteOptions]
func (s *Server) Hide() *Server {
	WithRouteOptions(
		OptionHide(),
	)(s)
	return s
}

// Show allows displaying the routes. Activated by default so useless in most cases,
// but this can be useful if you deactivated the parent group.
//
// Deprecated: Please use [OptionShow] with [WithRouteOptions]
func (s *Server) Show() *Server {
	WithRouteOptions(
		OptionShow(),
	)(s)
	return s
}

func validateSpecURL(specURL string) bool {
	specURLRegexp := regexp.MustCompile(`^\/[\/a-zA-Z0-9\-\_\.]+$`)
	return specURLRegexp.MatchString(specURL)
}

func validateSwaggerURL(swaggerURL string) bool {
	swaggerURLRegexp := regexp.MustCompile(`^\/[\/a-zA-Z0-9\-\_]+[a-zA-Z0-9\-\_]$`)
	return swaggerURLRegexp.MatchString(swaggerURL)
}

// RegisterOpenAPIOperation registers the route to the OpenAPI description.
// Modifies the route's Operation.
func (route *Route[ResponseBody, RequestBody, Params]) RegisterOpenAPIOperation(openapi *OpenAPI) error {
	if route.Hidden || route.Method == "" {
		return nil
	}
	route.MiddlewareConfig = &openapi.Config.MiddlewareConfig
	operation, err := RegisterOpenAPIOperation(openapi, *route)
	route.Operation = operation
	return err
}

// RegisterOpenAPIOperation registers an OpenAPI operation.
//
// Deprecated: Use `(*Route[ResponseBody, RequestBody]).RegisterOpenAPIOperation` instead.
func RegisterOpenAPIOperation[T, B, P any](openapi *OpenAPI, route Route[T, B, P]) (*openapi3.Operation, error) {
	if route.Operation == nil {
		route.Operation = openapi3.NewOperation()
	}

	if route.FullName == "" {
		route.FullName = route.Path
	}

	route.GenerateDefaultDescription()

	if route.Operation.Summary == "" {
		route.Operation.Summary = route.NameFromNamespace(camelToHuman)
	}

	if route.Operation.OperationID == "" {
		route.GenerateDefaultOperationID()
	}

	// Request Body
	if route.Operation.RequestBody == nil {
		bodyTag := SchemaTagFromType(openapi, *new(B))

		if bodyTag.Name != "unknown-interface" {
			requestBody := newRequestBody[B](bodyTag, route.RequestContentTypes)

			// add request body to operation
			route.Operation.RequestBody = &openapi3.RequestBodyRef{
				Value: requestBody,
			}
		}
	}

	// Response - globals
	for _, openAPIGlobalResponse := range openapi.globalOpenAPIResponses {
		addResponseIfNotSet(
			openapi,
			route.Operation,
			openAPIGlobalResponse.Code,
			openAPIGlobalResponse.Description,
			openAPIGlobalResponse.Response,
		)
	}

	// Automatically add non-declared 200 (or other) Response
	if route.DefaultStatusCode == 0 {
		route.DefaultStatusCode = 200
	}
	defaultStatusCode := strconv.Itoa(route.DefaultStatusCode)
	responseDefault := route.Operation.Responses.Value(defaultStatusCode)
	if responseDefault == nil {
		response := openapi3.NewResponse().WithDescription(http.StatusText(route.DefaultStatusCode))
		route.Operation.AddResponse(route.DefaultStatusCode, response)
		responseDefault = route.Operation.Responses.Value(defaultStatusCode)
	}

	// Automatically add non-declared Content for 200 (or other) Response
	if responseDefault.Value.Content == nil {
		responseSchema := SchemaTagFromType(openapi, *new(T))
		content := openapi3.NewContentWithSchemaRef(&responseSchema.SchemaRef, route.ResponseContentTypes)
		responseDefault.Value.WithContent(content)
	}

	// Automatically add non-declared Path parameters
	for _, pathParam := range parsePathParams(route.Path) {
		if exists := route.Operation.Parameters.GetByInAndName("path", pathParam); exists != nil {
			continue
		}
		parameter := openapi3.NewPathParameter(pathParam)
		parameter.Schema = openapi3.NewStringSchema().NewRef()
		if strings.HasSuffix(pathParam, "...") {
			parameter.Description += " (might contain slashes)"
		}

		route.Operation.AddParameter(parameter)
	}
	for _, params := range route.Operation.Parameters {
		if params.Value.In == "path" {
			if !strings.Contains(route.Path, "{"+params.Value.Name) {
				panic(fmt.Errorf("path parameter '%s' is declared in OpenAPI but not on the route", params.Value.Name))
			}
		}
	}

	err := route.RegisterParams()
	if err != nil {
		return nil, err
	}

	openapi.Description().AddOperation(route.Path, route.Method, route.Operation)

	return route.Operation, nil
}

// RegisterParams registers the parameters of a given type to an OpenAPI operation.
// It inspects the fields of the provided struct, looking for "header" tags, and creates
// OpenAPI parameters for each tagged field.
func (route *Route[ResponseBody, RequestBody, Params]) RegisterParams() error {
	if route.Operation == nil {
		route.Operation = openapi3.NewOperation()
	}
	params := *new(Params)
	typeOfParams := reflect.TypeOf(params)
	if typeOfParams == nil {
		return nil
	}
	if typeOfParams.Kind() == reflect.Pointer {
		typeOfParams = typeOfParams.Elem()
	}

	if typeOfParams.Kind() == reflect.Struct {
		for i := range typeOfParams.NumField() {
			field := typeOfParams.Field(i)
			var params []ParamOption
			example, _ := field.Tag.Lookup("description")
			if example != "" {
				params = append(params, ParamExample("example", example))
			}

			// Parse default tag
			if defaultValue, ok := field.Tag.Lookup("default"); ok && defaultValue != "" {
				var parsedDefault any
				var err error

				// Handle array/slice types
				if field.Type.Kind() == reflect.Slice || field.Type.Kind() == reflect.Array {
					parsedDefault, err = parseDefaultValueArray(defaultValue, field.Type.Elem().Kind())
				} else {
					parsedDefault, err = parseDefaultValue(defaultValue, field.Type.Kind())
				}

				if err != nil {
					return fmt.Errorf("invalid default value for field %s: %w", field.Name, err)
				}

				params = append(params, ParamDefault(parsedDefault))
			}

			description, _ := field.Tag.Lookup("description")
			if headerKey, ok := field.Tag.Lookup("header"); ok {
				OptionHeader(headerKey, description, params...)(&route.BaseRoute)
			}
			if queryKey, ok := field.Tag.Lookup("query"); ok {
				switch field.Type.Kind() {
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
					reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
					reflect.Float32, reflect.Float64:
					OptionQueryInt(queryKey, description, params...)(&route.BaseRoute)

				case reflect.Bool:
					OptionQueryBool(queryKey, description, params...)(&route.BaseRoute)
				case reflect.String:
					OptionQuery(queryKey, description, params...)(&route.BaseRoute)
				case reflect.Slice, reflect.Array:
					OptionQueryArray(queryKey, description, field.Type.Elem().Kind(), params...)(&route.BaseRoute)
				}
			}
			if cookieKey, ok := field.Tag.Lookup("cookie"); ok {
				OptionCookie(cookieKey, description, params...)(&route.BaseRoute)
			}
		}
	}

	return nil
}

// parseDefaultValue converts a string default value to the appropriate Go type
// based on the field's kind. Returns an error if conversion fails.
// For OpenAPI parameters, all integer types are normalized to int for validation.
func parseDefaultValue(defaultStr string, kind reflect.Kind) (any, error) {
	switch kind {
	case reflect.String:
		return defaultStr, nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		intValue, err := strconv.ParseInt(defaultStr, 10, bitSize(kind))
		if err != nil {
			return nil, fmt.Errorf("cannot convert %s to %s: %w", defaultStr, kind, err)
		}
		// OpenAPI validation expects int type for all integer parameters
		return int(intValue), nil

	case reflect.Float32, reflect.Float64:
		floatValue, err := strconv.ParseFloat(defaultStr, bitSize(kind))
		if err != nil {
			return nil, fmt.Errorf("cannot convert %s to %s: %w", defaultStr, kind, err)
		}
		// Return as float64 for OpenAPI
		return floatValue, nil

	case reflect.Bool:
		boolValue, err := strconv.ParseBool(defaultStr)
		if err != nil {
			return nil, fmt.Errorf("cannot convert %s to bool: %w", defaultStr, err)
		}
		return boolValue, nil

	default:
		return nil, fmt.Errorf("unsupported type %s for default value", kind)
	}
}

// parseDefaultValueArray validates and returns an array default for array parameters.
// For OpenAPI compliance, array defaults must be actual arrays, not comma-separated strings.
func parseDefaultValueArray(defaultStr string, elemKind reflect.Kind) (any, error) {
	if defaultStr == "" {
		return []any{}, nil
	}

	// Parse each element and build an array
	parts := strings.Split(defaultStr, ",")
	result := make([]any, 0, len(parts))

	for i, part := range parts {
		trimmed := strings.TrimSpace(part)
		parsed, err := parseDefaultValue(trimmed, elemKind)
		if err != nil {
			return nil, fmt.Errorf("invalid array element at index %d: %w", i, err)
		}
		result = append(result, parsed)
	}

	return result, nil
}

func newRequestBody[RequestBody any](tag SchemaTag, consumes []string) *openapi3.RequestBody {
	content := openapi3.NewContentWithSchemaRef(&tag.SchemaRef, consumes)
	return openapi3.NewRequestBody().
		WithRequired(true).
		WithDescription("Request body for " + reflect.TypeOf(*new(RequestBody)).String()).
		WithContent(content)
}

// SchemaTag is a struct that holds the name of the struct and the associated openapi3.SchemaRef
type SchemaTag struct {
	openapi3.SchemaRef
	Name string
}

func SchemaTagFromType(openapi *OpenAPI, v any) SchemaTag {
	if v == nil {
		// ensure we add unknown-interface to our schemas
		schema := openapi.getOrCreateSchema("unknown-interface", struct{}{})
		return SchemaTag{
			Name: "unknown-interface",
			SchemaRef: openapi3.SchemaRef{
				Ref:   "#/components/schemas/unknown-interface",
				Value: schema,
			},
		}
	}

	return dive(openapi, reflect.TypeOf(v), SchemaTag{}, 5)
}

// dive returns a schemaTag which includes the generated openapi3.SchemaRef and
// the name of the struct being passed in.
// If the type is a pointer, map, channel, function, or unsafe pointer,
// it will dive into the type and return the name of the type it points to.
// If the type is a slice or array type it will dive into the type as well as
// build and openapi3.Schema where Type is array and Ref is set to the proper
// components Schema
func dive(openapi *OpenAPI, t reflect.Type, tag SchemaTag, maxDepth int) SchemaTag {
	if maxDepth == 0 {
		return SchemaTag{
			Name: "default",
			SchemaRef: openapi3.SchemaRef{
				Ref: "#/components/schemas/default",
			},
		}
	}

	switch t.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return dive(openapi, t.Elem(), tag, maxDepth-1)

	case reflect.Slice, reflect.Array:
		item := dive(openapi, t.Elem(), tag, maxDepth-1)
		tag.Name = item.Name
		tag.Value = openapi3.NewArraySchema()
		tag.Value.Items = &item.SchemaRef
		return tag

	default:
		tag.Name = transformTypeName(t.Name())
		if t.Kind() == reflect.Struct && strings.HasPrefix(tag.Name, "DataOrTemplate") {
			return dive(openapi, t.Field(0).Type, tag, maxDepth-1)
		}

		// Check if type implements EnumValuer - if so, register as component schema
		if isEnumValuer(t) {
			tag.Ref = "#/components/schemas/" + tag.Name
			tag.Value = openapi.getOrCreateEnumSchema(tag.Name, t)
			return tag
		}

		tag.Ref = "#/components/schemas/" + tag.Name
		tag.Value = openapi.getOrCreateSchema(tag.Name, reflect.New(t).Interface())

		return tag
	}
}

// isEnumValuer checks if a type implements the EnumValuer interface
func isEnumValuer(t reflect.Type) bool {
	enumValuerType := reflect.TypeOf((*EnumValuer)(nil)).Elem()
	return reflect.PointerTo(t).Implements(enumValuerType) || t.Implements(enumValuerType)
}

// getOrCreateEnumSchema creates a schema for enum types that implement EnumValuer
func (openAPI *OpenAPI) getOrCreateEnumSchema(key string, t reflect.Type) *openapi3.Schema {
	schemaRef, ok := openAPI.Description().Components.Schemas[key]
	if ok {
		return schemaRef.Value
	}

	// Create the schema based on the underlying type
	schema := &openapi3.Schema{
		Description: key + " enum",
	}

	// Set the type based on the underlying kind
	switch t.Kind() {
	case reflect.String:
		schema.Type = &openapi3.Types{openapi3.TypeString}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		schema.Type = &openapi3.Types{openapi3.TypeInteger}
	default:
		schema.Type = &openapi3.Types{openapi3.TypeString}
	}

	// Get enum values from the EnumValuer interface
	enumValuerType := reflect.TypeOf((*EnumValuer)(nil)).Elem()
	if reflect.PointerTo(t).Implements(enumValuerType) {
		instance := reflect.New(t).Interface().(EnumValuer)
		schema.Enum = instance.EnumValues()
	} else if t.Implements(enumValuerType) {
		instance := reflect.Zero(t).Interface().(EnumValuer)
		schema.Enum = instance.EnumValues()
	}

	openAPI.Description().Components.Schemas[key] = &openapi3.SchemaRef{Value: schema}
	return schema
}

// getOrCreateSchema is used to get a schema from the OpenAPI spec.
// If the schema does not exist, it will create a new schema and add it to the OpenAPI spec.
func (openAPI *OpenAPI) getOrCreateSchema(key string, v any) *openapi3.Schema {
	schemaRef, ok := openAPI.Description().Components.Schemas[key]
	if !ok {
		schemaRef = openAPI.createSchema(key, v)
	}
	return schemaRef.Value
}

// createSchema is used to create a new schema and add it to the OpenAPI spec.
// Relies on the openapi3gen package to generate the schema, and adds custom struct tags.
func (openAPI *OpenAPI) createSchema(key string, v any) *openapi3.SchemaRef {
	schemaRef, err := openAPI.Generator().NewSchemaRefForValue(v, openAPI.Description().Components.Schemas)
	if err != nil {
		slog.Error("Error generating schema", "key", key, "error", err)
	}
	schemaRef.Value.Description = key + " schema"

	descriptionable, ok := v.(OpenAPIDescriptioner)
	if ok {
		schemaRef.Value.Description = descriptionable.Description()
	}

	openAPI.Description().Components.Schemas[key] = schemaRef

	return schemaRef
}

type OpenAPIDescriptioner interface {
	Description() string
}

// Transform the type name to a more readable & valid OpenAPI 3 format.
// Useful for generics.
// Example: "BareSuccessResponse[github.com/go-fuego/fuego/examples/petstore/models.Pets]" -> "BareSuccessResponse_models.Pets"
func transformTypeName(s string) string {
	// Find the positions of the '[' and ']'
	start := strings.Index(s, "[")
	if start == -1 {
		return s
	}
	end := strings.Index(s, "]")
	if end == -1 {
		return s
	}

	prefix := s[:start]

	inside := s[start+1 : end]

	lastSlash := strings.LastIndex(inside, "/")
	if lastSlash != -1 {
		inside = inside[lastSlash+1:]
	}

	return prefix + "_" + inside
}

// schemaCustomizerWithEnumRegistry wraps SchemaCustomizer and registers enum types
func (openAPI *OpenAPI) schemaCustomizerWithEnumRegistry(name string, t reflect.Type, tag reflect.StructTag, schema *openapi3.Schema) error {
	// First apply the standard schema customizer
	if err := SchemaCustomizer(name, t, tag, schema); err != nil {
		return err
	}

	// If this is an enum type, register it for later extraction
	if len(schema.Enum) > 0 && t.Name() != "" {
		typeName := t.Name()
		signature := enumSignature(schema.Enum)
		openAPI.enumRegistry[signature] = typeName
	}

	return nil
}

// enumSignature creates a unique signature from enum values for matching
func enumSignature(values []any) string {
	strs := make([]string, len(values))
	for i, v := range values {
		strs[i] = fmt.Sprintf("%v", v)
	}
	slices.Sort(strs)
	return strings.Join(strs, "|")
}

// extractEnumSchemas finds inline enum definitions and extracts them to component schemas
func (openAPI *OpenAPI) extractEnumSchemas() {
	desc := openAPI.Description()

	// Process all component schemas to find inline enums in properties
	for _, schemaRef := range desc.Components.Schemas {
		if schemaRef.Value == nil || schemaRef.Value.Properties == nil {
			continue
		}
		openAPI.processPropertiesForEnums(schemaRef.Value.Properties)
	}
}

// processPropertiesForEnums recursively processes schema properties to extract enums
func (openAPI *OpenAPI) processPropertiesForEnums(properties map[string]*openapi3.SchemaRef) {
	for propName, propRef := range properties {
		if propRef.Value == nil {
			continue
		}

		prop := propRef.Value

		// If property has enum values and we have a registered type for it
		if len(prop.Enum) > 0 {
			signature := enumSignature(prop.Enum)
			if typeName, ok := openAPI.enumRegistry[signature]; ok {
				// Create or get the enum component schema
				openAPI.ensureEnumComponentSchema(typeName, prop)

				// Replace inline enum with $ref
				properties[propName] = &openapi3.SchemaRef{
					Ref: "#/components/schemas/" + typeName,
				}
				continue
			}
		}

		// Recursively process nested properties
		if prop.Properties != nil {
			openAPI.processPropertiesForEnums(prop.Properties)
		}

		// Process array items
		if prop.Items != nil && prop.Items.Value != nil {
			if prop.Items.Value.Properties != nil {
				openAPI.processPropertiesForEnums(prop.Items.Value.Properties)
			}
			// Check if array items are enums
			if len(prop.Items.Value.Enum) > 0 {
				signature := enumSignature(prop.Items.Value.Enum)
				if typeName, ok := openAPI.enumRegistry[signature]; ok {
					openAPI.ensureEnumComponentSchema(typeName, prop.Items.Value)
					prop.Items = &openapi3.SchemaRef{
						Ref: "#/components/schemas/" + typeName,
					}
				}
			}
		}

		// Process allOf, oneOf, anyOf
		for _, subSchema := range prop.AllOf {
			if subSchema.Value != nil && subSchema.Value.Properties != nil {
				openAPI.processPropertiesForEnums(subSchema.Value.Properties)
			}
		}
		for _, subSchema := range prop.OneOf {
			if subSchema.Value != nil && subSchema.Value.Properties != nil {
				openAPI.processPropertiesForEnums(subSchema.Value.Properties)
			}
		}
		for _, subSchema := range prop.AnyOf {
			if subSchema.Value != nil && subSchema.Value.Properties != nil {
				openAPI.processPropertiesForEnums(subSchema.Value.Properties)
			}
		}
	}
}

// ensureEnumComponentSchema creates an enum component schema if it doesn't exist
func (openAPI *OpenAPI) ensureEnumComponentSchema(name string, sourceSchema *openapi3.Schema) {
	if _, exists := openAPI.Description().Components.Schemas[name]; exists {
		return
	}

	schema := &openapi3.Schema{
		Type:        sourceSchema.Type,
		Enum:        sourceSchema.Enum,
		Description: name + " enum",
	}

	openAPI.Description().Components.Schemas[name] = &openapi3.SchemaRef{Value: schema}
}
