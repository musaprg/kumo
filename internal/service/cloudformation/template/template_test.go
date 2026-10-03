//nolint:goconst // Template fixtures intentionally repeat intrinsic and logical names.
package template

import (
	"reflect"
	"strings"
	"testing"
)

const (
	testCapacity    = "Capacity"
	testEnvironment = "Environment"
	testARN         = "Arn"
	testThingARN    = "arn:thing"
	testOwner       = "owner"
)

//nolint:wsl // Each parser form is evaluated independently in this end-to-end test.
func TestParseNormalizesJSONAndYAMLShortTags(t *testing.T) {
	t.Parallel()

	jsonTemplate, err := Parse(`{"Resources":{"Bucket":{"Type":"AWS::S3::Bucket","Properties":{"Name":{"Fn::Sub":"${AWS::StackName}-${!Literal}"}}}}}`)
	if err != nil {
		t.Fatalf("Parse(JSON) error = %v", err)
	}
	jsonEvaluation, err := jsonTemplate.Evaluate(EvaluationInput{PseudoParameters: map[string]string{"AWS::StackName": "stack"}})
	if err != nil {
		t.Fatalf("Evaluate(JSON) error = %v", err)
	}
	if got := jsonEvaluation.Resources["Bucket"].Properties["Name"]; got != "stack-${Literal}" {
		t.Errorf("JSON property = %#v, want %q", got, "stack-${Literal}")
	}

	yamlTemplate, err := Parse(`
Mappings:
  Regions:
    us-east-1:
      AMI: ami-123
Resources:
  Instance:
    Type: AWS::EC2::Instance
    Properties:
      ImageId: !FindInMap [Regions, !Ref AWS::Region, AMI]
`)
	if err != nil {
		t.Fatalf("Parse(YAML) error = %v", err)
	}
	yamlEvaluation, err := yamlTemplate.Evaluate(EvaluationInput{PseudoParameters: map[string]string{"AWS::Region": "us-east-1"}})
	if err != nil {
		t.Fatalf("Evaluate(YAML) error = %v", err)
	}
	if got := yamlEvaluation.Resources["Instance"].Properties["ImageId"]; got != "ami-123" {
		t.Errorf("YAML short tag property = %#v, want %q", got, "ami-123")
	}
}

//nolint:wsl // The template and assertions repeat parameter names intentionally.
func TestEvaluateAppliesParameterDefaultsAndConstraints(t *testing.T) {
	t.Parallel()

	template, err := Parse(`
Parameters:
  Environment:
    Type: String
    Default: dev
    AllowedValues: [dev, prod]
    AllowedPattern: '^[a-z]+$'
    MinLength: 3
    MaxLength: 4
  Capacity:
    Type: Number
    Default: 2
    MinValue: 1
    MaxValue: 3
Resources:
  Thing:
    Type: Custom::Thing
    Properties:
      Environment: !Ref Environment
      Capacity: !Ref Capacity
`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	evaluation, err := template.Evaluate(EvaluationInput{})
	if err != nil {
		t.Fatalf("Evaluate(defaults) error = %v", err)
	}
	if got, want := evaluation.Parameters, map[string]string{testEnvironment: "dev", testCapacity: "2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Parameters = %#v, want %#v", got, want)
	}

	for _, parameters := range []map[string]string{
		{testEnvironment: "test"},
		{testEnvironment: "DEV"},
		{testEnvironment: "de"},
		{testEnvironment: "prod", testCapacity: "4"},
		{testEnvironment: "prod", testCapacity: "not-a-number"},
	} {
		if _, err := template.Evaluate(EvaluationInput{Parameters: parameters}); err == nil {
			t.Errorf("Evaluate(%#v) error = nil, want constraint violation", parameters)
		}
	}
}

