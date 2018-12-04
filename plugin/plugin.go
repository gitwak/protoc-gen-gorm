package plugin

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/protoc-gen-gogo/descriptor"
	"github.com/gogo/protobuf/protoc-gen-gogo/generator"
	jgorm "github.com/jinzhu/gorm"
	"github.com/jinzhu/inflection"

	"log"

	"github.com/infobloxopen/protoc-gen-gorm/options"
)

const (
	typeMessage = 11
	typeEnum    = 14

	protoTypeTimestamp = "Timestamp" // last segment, first will be *google_protobufX
	protoTypeJSON      = "JSONValue"
	protoTypeUUID      = "UUID"
	protoTypeUUIDValue = "UUIDValue"
	protoTypeResource  = "Identifier"
	protoTypeInet      = "InetValue"
)

// DB Engine Enum
const (
	ENGINE_UNSET = iota
	ENGINE_POSTGRES
)

var wellKnownTypes = map[string]string{
	"StringValue": "*string",
	"DoubleValue": "*double",
	"FloatValue":  "*float",
	"Int32Value":  "*int32",
	"Int64Value":  "*int64",
	"UInt32Value": "*uint32",
	"UInt64Value": "*uint64",
	"BoolValue":   "*bool",
	//  "BytesValue" : "*[]byte",
}

var builtinTypes = map[string]struct{}{
	"bool": struct{}{},
	"int":  struct{}{},
	"int8": struct{}{}, "int16": struct{}{},
	"int32": struct{}{}, "int64": struct{}{},
	"uint":  struct{}{},
	"uint8": struct{}{}, "uint16": struct{}{},
	"uint32": struct{}{}, "uint64": struct{}{},
	"uintptr": struct{}{},
	"float32": struct{}{}, "float64": struct{}{},
	"string": struct{}{},
	"[]byte": struct{}{},
}

type OrmableType struct {
	OriginName string
	Name       string
	Package    string
	File       *generator.FileDescriptor
	Fields     map[string]*Field
	Methods    map[string]*autogenMethod
}

type Field struct {
	ParentGoType string
	Type         string
	Package      string
	*gorm.GormFieldOptions
	ParentOriginName string
}

func NewOrmableType(oname, pkg string, file *generator.FileDescriptor) *OrmableType {
	return &OrmableType{
		OriginName: oname,
		Package:    pkg,
		File:       file,
		Fields:     make(map[string]*Field),
		Methods:    make(map[string]*autogenMethod),
	}
}

// OrmPlugin implements the plugin interface and creates GORM code from .protos
type OrmPlugin struct {
	*generator.Generator
	dbEngine        int
	stringEnums     bool
	gateway         bool
	ormableTypes    map[string]*OrmableType
	EmptyFiles      []string
	currentPackage  string
	currentFile     *generator.FileDescriptor
	fileImports     map[*generator.FileDescriptor]*fileImports
	messages        map[string]struct{}
	ormableServices []autogenService
	suppressWarn    bool
}

func (p *OrmPlugin) setFile(file *generator.FileDescriptor) {
	p.currentFile = file
	p.currentPackage = file.GetPackage()
	p.Generator.SetFile(file.FileDescriptorProto)
}

// Name identifies the plugin
func (p *OrmPlugin) Name() string {
	return "gorm"
}

// Init is called once after data structures are built but before
// code generation begins.
func (p *OrmPlugin) Init(g *generator.Generator) {
	p.Generator = g
	p.fileImports = make(map[*generator.FileDescriptor]*fileImports)
	p.messages = make(map[string]struct{})
	if strings.EqualFold(g.Param["engine"], "postgres") {
		p.dbEngine = ENGINE_POSTGRES
	} else {
		p.dbEngine = ENGINE_UNSET
	}
	if strings.EqualFold(g.Param["enums"], "string") {
		p.stringEnums = true
	}
	if _, ok := g.Param["gateway"]; ok {
		p.gateway = true
	}
	if _, ok := g.Param["quiet"]; ok {
		p.suppressWarn = true
	}
}

