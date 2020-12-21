package mapper

import (
	"fmt"

	"os"
	"strings"

	"github.com/apalchys/protoc-gen-graphql/descriptor"
	"github.com/apalchys/protoc-gen-graphql/graphql"
	pb "github.com/golang/protobuf/protoc-gen-go/descriptor"
	"github.com/golang/protobuf/protoc-gen-go/generator"
)

type Mapper struct {
	FilePbs []*pb.FileDescriptorProto

	Params                *Parameters
	FieldNameTransformer  func(string) string
	MethodNameTransformer func(string) string

	// Maps file names to descriptors.
	Files map[string]*descriptor.File
	// Maps protobuf types to descriptors.
	Messages map[string]*descriptor.Message
	Enums    map[string]*descriptor.Enum
	// Maps protobuf types to its method loader.
	Loaders map[string]*descriptor.Loader

	// Maps protobuf messages and enums to graphql type names.
	ObjectNames map[string]string
	InputNames  map[string]string

	// Maps protobuf types to graphql types.
	MessageMappers map[string]*MessageMapper
	EnumMappers    map[string]*EnumMapper
	ServiceMappers map[string]*ServiceMapper
}

type MessageMapper struct {
	Descriptor *descriptor.Message
	Empty      bool
	Object     *graphql.Object
	Input      *graphql.Input
	Oneofs     []*OneofMapper
}

type OneofMapper struct {
	Descriptor *descriptor.Oneof
	Union      *graphql.Union
	Objects    []*graphql.Object
	Input      *graphql.Input
}

type EnumMapper struct {
	Descriptor *descriptor.Enum
	Enum       *graphql.Enum
}

type ServiceMapper struct {
	Descriptor    *descriptor.Service
	ReferenceName string
	// All methods in service.
	Methods *MethodsMapper
	// Methods that have opted into each operation.
	Queries       *MethodsMapper
	Mutations     *MethodsMapper
	Subscriptions *MethodsMapper
}

type MethodsMapper struct {
	Methods          []*descriptor.Method
	ExtendRootObject *graphql.ExtendObject
	Object           *graphql.Object
}

// New creates a new Mapper with all mappings populated from the provided file
// descriptors. The provided file descriptors must be in topological order.
func New(filePbs []*pb.FileDescriptorProto, params *Parameters) *Mapper {
	m := &Mapper{
		FilePbs: filePbs,
		Params:  params,

		Files:    make(map[string]*descriptor.File),
		Messages: make(map[string]*descriptor.Message),
		Enums:    make(map[string]*descriptor.Enum),
		Loaders:  make(map[string]*descriptor.Loader),

		ObjectNames: make(map[string]string),
		InputNames:  make(map[string]string),

		MessageMappers: make(map[string]*MessageMapper),
		EnumMappers:    make(map[string]*EnumMapper),
		ServiceMappers: make(map[string]*ServiceMapper),
	}

	switch params.FieldName {
	case FieldNameDefault, "":
		m.FieldNameTransformer = LowerUnderscoreToLowerCamelTransformer
		m.MethodNameTransformer = UpperCamelToLowerCamelTransformer
	case FieldNamePreserve:
		m.FieldNameTransformer = PreserveTransformer
		m.MethodNameTransformer = PreserveTransformer
	}

	m.buildDescriptorMaps()
	m.buildTypeMaps()
	m.buildTypeLoader()
	m.buildMappers()
	return m
}

func (m *Mapper) buildDescriptorMaps() {
	for _, filePb := range m.FilePbs {
		file := descriptor.WrapFile(filePb)
		m.Files[filePb.GetName()] = file
		for _, enum := range file.Enums {
			m.Enums[enum.FullName] = enum
		}
		for _, message := range file.Messages {
			m.Messages[message.FullName] = message
		}
	}
}

func (m *Mapper) buildTypeMaps() {
	for _, filePb := range m.FilePbs {
		file := m.Files[filePb.GetName()]
		for _, enum := range file.Enums {
			m.ObjectNames[enum.FullName] = m.enumName(enum)
		}

		for _, message := range file.Messages {
			m.buildMessageTypeMaps(message, false)
			m.buildMessageTypeMaps(message, true)
		}
	}
}