//nolint:goconst,wsl // Assertions cover condition filtering and the selected Fn::If branch.
func TestEvaluateResolvesConditionsAndSelectedIfBranch(t *testing.T) {
	t.Parallel()

	template, err := Parse(`
Parameters:
  CreateThing:
    Type: String
    Default: "false"
Mappings:
  Names:
    enabled:
      Value: selected
Conditions:
  IsEnabled: !Equals [!Ref CreateThing, "true"]
Resources:
  Conditional:
    Type: Custom::Thing
    Condition: IsEnabled
  Always:
    Type: Custom::Thing
    DependsOn: Conditional
    Properties:
      Name: !If [IsEnabled, !FindInMap [Names, missing, Value], fallback]
Outputs:
  Result:
    Value: !If [IsEnabled, !Ref Missing, fallback]
    Export:
      Name: !If [IsEnabled, !Ref Missing, exported]
`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	evaluation, err := template.Evaluate(EvaluationInput{})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if _, ok := evaluation.Resources["Conditional"]; ok {
		t.Fatal("condition-false resource was retained")
	}
	if got := evaluation.Resources["Always"].Properties["Name"]; got != "fallback" {
		t.Errorf("selected property = %#v, want %q", got, "fallback")
	}
	if got := evaluation.Resources["Always"].DependsOn; len(got) != 0 {
		t.Errorf("DependsOn = %#v, want empty after condition filtering", got)
	}
	if got := evaluation.Outputs["Result"]; got.Value != "fallback" || got.ExportName != "exported" {
		t.Errorf("Output = %#v, want selected values", got)
	}
}

//nolint:wsl // The fixture covers resource/output conditions and both Fn::If branches.
func TestImportedExportNamesHonorsConditionsAndSelectedIfBranches(t *testing.T) {
	t.Parallel()
	tmpl, err := Parse(`
Parameters:
  Enabled:
    Type: String
    Default: "false"
Conditions:
  IsEnabled: !Equals [!Ref Enabled, "true"]
Resources:
  Conditional:
    Type: Custom::Thing
    Condition: IsEnabled
    Properties:
      Value: !ImportValue InactiveResourceExport
  Selected:
    Type: Custom::Thing
    Properties:
      Value: !If [IsEnabled, !ImportValue TrueBranchExport, !ImportValue FalseBranchExport]
Outputs:
  Conditional:
    Condition: IsEnabled
    Value: !ImportValue InactiveOutputExport
  Selected:
    Value: !If [IsEnabled, !ImportValue TrueOutputExport, !ImportValue FalseOutputExport]
`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	names, err := tmpl.ImportedExportNames(EvaluationInput{})
	if err != nil {
		t.Fatalf("ImportedExportNames() error = %v", err)
	}
	want := []string{"FalseBranchExport", "FalseOutputExport"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("inactive import names = %v, want %v", names, want)
	}

	names, err = tmpl.ImportedExportNames(EvaluationInput{Parameters: map[string]string{"Enabled": "true"}})
	if err != nil {
		t.Fatalf("ImportedExportNames(enabled) error = %v", err)
	}
	want = []string{"InactiveOutputExport", "InactiveResourceExport", "TrueBranchExport", "TrueOutputExport"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("active import names = %v, want %v", names, want)
	}
}

//nolint:wsl // Assertions intentionally enumerate each supported intrinsic result.
func TestEvaluateSupportsIntrinsicFunctionsAndPseudoParameters(t *testing.T) {
	t.Parallel()

	template, err := Parse(`
Mappings:
  Data:
    key:
      value: mapped
Conditions:
  Combined: !And [!Equals [one, one], !Not [!Equals [one, two]]]
Resources:
  Thing:
    Type: Custom::Thing
    Properties:
      Joined: {"Fn::Join": [":", [!Ref AWS::Region, !Select [1, [zero, one]], !FindInMap [Data, key, value]]]}
      Split: !Split [",", "a,b"]
      Sub: !Sub ["${Prefix}-${AWS::AccountId}-${Thing.Arn}", {Prefix: local}]
      Import: {"Fn::ImportValue": exported-name}
      Attribute: !GetAtt Thing.Arn
Outputs:
  Condition:
    Value: !Condition Combined
`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	evaluation, err := template.Evaluate(EvaluationInput{
		PseudoParameters: map[string]string{"AWS::Region": "us-east-1", "AWS::AccountId": "123456789012"},
		Imports:          map[string]string{"exported-name": "imported"},
		ResourceAttributes: map[string]map[string]string{
			"Thing": {testARN: testThingARN},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}

	properties := evaluation.Resources["Thing"].Properties
	if got := properties["Joined"]; got != "us-east-1:one:mapped" {
		t.Errorf("Joined = %#v", got)
	}
	if got, want := properties["Split"], []any{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Split = %#v, want %#v", got, want)
	}
	if got := properties["Sub"]; got != "local-123456789012-arn:thing" {
		t.Errorf("Sub = %#v", got)
	}
	if got := properties["Import"]; got != "imported" {
		t.Errorf("Import = %#v", got)
	}
	if got := properties["Attribute"]; got != testThingARN {
		t.Errorf("Attribute = %#v", got)
	}
	if got, ok := evaluation.Outputs["Condition"].Value.(bool); !ok || !got {
		t.Errorf("Condition = %#v, want true", got)
	}
}

//nolint:wsl // The test keeps incremental evaluation and its assertions together.
func TestEvaluateResourcesInDependencyOrderUsesNewAttributesForPropertiesAndOutputs(t *testing.T) {
	parsed, err := Parse(`
Resources:
  Producer:
    Type: Custom::Producer
  Consumer:
    Type: Custom::Consumer
    Properties:
      Source: !GetAtt Producer.Arn
Outputs:
  Source:
    Value: !GetAtt Producer.Arn
`)
	if err != nil {
		t.Fatal(err)
	}

	evaluation, err := parsed.EvaluateResources(EvaluationInput{}, []string{"Producer"})
	if err != nil {
		t.Fatalf("EvaluateResources(Producer) error = %v", err)
	}
	if _, ok := evaluation.Resources["Consumer"]; ok {
		t.Fatal("EvaluateResources(Producer) evaluated dependent resource")
	}

	attributes := map[string]map[string]string{"Producer": {"Ref": "producer-id", "Arn": "arn:producer"}}
	evaluation, err = parsed.EvaluateResources(EvaluationInput{ResourceAttributes: attributes}, []string{"Producer", "Consumer"})
	if err != nil {
		t.Fatalf("EvaluateResources(all) error = %v", err)
	}
	if got := evaluation.Resources["Consumer"].Properties["Source"]; got != "arn:producer" {
		t.Fatalf("consumer source = %#v, want arn:producer", got)
	}
	if got := evaluation.Outputs["Source"].Value; got != "arn:producer" {
		t.Fatalf("output source = %#v, want arn:producer", got)
	}
}

//nolint:wsl // Invalid inputs are independently asserted to fail closed.
func TestParseAndEvaluateRejectUnsupportedOrInvalidTemplates(t *testing.T) {
	t.Parallel()
	if _, err := Parse("", "https://example.invalid/template.yaml"); err == nil {
		t.Error("Parse(TemplateURL) error = nil, want rejection")
	}

	for name, body := range map[string]string{
		"invalid YAML":                "Resources: [",
		"unsupported YAML tag":        "Resources:\n  Thing:\n    Type: Custom::Thing\n    Properties: !Base64 value",
		"transform":                   "Transform: AWS::Serverless-2016-10-31\nResources: {}",
		"malformed resources":         "Resources: []",
		"malformed parameter":         "Parameters:\n  Name: string\nResources: {}",
		"unsupported intrinsic":       `{"Resources":{"Thing":{"Type":"Custom::Thing","Properties":{"Value":{"Fn::Cidr":[]}}}}}`,
		"unresolved reference":        "Resources:\n  Thing:\n    Type: Custom::Thing\n    Properties:\n      Value: !Ref Unknown",
		"unresolved import":           "Resources:\n  Thing:\n    Type: Custom::Thing\n    Properties:\n      Value: !ImportValue Unknown",
		"invalid condition reference": "Conditions:\n  Enabled: !Condition Missing\nResources: {}",
	} {
		t.Run(name, func(t *testing.T) {
			template, err := Parse(body)
			if err == nil {
				_, err = template.Evaluate(EvaluationInput{})
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.TrimSpace(err.Error()) == "" {
				t.Fatal("error message is empty")
			}
		})
	}
}

func TestParseRejectsMissingResourcesAndMalformedMappings(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"missing Resources": "Description: no resources",
		"invalid mapping level": `
Mappings:
  Names: value
Resources: {}`,
		"invalid mapping value": `
Mappings:
  Names:
    development:
      Value: [not, scalar]
Resources: {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(body); err == nil {
				t.Fatal("Parse() error = nil, want invalid section shape")
			}
		})
	}
}

func TestParseRejectsMalformedResourceAndOutputFields(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"resource Condition is not a string": `
Resources:
  Thing:
    Type: Custom::Thing
    Condition: true`,
		"output Condition is not a string": `
Resources: {}
Outputs:
  Result:
    Value: value
    Condition: true`,
		"output Export lacks Name": `
Resources: {}
Outputs:
  Result:
    Value: value
    Export: {}`,
		"output Export Name is not a string": `
Resources: {}
Outputs:
  Result:
    Value: value
    Export:
      Name: [invalid]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(body); err == nil {
				t.Fatal("Parse() error = nil, want invalid field shape")
			}
		})
	}
}

func TestParseRejectsMalformedParameterValueShapes(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"Default is not a scalar": `
Parameters:
  Name:
    Type: String
    Default: [invalid]
Resources: {}`,
		"AllowedValues contains non-scalar": `
Parameters:
  Name:
    Type: String
    AllowedValues: [valid, {invalid: value}]
Resources: {}`,
		"AllowedPattern is not a string": `
Parameters:
  Name:
    Type: String
    AllowedPattern: 1
Resources: {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(body); err == nil {
				t.Fatal("Parse() error = nil, want invalid parameter shape")
			}
		})
	}
}

//nolint:wsl // Each intrinsic form is independently evaluated with the same undeclared resource.
func TestEvaluateRejectsAttributesForUndeclaredResources(t *testing.T) {
	t.Parallel()

	for name, property := range map[string]string{
		intrinsicRef: "Value: !Ref Missing",
		"GetAtt":     "Value: !GetAtt Missing.Arn",
	} {
		t.Run(name, func(t *testing.T) {
			template, err := Parse("Resources:\n  Thing:\n    Type: Custom::Thing\n    Properties:\n      " + property)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			_, err = template.Evaluate(EvaluationInput{ResourceAttributes: map[string]map[string]string{
				"Missing": {intrinsicRef: "unexpected", testARN: "arn:unexpected"},
			}})
			if err == nil {
				t.Fatal("Evaluate() error = nil, want undeclared resource rejection")
			}
		})
	}
}

func TestParseRejectsMalformedParameterMetadata(t *testing.T) {
	t.Parallel()

	for name, field := range map[string]string{
		"NoEcho is not boolean":               "NoEcho: \"true\"",
		"Description is not string":           "Description: 1",
		"ConstraintDescription is not string": "ConstraintDescription: false",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("Parameters:\n  Name:\n    Type: String\n    " + field + "\nResources: {}"); err == nil {
				t.Fatal("Parse() error = nil, want invalid parameter metadata")
			}
		})
	}
}

func TestParseRejectsMultiKeyIntrinsicMaps(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		intrinsicRef:     "{Ref: Name, Extra: value}",
		"supported Fn":   "{'Fn::Join': [':', [one, two]], Extra: value}",
		"unsupported Fn": "{'Fn::Cidr': [], Extra: value}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("Resources:\n  Thing:\n    Type: Custom::Thing\n    Properties:\n      Value: " + value); err == nil {
				t.Fatal("Parse() error = nil, want malformed intrinsic map rejection")
			}
		})
	}
}

func TestParseRejectsNonExpressionConditionsAndIncompatibleParameterConstraints(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"boolean condition": "Conditions:\n  Enabled: true\nResources: {}",
		"scalar condition":  "Conditions:\n  Enabled: value\nResources: {}",
		"String MinValue": `
Parameters:
  Value:
    Type: String
    MinValue: 1
Resources: {}`,
		"Number MinLength": `
Parameters:
  Value:
    Type: Number
    MinLength: 1
Resources: {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(body); err == nil {
				t.Fatal("Parse() error = nil, want strict validation failure")
			}
		})
	}
}

//nolint:wsl // Each reference form is independently evaluated against a condition-excluded resource.
func TestEvaluateRejectsReferencesToConditionExcludedResources(t *testing.T) {
	t.Parallel()

	for name, property := range map[string]string{
		"Ref":    "Value: !Ref Conditional",
		"GetAtt": "Value: !GetAtt Conditional.Arn",
	} {
		t.Run(name, func(t *testing.T) {
			template, err := Parse("Conditions:\n  Never: !Equals [one, two]\nResources:\n  Conditional:\n    Type: Custom::Thing\n    Condition: Never\n  Consumer:\n    Type: Custom::Thing\n    Properties:\n      " + property)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			_, err = template.Evaluate(EvaluationInput{ResourceAttributes: map[string]map[string]string{
				"Conditional": {"Ref": "unexpected", "Arn": "arn:unexpected"},
			}})
			if err == nil {
				t.Fatal("Evaluate() error = nil, want condition-excluded resource rejection")
			}
		})
	}
}

func TestParseRejectsDuplicateKeysAndInvalidOutputDescriptions(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"duplicate JSON key": `{"Resources":{"Thing":{"Type":"Custom::Thing"},"Thing":{"Type":"Custom::Thing"}}}`,
		"duplicate YAML nested key": `
Resources:
  Thing:
    Type: Custom::Thing
    Properties:
      Name: first
      Name: second`,
		"output Description is not a string": `
Resources: {}
Outputs:
  Result:
    Value: value
    Description: false`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(body); err == nil {
				t.Fatal("Parse() error = nil, want strict shape failure")
			}
		})
	}
}

