package cloudformation

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sivchari/kumo/internal/service/cloudformation/resource"
	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

// DependencyGraph contains the resource dependencies for one evaluated stack.
type DependencyGraph struct {
	dependencies map[string]map[string]struct{}
}

// BuildDependencyGraph parses and evaluates body, then builds dependencies from
// the source expressions while using the evaluation to select active resources.
func BuildDependencyGraph(body string, input template.EvaluationInput, registry *resource.Registry) (*DependencyGraph, error) {
	parsed, err := template.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}

	evaluation, err := parsed.Evaluate(input)
	if err != nil {
		return nil, fmt.Errorf("evaluate template: %w", err)
	}

	root, err := parseGraphTemplate(body)
	if err != nil {
		return nil, fmt.Errorf("parse dependency expressions: %w", err)
	}

	resources, ok := root["Resources"].(map[string]any)

	if !ok {
		return nil, fmt.Errorf("template resources must be a mapping")
	}

	conditions := evaluation.Conditions

	if registry == nil {
		registry = resource.DefaultRegistry()
	}

	graph := &DependencyGraph{dependencies: make(map[string]map[string]struct{}, len(evaluation.Resources))}
	active := make(map[string]struct{}, len(evaluation.Resources))
	resourceTypes := graphResourceTypes(resources)

	graph.initializeActiveResources(evaluation.Resources, active)

	for name, evaluated := range evaluation.Resources {
		for _, dependency := range evaluated.DependsOn {
			if _, ok := active[dependency]; !ok {
				continue
			}

			graph.dependencies[name][dependency] = struct{}{}
		}

		definition, ok := resources[name].(map[string]any)

		if !ok {
			return nil, fmt.Errorf("resource %q must be a mapping", name)
		}

		properties := definition["Properties"]

		if err := graphValue(properties, graph.dependencies[name], active, conditions, nil, resourceTypes, registry); err != nil {
			return nil, fmt.Errorf("resource %q dependencies: %w", name, err)
		}
	}

	if _, err := graph.createOrder(); err != nil {
		return nil, err
	}

	return graph, nil
}

func (g *DependencyGraph) initializeActiveResources(resources map[string]template.Resource, active map[string]struct{}) {
	for name := range resources {
		active[name] = struct{}{}
		g.dependencies[name] = make(map[string]struct{})
	}
}

func graphResourceTypes(resources map[string]any) map[string]string {
	resourceTypes := make(map[string]string, len(resources))

	for name, definition := range resources {
		if definition, ok := definition.(map[string]any); ok {
			resourceTypes[name], _ = definition["Type"].(string)
		}
	}

	return resourceTypes
}

// Dependencies returns a sorted copy of the direct dependencies of logicalID.
func (g *DependencyGraph) Dependencies(logicalID string) []string {
	dependencies := append([]string(nil), mapKeys(g.dependencies[logicalID])...)
	sort.Strings(dependencies)

	return dependencies
}

// CreateOrder returns a deterministic dependency-first resource order.
func (g *DependencyGraph) CreateOrder() []string {
	order, _ := g.createOrder()

	return order
}

// DeleteOrder returns the reverse of the deterministic creation order.
func (g *DependencyGraph) DeleteOrder() []string {
	order := g.CreateOrder()
	for left, right := 0, len(order)-1; left < right; left, right = left+1, right-1 {
		order[left], order[right] = order[right], order[left]
	}

	return order
}

func (g *DependencyGraph) createOrder() ([]string, error) {
	remaining := make(map[string]map[string]struct{}, len(g.dependencies))
	for name, dependencies := range g.dependencies {
		remaining[name] = make(map[string]struct{}, len(dependencies))
		for dependency := range dependencies {
			remaining[name][dependency] = struct{}{}
		}
	}

	order := make([]string, 0, len(remaining))

	for len(remaining) > 0 {
		ready := make([]string, 0)

		for name, dependencies := range remaining {
			if len(dependencies) == 0 {
				ready = append(ready, name)
			}
		}

		if len(ready) == 0 {
			return nil, fmt.Errorf("resource dependency cycle detected")
		}

		sort.Strings(ready)

		for _, name := range ready {
			order = append(order, name)
			delete(remaining, name)
		}

		for _, dependencies := range remaining {
			for _, name := range ready {
				delete(dependencies, name)
			}
		}
	}

	return order, nil
}