// Generate produces the code generated by the plugin for this file,
// except for the imports, by calling the generator's methods P, In, and Out.
func (p *OrmPlugin) Generate(file *generator.FileDescriptor) {
	// On the first file, go through and fill out all the objects and associations
	// so that cross-file assocations within the same package work
	if p.ormableTypes == nil {
		p.ormableTypes = make(map[string]*OrmableType)
		for _, fileProto := range p.AllFiles().GetFile() {
			file := p.FileOf(fileProto)
			p.fileImports[file] = newFileImports()
			p.setFile(file)
			// Preload just the types we'll be creating
			for _, msg := range file.Messages() {
				// We don't want to bother with the MapEntry stuff
				if msg.GetOptions().GetMapEntry() {
					continue
				}
				typeName := p.getMsgName(msg)
				p.messages[typeName] = struct{}{}

				if getMessageOptions(msg).GetOrmable() {
					ormable := NewOrmableType(typeName, fileProto.GetPackage(), file)
					if _, ok := p.ormableTypes[typeName]; !ok {
						p.ormableTypes[typeName] = ormable
					}
				}
			}
			for _, msg := range file.Messages() {
				typeName := p.getMsgName(msg)
				if p.isOrmable(typeName) {
					p.parseBasicFields(msg)
				}
			}
			for _, msg := range file.Messages() {
				typeName := p.getMsgName(msg)
				if p.isOrmable(typeName) {
					p.parseAssociations(msg)
					o := p.getOrmable(typeName)
					if p.hasPrimaryKey(o) {
						_, fd := p.findPrimaryKey(o)
						fd.ParentOriginName = o.OriginName
					}
				}
			}
		}
		for _, fileProto := range p.AllFiles().GetFile() {
			file := p.FileOf(fileProto)
			p.setFile(file)
			p.parseServices(file)
		}
	}
	// Return to the file at hand and then generate anything needed
	p.setFile(file)
	empty := true
	for _, msg := range file.Messages() {
		typeName := p.getMsgName(msg)
		if p.isOrmable(typeName) {
			empty = false
			p.generateOrmable(msg)
			p.generateTableNameFunction(msg)
			p.generateConvertFunctions(msg)
			p.generateHookInterfaces(msg)
		}
	}
	p.generateDefaultHandlers(file)
	p.generateDefaultServer(file)
	// no ormable objects, and no imports (means no services generated)
	if empty && len(p.GetFileImports().packages) == 0 {
		p.EmptyFiles = append(p.EmptyFiles, file.GetName())
	}
}

func (p *OrmPlugin) parseBasicFields(msg *generator.Descriptor) {
	typeName := p.getMsgName(msg)
	ormable := p.getOrmable(typeName)
	ormable.Name = fmt.Sprintf("%sORM", typeName)
	for _, field := range msg.GetField() {
		fieldOpts := getFieldOptions(field)
		if fieldOpts == nil {
			fieldOpts = &gorm.GormFieldOptions{}
		}
		if fieldOpts.GetDrop() {
			continue
		}
		tag := fieldOpts.GetTag()
		fieldName := generator.CamelCase(field.GetName())
		fieldType, _ := p.GoType(msg, field)
		var typePackage string
		if (*(field.Type) != typeMessage || !p.isOrmable(fieldType)) && field.IsRepeated() {
			// Not implemented yet
			continue
		} else if *(field.Type) == typeEnum {
			fieldType = "int32"
			if p.stringEnums {
				fieldType = "string"
			}
		} else if *(field.Type) == typeMessage {
			//Check for WKTs or fields of nonormable types
			parts := strings.Split(fieldType, ".")
			rawType := parts[len(parts)-1]
			if v, exists := wellKnownTypes[rawType]; exists {
				p.GetFileImports().typesToRegister = append(p.GetFileImports().typesToRegister, field.GetTypeName())
				p.GetFileImports().wktPkgName = strings.Trim(parts[0], "*")
				fieldType = v
				typePackage = wktImport
			} else if rawType == protoTypeUUID {
				fieldType = fmt.Sprintf("%s.UUID", p.Import(uuidImport))
				typePackage = uuidImport
				if p.dbEngine == ENGINE_POSTGRES {
					fieldOpts.Tag = tagWithType(tag, "uuid")
				}
			} else if rawType == protoTypeUUIDValue {
				fieldType = fmt.Sprintf("*%s.UUID", p.Import(uuidImport))
				typePackage = uuidImport
				if p.dbEngine == ENGINE_POSTGRES {
					fieldOpts.Tag = tagWithType(tag, "uuid")
				}
			} else if rawType == protoTypeTimestamp {
				fieldType = "time.Time"
				typePackage = "time"
				p.UsingGoImports("time")

				tag := getFieldOptions(field).GetTag()
				if !tag.GetNotNull() {
					fieldType = "*" + fieldType
				}				
			} else if rawType == protoTypeJSON {
				if p.dbEngine == ENGINE_POSTGRES {
					fieldType = fmt.Sprintf("*%s.Jsonb", p.Import(gormpqImport))
					typePackage = gormpqImport
					fieldOpts.Tag = tagWithType(tag, "jsonb")
				} else {
					// Potential TODO: add types we want to use in other/default DB engine
					continue
				}
			} else if rawType == protoTypeResource {
				p.Import(resourceImport)

				tag := getFieldOptions(field).GetTag()
				ttype := tag.GetType()
				ttype = strings.ToLower(ttype)
				if strings.Contains(ttype, "char") {
					ttype = "char"
				}
				if strings.Contains(ttype, "array") || strings.ContainsAny(ttype, "[]") {
					ttype = "array"
				}
				switch ttype {
				case "uuid", "text", "char", "array", "cidr", "inet", "macaddr":
					fieldType = "*string"
				case "smallint", "integer", "bigint", "numeric", "smallserial", "serial", "bigserial":
					fieldType = "*int64"
				case "jsonb", "bytea":
					fieldType = "[]byte"
				case "":
					fieldType = "interface{}" // we do not know the type yet (if it association we will fix the type later)
				default:
					p.Fail("unknown tag type of atlas.rpc.Identifier")
				}
				if tag.GetNotNull() || tag.GetPrimaryKey() {
					fieldType = strings.TrimPrefix(fieldType, "*")
				}
			} else if rawType == protoTypeInet {
				fieldType = fmt.Sprintf("*%s.Inet", p.Import(gtypesImport))
				typePackage = gtypesImport
				if p.dbEngine == ENGINE_POSTGRES {
					fieldOpts.Tag = tagWithType(tag, "inet")
				} else {
					fieldOpts.Tag = tagWithType(tag, "varchar(48)")
				}
			} else {
				continue
			}
		}
		f := &Field{Type: fieldType, Package: typePackage, GormFieldOptions: fieldOpts}
		if tname := getFieldOptions(field).GetReferenceOf(); tname != "" {
			if _, ok := p.messages[tname]; !ok {
				p.Fail("unknown message type in refers_to: ", tname, " in field: ", fieldName, " of type: ", typeName)
			}
			f.ParentOriginName = tname
		}
		ormable.Fields[fieldName] = f
	}
	if getMessageOptions(msg).GetMultiAccount() {
		if accID, ok := ormable.Fields["AccountID"]; !ok {
			ormable.Fields["AccountID"] = &Field{Type: "string"}
		} else {
			if accID.Type != "string" {
				p.Fail("Cannot include AccountID field into", ormable.Name, "as it already exists there with a different type.")
			}
		}
	}
	for _, field := range getMessageOptions(msg).GetInclude() {
		fieldName := generator.CamelCase(field.GetName())
		if _, ok := ormable.Fields[fieldName]; !ok {
			p.addIncludedField(ormable, field)
		} else {
			p.Fail("Cannot include", fieldName, "field into", ormable.Name, "as it aready exists there.")
		}
	}
}