func (m *Mapper) buildTypeLoader() {
	for _, file := range m.Files {
		for _, service := range file.Services {
			for _, method := range service.Methods {
				for _, loader := range method.Loaders {
					if m.Messages[loader.FullName] == nil {
						panic(fmt.Sprintf("unknown type for loader: %s", loader.FullName))
					}
					if _, ok := m.Loaders[loader.FullName]; ok {
						panic(fmt.Sprintf("multiple loaders specified for Protobuf type: %s", loader.FullName))
					}
					m.Loaders[loader.FullName] = loader
				}
			}
		}
	}
}

func (m *Mapper) buildMessageTypeMaps(message *descriptor.Message, input bool) {
	nameMap := m.ObjectNames
	if input {
		nameMap = m.InputNames
	}

	if nameMap[message.FullName] != "" {
		return
	}

	nameMap[message.FullName] = m.messageName(message, input)

	for _, field := range message.Proto.GetField() {
		if field.GetType() == pb.FieldDescriptorProto_TYPE_MESSAGE {
			m.buildMessageTypeMaps(m.Messages[field.GetTypeName()], input)
		}
	}
}

func (m *Mapper) buildMappers() {
	for _, filePb := range m.FilePbs {
		file := m.Files[filePb.GetName()]

		// Build enum mapper first as it has no dependencies.
		for _, enum := range file.Enums {
			m.buildEnumMapper(enum)
		}
		for _, message := range file.Messages {
			m.buildMessageMapper(message, false)
		}

		if m.Params.InputMode == InputModeAll {
			for _, message := range file.Messages {
				m.buildMessageMapper(message, true)
			}
		}

		for _, service := range file.Services {
			if m.Params.InputMode == InputModeService {
				for _, method := range service.Proto.GetMethod() {
					m.buildMessageMapper(m.Messages[method.GetInputType()], true)
				}
			}

			// Build service mapper last, after all dependencies are mapped.
			if m.Params.InputMode != InputModeNone {
				m.buildServiceMapper(service)
			}
		}
	}
}

// Do not call buildMessageMapper with the same message and input=false
// after calling it with input=true, otherwise the input objects for
// the oneofs will be overwritten.
func (m *Mapper) buildMessageMapper(message *descriptor.Message, input bool) {
	mapper, ok := m.MessageMappers[message.FullName]
	if ok {
		if (input && mapper.Input != nil) || (!input && mapper.Object != nil) {
			return
		}
	}

	if !ok {
		mapper = &MessageMapper{Descriptor: message}
		m.MessageMappers[message.FullName] = mapper
	}

	if len(message.Fields) == 0 {
		mapper.Empty = true
	}

	typeName := m.ObjectNames[message.FullName]
	mapper.Object = &graphql.Object{
		Name:   typeName,
		Fields: m.graphqlFields(message, false),
	}
	if input {
		typeName = m.InputNames[message.FullName]
		mapper.Input = &graphql.Input{
			Name:   typeName,
			Fields: m.graphqlFields(message, true),
		}
	}

	var oneofMappers []*OneofMapper
	for _, oneof := range message.Oneofs {
		var existedMapper *OneofMapper
		for _, mapper := range mapper.Oneofs {
			if mapper.Descriptor.Proto.GetName() == oneof.Proto.GetName() {
				existedMapper = mapper
			}
		}
		oneofMappers = append(oneofMappers, m.buildOneofMapper(oneof, input, typeName, existedMapper))
	}

	mapper.Oneofs = oneofMappers

	for _, field := range message.Proto.GetField() {
		if field.GetType() == pb.FieldDescriptorProto_TYPE_MESSAGE {
			m.buildMessageMapper(m.Messages[field.GetTypeName()], input)
		}
	}
}