//nolint:goconst // Intrinsic names are part of the CloudFormation expression syntax.
func graphValue(value any, dependencies, active map[string]struct{}, conditions map[string]bool, locals map[string]struct{}, resourceTypes map[string]string, registry *resource.Registry) error {
	switch value := value.(type) {
	case map[string]any:
		if len(value) == 1 {
			for name, argument := range value {
				switch name {
				case "Ref":
					return graphRef(argument, dependencies, active)
				case "Fn::GetAtt":
					return graphGetAtt(argument, dependencies, active, resourceTypes, registry)
				case "Fn::If":
					return graphIf(argument, dependencies, active, conditions, locals, resourceTypes, registry)
				case "Fn::Sub":
					return graphSub(argument, dependencies, active, conditions, locals, resourceTypes, registry)
				}
			}
		}

		for _, child := range value {
			if err := graphValue(child, dependencies, active, conditions, locals, resourceTypes, registry); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := graphValue(child, dependencies, active, conditions, locals, resourceTypes, registry); err != nil {
				return err
			}
		}
	}

	return nil
}

func graphRef(value any, dependencies, active map[string]struct{}) error {
	name, ok := value.(string)
	if !ok {
		return fmt.Errorf("ref argument must be a string")
	}

	if _, ok := active[name]; ok {
		dependencies[name] = struct{}{}
	}

	return nil
}

func graphGetAtt(value any, dependencies, active map[string]struct{}, resourceTypes map[string]string, registry *resource.Registry) error {
	parts, ok := graphGetAttParts(value)
	if !ok {
		return fmt.Errorf("Fn::GetAtt argument must name a resource attribute")
	}

	if _, ok := active[parts[0]]; !ok {
		return nil
	}

	if err := validateGraphAttribute(parts[0], parts[1], resourceTypes, registry); err != nil {
		return err
	}

	dependencies[parts[0]] = struct{}{}

	return nil
}

func validateGraphAttribute(resourceName, attribute string, resourceTypes map[string]string, registry *resource.Registry) error {
	typeName, ok := resourceTypes[resourceName]

	if !ok || registry == nil {
		return nil
	}

	adapter, found := registry.Get(typeName)

	if !found {
		return nil
	}

	for _, supportedAttribute := range adapter.Schema().Attributes {
		if supportedAttribute == attribute {
			return nil
		}
	}

	return fmt.Errorf("Fn::GetAtt attribute %q is not declared by %s", attribute, typeName)
}

func graphIf(value any, dependencies, active map[string]struct{}, conditions map[string]bool, locals map[string]struct{}, resourceTypes map[string]string, registry *resource.Registry) error {
	arguments, ok := value.([]any)
	if !ok || len(arguments) != 3 {
		return fmt.Errorf("Fn::If requires three arguments")
	}

	conditionName, ok := arguments[0].(string)

	if !ok {
		return fmt.Errorf("Fn::If condition must be a string")
	}

	condition, ok := conditions[conditionName]

	if !ok {
		return fmt.Errorf("condition %q is not defined", conditionName)
	}

	selected := arguments[2]

	if condition {
		selected = arguments[1]
	}

	return graphValue(selected, dependencies, active, conditions, locals, resourceTypes, registry)
}

func graphSub(value any, dependencies, active map[string]struct{}, conditions map[string]bool, locals map[string]struct{}, resourceTypes map[string]string, registry *resource.Registry) error {
	var source string

	variables := map[string]any{}

	switch value := value.(type) {
	case string:
		source = value
	case []any:
		if len(value) != 2 {
			return fmt.Errorf("Fn::Sub requires a string or two-element sequence")
		}

		var ok bool

		source, ok = value[0].(string)
		if !ok {
			return fmt.Errorf("Fn::Sub template must be a string")
		}

		variables, ok = value[1].(map[string]any)

		if !ok {
			return fmt.Errorf("Fn::Sub variables must be a mapping")
		}

		for name, child := range variables {
			if err := graphValue(child, dependencies, active, conditions, locals, resourceTypes, registry); err != nil {
				return fmt.Errorf("Fn::Sub variable %q: %w", name, err)
			}
		}
	default:
		return fmt.Errorf("Fn::Sub argument must be a string or sequence")
	}

	for _, match := range graphSubReferences(source) {
		if strings.HasPrefix(match, "!") {
			continue
		}

		if _, local := variables[match]; local {
			continue
		}

		name := strings.SplitN(match, ".", 2)[0]
		if _, ok := active[name]; ok {
			if strings.Contains(match, ".") {
				parts := strings.SplitN(match, ".", 2)
				if err := graphGetAtt([]any{parts[0], parts[1]}, dependencies, active, resourceTypes, registry); err != nil {
					return err
				}

				continue
			}

			dependencies[name] = struct{}{}
		}
	}

	return nil
}

