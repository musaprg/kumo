// Package template parses and evaluates the supported CloudFormation template subset.
package template

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Template is a parsed CloudFormation template.
type Template struct {
	parameters map[string]parameter
	mappings   map[string]any
	conditions map[string]any
	resources  map[string]resource
	outputs    map[string]output
}

// EvaluationInput supplies values that cannot be resolved from a template alone.
type EvaluationInput struct {
	Parameters         map[string]string
	PseudoParameters   map[string]string
	Imports            map[string]string
	ResourceAttributes map[string]map[string]string
}

// Evaluation is the canonical, condition-filtered template representation.
type Evaluation struct {
	Parameters map[string]string
	Resources  map[string]Resource
	Outputs    map[string]Output
}

// Resource is an evaluated resource definition.
type Resource struct {
	Type       string
	Properties map[string]any
	DependsOn  []string
}

// Output is an evaluated output definition.
type Output struct {
	Value      any
	ExportName string
}

type parameter struct {
	typeName       string
	defaultValue   string
	hasDefault     bool
	allowedValues  []string
	allowedPattern string
	minLength      *int
	maxLength      *int
	minValue       *float64
	maxValue       *float64
}

type resource struct {
	typeName   string
	properties map[string]any
	condition  string
	dependsOn  []string
}

type output struct {
	value     any
	condition string
	export    any
}

const (
	intrinsicRef         = "Ref"
	intrinsicCondition   = "Condition"
	intrinsicPrefix      = "Fn::"
	intrinsicGetAtt      = "Fn::GetAtt"
	intrinsicFindInMap   = "Fn::FindInMap"
	intrinsicJoin        = "Fn::Join"
	intrinsicSelect      = "Fn::Select"
	intrinsicSplit       = "Fn::Split"
	intrinsicSub         = "Fn::Sub"
	intrinsicIf          = "Fn::If"
	intrinsicEquals      = "Fn::Equals"
	intrinsicAnd         = "Fn::And"
	intrinsicOr          = "Fn::Or"
	intrinsicNot         = "Fn::Not"
	intrinsicImportValue = "Fn::ImportValue"
)

var shortTags = map[string]string{
	"!Ref":         intrinsicRef,
	"!GetAtt":      intrinsicGetAtt,
	"!FindInMap":   intrinsicFindInMap,
	"!Join":        intrinsicJoin,
	"!Select":      intrinsicSelect,
	"!Split":       intrinsicSplit,
	"!Sub":         intrinsicSub,
	"!If":          intrinsicIf,
	"!Equals":      intrinsicEquals,
	"!And":         intrinsicAnd,
	"!Or":          intrinsicOr,
	"!Not":         intrinsicNot,
	"!Condition":   intrinsicCondition,
	"!ImportValue": intrinsicImportValue,
}

// Parse parses a JSON or YAML CloudFormation template.
//
// A non-empty TemplateURL is rejected because remote template retrieval is unsupported.
//
//nolint:nlreturn,wsl // Parsing is a linear validation pipeline.
func Parse(body string, templateURLs ...string) (*Template, error) {
	if len(templateURLs) > 1 || len(templateURLs) == 1 && templateURLs[0] != "" {
		return nil, fmt.Errorf("TemplateURL is unsupported")
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(body), &node); err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("template must be a mapping")
	}
	root, err := normalize(node.Content[0])
	if err != nil {
		return nil, err
	}
	if err := validateIntrinsicShapes(root, resourceNames(root)); err != nil {
		return nil, err
	}
	m, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("template must be a mapping")
	}
	return parseTemplate(m)
}

//nolint:cyclop,gocognit,nlreturn,wsl // Intrinsic shape validation recursively checks every map and sequence value.
func validateIntrinsicShapes(value any, resources map[string]struct{}) error {
	switch value := value.(type) {
	case map[string]any:
		hasIntrinsic := false
		var intrinsicName string
		var intrinsicArgument any
		for key, child := range value {
			if key == intrinsicRef || strings.HasPrefix(key, intrinsicPrefix) {
				hasIntrinsic = true
				intrinsicName = key
				intrinsicArgument = child
				if !supportedIntrinsic(key) {
					return fmt.Errorf("unsupported intrinsic function %q", key)
				}
			}
			if err := validateIntrinsicShapes(child, resources); err != nil {
				return err
			}
		}
		if hasIntrinsic && len(value) != 1 {
			return fmt.Errorf("intrinsic function maps must contain exactly one key")
		}
		if hasIntrinsic {
			return validateIntrinsicArguments(intrinsicName, intrinsicArgument, resources)
		}
		if len(value) == 1 {
			if argument, ok := value[intrinsicCondition]; ok {
				if isSectionDefinition(argument) {
					return nil
				}
				return validateIntrinsicArguments(intrinsicCondition, argument, resources)
			}
		}
	case []any:
		for _, child := range value {
			if err := validateIntrinsicShapes(child, resources); err != nil {
				return err
			}
		}
	}
	return nil
}

//nolint:nlreturn,wsl // Resource names are extracted from the optional Resources section.
func resourceNames(value any) map[string]struct{} {
	root, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	resources, ok := root["Resources"].(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]struct{}, len(resources))
	for name := range resources {
		result[name] = struct{}{}
	}
	return result
}