func TestEvaluateRejectsNonStringIntrinsicArguments(t *testing.T) {
	t.Parallel()

	for name, property := range map[string]string{
		"Join delimiter":  "Value: {'Fn::Join': [1, [value]]}",
		"Split delimiter": "Value: {'Fn::Split': [1, value]}",
		"Split source":    "Value: {'Fn::Split': [',', 1]}",
		"GetAtt list":     "Value: {'Fn::GetAtt': [Thing, 1]}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("Resources:\n  Thing:\n    Type: Custom::Thing\n  Consumer:\n    Type: Custom::Thing\n    Properties:\n      " + property); err == nil {
				t.Fatal("Parse() error = nil, want invalid intrinsic argument rejection")
			}
		})
	}
}

//nolint:wsl // The paired assertions distinguish structural validation from lazy reference evaluation.
func TestParseRejectsUnsupportedIntrinsicInUnselectedIfBranch(t *testing.T) {
	t.Parallel()

	unsupported, err := Parse(`
Conditions:
  Never: !Equals [one, two]
Resources:
  Thing:
    Type: Custom::Thing
    Properties:
      Value: !If [Never, {"Fn::Cidr": []}, fallback]
`)
	if err == nil || unsupported != nil {
		t.Fatal("Parse() accepted unsupported intrinsic in unselected branch")
	}

	template, err := Parse(`
Conditions:
  Never: !Equals [one, two]
Resources:
  Thing:
    Type: Custom::Thing
    Properties:
      Value: !If [Never, !Ref Missing, fallback]
`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	evaluation, err := template.Evaluate(EvaluationInput{})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if got := evaluation.Resources["Thing"].Properties["Value"]; got != "fallback" {
		t.Errorf("selected Fn::If value = %#v, want %q", got, "fallback")
	}
}

//nolint:wsl // Rune-count and full-match parameter validation use separate template inputs.
func TestEvaluateUsesRunesForStringLengthAndFullPatternMatching(t *testing.T) {
	t.Parallel()

	template, err := Parse(`
Parameters:
  Name:
    Type: String
    MinLength: 2
    MaxLength: 2
Resources: {}`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if _, err := template.Evaluate(EvaluationInput{Parameters: map[string]string{"Name": "éa"}}); err != nil {
		t.Fatalf("Evaluate(rune length) error = %v", err)
	}
	patternTemplate, err := Parse(`
Parameters:
  Name:
    Type: String
    AllowedPattern: '[a-z]+'
Resources: {}`)
	if err != nil {
		t.Fatalf("Parse(pattern) error = %v", err)
	}
	if _, err := patternTemplate.Evaluate(EvaluationInput{Parameters: map[string]string{"Name": "a!"}}); err == nil {
		t.Fatal("Evaluate(partial AllowedPattern) error = nil, want rejection")
	}
}

//nolint:wsl // The exact dotted attribute is supplied through the resource adapter input.
func TestEvaluateSupportsDottedGetAttAttributes(t *testing.T) {
	t.Parallel()

	template, err := Parse(`
Resources:
  LoadBalancer:
    Type: Custom::LoadBalancer
  Consumer:
    Type: Custom::Thing
    Properties:
      Value: !GetAtt LoadBalancer.SourceSecurityGroup.OwnerAlias`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	evaluation, err := template.Evaluate(EvaluationInput{ResourceAttributes: map[string]map[string]string{
		"LoadBalancer": {"SourceSecurityGroup.OwnerAlias": testOwner},
	}})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if got := evaluation.Resources["Consumer"].Properties["Value"]; got != testOwner {
		t.Errorf("GetAtt value = %#v, want %q", got, testOwner)
	}
}

func TestParseValidatesUnselectedIfIntrinsicShapes(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		"Join delimiter":       `!If [Never, {"Fn::Join": [1, [value]]}, fallback]`,
		"Split source":         `!If [Never, {"Fn::Split": [",", 1]}, fallback]`,
		"GetAtt list":          `!If [Never, {"Fn::GetAtt": [Thing, 1]}, fallback]`,
		"If condition":         `!If [1, selected, fallback]`,
		"Equals arity":         `!If [Never, {"Fn::Equals": [one]}, fallback]`,
		"And operand":          `!If [Never, {"Fn::And": [true, value]}, fallback]`,
		"ImportValue sequence": `!If [Never, {"Fn::ImportValue": [name]}, fallback]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("Conditions:\n  Never: !Equals [one, two]\nResources:\n  Thing:\n    Type: Custom::Thing\n    Properties:\n      Value: " + value); err == nil {
				t.Fatal("Parse() error = nil, want invalid unselected intrinsic shape")
			}
		})
	}
}

func TestParseRejectsUnselectedIfSelectAndNonIntrinsicStringMappings(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		"Select values":      `!If [Never, {"Fn::Select": [0, {Value: value}]}, fallback]`,
		"Join delimiter map": `!If [Never, {"Fn::Join": [{Value: delimiter}, [value]]}, fallback]`,
		"ImportValue map":    `!If [Never, {"Fn::ImportValue": {Value: name}}, fallback]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("Conditions:\n  Never: !Equals [one, two]\nResources:\n  Thing:\n    Type: Custom::Thing\n    Properties:\n      Value: " + value); err == nil {
				t.Fatal("Parse() error = nil, want invalid unselected intrinsic shape")
			}
		})
	}
}