func graphSubReferences(value string) []string {
	var result []string

	for start := 0; start < len(value); {
		begin := strings.Index(value[start:], "${")
		if begin < 0 {
			break
		}

		begin += start + 2
		end := strings.IndexByte(value[begin:], '}')

		if end < 0 {
			break
		}

		result = append(result, value[begin:begin+end])
		start = begin + end + 1
	}

	return result
}

func graphGetAttParts(value any) ([]string, bool) {
	switch value := value.(type) {
	case string:
		parts := strings.SplitN(value, ".", 2)

		return parts, len(parts) == 2 && parts[0] != "" && parts[1] != ""
	case []any:
		if len(value) != 2 {
			return nil, false
		}

		resourceName, resourceOK := value[0].(string)
		attribute, attributeOK := value[1].(string)

		return []string{resourceName, attribute}, resourceOK && attributeOK && resourceName != "" && attribute != ""
	case []string:
		if len(value) != 2 {
			return nil, false
		}

		return value, value[0] != "" && value[1] != ""
	default:
		return nil, false
	}
}

func parseGraphTemplate(body string) (map[string]any, error) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(body), &node); err != nil {
		return nil, fmt.Errorf("unmarshal template: %w", err)
	}

	if len(node.Content) != 1 {
		return nil, fmt.Errorf("template must contain one document")
	}

	value, err := normalizeGraphNode(node.Content[0])
	if err != nil {
		return nil, err
	}

	root, ok := value.(map[string]any)

	if !ok {
		return nil, fmt.Errorf("template must be a mapping")
	}

	return root, nil
}

func normalizeGraphNode(node *yaml.Node) (any, error) {
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return nil, fmt.Errorf("template must contain one document")
		}

		return normalizeGraphNode(node.Content[0])
	}

	if !isGraphStandardTag(node.Tag) {
		value, err := normalizeGraphTaggedValue(node)
		if err != nil {
			return nil, err
		}

		return map[string]any{graphShortTag(node.Tag): value}, nil
	}

	return normalizeGraphNodeValue(node)
}

func isGraphStandardTag(tag string) bool {
	switch tag {
	case "", "!!map", "!!seq", "!!str", "!!int", "!!float", "!!bool":
		return true
	default:
		return false
	}
}

func normalizeGraphNodeValue(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.MappingNode:
		return normalizeGraphMapping(node)
	case yaml.SequenceNode:
		return normalizeGraphSequence(node)
	case yaml.ScalarNode:
		return normalizeGraphScalar(node)
	case yaml.DocumentNode, yaml.AliasNode:
		return nil, fmt.Errorf("unsupported YAML node")
	default:
		return nil, fmt.Errorf("unsupported YAML node")
	}
}

func normalizeGraphMapping(node *yaml.Node) (map[string]any, error) {
	result := make(map[string]any, len(node.Content)/2)

	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("mapping keys must be scalars")
		}

		value, err := normalizeGraphNode(node.Content[index+1])
		if err != nil {
			return nil, err
		}

		result[key.Value] = value
	}

	return result, nil
}

func normalizeGraphSequence(node *yaml.Node) ([]any, error) {
	result := make([]any, len(node.Content))

	for index, child := range node.Content {
		value, err := normalizeGraphNode(child)
		if err != nil {
			return nil, err
		}

		result[index] = value
	}

	return result, nil
}

func normalizeGraphScalar(node *yaml.Node) (any, error) {
	switch node.Tag {
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
	default:
		return node.Value, nil
	}
}

func normalizeGraphTaggedValue(node *yaml.Node) (any, error) {
	clone := *node
	switch clone.Kind {
	case yaml.ScalarNode:
		clone.Tag = "!!str"
	case yaml.MappingNode:
		clone.Tag = "!!map"
	case yaml.SequenceNode:
		clone.Tag = "!!seq"
	case yaml.DocumentNode, yaml.AliasNode:
		return nil, fmt.Errorf("unsupported intrinsic node")
	default:
		return nil, fmt.Errorf("unsupported intrinsic node")
	}

	return normalizeGraphNode(&clone)
}

func graphShortTag(tag string) string {
	return map[string]string{
		"!Ref": "Ref", "!Condition": "Condition", "!GetAtt": "Fn::GetAtt", "!Sub": "Fn::Sub", "!If": "Fn::If",
		"!Equals": "Fn::Equals", "!And": "Fn::And", "!Or": "Fn::Or", "!Not": "Fn::Not", "!Join": "Fn::Join", "!Select": "Fn::Select", "!Split": "Fn::Split", "!FindInMap": "Fn::FindInMap", "!ImportValue": "Fn::ImportValue",
	}[tag]
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	return keys
}