//nolint:nestif,nlreturn,wsl // Resource dependency checks must inspect supported nested intrinsic forms.
func resourceDependent(value any, resources map[string]struct{}) bool {
	switch value := value.(type) {
	case map[string]any:
		if len(value) == 1 {
			if name, ok := value[intrinsicRef].(string); ok {
				_, found := resources[name]
				return found
			}
			if argument, ok := value[intrinsicGetAtt]; ok {
				parts, ok := getAttParts(argument)
				if ok {
					_, found := resources[parts[0]]
					return found
				}
			}
			if argument, ok := value[intrinsicSub]; ok {
				return subReferencesResource(argument, resources)
			}
		}
		for _, child := range value {
			if resourceDependent(child, resources) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if resourceDependent(child, resources) {
				return true
			}
		}
	}
	return false
}

//nolint:nlreturn,wsl // Explicit substitution bindings override matching source placeholders.
func subReferencesResource(value any, resources map[string]struct{}) bool {
	var source string
	variables := map[string]any{}
	switch value := value.(type) {
	case string:
		source = value
	case []any:
		if len(value) != 2 {
			return false
		}
		source, _ = value[0].(string)
		var ok bool
		variables, ok = value[1].(map[string]any)
		if !ok || resourceDependent(variables, resources) {
			return true
		}
	default:
		return false
	}
	for _, match := range subPattern.FindAllString(source, -1) {
		name := match[2 : len(match)-1]
		if strings.HasPrefix(name, "!") {
			continue
		}
		if _, ok := variables[name]; ok {
			continue
		}
		logicalID := strings.SplitN(name, ".", 2)[0]
		if _, ok := resources[logicalID]; ok {
			return true
		}
	}
	return false
}

//nolint:nlreturn,wsl // Section definitions use Type or Value as their required discriminator.
func isSectionDefinition(value any) bool {
	definition, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, resource := definition["Type"]
	_, output := definition["Value"]
	return resource || output
}

//nolint:cyclop,funlen,gocognit,gocyclo,nlreturn,wsl // Structural checks mirror the supported intrinsic argument grammar.
func validateIntrinsicArguments(name string, argument any, resources map[string]struct{}) error {
	switch name {
	case intrinsicRef, intrinsicCondition:
		if _, ok := argument.(string); !ok {
			return fmt.Errorf("%s argument must be a string", name)
		}
	case intrinsicGetAtt:
		if _, ok := getAttParts(argument); !ok {
			return fmt.Errorf("Fn::GetAtt argument must name a resource attribute")
		}
	case intrinsicFindInMap:
		if !sequenceLength(argument, 3) {
			return fmt.Errorf("Fn::FindInMap requires three arguments")
		}
	case intrinsicJoin:
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 2 {
			return fmt.Errorf("Fn::Join requires a string delimiter and sequence values")
		}
		if _, ok := arguments[0].(string); !ok {
			return fmt.Errorf("Fn::Join delimiter must be a literal string")
		}
		if _, ok := arguments[1].([]any); !ok {
			return fmt.Errorf("Fn::Join requires a string delimiter and sequence values")
		}
	case intrinsicSelect:
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 2 {
			return fmt.Errorf("Fn::Select requires two arguments")
		}
		if _, ok := arguments[1].([]any); !ok {
			return fmt.Errorf("Fn::Select values must be a sequence")
		}
	case intrinsicSplit:
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 2 {
			return fmt.Errorf("Fn::Split requires string delimiter and source arguments")
		}
		if _, ok := arguments[0].(string); !ok {
			return fmt.Errorf("Fn::Split delimiter must be a literal string")
		}
		if !isStringExpression(arguments[1]) {
			return fmt.Errorf("Fn::Split source must be a string")
		}
	case intrinsicSub:
		if _, ok := argument.(string); ok {
			return nil
		}
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 2 {
			return fmt.Errorf("Fn::Sub argument must be a string or two-element sequence")
		}
		if _, ok := arguments[0].(string); !ok {
			return fmt.Errorf("Fn::Sub template must be a string")
		}
		if _, ok := arguments[1].(map[string]any); !ok {
			return fmt.Errorf("Fn::Sub variables must be a mapping")
		}
	case intrinsicIf:
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 3 {
			return fmt.Errorf("Fn::If requires three arguments")
		}
		if _, ok := arguments[0].(string); !ok {
			return fmt.Errorf("Fn::If condition must be a string")
		}
	case intrinsicEquals:
		if !sequenceLength(argument, 2) {
			return fmt.Errorf("Fn::Equals requires two arguments")
		}
	case intrinsicAnd, intrinsicOr:
		arguments, ok := argument.([]any)
		if !ok || len(arguments) < 2 || len(arguments) > 10 {
			return fmt.Errorf("%s requires two to ten arguments", name)
		}
		for _, value := range arguments {
			if !isConditionExpression(value) {
				return fmt.Errorf("%s arguments must be condition expressions", name)
			}
		}
	case intrinsicNot:
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 1 || !isConditionExpression(arguments[0]) {
			return fmt.Errorf("Fn::Not requires one condition expression")
		}
	case intrinsicImportValue:
		if !isStringExpression(argument) {
			return fmt.Errorf("Fn::ImportValue argument must be a string")
		}
		if resourceDependent(argument, resources) {
			return fmt.Errorf("Fn::ImportValue cannot depend on a resource")
		}
	}
	return nil
}