//nolint:wsl // The paired assertions distinguish invalid condition shape from lazy reference evaluation.
func TestParseValidatesConditionIntrinsicInUnselectedIfBranch(t *testing.T) {
	t.Parallel()

	if _, err := Parse(`
Conditions:
  Never: !Equals [one, two]
Resources:
  Thing:
    Type: Custom::Thing
    Properties:
      Value: !If [Never, {Condition: 1}, fallback]
`); err == nil {
		t.Fatal("Parse() error = nil, want invalid Condition argument rejection")
	}

	template, err := Parse(`
Conditions:
  Never: !Equals [one, two]
  Other: !Equals [one, one]
Resources:
  Thing:
    Type: Custom::Thing
    Properties:
      Value: !If [Never, !Ref Missing, {Condition: Other}]
`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	evaluation, err := template.Evaluate(EvaluationInput{})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if got, ok := evaluation.Resources["Thing"].Properties["Value"].(bool); !ok || !got {
		t.Errorf("selected Condition value = %#v, want true", got)
	}
}

//nolint:wsl // The exact dotted attribute is supplied through the resource adapter input.
func TestEvaluateSupportsDottedSubAttributes(t *testing.T) {
	t.Parallel()

	template, err := Parse(`
Resources:
  LoadBalancer:
    Type: Custom::LoadBalancer
  Consumer:
    Type: Custom::Thing
    Properties:
      Value: !Sub '${LoadBalancer.SourceSecurityGroup.OwnerAlias}'`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	evaluation, err := template.Evaluate(EvaluationInput{ResourceAttributes: map[string]map[string]string{
		"LoadBalancer": {"SourceSecurityGroup.OwnerAlias": "owner"},
	}})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if got := evaluation.Resources["Consumer"].Properties["Value"]; got != "owner" {
		t.Errorf("Sub value = %#v, want %q", got, "owner")
	}
}

func TestParseRejectsOrdinaryMappingsForExportAndCondition(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"Export Name": `
Resources: {}
Outputs:
  Result:
    Value: value
    Export:
      Name: {Literal: value}`,
		"Condition": `
Conditions:
  Never: !Equals [one, two]
Resources:
  Thing:
    Type: Custom::Thing
    Properties:
      Value: !If [Never, {Condition: {Literal: value}}, fallback]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(body); err == nil {
				t.Fatal("Parse() error = nil, want ordinary mapping rejection")
			}
		})
	}
}

//nolint:wsl // The evaluation setup asserts both requested success cases against one template.
func TestEvaluateSupportsOrAndSequenceGetAtt(t *testing.T) {
	t.Parallel()

	template, err := Parse(`
Conditions:
  AnyMatch: !Or [!Equals [one, two], !Equals [one, one]]
Resources:
  Thing:
    Type: Custom::Thing
  Consumer:
    Type: Custom::Thing
    Properties:
      Enabled: !Condition AnyMatch
      Attribute: {"Fn::GetAtt": [Thing, Arn]}`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	evaluation, err := template.Evaluate(EvaluationInput{ResourceAttributes: map[string]map[string]string{
		"Thing": {"Arn": "arn:thing"},
	}})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	properties := evaluation.Resources["Consumer"].Properties
	if got, ok := properties["Enabled"].(bool); !ok || !got {
		t.Errorf("Fn::Or result = %#v, want true", got)
	}
	if got := properties["Attribute"]; got != "arn:thing" {
		t.Errorf("sequence Fn::GetAtt result = %#v, want %q", got, "arn:thing")
	}
}

func TestParseRejectsIntrinsicJoinAndSplitDelimiters(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		"Join":  `!If [Never, {"Fn::Join": [!Ref Delimiter, [value]]}, fallback]`,
		"Split": `!If [Never, {"Fn::Split": [!Ref Delimiter, value]}, fallback]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("Parameters:\n  Delimiter:\n    Type: String\n    Default: ':'\nConditions:\n  Never: !Equals [one, two]\nResources:\n  Thing:\n    Type: Custom::Thing\n    Properties:\n      Value: " + value); err == nil {
				t.Fatal("Parse() error = nil, want literal delimiter rejection")
			}
		})
	}
}