func tagWithType(tag *gorm.GormTag, typename string) *gorm.GormTag {
	if tag == nil {
		tag = &gorm.GormTag{}
	}
	tag.Type = proto.String(typename)
	return tag
}

func (p *OrmPlugin) addIncludedField(ormable *OrmableType, field *gorm.ExtraField) {
	fieldName := generator.CamelCase(field.GetName())
	isPtr := strings.HasPrefix(field.GetType(), "*")
	rawType := strings.TrimPrefix(field.GetType(), "*")
	// cut off any package subpaths
	rawType = rawType[strings.LastIndex(rawType, ".")+1:]
	var typePackage string
	// Handle types with a package defined
	if field.GetPackage() != "" {
		alias := p.Import(field.GetPackage())
		rawType = fmt.Sprintf("%s.%s", alias, rawType)
		typePackage = field.GetPackage()
	} else {
		// Handle types without a package defined
		if _, ok := builtinTypes[rawType]; ok {
			// basic type, 100% okay, no imports or changes needed
		} else if rawType == "Time" {
			p.UsingGoImports("time")
			rawType = "time.Time"
			typePackage = "time"
		} else if rawType == "UUID" {
			rawType = fmt.Sprintf("%s.UUID", p.Import(uuidImport))
			typePackage = uuidImport
		} else if field.GetType() == "Jsonb" && p.dbEngine == ENGINE_POSTGRES {
			rawType = fmt.Sprintf("%s.Jsonb", p.Import(gormpqImport))
			typePackage = gormpqImport
		} else if rawType == "Inet" {
			rawType = fmt.Sprintf("%s.Inet", p.Import(gtypesImport))
			typePackage = gtypesImport
		} else {
			p.warning(`included field %q of type %q is not a recognized special type, and no package specified. This type is assumed to be in the same package as the generated code`,
				field.GetName(), field.GetType())
		}
	}
	if isPtr {
		rawType = fmt.Sprintf("*%s", rawType)
	}
	ormable.Fields[fieldName] = &Field{Type: rawType, Package: typePackage, GormFieldOptions: &gorm.GormFieldOptions{Tag: field.GetTag()}}
}