//nolint:nlreturn // Sequence shape checks return the combined type and length result.
func sequenceLength(value any, length int) bool {
	sequence, ok := value.([]any)
	return ok && len(sequence) == length
}

//nolint:nlreturn,wsl // String expressions are either literals or one intrinsic map.
func isStringExpression(value any) bool {
	if _, ok := value.(string); ok {
		return true
	}
	function, ok := value.(map[string]any)
	if !ok || len(function) != 1 {
		return false
	}
	for name, argument := range function {
		switch name {
		case intrinsicRef, intrinsicFindInMap, intrinsicGetAtt, intrinsicImportValue, intrinsicJoin, intrinsicSelect, intrinsicSub:
			return true
		case intrinsicIf:
			arguments, ok := argument.([]any)
			return ok && len(arguments) == 3 && isStringExpression(arguments[1]) && isStringExpression(arguments[2])
		default:
			return false
		}
	}
	return false
}

func supportedIntrinsic(name string) bool {
	switch name {
	case intrinsicRef, intrinsicCondition, intrinsicGetAtt, intrinsicFindInMap, intrinsicJoin, intrinsicSelect, intrinsicSplit, intrinsicSub, intrinsicIf, intrinsicEquals, intrinsicAnd, intrinsicOr, intrinsicNot, intrinsicImportValue:
		return true
	default:
		return false
	}
}

//nolint:cyclop,goconst,nlreturn,wsl // YAML node normalization deliberately handles every supported node form in one place.
func normalize(node *yaml.Node) (any, error) {
	if node.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("YAML aliases are unsupported")
	}
	if node.Tag != "" && node.Tag != "!!map" && node.Tag != "!!seq" && node.Tag != "!!str" && node.Tag != "!!int" && node.Tag != "!!float" && node.Tag != "!!bool" && node.Tag != "!!null" {
		function, ok := shortTags[node.Tag]
		if !ok {
			return nil, fmt.Errorf("unsupported YAML tag %q", node.Tag)
		}
		value, err := normalizeTaggedValue(node)
		if err != nil {
			return nil, err
		}
		return map[string]any{function: value}, nil
	}
	switch node.Kind {
	case yaml.MappingNode:
		result := make(map[string]any, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Kind != yaml.ScalarNode || node.Content[i].Tag != "!!str" {
				return nil, fmt.Errorf("mapping keys must be strings")
			}
			value, err := normalize(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			if _, exists := result[node.Content[i].Value]; exists {
				return nil, fmt.Errorf("duplicate mapping key %q", node.Content[i].Value)
			}
			result[node.Content[i].Value] = value
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, len(node.Content))
		for i := range node.Content {
			value, err := normalize(node.Content[i])
			if err != nil {
				return nil, err
			}
			result[i] = value
		}
		return result, nil
	case yaml.ScalarNode:
		return scalar(node)
	case yaml.DocumentNode, yaml.AliasNode:
		return nil, fmt.Errorf("unsupported YAML node")
	default:
		return nil, fmt.Errorf("unsupported YAML node")
	}
}

//nolint:nlreturn,wsl // Tagged values are normalized through the same node path.
func normalizeTaggedValue(node *yaml.Node) (any, error) {
	clone := *node
	switch clone.Kind {
	case yaml.ScalarNode:
		clone.Tag = "!!str"
	case yaml.MappingNode:
		clone.Tag = "!!map"
	case yaml.SequenceNode:
		clone.Tag = "!!seq"
	case yaml.DocumentNode, yaml.AliasNode:
		return nil, fmt.Errorf("unsupported YAML tag node")
	}
	return normalize(&clone)
}

//nolint:nlreturn,wsl // Scalar conversion branches return their canonical values directly.
func scalar(node *yaml.Node) (any, error) {
	switch node.Tag {
	case "!!str":
		return node.Value, nil
	case "!!int":
		value, err := strconv.ParseInt(node.Value, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("parse integer %q: %w", node.Value, err)
		}
		return value, nil
	case "!!float":
		value, err := strconv.ParseFloat(node.Value, 64)
		if err != nil {
			return nil, fmt.Errorf("parse float %q: %w", node.Value, err)
		}
		return value, nil
	case "!!bool":
		value, err := strconv.ParseBool(node.Value)
		if err != nil {
			return nil, fmt.Errorf("parse boolean %q: %w", node.Value, err)
		}
		return value, nil
	case "!!null":
		return nil, fmt.Errorf("null values are unsupported")
	default:
		return nil, fmt.Errorf("unsupported scalar tag %q", node.Tag)
	}
}