func (m *Mapper) graphqlFields(message *descriptor.Message, input bool) []*graphql.Field {
	var fields []*graphql.Field

	if len(message.Fields) == 0 {
		fields = append(fields, &graphql.Field{
			Name:     "_empty",
			TypeName: graphql.ScalarBoolean.TypeName(),
		})
		return fields
	}

	for _, field := range message.Fields {
		if field.Options.GetSkip() {
			continue
		}

		if field.IsOneof {
			oneofObjectName := field.Name + "Oneof"
			fields = append(fields, &graphql.Field{
				Name: m.fieldName(field),
				TypeName: m.buildGraphqlTypeName(&GraphqlTypeNameParts{
					Namespace: message.File.Options.GetNamespace(),
					Package:   message.Package,
					TypeName:  append(message.TypeName, oneofObjectName),
					Input:     input,
				}),
			})
			continue
		}

		fields = append(fields, m.graphqlField(field, input))

		if field.ForeignKey != nil && !input {
			referencedObjectName, ok := m.ObjectNames[field.ForeignKey.FullName]
			if !ok {
				panic(fmt.Sprintf("unknown type for foreign key: %s", field.Options.GetForeignKey()))
			}

			var modifiers graphql.TypeModifier
			if field.Proto.GetLabel() == pb.FieldDescriptorProto_LABEL_REPEATED {
				modifiers = graphql.TypeModifierList | graphql.TypeModifierNonNull
			}

			fields = append(fields, &graphql.Field{
				Name:      field.ForeignKey.FieldName,
				TypeName:  referencedObjectName,
				Modifiers: modifiers,
			})
		}
	}
	return fields
}