func (p *OrmPlugin) isOrmable(typeName string) bool {
	parts := strings.Split(typeName, ".")
	_, ok := p.ormableTypes[strings.Trim(parts[len(parts)-1], "[]*")]
	return ok
}

func (p *OrmPlugin) getOrmable(typeName string) *OrmableType {
	parts := strings.Split(typeName, ".")
	if ormable, ok := p.ormableTypes[strings.TrimSuffix(strings.Trim(parts[len(parts)-1], "[]*"), "ORM")]; ok {
		return ormable
	} else {
		p.Fail(typeName, "is not ormable.")
		return nil
	}
}

func (p *OrmPlugin) getSortedFieldNames(fields map[string]*Field) []string {
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (p *OrmPlugin) generateOrmable(message *generator.Descriptor) {
	ormable := p.getOrmable(p.TypeName(message))
	p.P(`type `, ormable.Name, ` struct {`)
	for _, fieldName := range p.getSortedFieldNames(ormable.Fields) {
		field := ormable.Fields[fieldName]
		p.P(fieldName, ` `, field.Type, p.renderGormTag(field))

	}
	p.P(`}`)
}

func (p *OrmPlugin) renderGormTag(field *Field) string {
	var gormRes, atlasRes string
	tag := field.GetTag()
	if tag == nil {
		tag = &gorm.GormTag{}
	}

	if tag.Column != nil {
		gormRes += fmt.Sprintf("column:%s;", tag.GetColumn())
	}
	if tag.Type != nil {
		gormRes += fmt.Sprintf("type:%s;", string(tag.GetType()))
	}
	if tag.Size_ != nil {
		gormRes += fmt.Sprintf("size:%s;", fmt.Sprintf("%d", tag.GetSize_()))
	}
	if tag.Precision != nil {
		gormRes += fmt.Sprintf("precision:%s;", fmt.Sprintf("%d", tag.GetPrecision()))
	}
	if tag.GetPrimaryKey() {
		gormRes += "primary_key;"
	}
	if tag.GetUnique() {
		gormRes += "unique;"
	}
	if tag.Default != nil {
		gormRes += fmt.Sprintf("default:%s;", tag.GetDefault())
	}
	if tag.GetNotNull() {
		gormRes += "not null;"
	}
	if tag.GetAutoIncrement() {
		gormRes += "auto_increment;"
	}
	if tag.Index != nil {
		if tag.GetIndex() == "" {
			gormRes += "index;"
		} else {
			gormRes += fmt.Sprintf("index:%s;", tag.GetIndex())
		}
	}
	if tag.UniqueIndex != nil {
		if tag.GetUniqueIndex() == "" {
			gormRes += "unique_index;"
		} else {
			gormRes += fmt.Sprintf("unique_index:%s;", tag.GetUniqueIndex())
		}
	}
	if tag.GetEmbedded() {
		gormRes += "embedded;"
	}
	if tag.EmbeddedPrefix != nil {
		gormRes += fmt.Sprintf("embedded_prefix:%s;", tag.GetEmbeddedPrefix())
	}
	if tag.GetIgnore() {
		gormRes += "-;"
	}

	var foreignKey, associationForeignKey, joinTable, joinTableForeignKey, associationJoinTableForeignKey *string
	var associationAutoupdate, associationAutocreate, associationSaveReference, preload *bool
	if hasOne := field.GetHasOne(); hasOne != nil {
		foreignKey = hasOne.Foreignkey
		associationForeignKey = hasOne.AssociationForeignkey
		associationAutoupdate = hasOne.AssociationAutoupdate
		associationAutocreate = hasOne.AssociationAutocreate
		associationSaveReference = hasOne.AssociationSaveReference
		preload = hasOne.Preload
	} else if belongsTo := field.GetBelongsTo(); belongsTo != nil {
		foreignKey = belongsTo.Foreignkey
		associationForeignKey = belongsTo.AssociationForeignkey
		associationAutoupdate = belongsTo.AssociationAutoupdate
		associationAutocreate = belongsTo.AssociationAutocreate
		associationSaveReference = belongsTo.AssociationSaveReference
		preload = belongsTo.Preload
	} else if hasMany := field.GetHasMany(); hasMany != nil {
		foreignKey = hasMany.Foreignkey
		associationForeignKey = hasMany.AssociationForeignkey
		associationAutoupdate = hasMany.AssociationAutoupdate
		associationAutocreate = hasMany.AssociationAutocreate
		associationSaveReference = hasMany.AssociationSaveReference
		preload = hasMany.Preload
		if hasMany.PositionField != nil {
			atlasRes += fmt.Sprintf("position:%s;", hasMany.GetPositionField())
		}
	} else if mtm := field.GetManyToMany(); mtm != nil {
		foreignKey = mtm.Foreignkey
		associationForeignKey = mtm.AssociationForeignkey
		joinTable = mtm.Jointable
		joinTableForeignKey = mtm.JointableForeignkey
		associationJoinTableForeignKey = mtm.AssociationJointableForeignkey
		associationAutoupdate = mtm.AssociationAutoupdate
		associationAutocreate = mtm.AssociationAutocreate
		associationSaveReference = mtm.AssociationSaveReference
		preload = mtm.Preload
	} else {
		foreignKey = tag.Foreignkey
		associationForeignKey = tag.AssociationForeignkey
		joinTable = tag.ManyToMany
		joinTableForeignKey = tag.JointableForeignkey
		associationJoinTableForeignKey = tag.AssociationJointableForeignkey
		associationAutoupdate = tag.AssociationAutoupdate
		associationAutocreate = tag.AssociationAutocreate
		associationSaveReference = tag.AssociationSaveReference
		preload = tag.Preload
	}

	if foreignKey != nil {
		gormRes += fmt.Sprintf("foreignkey:%s;", *foreignKey)
	}
	if associationForeignKey != nil {
		gormRes += fmt.Sprintf("association_foreignkey:%s;", *associationForeignKey)
	}
	if joinTable != nil {
		gormRes += fmt.Sprintf("many2many:%s;", *joinTable)
	}
	if joinTableForeignKey != nil {
		gormRes += fmt.Sprintf("jointable_foreignkey:%s;", *joinTableForeignKey)
	}
	if associationJoinTableForeignKey != nil {
		gormRes += fmt.Sprintf("association_jointable_foreignkey:%s;", *associationJoinTableForeignKey)
	}
	if associationAutoupdate != nil {
		gormRes += fmt.Sprintf("association_autoupdate:%s;", strconv.FormatBool(*associationAutoupdate))
	}
	if associationAutocreate != nil {
		gormRes += fmt.Sprintf("association_autocreate:%s;", strconv.FormatBool(*associationAutocreate))
	}
	if associationSaveReference != nil {
		gormRes += fmt.Sprintf("association_save_reference:%s;", strconv.FormatBool(*associationSaveReference))
	}
	if preload != nil {
		gormRes += fmt.Sprintf("preload:%s;", strconv.FormatBool(*preload))
	}

	var gormTag, atlasTag string
	if gormRes != "" {
		gormTag = fmt.Sprintf("gorm:\"%s\"", strings.TrimRight(gormRes, ";"))
	}
	if atlasRes != "" {
		atlasTag = fmt.Sprintf("atlas:\"%s\"", strings.TrimRight(atlasRes, ";"))
	}
	finalTag := strings.TrimSpace(strings.Join([]string{gormTag, atlasTag}, " "))
	if finalTag == "" {
		return ""
	} else {
		return fmt.Sprintf("`%s`", finalTag)
	}
}

// generateTableNameFunction the function to set the gorm table name
// back to gorm default, removing "ORM" suffix
func (p *OrmPlugin) generateTableNameFunction(message *generator.Descriptor) {
	typeName := p.TypeName(message)

	p.P(`// TableName overrides the default tablename generated by GORM`)
	p.P(`func (`, typeName, `ORM) TableName() string {`)

	tableName := inflection.Plural(jgorm.ToDBName(message.GetName()))
	if opts := getMessageOptions(message); opts != nil && opts.Table != nil {
		tableName = opts.GetTable()
	}
	p.P(`return "`, tableName, `"`)
	p.P(`}`)
}

// generateMapFunctions creates the converter functions
func (p *OrmPlugin) generateConvertFunctions(message *generator.Descriptor) {
	typeName := p.TypeName(message)
	ormable := p.getOrmable(generator.CamelCaseSlice(message.TypeName()))

	///// To Orm
	p.P(`// ToORM runs the BeforeToORM hook if present, converts the fields of this`)
	p.P(`// object to ORM format, runs the AfterToORM hook, then returns the ORM object`)
	p.P(`func (m *`, typeName, `) ToORM (ctx context.Context) (`, typeName, `ORM, error) {`)
	p.P(`to := `, typeName, `ORM{}`)
	p.P(`var err error`)
	p.P(`if prehook, ok := interface{}(m).(`, typeName, `WithBeforeToORM); ok {`)
	p.P(`if err = prehook.BeforeToORM(ctx, &to); err != nil {`)
	p.P(`return to, err`)
	p.P(`}`)
	p.P(`}`)
	for _, field := range message.GetField() {
		// Checking if field is skipped
		if getFieldOptions(field).GetDrop() {
			continue
		}
		ofield := ormable.Fields[generator.CamelCase(field.GetName())]
		p.generateFieldConversion(message, field, true, ofield)
	}
	if getMessageOptions(message).GetMultiAccount() {
		p.P("accountID, err := ", p.Import(authImport), ".GetAccountID(ctx, nil)")
		p.P("if err != nil {")
		p.P("return to, err")
		p.P("}")
		p.P("to.AccountID = accountID")
	}
	p.setupOrderedHasMany(message)
	p.P(`if posthook, ok := interface{}(m).(`, typeName, `WithAfterToORM); ok {`)
	p.P(`err = posthook.AfterToORM(ctx, &to)`)
	p.P(`}`)
	p.P(`return to, err`)
	p.P(`}`)

	p.P()
	///// To Pb
	p.P(`// ToPB runs the BeforeToPB hook if present, converts the fields of this`)
	p.P(`// object to PB format, runs the AfterToPB hook, then returns the PB object`)
	p.P(`func (m *`, typeName, `ORM) ToPB (ctx context.Context) (`,
		typeName, `, error) {`)
	p.P(`to := `, typeName, `{}`)
	p.P(`var err error`)
	p.P(`if prehook, ok := interface{}(m).(`, typeName, `WithBeforeToPB); ok {`)
	p.P(`if err = prehook.BeforeToPB(ctx, &to); err != nil {`)
	p.P(`return to, err`)
	p.P(`}`)
	p.P(`}`)
	for _, field := range message.GetField() {
		// Checking if field is skipped
		if getFieldOptions(field).GetDrop() {
			continue
		}
		ofield := ormable.Fields[generator.CamelCase(field.GetName())]
		p.generateFieldConversion(message, field, false, ofield)
	}
	p.P(`if posthook, ok := interface{}(m).(`, typeName, `WithAfterToPB); ok {`)
	p.P(`err = posthook.AfterToPB(ctx, &to)`)
	p.P(`}`)
	p.P(`return to, err`)
	p.P(`}`)
}

// Output code that will convert a field to/from orm.
func (p *OrmPlugin) generateFieldConversion(message *generator.Descriptor, field *descriptor.FieldDescriptorProto, toORM bool, ofield *Field) error {
	fieldName := generator.CamelCase(field.GetName())
	fieldType, _ := p.GoType(message, field)
	if field.IsRepeated() { // Repeated Object ----------------------------------
		if p.isOrmable(fieldType) { // Repeated ORMable type
			//fieldType = strings.Trim(fieldType, "[]*")

			p.P(`for _, v := range m.`, fieldName, ` {`)
			p.P(`if v != nil {`)
			if toORM {
				p.P(`if temp`, fieldName, `, cErr := v.ToORM(ctx); cErr == nil {`)
			} else {
				p.P(`if temp`, fieldName, `, cErr := v.ToPB(ctx); cErr == nil {`)
			}
			p.P(`to.`, fieldName, ` = append(to.`, fieldName, `, &temp`, fieldName, `)`)
			p.P(`} else {`)
			p.P(`return to, cErr`)
			p.P(`}`)
			p.P(`} else {`)
			p.P(`to.`, fieldName, ` = append(to.`, fieldName, `, nil)`)
			p.P(`}`)
			p.P(`}`) // end repeated for
		} else {
			p.P(`// Repeated type `, fieldType, ` is not an ORMable message type`)
		}
	} else if *(field.Type) == typeEnum { // Singular Enum, which is an int32 ---
		if toORM {
			if p.stringEnums {
				p.P(`to.`, fieldName, ` = `, fieldType, `_name[int32(m.`, fieldName, `)]`)
			} else {
				p.P(`to.`, fieldName, ` = int32(m.`, fieldName, `)`)
			}
		} else {
			if p.stringEnums {
				p.P(`to.`, fieldName, ` = `, fieldType, `(`, fieldType, `_value[m.`, fieldName, `])`)
			} else {
				p.P(`to.`, fieldName, ` = `, fieldType, `(m.`, fieldName, `)`)
			}
		}
	} else if *(field.Type) == typeMessage { // Singular Object -------------
		//Check for WKTs
		parts := strings.Split(fieldType, ".")
		coreType := parts[len(parts)-1]
		// Type is a WKT, convert to/from as ptr to base type
		if _, exists := wellKnownTypes[coreType]; exists { // Singular WKT -----
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`v := m.`, fieldName, `.Value`)
				p.P(`to.`, fieldName, ` = &v`)
				p.P(`}`)
			} else {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`to.`, fieldName, ` = &`, p.GetFileImports().wktPkgName, ".", coreType,
					`{Value: *m.`, fieldName, `}`)
				p.P(`}`)
			}
		} else if coreType == protoTypeUUIDValue { // Singular UUIDValue type ----
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`tempUUID, uErr := `, p.Import(uuidImport), `.FromString(m.`, fieldName, `.Value)`)
				p.P(`if uErr != nil {`)
				p.P(`return to, uErr`)
				p.P(`}`)
				p.P(`to.`, fieldName, ` = &tempUUID`)
				p.P(`}`)
			} else {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`to.`, fieldName, ` = &`, p.Import(gtypesImport), `.UUIDValue{Value: m.`, fieldName, `.String()}`)
				p.P(`}`)
			}
		} else if coreType == protoTypeUUID { // Singular UUID type --------------
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`to.`, fieldName, `, err = `, p.Import(uuidImport), `.FromString(m.`, fieldName, `.Value)`)
				p.P(`if err != nil {`)
				p.P(`return to, err`)
				p.P(`}`)
				p.P(`} else {`)
				p.P(`to.`, fieldName, ` = `, p.Import(uuidImport), `.Nil`)
				p.P(`}`)
			} else {
				p.P(`to.`, fieldName, ` = &`, p.Import(gtypesImport), `.UUID{Value: m.`, fieldName, `.String()}`)
			}
		} else if coreType == protoTypeTimestamp { // Singular WKT Timestamp ---
			nillable := strings.HasPrefix(ofield.Type, "*")
			
			if toORM {
				if nillable {
					p.P(`if m.`, fieldName, ` != nil {`)
				}
				p.P(`if v, err := `, p.Import(ptypesImport), `.Timestamp(m.`, fieldName, `); err != nil {`)
				p.P(`return to, err`)
				p.P(`} else {`)
				if nillable {
					p.P(`to.`, fieldName, ` = &v`)
				} else {
					p.P(`to.`, fieldName, ` = v`)
				}
				p.P(`}`)
				if nillable {
					p.P(`}`)
				}
			} else {
				if nillable {
					p.P(`if v, err := `, p.Import(ptypesImport), `.TimestampProto(*m.`, fieldName, `); err != nil {`)
				} else {
					p.P(`if v, err := `, p.Import(ptypesImport), `.TimestampProto(m.`, fieldName, `); err != nil {`)
				}
				p.P(`return to, err`)
				p.P(`} else {`)
				if nillable {
					p.P(`to.`, fieldName, ` = &v`)
				} else {
					p.P(`to.`, fieldName, ` = v`)
				}
				p.P(`}`)
			}
		} else if coreType == protoTypeJSON {
			if p.dbEngine == ENGINE_POSTGRES {
				if toORM {
					p.P(`if m.`, fieldName, ` != nil {`)
					p.P(`to.`, fieldName, ` = &`, p.Import(gormpqImport), `.Jsonb{[]byte(m.`, fieldName, `.Value)}`)
					p.P(`}`)
				} else {
					p.P(`if m.`, fieldName, ` != nil {`)
					p.P(`to.`, fieldName, ` = &`, p.Import(gtypesImport), `.JSONValue{Value: string(m.`, fieldName, `.RawMessage)}`)
					p.P(`}`)
				}
			} // Potential TODO other DB engine handling if desired
		} else if coreType == protoTypeResource {
			resource := "nil" // assuming we do not know the PB type, nil means call codec for any resource
			if ofield != nil && ofield.ParentOriginName != "" {
				resource = "&" + ofield.ParentOriginName + "{}"
			}
			btype := strings.TrimPrefix(ofield.Type, "*")
			nillable := strings.HasPrefix(ofield.Type, "*")
			iface := ofield.Type == "interface{}"

			if toORM {
				if nillable {
					p.P(`if m.`, fieldName, ` != nil {`)
				}
				switch btype {
				case "int64":
					p.P(`if v, err :=`, p.Import(resourceImport), `.DecodeInt64(`, resource, `, m.`, fieldName, `); err != nil {`)
					p.P(`	return to, err`)
					p.P(`} else {`)
					if nillable {
						p.P(`to.`, fieldName, ` = &v`)
					} else {
						p.P(`to.`, fieldName, ` = v`)
					}
					p.P(`}`)
				case "[]byte":
					p.P(`if v, err :=`, p.Import(resourceImport), `.DecodeBytes(`, resource, `, m.`, fieldName, `); err != nil {`)
					p.P(`	return to, err`)
					p.P(`} else {`)
					p.P(`	to.`, fieldName, ` = v`)
					p.P(`}`)
				default:
					p.P(`if v, err :=`, p.Import(resourceImport), `.Decode(`, resource, `, m.`, fieldName, `); err != nil {`)
					p.P(`return to, err`)
					p.P(`} else if v != nil {`)
					if nillable {
						p.P(`vv := v.(`, btype, `)`)
						p.P(`to.`, fieldName, ` = &vv`)
					} else if iface {
						p.P(`to.`, fieldName, `= v`)
					} else {
						p.P(`to.`, fieldName, ` = v.(`, btype, `)`)
					}
					p.P(`}`)
				}
				if nillable {
					p.P(`}`)
				}
			}

			if !toORM {
				if nillable {
					p.P(`if m.`, fieldName, `!= nil {`)
					p.P(`	if v, err := `, p.Import(resourceImport), `.Encode(`, resource, `, *m.`, fieldName, `); err != nil {`)
					p.P(`		return to, err`)
					p.P(`	} else {`)
					p.P(`		to.`, fieldName, ` = v`)
					p.P(`	}`)
					p.P(`}`)

				} else {
					p.P(`if v, err := `, p.Import(resourceImport), `.Encode(`, resource, `, m.`, fieldName, `); err != nil {`)
					p.P(`return to, err`)
					p.P(`} else {`)
					p.P(`to.`, fieldName, ` = v`)
					p.P(`}`)
				}
			}
		} else if coreType == protoTypeInet { // Inet type for Postgres only, currently
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`if to.`, fieldName, `, err = `, p.Import(gtypesImport), `.ParseInet(m.`, fieldName, `.Value); err != nil {`)
				p.P(`return to, err`)
				p.P(`}`)
				p.P(`}`)
			} else {
				p.P(`if m.`, fieldName, ` != nil && m.`, fieldName, `.IPNet != nil {`)
				p.P(`to.`, fieldName, ` = &`, p.Import(gtypesImport), `.InetValue{Value: m.`, fieldName, `.String()}`)
				p.P(`}`)
			}
		} else if p.isOrmable(fieldType) {
			// Not a WKT, but a type we're building converters for
			p.P(`if m.`, fieldName, ` != nil {`)
			if toORM {
				p.P(`temp`, fieldName, `, err := m.`, fieldName, `.ToORM (ctx)`)
			} else {
				p.P(`temp`, fieldName, `, err := m.`, fieldName, `.ToPB (ctx)`)
			}
			p.P(`if err != nil {`)
			p.P(`return to, err`)
			p.P(`}`)
			p.P(`to.`, fieldName, ` = &temp`, fieldName)
			p.P(`}`)
		}
	} else { // Singular raw ----------------------------------------------------
		p.P(`to.`, fieldName, ` = m.`, fieldName)
	}
	return nil
}