//nolint:cyclop,funlen,goconst,nlreturn,wsl // Section validation is intentionally centralized at the parsing boundary.
func parseTemplate(root map[string]any) (*Template, error) {
	template := &Template{parameters: map[string]parameter{}, mappings: map[string]any{}, conditions: map[string]any{}, resources: map[string]resource{}, outputs: map[string]output{}}
	resources := resourceNames(root)
	hasResources := false
	for section, value := range root {
		switch section {
		case "AWSTemplateFormatVersion", "Description":
			if _, ok := value.(string); !ok {
				return nil, fmt.Errorf("%s must be a string", section)
			}
		case "Transform":
			return nil, fmt.Errorf("transform is unsupported")
		case "Parameters":
			parameters, err := parseParameters(value)
			if err != nil {
				return nil, err
			}
			template.parameters = parameters
		case "Mappings":
			mappings, err := parseMappings(value)
			if err != nil {
				return nil, err
			}
			template.mappings = mappings
		case "Conditions":
			conditions, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("conditions must be a mapping")
			}
			for name, expression := range conditions {
				if !isConditionExpression(expression) {
					return nil, fmt.Errorf("condition %q must be a condition expression", name)
				}
			}
			template.conditions = conditions
		case "Resources":
			resources, err := parseResources(value)
			if err != nil {
				return nil, err
			}
			template.resources = resources
			hasResources = true
		case "Outputs":
			outputs, err := parseOutputs(value, resources)
			if err != nil {
				return nil, err
			}
			template.outputs = outputs
		default:
			return nil, fmt.Errorf("unsupported template section %q", section)
		}
	}
	if !hasResources {
		return nil, fmt.Errorf("resources section is required")
	}
	return template, nil
}

//nolint:nlreturn,wsl // Conditions accept only a single supported condition intrinsic.
func isConditionExpression(value any) bool {
	expression, ok := value.(map[string]any)
	if !ok || len(expression) != 1 {
		return false
	}
	for name := range expression {
		switch name {
		case intrinsicCondition, intrinsicAnd, intrinsicEquals, intrinsicNot, intrinsicOr:
			return true
		default:
			return false
		}
	}
	return false
}

//nolint:nlreturn,wsl // Mapping validation walks the required three-level CloudFormation shape.
func parseMappings(value any) (map[string]any, error) {
	mappings, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("mappings must be a mapping")
	}
	for name, mappingValue := range mappings {
		topLevel, ok := mappingValue.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mapping %q must be a mapping", name)
		}
		for topKey, attributesValue := range topLevel {
			attributes, ok := attributesValue.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("mapping %q key %q must be a mapping", name, topKey)
			}
			for attribute, attributeValue := range attributes {
				if _, ok := attributeValue.(string); !ok {
					return nil, fmt.Errorf("mapping %q key %q attribute %q must be a string", name, topKey, attribute)
				}
			}
		}
	}
	return mappings, nil
}

//nolint:nlreturn,wsl // Parameter declarations use a single strict shape.
func parseParameters(value any) (map[string]parameter, error) {
	definitions, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parameters must be a mapping")
	}
	result := make(map[string]parameter, len(definitions))
	for name, value := range definitions {
		definition, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("parameter %q must be a mapping", name)
		}
		parameter, err := parseParameter(name, definition)
		if err != nil {
			return nil, err
		}
		result[name] = parameter
	}
	return result, nil
}

//nolint:cyclop,funlen,gocognit,gocyclo,goconst,nlreturn,wsl // Parameter constraints are validated together to reject partial definitions.
func parseParameter(name string, definition map[string]any) (parameter, error) {
	result := parameter{}
	for key, value := range definition {
		switch key {
		case "Type":
			result.typeName, _ = value.(string)
		case "Default":
			defaultValue, ok := parameterScalar(value)
			if !ok {
				return result, fmt.Errorf("parameter %q Default must be a scalar", name)
			}
			result.defaultValue = defaultValue
			result.hasDefault = true
		case "AllowedValues":
			values, ok := value.([]any)
			if !ok {
				return result, fmt.Errorf("parameter %q AllowedValues must be a sequence", name)
			}
			for _, value := range values {
				allowedValue, ok := parameterScalar(value)
				if !ok {
					return result, fmt.Errorf("parameter %q AllowedValues must contain only scalars", name)
				}
				result.allowedValues = append(result.allowedValues, allowedValue)
			}
		case "AllowedPattern":
			var ok bool
			result.allowedPattern, ok = value.(string)
			if !ok {
				return result, fmt.Errorf("parameter %q AllowedPattern must be a string", name)
			}
			if _, err := regexp.Compile(result.allowedPattern); err != nil {
				return result, fmt.Errorf("parameter %q AllowedPattern: %w", name, err)
			}
		case "MinLength", "MaxLength":
			integer, ok := asInt(value)
			if !ok || integer < 0 {
				return result, fmt.Errorf("parameter %q %s must be a non-negative integer", name, key)
			}
			if key == "MinLength" {
				result.minLength = &integer
			} else {
				result.maxLength = &integer
			}
		case "MinValue", "MaxValue":
			number, ok := asFloat(value)
			if !ok {
				return result, fmt.Errorf("parameter %q %s must be a number", name, key)
			}
			if key == "MinValue" {
				result.minValue = &number
			} else {
				result.maxValue = &number
			}
		case "NoEcho":
			if _, ok := value.(bool); !ok {
				return result, fmt.Errorf("parameter %q NoEcho must be a boolean", name)
			}
		case "Description", "ConstraintDescription":
			if _, ok := value.(string); !ok {
				return result, fmt.Errorf("parameter %q %s must be a string", name, key)
			}
		default:
			return result, fmt.Errorf("parameter %q has unsupported field %q", name, key)
		}
	}
	if result.typeName != "String" && result.typeName != "Number" {
		return result, fmt.Errorf("parameter %q has unsupported Type %q", name, result.typeName)
	}
	if result.typeName == "String" && (result.minValue != nil || result.maxValue != nil) {
		return result, fmt.Errorf("parameter %q String type cannot declare MinValue or MaxValue", name)
	}
	if result.typeName == "Number" && (result.minLength != nil || result.maxLength != nil) {
		return result, fmt.Errorf("parameter %q Number type cannot declare MinLength or MaxLength", name)
	}
	if result.hasDefault {
		if err := result.validate(name, result.defaultValue); err != nil {
			return result, err
		}
	}
	return result, nil
}