func (m *Mapper) graphqlField(f *descriptor.Field, input bool) *graphql.Field {
	field := &graphql.Field{
		Name: m.fieldName(f),
	}
	if input {
		field.Directives = f.Options.GetInputDirective()
	} else {
		field.Directives = f.Options.GetDirective()
	}
	// @deprecated directive is not supported for input types yet.
	// See: https://github.com/graphql/graphql-spec/pull/525
	if !input && f.Proto.Options.GetDeprecated() {
		field.Directives = append(field.Directives, "deprecated")
	}

	if f.Options.GetType() != "" {
		field.TypeName = f.Options.GetType()
		return field
	}

	proto := f.Proto
	nullableScalars := m.nullableScalars(f, input)

	switch proto.GetType() {
	case pb.FieldDescriptorProto_TYPE_STRING, pb.FieldDescriptorProto_TYPE_BYTES:
		field.TypeName = graphql.ScalarString.TypeName()
		if !nullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_INT32, pb.FieldDescriptorProto_TYPE_UINT32, pb.FieldDescriptorProto_TYPE_SINT32,
		pb.FieldDescriptorProto_TYPE_FIXED32, pb.FieldDescriptorProto_TYPE_SFIXED32:

		field.TypeName = graphql.ScalarInt.TypeName()
		if !nullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_FLOAT, pb.FieldDescriptorProto_TYPE_DOUBLE:

		field.TypeName = graphql.ScalarFloat.TypeName()
		if !nullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_INT64, pb.FieldDescriptorProto_TYPE_UINT64, pb.FieldDescriptorProto_TYPE_SINT64,
		pb.FieldDescriptorProto_TYPE_FIXED64, pb.FieldDescriptorProto_TYPE_SFIXED64:

		if m.Params.JS64BitType == JS64BitTypeString {
			field.TypeName = graphql.ScalarString.TypeName()
		} else {
			field.TypeName = graphql.ScalarFloat.TypeName()
		}
		if !nullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_BOOL:
		field.TypeName = graphql.ScalarBoolean.TypeName()
		if !nullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_ENUM:
		field.TypeName = m.EnumMappers[proto.GetTypeName()].Enum.Name
		if !nullableScalars {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	case pb.FieldDescriptorProto_TYPE_MESSAGE:
		if input {
			field.TypeName = m.InputNames[proto.GetTypeName()]
		} else {
			field.TypeName = m.ObjectNames[proto.GetTypeName()]
		}

		// IsProtoMap elements are non-nullable.
		if m.Messages[proto.GetTypeName()].IsMap {
			field.Modifiers = graphql.TypeModifierNonNull
		}

	default:
		panic(fmt.Sprintf("unexpected protobuf descriptor type: %s", proto.GetType().String()))
	}

	field = m.graphqlSpecialTypes(field, proto.GetTypeName())

	if proto.GetLabel() == pb.FieldDescriptorProto_LABEL_REPEATED {
		field.Modifiers = field.Modifiers | graphql.TypeModifierNonNull | graphql.TypeModifierList
		if !input {
			field.Modifiers = field.Modifiers | graphql.TypeModifierNonNullList
		}
	}

	return field
}

func (m *Mapper) graphqlSpecialTypes(field *graphql.Field, protoTypeName string) *graphql.Field {
	if protoTypeName == ".google.protobuf.Timestamp" && m.Params.TimestampTypeName != "" {
		field.TypeName = m.Params.TimestampTypeName
	}
	if protoTypeName == ".google.protobuf.Duration" && m.Params.DurationTypeName != "" {
		field.TypeName = m.Params.DurationTypeName
	}
	if protoTypeName == ".google.protobuf.Struct" && m.Params.StructTypeName != "" {
		field.TypeName = m.Params.StructTypeName
	}

	if m.Params.WrappersAsNull {
		switch protoTypeName {
		case ".google.protobuf.FloatValue", ".google.protobuf.DoubleValue", ".google.protobuf.UInt32Value":
			field.TypeName = graphql.ScalarFloat.TypeName()
		case ".google.protobuf.StringValue", ".google.protobuf.BytesValue":
			field.TypeName = graphql.ScalarString.TypeName()
		case ".google.protobuf.Int64Value", ".google.protobuf.UInt64Value":
			if m.Params.JS64BitType == JS64BitTypeString {
				field.TypeName = graphql.ScalarString.TypeName()
			} else {
				field.TypeName = graphql.ScalarFloat.TypeName()
			}
		case ".google.protobuf.Int32Value":
			field.TypeName = graphql.ScalarInt.TypeName()
		case ".google.protobuf.BoolValue":
			field.TypeName = graphql.ScalarBoolean.TypeName()
		}
	}

	return field
}

func (m *Mapper) buildOneofMapper(oneof *descriptor.Oneof, input bool, unionTypeName string, existedMapper *OneofMapper) *OneofMapper {
	var mapper *OneofMapper

	if existedMapper != nil {
		mapper = existedMapper
	} else {
		mapper = m.createMapper(oneof, unionTypeName)
	}

	if !input {
		return mapper
	}

	var inputFields []*graphql.Field
	for _, field := range oneof.Fields {
		inputFields = append(inputFields, m.graphqlField(field, true))
	}

	someName := unionTypeName

	mapper.Input = &graphql.Input{
		Name:   someName,
		Fields: inputFields,
	}

	return mapper
}

func (m *Mapper) createMapper(oneof *descriptor.Oneof, unionTypeName string) *OneofMapper {
	oneofObjectName := oneof.Proto.GetName() + "Oneof"
	mapper := &OneofMapper{
		Descriptor: oneof,
		Union: &graphql.Union{
			Name: unionTypeName,
		},
	}

	for _, field := range oneof.Fields {
		typeName := m.buildGraphqlTypeName(&GraphqlTypeNameParts{
			Namespace: oneof.Parent.File.Options.GetNamespace(),
			Package:   oneof.Parent.Package,
			TypeName:  append(oneof.Parent.TypeName, oneofObjectName, field.Name),
		})

		mapper.Union.TypeNames = append(mapper.Union.TypeNames, typeName)
		mapper.Objects = append(mapper.Objects, &graphql.Object{
			Name: typeName,
			Fields: []*graphql.Field{
				// Include _typename field so we can differentiate between messages in a oneof.
				{
					Name:     "_typename",
					TypeName: graphql.ScalarString.TypeName(),
				},
				m.graphqlField(field, false),
			},
		})
	}

	return mapper
}

func (m *Mapper) buildEnumMapper(enum *descriptor.Enum) {
	var enumValues []*graphql.EnumValue
	for _, value := range enum.Values {
		if value.Options.GetSkip() {
			continue
		}

		valueName := value.Proto.GetName()
		if value.Options.GetValue() != "" {
			valueName = value.Options.GetValue()
		}

		enumValue := &graphql.EnumValue{
			Name:       valueName,
			Directives: value.Options.GetDirective(),
		}
		if value.Proto.Options.GetDeprecated() {
			enumValue.Directives = append(enumValue.Directives, "deprecated")
		}
		enumValues = append(enumValues, enumValue)
	}

	m.EnumMappers[enum.FullName] = &EnumMapper{
		Descriptor: enum,
		Enum: &graphql.Enum{
			Name:   m.ObjectNames[enum.FullName],
			Values: enumValues,
		},
	}
}

func (m *Mapper) buildServiceMapper(service *descriptor.Service) {
	var (
		allMethods    = m.buildMethodsMapper(service, "")
		queries       = m.buildMethodsMapper(service, "Query")
		mutations     = m.buildMethodsMapper(service, "Mutation")
		subscriptions = m.buildMethodsMapper(service, "Subscription")
	)

	if service.Options.GetSkip() {
		return
	}

	for _, method := range service.Methods {
		// Ignore streaming RPC methods.
		if method.Proto.GetClientStreaming() || method.Proto.GetServerStreaming() {
			continue
		}

		field := m.graphqlFieldFromMethod(method)
		allMethods.Object.Fields = append(allMethods.Object.Fields, field)
		allMethods.Methods = append(allMethods.Methods, method)

		if method.Options.GetSkip() {
			fmt.Fprintf(
				os.Stderr,
				"%s: option (method.skip) for %s.%s is deprecated, methods now opt in with the 'operation' option\n",
				service.File.Proto.GetName(),
				service.Proto.GetName(),
				method.Proto.GetName(),
			)
			continue
		}

		switch operation := method.Options.GetOperation(); operation {
		case "":
			// No operation specified, ignore method.
			continue
		case "query":
			queries.Object.Fields = append(queries.Object.Fields, field)
			queries.Methods = append(queries.Methods, method)
		case "mutation":
			mutations.Object.Fields = append(mutations.Object.Fields, field)
			mutations.Methods = append(mutations.Methods, method)
		case "subscription":
			subscriptions.Object.Fields = append(subscriptions.Object.Fields, field)
			subscriptions.Methods = append(subscriptions.Methods, method)
		default:
			panic(fmt.Sprintf(`invalid operation: "%s" (expected "query", "mutation", or "subscription")`, operation))
		}
	}

	mapper := &ServiceMapper{
		Descriptor:    service,
		ReferenceName: m.referenceName(service),
		Methods:       allMethods,
	}

	if len(queries.Methods) > 0 {
		queries.Object.Name = m.buildGraphqlTypeName(&GraphqlTypeNameParts{
			Namespace: service.File.Options.GetNamespace(),
			Package:   service.Package,
			TypeName:  append(service.TypeName, "Query"),
		})
		mapper.Queries = queries
	}
	if len(mutations.Methods) > 0 {
		mutations.Object.Name = m.buildGraphqlTypeName(&GraphqlTypeNameParts{
			Namespace: service.File.Options.GetNamespace(),
			Package:   service.Package,
			TypeName:  append(service.TypeName, "Mutation"),
		})
		mapper.Mutations = mutations
	}
	if len(subscriptions.Methods) > 0 {
		subscriptions.Object.Name = m.buildGraphqlTypeName(&GraphqlTypeNameParts{
			Namespace: service.File.Options.GetNamespace(),
			Package:   service.Package,
			TypeName:  append(service.TypeName, "Subscription"),
		})
		mapper.Subscriptions = subscriptions
	}

	m.ServiceMappers[service.FullName] = mapper
}

func (m *Mapper) buildMethodsMapper(service *descriptor.Service, rootType string) *MethodsMapper {
	var extends *graphql.ExtendObject
	if m.Params.RootTypePrefix != nil {
		extends = &graphql.ExtendObject{
			Name: fmt.Sprintf("%s%s", *m.Params.RootTypePrefix, rootType),
			Fields: []*graphql.Field{{
				Name: m.FieldNameTransformer(m.referenceName(service)),
				TypeName: m.buildGraphqlTypeName(&GraphqlTypeNameParts{
					Namespace: service.File.Options.GetNamespace(),
					Package:   service.Package,
					TypeName:  append(service.TypeName, rootType),
				}),
				Modifiers: graphql.TypeModifierNonNull,
			}},
		}
	}

	return &MethodsMapper{
		ExtendRootObject: extends,
		Object:           &graphql.Object{},
	}
}

func (m *Mapper) graphqlFieldFromMethod(method *descriptor.Method) *graphql.Field {
	// Only add an argument if there are fields in the gRPC request message.
	var arguments []*graphql.Argument
	inputType := m.Messages[method.Proto.GetInputType()]
	if len(inputType.Fields) != 0 {
		arguments = append(arguments, &graphql.Argument{
			Name:      "input",
			TypeName:  m.MessageMappers[method.Proto.GetInputType()].Input.Name,
			Modifiers: graphql.TypeModifierNonNull,
		})
	}

	methodName := method.Options.GetField()
	if methodName == "" {
		methodName = m.MethodNameTransformer(method.Proto.GetName())
	}

	field := &graphql.Field{
		Name:       methodName,
		TypeName:   m.MessageMappers[method.Proto.GetOutputType()].Object.Name,
		Arguments:  arguments,
		Directives: method.Options.GetDirective(),
	}
	if method.Proto.Options.GetDeprecated() {
		field.Directives = append(field.Directives, "deprecated")
	}
	return field
}

type GraphqlTypeNameParts struct {
	Namespace  string
	Package    string
	TypeName   []string
	IsProtoMap bool
	Input      bool
}

func (m *Mapper) buildGraphqlTypeName(parts *GraphqlTypeNameParts) string {
	var b strings.Builder

	if parts.Namespace != "" {
		b.WriteString(parts.Namespace)
	} else {
		b.WriteString(generator.CamelCaseSlice(strings.Split(parts.Package, ".")))
	}

	for i, name := range parts.TypeName {
		if parts.IsProtoMap && i == len(parts.TypeName)-1 {
			name = strings.TrimSuffix(name, "Entry")
		}

		b.WriteString("_")
		b.WriteString(generator.CamelCase(name))
	}
	if parts.IsProtoMap {
		b.WriteString("Entry")
	}
	if parts.Input {
		b.WriteString("Input")
	}

	return strings.TrimPrefix(b.String(), m.Params.TrimPrefix)
}

func (m *Mapper) referenceName(service *descriptor.Service) string {
	if service.Options.GetReferenceName() != "" {
		return service.Options.GetReferenceName()
	}

	// e.g. .foo.bar.Baz -> foo_bar_baz
	name := service.FullName
	name = strings.TrimPrefix(name, ".")
	name = strings.Replace(name, ".", "_", -1)
	return m.MethodNameTransformer(name)
}

func (m *Mapper) messageName(message *descriptor.Message, input bool) string {
	if message.Options.GetType() != "" {
		name := message.Options.GetType()
		if input {
			name += "Input"
		}
		return name
	}

	return m.buildGraphqlTypeName(&GraphqlTypeNameParts{
		Namespace:  message.File.Options.GetNamespace(),
		Package:    message.Package,
		TypeName:   message.TypeName,
		Input:      input,
		IsProtoMap: message.IsMap,
	})
}

func (m *Mapper) fieldName(field *descriptor.Field) string {
	if field.Options.GetField() != "" {
		return field.Options.GetField()
	}
	return m.FieldNameTransformer(field.Name)
}

func (m *Mapper) enumName(enum *descriptor.Enum) string {
	if enum.Options.GetType() != "" {
		return enum.Options.GetType()
	}

	return m.buildGraphqlTypeName(&GraphqlTypeNameParts{
		Namespace: enum.File.Options.GetNamespace(),
		Package:   enum.Package,
		TypeName:  enum.TypeName,
	})
}

func (m *Mapper) nullableScalars(field *descriptor.Field, input bool) bool {
	if input {
		return true
	}
	switch field.Parent.File.Proto.GetSyntax() {
	case "proto2", "":
		return field.Proto.GetLabel() == pb.FieldDescriptorProto_LABEL_OPTIONAL
	}
	return false
}