func TestParseRejectsResourceDependentImportsAndExports(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"ImportValue Ref": `
Conditions:
  Never: !Equals [one, two]
Resources:
  Thing:
    Type: Custom::Thing
  Consumer:
    Type: Custom::Thing
    Properties:
      Value: !If [Never, {"Fn::ImportValue": !Ref Thing}, fallback]`,
		"Export GetAtt": `
Resources:
  Thing:
    Type: Custom::Thing
Outputs:
  Result:
    Value: value
    Export:
      Name: !GetAtt Thing.Arn`,
		"Export Sub": `
Resources:
  Thing:
    Type: Custom::Thing
Outputs:
  Result:
    Value: value
    Export:
      Name: !Sub '${Thing.Arn}'`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(body); err == nil {
				t.Fatal("Parse() error = nil, want resource-dependent value rejection")
			}
		})
	}

	if _, err := Parse(`
Parameters:
  Name:
    Type: String
    Default: export
Resources:
  Consumer:
    Type: Custom::Thing
    Properties:
      Value: {"Fn::ImportValue": !Ref Name}
Outputs:
  Result:
    Value: value
    Export:
      Name: !Sub '${Name}-${AWS::Region}'`); err != nil {
		t.Fatalf("Parse(valid parameter and pseudo references) error = %v", err)
	}
}