func parameterScalar(value any) (string, bool) {
	switch value := value.(type) {
	case string, int64, float64, bool:
		return stringify(value), true
	default:
		return "", false
	}
}

//nolint:nlreturn,wsl // Resource definitions are validated as a single restricted shape.
func parseResources(value any) (map[string]resource, error) {
	definitions, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resources must be a mapping")
	}
	result := make(map[string]resource, len(definitions))
	for name, value := range definitions {
		definition, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resource %q must be a mapping", name)
		}
		resource := resource{properties: map[string]any{}}
		for key, value := range definition {
			switch key {
			case "Type":
				resource.typeName, _ = value.(string)
			case "Properties":
				resource.properties, ok = value.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("resource %q Properties must be a mapping", name)
				}
			case intrinsicCondition:
				resource.condition, ok = value.(string)
				if !ok {
					return nil, fmt.Errorf("resource %q Condition must be a string", name)
				}
			case "DependsOn":
				resource.dependsOn, ok = stringList(value)
				if !ok {
					return nil, fmt.Errorf("resource %q DependsOn must be a string or sequence", name)
				}
			default:
				return nil, fmt.Errorf("resource %q has unsupported field %q", name, key)
			}
		}
		if resource.typeName == "" {
			return nil, fmt.Errorf("resource %q Type is required", name)
		}
		result[name] = resource
	}
	return result, nil
}

//nolint:cyclop,gocognit,nlreturn,wsl // Output definitions are validated as a single restricted shape.
func parseOutputs(value any, resources map[string]struct{}) (map[string]output, error) {
	definitions, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("outputs must be a mapping")
	}
	result := make(map[string]output, len(definitions))
	for name, value := range definitions {
		definition, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("output %q must be a mapping", name)
		}
		output := output{}
		for key, value := range definition {
			switch key {
			case "Value":
				output.value = value
			case intrinsicCondition:
				output.condition, ok = value.(string)
				if !ok {
					return nil, fmt.Errorf("output %q Condition must be a string", name)
				}
			case "Export":
				export, ok := value.(map[string]any)
				if !ok || len(export) != 1 {
					return nil, fmt.Errorf("output %q Export must be a mapping", name)
				}
				exportName, ok := export["Name"]
				if !ok || !isExportName(exportName) {
					return nil, fmt.Errorf("output %q Export Name must be a string or intrinsic function", name)
				}
				if resourceDependent(exportName, resources) {
					return nil, fmt.Errorf("output %q Export Name cannot depend on a resource", name)
				}
				output.export = exportName
			case "Description":
				if _, ok := value.(string); !ok {
					return nil, fmt.Errorf("output %q Description must be a string", name)
				}
			default:
				return nil, fmt.Errorf("output %q has unsupported field %q", name, key)
			}
		}
		if output.value == nil {
			return nil, fmt.Errorf("output %q Value is required", name)
		}
		result[name] = output
	}
	return result, nil
}

func isExportName(value any) bool {
	return isStringExpression(value)
}

// Evaluate resolves parameter values, conditions, and supported intrinsic functions.
//
//nolint:cyclop,funlen,gocognit,nlreturn,wsl // Evaluation order is kept together to make condition filtering explicit.
func (t *Template) Evaluate(input EvaluationInput) (*Evaluation, error) {
	parameters := make(map[string]string, len(t.parameters))
	for name, definition := range t.parameters {
		value, ok := input.Parameters[name]
		if !ok {
			if !definition.hasDefault {
				return nil, fmt.Errorf("parameter %q is required", name)
			}
			value = definition.defaultValue
		}
		if err := definition.validate(name, value); err != nil {
			return nil, err
		}
		parameters[name] = value
	}
	for name := range input.Parameters {
		if _, ok := t.parameters[name]; !ok {
			return nil, fmt.Errorf("parameter %q is not defined", name)
		}
	}

	evaluator := evaluator{template: t, input: input, parameters: parameters, conditions: map[string]bool{}, evaluating: map[string]bool{}, activeResources: map[string]bool{}}
	for name := range t.conditions {
		if _, err := evaluator.condition(name); err != nil {
			return nil, err
		}
	}
	for name, definition := range t.resources {
		if definition.condition == "" {
			evaluator.activeResources[name] = true
			continue
		}
		condition, err := evaluator.condition(definition.condition)
		if err != nil {
			return nil, err
		}
		evaluator.activeResources[name] = condition
	}
	result := &Evaluation{Parameters: parameters, Resources: map[string]Resource{}, Outputs: map[string]Output{}}
	for name, definition := range t.resources {
		if !evaluator.activeResources[name] {
			continue
		}
		properties, err := evaluator.value(definition.properties)
		if err != nil {
			return nil, fmt.Errorf("resource %q properties: %w", name, err)
		}
		dependsOn := make([]string, 0, len(definition.dependsOn))
		for _, dependency := range definition.dependsOn {
			if _, ok := result.Resources[dependency]; ok {
				dependsOn = append(dependsOn, dependency)
				continue
			}
			if _, ok := t.resources[dependency]; ok && !evaluator.activeResources[dependency] {
				continue
			}
			if _, ok := t.resources[dependency]; !ok {
				return nil, fmt.Errorf("resource %q DependsOn unresolved resource %q", name, dependency)
			}
			dependsOn = append(dependsOn, dependency)
		}
		resolvedProperties, ok := properties.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resource %q Properties must resolve to a mapping", name)
		}
		result.Resources[name] = Resource{Type: definition.typeName, Properties: resolvedProperties, DependsOn: dependsOn}
	}
	for name, definition := range t.outputs {
		if definition.condition != "" {
			condition, err := evaluator.condition(definition.condition)
			if err != nil {
				return nil, err
			}
			if !condition {
				continue
			}
		}
		value, err := evaluator.value(definition.value)
		if err != nil {
			return nil, fmt.Errorf("output %q: %w", name, err)
		}
		output := Output{Value: value}
		if definition.export != nil {
			export, err := evaluator.value(definition.export)
			if err != nil {
				return nil, fmt.Errorf("output %q export: %w", name, err)
			}
			exportName, ok := export.(string)
			if !ok {
				return nil, fmt.Errorf("output %q Export Name must resolve to a string", name)
			}
			output.ExportName = exportName
		}
		result.Outputs[name] = output
	}
	return result, nil
}