func (p *OrmPlugin) generateHookInterfaces(message *generator.Descriptor) {
	typeName := p.TypeName(message)
	p.P(`// The following are interfaces you can implement for special behavior during ORM/PB conversions`)
	p.P(`// of type `, typeName, ` the arg will be the target, the caller the one being converted from`)
	p.P()
	for _, desc := range [][]string{
		{"BeforeToORM", typeName + "ORM", " called before default ToORM code"},
		{"AfterToORM", typeName + "ORM", " called after default ToORM code"},
		{"BeforeToPB", typeName, " called before default ToPB code"},
		{"AfterToPB", typeName, " called after default ToPB code"},
	} {
		p.P(`// `, typeName, desc[0], desc[2])
		p.P(`type `, typeName, `With`, desc[0], ` interface {`)
		p.P(desc[0], `(context.Context, *`, desc[1], `) error`)
		p.P(`}`)
		p.P()
	}
}

func (p *OrmPlugin) setupOrderedHasMany(message *generator.Descriptor) {
	ormable := p.getOrmable(p.TypeName(message))
	for _, fieldName := range p.getSortedFieldNames(ormable.Fields) {
		p.setupOrderedHasManyByName(message, fieldName)
	}
}

func (p *OrmPlugin) setupOrderedHasManyByName(message *generator.Descriptor, fieldName string) {
	ormable := p.getOrmable(p.TypeName(message))
	field := ormable.Fields[fieldName]

	if field == nil {
		return
	}

	if field.GetHasMany().GetPositionField() != "" {
		positionField := field.GetHasMany().GetPositionField()
		positionFieldType := p.getOrmable(field.Type).Fields[positionField].Type
		p.P(`for i, e := range `, `to.`, fieldName, `{`)
		p.P(`e.`, positionField, ` = `, positionFieldType, `(i)`)
		p.P(`}`)
	}
}

func (p *OrmPlugin) warning(format string, v ...interface{}) {
	if !p.suppressWarn {
		log.Printf("WARNING: "+format, v...)
	}
}