func TestParseAllowsParameterBackedSubstitutionVariableInImportValue(t *testing.T) {
	t.Parallel()

	if _, err := Parse(`
Parameters:
  Name:
    Type: String
    Default: exported
Resources:
  Thing:
    Type: Custom::Thing
  Consumer:
    Type: Custom::Thing
    Properties:
      Value: !ImportValue
        Fn::Sub:
          - '${Thing}'
          - Thing: !Ref Name
`); err != nil {
		t.Fatalf("Parse() error = %v, want parameter-backed substitution variables to be accepted", err)
	}
}

func TestParseAllowsParameterBackedSubstitutionVariableInExportName(t *testing.T) {
	t.Parallel()

	if _, err := Parse(`
Parameters:
  Name:
    Type: String
    Default: exported
Resources:
  Thing:
    Type: Custom::Thing
Outputs:
  Result:
    Value: value
    Export:
      Name: !Sub
        - '${Thing}'
        - Thing: !Ref Name`); err != nil {
		t.Fatalf("Parse() error = %v, want parameter-backed substitution variables to be accepted", err)
	}
}

func TestParseRejectsNumericIfBranchInImportValue(t *testing.T) {
	t.Parallel()

	template, err := Parse(`
Conditions:
  UseNumber: !Equals [one, one]
Resources:
  Consumer:
    Type: Custom::Thing
    Properties:
      Value: {"Fn::ImportValue": {"Fn::If": [UseNumber, 42, exported-name]}}`)
	if err == nil {
		_, err = template.Evaluate(EvaluationInput{Imports: map[string]string{"42": "resolved-import"}})
	}

	if err == nil {
		t.Fatal("numeric Fn::If branch was coerced into an Fn::ImportValue name")
	}
}

func TestEvaluateRejectsNumericSelectResultInImportValue(t *testing.T) {
	t.Parallel()

	template, err := Parse(`
Resources:
  Consumer:
    Type: Custom::Thing
    Properties:
      Value: {"Fn::ImportValue": {"Fn::Select": [0, [42, exported-name]]}}`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if _, err := template.Evaluate(EvaluationInput{Imports: map[string]string{"42": "resolved-import"}}); err == nil {
		t.Fatal("numeric Fn::Select result was coerced into an Fn::ImportValue name")
	}
}

func TestEvaluateNamesOutputForNonStringExportValue(t *testing.T) {
	t.Parallel()

	template, err := Parse(`
Resources: {}
Outputs:
  Result:
    Value: value
    Export:
      Name: {"Fn::Select": [0, [42]]}`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	_, err = template.Evaluate(EvaluationInput{})
	if err == nil || !strings.Contains(err.Error(), `output "Result" Export Name must resolve to a string`) {
		t.Fatalf("Evaluate() error = %v, want output logical ID", err)
	}
}