type evaluator struct {
	template        *Template
	input           EvaluationInput
	parameters      map[string]string
	conditions      map[string]bool
	evaluating      map[string]bool
	activeResources map[string]bool
}

//nolint:nlreturn,wsl // Condition evaluation caches values and detects recursion in one control flow.
func (e *evaluator) condition(name string) (bool, error) {
	if value, ok := e.conditions[name]; ok {
		return value, nil
	}
	expression, ok := e.template.conditions[name]
	if !ok {
		return false, fmt.Errorf("condition %q is not defined", name)
	}
	if e.evaluating[name] {
		return false, fmt.Errorf("condition %q is recursive", name)
	}
	e.evaluating[name] = true
	value, err := e.value(expression)
	delete(e.evaluating, name)
	if err != nil {
		return false, fmt.Errorf("condition %q: %w", name, err)
	}
	boolean, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("condition %q must resolve to a boolean", name)
	}
	e.conditions[name] = boolean
	return boolean, nil
}

//nolint:nlreturn,wsl // Recursive value traversal resolves every child before returning its container.
func (e *evaluator) value(value any) (any, error) {
	switch value := value.(type) {
	case map[string]any:
		if len(value) == 1 {
			for name, argument := range value {
				if strings.HasPrefix(name, "Fn::") || name == "Ref" || name == "Condition" {
					return e.function(name, argument)
				}
			}
		}
		result := make(map[string]any, len(value))
		for key, child := range value {
			resolved, err := e.value(child)
			if err != nil {
				return nil, err
			}
			result[key] = resolved
		}
		return result, nil
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			resolved, err := e.value(child)
			if err != nil {
				return nil, err
			}
			result[i] = resolved
		}
		return result, nil
	default:
		return value, nil
	}
}

//nolint:cyclop,funlen,gocognit,gocyclo,nlreturn,wsl // Each supported intrinsic is validated at this single fail-closed dispatch point.
func (e *evaluator) function(name string, argument any) (any, error) {
	switch name {
	case "Ref":
		name, ok := argument.(string)
		if !ok {
			return nil, fmt.Errorf("ref argument must be a string")
		}
		if value, ok := e.parameters[name]; ok {
			return value, nil
		}
		if value, ok := e.input.PseudoParameters[name]; ok {
			return value, nil
		}
		if _, ok := e.template.resources[name]; !ok {
			return nil, fmt.Errorf("unresolved Ref %q", name)
		}
		if !e.activeResources[name] {
			return nil, fmt.Errorf("unresolved Ref %q", name)
		}
		if attributes, ok := e.input.ResourceAttributes[name]; ok {
			if value, ok := attributes["Ref"]; ok {
				return value, nil
			}
		}
		return nil, fmt.Errorf("unresolved Ref %q", name)
	case "Condition":
		name, ok := argument.(string)
		if !ok {
			return nil, fmt.Errorf("condition argument must be a string")
		}
		return e.condition(name)
	case "Fn::GetAtt":
		parts, ok := getAttParts(argument)
		if !ok {
			return nil, fmt.Errorf("Fn::GetAtt argument must name a resource attribute")
		}
		if _, ok := e.template.resources[parts[0]]; !ok {
			return nil, fmt.Errorf("unresolved Fn::GetAtt resource %q", parts[0])
		}
		if !e.activeResources[parts[0]] {
			return nil, fmt.Errorf("unresolved Fn::GetAtt resource %q", parts[0])
		}
		attributes, ok := e.input.ResourceAttributes[parts[0]]
		if !ok {
			return nil, fmt.Errorf("unresolved Fn::GetAtt resource %q", parts[0])
		}
		value, ok := attributes[parts[1]]
		if !ok {
			return nil, fmt.Errorf("unresolved Fn::GetAtt attribute %q", strings.Join(parts, "."))
		}
		return value, nil
	case "Fn::FindInMap":
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 3 {
			return nil, fmt.Errorf("Fn::FindInMap requires three arguments")
		}
		values, err := e.arguments(arguments)
		if err != nil {
			return nil, err
		}
		current, ok := e.template.mappings[stringify(values[0])]
		if !ok {
			return nil, fmt.Errorf("mapping %q is not defined", stringify(values[0]))
		}
		for _, key := range values[1:] {
			mapping, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("mapping value is not a mapping")
			}
			current, ok = mapping[stringify(key)]
			if !ok {
				return nil, fmt.Errorf("mapping key %q is not defined", stringify(key))
			}
		}
		return current, nil
	case "Fn::Join":
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 2 {
			return nil, fmt.Errorf("Fn::Join requires two arguments")
		}
		delimiter, err := e.value(arguments[0])
		if err != nil {
			return nil, err
		}
		values, err := e.value(arguments[1])
		if err != nil {
			return nil, err
		}
		items, ok := values.([]any)
		if !ok {
			return nil, fmt.Errorf("Fn::Join values must be a sequence")
		}
		delimiterString, ok := delimiter.(string)
		if !ok {
			return nil, fmt.Errorf("Fn::Join delimiter must be a string")
		}
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = stringify(item)
		}
		return strings.Join(parts, delimiterString), nil
	case "Fn::Select":
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 2 {
			return nil, fmt.Errorf("Fn::Select requires two arguments")
		}
		index, err := e.value(arguments[0])
		if err != nil {
			return nil, err
		}
		values, err := e.value(arguments[1])
		if err != nil {
			return nil, err
		}
		integer, err := strconv.Atoi(stringify(index))
		if err != nil {
			return nil, fmt.Errorf("Fn::Select index: %w", err)
		}
		items, ok := values.([]any)
		if !ok || integer < 0 || integer >= len(items) {
			return nil, fmt.Errorf("Fn::Select index is out of range")
		}
		return items[integer], nil
	case "Fn::Split":
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 2 {
			return nil, fmt.Errorf("Fn::Split requires two arguments")
		}
		delimiter, err := e.value(arguments[0])
		if err != nil {
			return nil, err
		}
		source, err := e.value(arguments[1])
		if err != nil {
			return nil, err
		}
		delimiterString, ok := delimiter.(string)
		if !ok {
			return nil, fmt.Errorf("Fn::Split delimiter must be a string")
		}
		sourceString, ok := source.(string)
		if !ok {
			return nil, fmt.Errorf("Fn::Split source must be a string")
		}
		strings := strings.Split(sourceString, delimiterString)
		result := make([]any, len(strings))
		for i := range strings {
			result[i] = strings[i]
		}
		return result, nil
	case "Fn::Sub":
		return e.sub(argument)
	case "Fn::If":
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 3 {
			return nil, fmt.Errorf("Fn::If requires three arguments")
		}
		name, ok := arguments[0].(string)
		if !ok {
			return nil, fmt.Errorf("Fn::If condition must be a string")
		}
		condition, err := e.condition(name)
		if err != nil {
			return nil, err
		}
		if condition {
			return e.value(arguments[1])
		}
		return e.value(arguments[2])
	case "Fn::Equals":
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 2 {
			return nil, fmt.Errorf("Fn::Equals requires two arguments")
		}
		values, err := e.arguments(arguments)
		if err != nil {
			return nil, err
		}
		return stringify(values[0]) == stringify(values[1]), nil
	case intrinsicAnd, intrinsicOr:
		arguments, ok := argument.([]any)
		if !ok || len(arguments) < 2 || len(arguments) > 10 {
			return nil, fmt.Errorf("%s requires two to ten arguments", name)
		}
		values, err := e.arguments(arguments)
		if err != nil {
			return nil, err
		}
		result := name == "Fn::And"
		for _, value := range values {
			boolean, ok := value.(bool)
			if !ok {
				return nil, fmt.Errorf("%s arguments must be booleans", name)
			}
			if name == "Fn::And" {
				result = result && boolean
			} else {
				result = result || boolean
			}
		}
		return result, nil
	case "Fn::Not":
		arguments, ok := argument.([]any)
		if !ok || len(arguments) != 1 {
			return nil, fmt.Errorf("Fn::Not requires one argument")
		}
		value, err := e.value(arguments[0])
		if err != nil {
			return nil, err
		}
		boolean, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("Fn::Not argument must be a boolean")
		}
		return !boolean, nil
	case "Fn::ImportValue":
		name, err := e.value(argument)
		if err != nil {
			return nil, err
		}
		nameString, ok := name.(string)
		if !ok {
			return nil, fmt.Errorf("Fn::ImportValue argument must resolve to a string")
		}
		value, ok := e.input.Imports[nameString]
		if !ok {
			return nil, fmt.Errorf("unresolved Fn::ImportValue %q", nameString)
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported intrinsic function %q", name)
	}
}

//nolint:nlreturn,wsl // Each argument is resolved independently before function-specific validation.
func (e *evaluator) arguments(arguments []any) ([]any, error) {
	values := make([]any, len(arguments))
	for i, argument := range arguments {
		value, err := e.value(argument)
		if err != nil {
			return nil, err
		}
		values[i] = value
	}
	return values, nil
}

//nolint:nlreturn,wsl // Fn::Sub has two distinct accepted CloudFormation forms.
func (e *evaluator) sub(argument any) (any, error) {
	var source string
	variables := map[string]any{}
	switch argument := argument.(type) {
	case string:
		source = argument
	case []any:
		if len(argument) != 2 {
			return nil, fmt.Errorf("Fn::Sub sequence must contain two arguments")
		}
		var ok bool
		source, ok = argument[0].(string)
		if !ok {
			return nil, fmt.Errorf("Fn::Sub template must be a string")
		}
		var values map[string]any
		values, ok = argument[1].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Fn::Sub variables must be a mapping")
		}
		for name, value := range values {
			resolved, err := e.value(value)
			if err != nil {
				return nil, err
			}
			variables[name] = resolved
		}
	default:
		return nil, fmt.Errorf("Fn::Sub argument must be a string or sequence")
	}
	var resolveErr error
	result := subPattern.ReplaceAllStringFunc(source, func(match string) string {
		name := match[2 : len(match)-1]
		if strings.HasPrefix(name, "!") {
			return "${" + name[1:] + "}"
		}
		if value, ok := variables[name]; ok {
			return stringify(value)
		}
		value, err := e.subReference(name)
		if err != nil {
			resolveErr = err
			return match
		}
		return stringify(value)
	})
	if resolveErr != nil {
		return nil, resolveErr
	}
	return result, nil
}

var subPattern = regexp.MustCompile(`\$\{[^}]+\}`)

//nolint:nlreturn,wsl // Fn::Sub resolves attributes before falling back to Ref.
func (e *evaluator) subReference(name string) (any, error) {
	if parts := strings.SplitN(name, ".", 2); len(parts) == 2 {
		return e.function("Fn::GetAtt", []any{parts[0], parts[1]})
	}
	return e.function("Ref", name)
}

//nolint:cyclop,gocognit,nlreturn,wsl // Constraint checks must report the specific failed parameter rule.
func (p *parameter) validate(name, value string) error {
	if p.typeName == "Number" {
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return fmt.Errorf("parameter %q must be a number", name)
		}
	}
	if len(p.allowedValues) > 0 {
		found := false
		for _, allowed := range p.allowedValues {
			if value == allowed {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("parameter %q is not an allowed value", name)
		}
	}
	if p.allowedPattern != "" {
		pattern, err := regexp.Compile(p.allowedPattern)
		if err != nil {
			return fmt.Errorf("parameter %q AllowedPattern: %w", name, err)
		}
		match := pattern.FindStringIndex(value)
		if len(match) != 2 || match[0] != 0 || match[1] != len(value) {
			return fmt.Errorf("parameter %q does not match AllowedPattern", name)
		}
	}
	length := utf8.RuneCountInString(value)
	if p.minLength != nil && length < *p.minLength {
		return fmt.Errorf("parameter %q is shorter than MinLength", name)
	}
	if p.maxLength != nil && length > *p.maxLength {
		return fmt.Errorf("parameter %q is longer than MaxLength", name)
	}
	if p.minValue != nil || p.maxValue != nil {
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("parameter %q must be a number", name)
		}
		if p.minValue != nil && number < *p.minValue {
			return fmt.Errorf("parameter %q is less than MinValue", name)
		}
		if p.maxValue != nil && number > *p.maxValue {
			return fmt.Errorf("parameter %q is greater than MaxValue", name)
		}
	}
	return nil
}

//nolint:nlreturn,wsl // Fn::GetAtt has two accepted CloudFormation forms.
func getAttParts(value any) ([]string, bool) {
	switch value := value.(type) {
	case string:
		parts := strings.SplitN(value, ".", 2)
		return parts, len(parts) == 2 && parts[0] != "" && parts[1] != ""
	case []any:
		if len(value) != 2 {
			return nil, false
		}
		resource, resourceOK := value[0].(string)
		attribute, attributeOK := value[1].(string)
		if !resourceOK || !attributeOK {
			return nil, false
		}
		return []string{resource, attribute}, true
	default:
		return nil, false
	}
}

//nolint:nlreturn,wsl // DependsOn accepts either one string or a sequence of strings.
func stringList(value any) ([]string, bool) {
	if value, ok := value.(string); ok {
		return []string{value}, true
	}
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]string, len(values))
	for i, value := range values {
		stringValue, ok := value.(string)
		if !ok {
			return nil, false
		}
		result[i] = stringValue
	}
	return result, true
}

//nolint:nlreturn,wsl // Bounds must be representable as platform integers.
func asInt(value any) (int, bool) {
	integer, ok := value.(int64)
	if !ok || integer > int64(^uint(0)>>1) {
		return 0, false
	}
	return int(integer), true
}

func asFloat(value any) (float64, bool) {
	switch value := value.(type) {
	case int64:
		return float64(value), true
	case float64:
		return value, true
	default:
		return 0, false
	}
}

func stringify(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case int64:
		return strconv.FormatInt(value, 10)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	default:
		return fmt.Sprint(value)
	}
}
