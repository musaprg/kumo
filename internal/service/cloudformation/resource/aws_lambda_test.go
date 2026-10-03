//nolint:goconst,wsl // CloudFormation fixtures intentionally repeat AWS property names.
package resource

import (
	"context"
	"strings"
	"testing"

	"github.com/sivchari/kumo/internal/service/lambda"
)

func TestLambdaFunctionCreatesAndDeletesThroughLambdaStorage(t *testing.T) {
	adapter := mustAdapter(t, "AWS::Lambda::Function")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": "cfn-lambda-function",
		"Runtime":      "provided.al2023",
		"Role":         "arn:aws:iam::123456789012:role/cfn-role",
		"Handler":      "bootstrap",
		"Code":         map[string]any{"ZipFile": "hello lambda"},
		"Description":  "CloudFormation test function",
		"Timeout":      int64(10),
		"MemorySize":   int64(256),
		"Environment":  map[string]any{"Variables": map[string]any{"MODE": "test"}},
		"Tags":         []any{map[string]any{"Key": "environment", "Value": "test"}},
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	assertExactAttributes(t, []string{attributeARN}, result)

	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	function, err := storage.GetFunction(context.Background(), result.Identifier)
	if err != nil {
		t.Fatalf("GetFunction() error = %v", err)
	}
	if function.FunctionArn != result.Attributes[attributeARN] || function.Timeout != 10 || function.MemorySize != 256 {
		t.Fatalf("stored function = %#v, result = %#v", function, result)
	}
	if function.Environment == nil || function.Environment.Variables["MODE"] != "test" {
		t.Fatalf("stored environment = %#v, want MODE=test", function.Environment)
	}
	if function.Tags["environment"] != "test" {
		t.Fatalf("stored tags = %#v, want environment=test", function.Tags)
	}

	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := storage.GetFunction(context.Background(), result.Identifier); err == nil {
		t.Fatal("GetFunction() after Delete unexpectedly succeeded")
	}
}

func TestLambdaFunctionUpdateResetsOmittedDesiredConfiguration(t *testing.T) {
	adapter := mustAdapter(t, "AWS::Lambda::Function")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": "cfn-lambda-reset-function", "Runtime": "provided.al2023", "Role": "arn:aws:iam::123456789012:role/cfn-role",
		"Handler": "bootstrap", "Code": map[string]any{"ZipFile": "old"}, "Description": "old", "Timeout": int64(10),
		"MemorySize": int64(256), "Environment": map[string]any{"Variables": map[string]any{"OLD": "value"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}) })
	updater, ok := adapter.(UpdateCapable)
	if !ok {
		t.Fatal("Lambda function does not implement Update")
	}
	if _, err := updater.Update(context.Background(), UpdateRequest{Identifier: result.Identifier, Properties: map[string]any{
		"Role": "arn:aws:iam::123456789012:role/cfn-role", "Code": map[string]any{"ZipFile": "new"},
	}, PreviousProperties: map[string]any{
		"Role": "arn:aws:iam::123456789012:role/cfn-role", "Code": map[string]any{"ZipFile": "old"},
		"Runtime": "provided.al2023", "Handler": "bootstrap", "Description": "old", "Timeout": int64(10), "MemorySize": int64(256),
		"Environment": map[string]any{"Variables": map[string]any{"OLD": "value"}},
	}}); err != nil {
		t.Fatal(err)
	}
	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		t.Fatal(err)
	}
	function, err := storage.GetFunction(context.Background(), result.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	if function.Description != "" || function.Handler != "" || function.Runtime != "" || function.Timeout != 3 || function.MemorySize != 128 || function.Environment != nil {
		t.Fatalf("function after reset = %#v", function)
	}
}

func TestLambdaFunctionUpdatesArchitecturesWithoutCodeChange(t *testing.T) {
	adapter := mustAdapter(t, "AWS::Lambda::Function")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": "cfn-lambda-architecture-function", "Runtime": "provided.al2023", "Role": "arn:aws:iam::123456789012:role/cfn-role",
		"Handler": "bootstrap", "Code": map[string]any{"ZipFile": "code"}, "Architectures": []any{"x86_64"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}) })
	updater, ok := adapter.(UpdateCapable)
	if !ok {
		t.Fatal("Lambda function does not implement Update")
	}
	if _, err := updater.Update(context.Background(), UpdateRequest{Identifier: result.Identifier, Properties: map[string]any{
		"Role": "arn:aws:iam::123456789012:role/cfn-role", "Architectures": []any{"arm64"},
	}}); err != nil {
		t.Fatal(err)
	}
	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		t.Fatal(err)
	}
	function, err := storage.GetFunction(context.Background(), result.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	if len(function.Architectures) != 1 || function.Architectures[0] != "arm64" {
		t.Fatalf("architectures after update = %#v, want [arm64]", function.Architectures)
	}
}

func TestLambdaEventSourceMappingUpdateResetsOmittedDesiredProperties(t *testing.T) {
	functionAdapter := mustAdapter(t, "AWS::Lambda::Function")
	function, err := functionAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": "cfn-lambda-mapping-reset-function", "Role": "arn:aws:iam::123456789012:role/cfn-role", "Code": map[string]any{"ZipFile": "code"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = functionAdapter.Delete(context.Background(), DeleteRequest{Identifier: function.Identifier})
	})
	adapter := mustAdapter(t, "AWS::Lambda::EventSourceMapping")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": function.Identifier, "EventSourceArn": "arn:aws:sqs:us-east-1:000000000000:source",
		"BatchSize": int64(25), "MaximumBatchingWindowInSeconds": int64(12), "Enabled": false,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}) })
	updater, ok := adapter.(UpdateCapable)
	if !ok {
		t.Fatal("Lambda event source mapping does not implement Update")
	}
	if _, err := updater.Update(context.Background(), UpdateRequest{Identifier: result.Identifier, Properties: map[string]any{"FunctionName": function.Identifier}}); err != nil {
		t.Fatal(err)
	}
	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := storage.GetEventSourceMapping(context.Background(), result.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	if mapping.BatchSize != 10 || mapping.MaximumBatchingWindowInSeconds != 0 || mapping.Enabled == nil || !*mapping.Enabled {
		t.Fatalf("mapping after reset = %#v", mapping)
	}
}

func TestLambdaFunctionRejectsUnobservableProperties(t *testing.T) {
	adapter := mustAdapter(t, "AWS::Lambda::Function")
	_, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"Role":   "arn:aws:iam::123456789012:role/cfn-role",
		"Code":   map[string]any{"ZipFile": "hello lambda"},
		"Layers": []any{"arn:aws:lambda:us-east-1:123456789012:layer:unsupported"},
	}})
	if err == nil || !strings.Contains(err.Error(), "Layers") {
		t.Fatalf("Create() error = %v, want unobservable Layers rejection", err)
	}
}

func TestLambdaNestedPropertiesRejectUnknownFields(t *testing.T) {
	adapter := mustAdapter(t, "AWS::Lambda::Function")
	for _, properties := range []map[string]any{
		{
			"Role":        "arn:aws:iam::123456789012:role/cfn-role",
			"Code":        map[string]any{"ZipFile": "hello lambda"},
			"Environment": map[string]any{"Variables": map[string]any{"MODE": "test"}, "Unsupported": true},
		},
		{
			"Role": "arn:aws:iam::123456789012:role/cfn-role",
			"Code": map[string]any{"ZipFile": "hello lambda"},
			"Tags": []any{map[string]any{"Key": "environment", "Value": "test", "Unsupported": true}},
		},
	} {
		if _, err := adapter.Create(context.Background(), CreateRequest{Properties: properties}); err == nil || !strings.Contains(err.Error(), "Unsupported") {
			t.Fatalf("Create() error = %v, want nested property rejection", err)
		}
	}
}

func TestLambdaPermissionAddsAndRemovesFunctionPolicyStatement(t *testing.T) {
	functionAdapter := mustAdapter(t, "AWS::Lambda::Function")
	functionResult, err := functionAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": "cfn-lambda-permission-function",
		"Role":         "arn:aws:iam::123456789012:role/cfn-role",
		"Code":         map[string]any{"ZipFile": "hello lambda"},
	}})
	if err != nil {
		t.Fatalf("Create() function error = %v", err)
	}
	t.Cleanup(func() {
		_ = functionAdapter.Delete(context.Background(), DeleteRequest{Identifier: functionResult.Identifier})
	})

	adapter := mustAdapter(t, "AWS::Lambda::Permission")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"Action":         "lambda:InvokeFunction",
		"FunctionName":   functionResult.Identifier,
		"Principal":      "sns.amazonaws.com",
		"SourceArn":      "arn:aws:sns:us-east-1:000000000000:cfn-topic",
		"SourceAccount":  "000000000000",
		"PrincipalOrgID": "o-example",
	}})
	if err != nil {
		t.Fatalf("Create() permission error = %v", err)
	}

	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	policy, err := storage.GetPolicy(context.Background(), functionResult.Identifier)
	if err != nil {
		t.Fatalf("GetPolicy() error = %v", err)
	}
	if len(policy.Statements) != 1 || policy.Statements[0].Sid == "" {
		t.Fatalf("policy statements = %#v, want an internally generated statement ID", policy.Statements)
	}
	if policy.Statements[0].Principal["Service"] != "sns.amazonaws.com" {
		t.Fatalf("policy principal = %#v, want service principal", policy.Statements[0].Principal)
	}
	stringEquals, ok := policy.Statements[0].Condition["StringEquals"].(map[string]string)
	if !ok || stringEquals["AWS:SourceAccount"] != "000000000000" || stringEquals["aws:PrincipalOrgID"] != "o-example" {
		t.Fatalf("policy StringEquals condition = %#v, want source account and organization", policy.Statements[0].Condition["StringEquals"])
	}

	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() permission error = %v", err)
	}
	policy, err = storage.GetPolicy(context.Background(), functionResult.Identifier)
	if err != nil {
		t.Fatalf("GetPolicy() after permission delete error = %v", err)
	}
	if len(policy.Statements) != 0 {
		t.Fatalf("policy statements after delete = %#v, want empty", policy.Statements)
	}
}

func TestLambdaPermissionRejectsUnsupportedPrincipalShapes(t *testing.T) {
	adapter := mustAdapter(t, "AWS::Lambda::Permission")
	for _, principal := range []any{
		map[string]any{"AWS": "*"},
		[]any{"sns.amazonaws.com"},
	} {
		_, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
			"Action":       "lambda:InvokeFunction",
			"FunctionName": "missing-function",
			"Principal":    principal,
		}})
		if err == nil || !strings.Contains(err.Error(), "Principal") {
			t.Fatalf("Create() principal %#v error = %v, want unsupported Principal rejection", principal, err)
		}
	}
}

func TestLambdaPermissionClassifiesAccountPrincipalAsAWS(t *testing.T) {
	functionAdapter := mustAdapter(t, "AWS::Lambda::Function")
	functionResult, err := functionAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": "cfn-lambda-account-principal-function",
		"Role":         "arn:aws:iam::123456789012:role/cfn-role",
		"Code":         map[string]any{"ZipFile": "hello lambda"},
	}})
	if err != nil {
		t.Fatalf("Create() function error = %v", err)
	}
	t.Cleanup(func() {
		_ = functionAdapter.Delete(context.Background(), DeleteRequest{Identifier: functionResult.Identifier})
	})

	permissionAdapter := mustAdapter(t, "AWS::Lambda::Permission")
	_, err = permissionAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"Action":       "lambda:InvokeFunction",
		"FunctionName": functionResult.Identifier,
		"Principal":    "123456789012",
	}})
	if err != nil {
		t.Fatalf("Create() permission error = %v", err)
	}

	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	policy, err := storage.GetPolicy(context.Background(), functionResult.Identifier)
	if err != nil {
		t.Fatalf("GetPolicy() error = %v", err)
	}
	if policy.Statements[0].Principal["AWS"] != "123456789012" {
		t.Fatalf("policy principal = %#v, want AWS account principal", policy.Statements[0].Principal)
	}
}

func TestLambdaPermissionUsesStableInternalStatementIDWhenOmitted(t *testing.T) {
	functionAdapter := mustAdapter(t, "AWS::Lambda::Function")
	functionResult, err := functionAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": "cfn-lambda-stable-permission-function",
		"Role":         "arn:aws:iam::123456789012:role/cfn-role",
		"Code":         map[string]any{"ZipFile": "hello lambda"},
	}})
	if err != nil {
		t.Fatalf("Create() function error = %v", err)
	}
	t.Cleanup(func() {
		_ = functionAdapter.Delete(context.Background(), DeleteRequest{Identifier: functionResult.Identifier})
	})

	adapter := mustAdapter(t, "AWS::Lambda::Permission")
	properties := map[string]any{
		"Action":       "lambda:InvokeFunction",
		"FunctionName": functionResult.Identifier,
		"Principal":    "sns.amazonaws.com",
	}

	first, err := adapter.Create(context.Background(), CreateRequest{Properties: properties})
	if err != nil {
		t.Fatalf("first Create() permission error = %v", err)
	}
	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	policy, err := storage.GetPolicy(context.Background(), functionResult.Identifier)
	if err != nil {
		t.Fatalf("GetPolicy() error = %v", err)
	}
	firstSID := policy.Statements[0].Sid
	if firstSID == "" {
		t.Fatal("generated statement ID is empty")
	}
	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: first.Identifier}); err != nil {
		t.Fatalf("Delete() first permission error = %v", err)
	}

	second, err := adapter.Create(context.Background(), CreateRequest{Properties: properties})
	if err != nil {
		t.Fatalf("second Create() permission error = %v", err)
	}
	policy, err = storage.GetPolicy(context.Background(), functionResult.Identifier)
	if err != nil {
		t.Fatalf("GetPolicy() second error = %v", err)
	}
	if got := policy.Statements[0].Sid; got != firstSID {
		t.Fatalf("generated statement ID = %q on second create, want stable %q", got, firstSID)
	}
	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: second.Identifier}); err != nil {
		t.Fatalf("Delete() second permission error = %v", err)
	}
}

func TestLambdaPermissionSchemaUsesOnlyDocumentedProperties(t *testing.T) {
	assertPropertyNames(t, mustAdapter(t, "AWS::Lambda::Permission").Schema(), []string{
		"Action", "EventSourceToken", "FunctionName", "FunctionUrlAuthType",
		"InvokedViaFunctionUrl", "Principal", "PrincipalOrgID", "SourceAccount", "SourceArn",
	})
}

func TestLambdaPermissionRejectsUndocumentedStatementID(t *testing.T) {
	_, err := mustAdapter(t, "AWS::Lambda::Permission").Create(context.Background(), CreateRequest{Properties: map[string]any{
		"Action":       "lambda:InvokeFunction",
		"FunctionName": "cfn-function",
		"Principal":    "sns.amazonaws.com",
		"StatementId":  "undocumented",
	}})
	if err == nil || !strings.Contains(err.Error(), "StatementId") {
		t.Fatalf("Create() error = %v, want StatementId rejection", err)
	}
}

func TestLambdaPermissionResolvesSupportedFunctionReferenceForms(t *testing.T) {
	functionAdapter := mustAdapter(t, "AWS::Lambda::Function")
	functionResult, err := functionAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": "cfn-lambda-reference-forms-function",
		"Role":         "arn:aws:iam::123456789012:role/cfn-role",
		"Code":         map[string]any{"ZipFile": "hello lambda"},
	}})
	if err != nil {
		t.Fatalf("Create() function error = %v", err)
	}
	t.Cleanup(func() {
		_ = functionAdapter.Delete(context.Background(), DeleteRequest{Identifier: functionResult.Identifier})
	})

	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	function, err := storage.GetFunction(context.Background(), functionResult.Identifier)
	if err != nil {
		t.Fatalf("GetFunction() error = %v", err)
	}
	permissionAdapter := mustAdapter(t, "AWS::Lambda::Permission")
	for _, reference := range []string{function.FunctionArn, "000000000000:function:" + functionResult.Identifier} {
		result, createErr := permissionAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
			"Action":       "lambda:InvokeFunction",
			"FunctionName": reference,
			"Principal":    "sns.amazonaws.com",
		}})
		if createErr != nil {
			t.Fatalf("Create() permission for %q error = %v", reference, createErr)
		}
		if deleteErr := permissionAdapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); deleteErr != nil {
			t.Fatalf("Delete() permission for %q error = %v", reference, deleteErr)
		}
	}

	for _, reference := range []string{functionResult.Identifier + ":alias", function.FunctionArn + ":1"} {
		_, createErr := permissionAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
			"Action":       "lambda:InvokeFunction",
			"FunctionName": reference,
			"Principal":    "sns.amazonaws.com",
		}})
		if createErr == nil || !strings.Contains(createErr.Error(), "qualified Lambda function reference") {
			t.Fatalf("Create() permission for unsupported %q error = %v, want explicit rejection", reference, createErr)
		}
	}
}

func TestLambdaEventSourceMappingCreatesAndDeletesThroughLambdaStorage(t *testing.T) {
	functionAdapter := mustAdapter(t, "AWS::Lambda::Function")
	functionResult, err := functionAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": "cfn-lambda-mapping-function",
		"Role":         "arn:aws:iam::123456789012:role/cfn-role",
		"Code":         map[string]any{"ZipFile": "hello lambda"},
	}})
	if err != nil {
		t.Fatalf("Create() function error = %v", err)
	}
	t.Cleanup(func() {
		_ = functionAdapter.Delete(context.Background(), DeleteRequest{Identifier: functionResult.Identifier})
	})

	adapter := mustAdapter(t, "AWS::Lambda::EventSourceMapping")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName":                   functionResult.Attributes[attributeARN],
		"EventSourceArn":                 "arn:aws:sqs:us-east-1:000000000000:cfn-queue",
		"BatchSize":                      int64(5),
		"MaximumBatchingWindowInSeconds": int64(10),
		"Enabled":                        false,
	}})
	if err != nil {
		t.Fatalf("Create() mapping error = %v", err)
	}
	if result.Attributes["Id"] != result.Identifier {
		t.Fatalf("mapping Id = %v, want identifier %q", result.Attributes["Id"], result.Identifier)
	}
	if result.Attributes["EventSourceMappingArn"] == "" {
		t.Fatal("mapping EventSourceMappingArn is empty")
	}
	assertExactAttributes(t, []string{"EventSourceMappingArn", "Id"}, result)

	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	mapping, err := storage.GetEventSourceMapping(context.Background(), result.Identifier)
	if err != nil {
		t.Fatalf("GetEventSourceMapping() error = %v", err)
	}
	if mapping.BatchSize != 5 || mapping.MaximumBatchingWindowInSeconds != 10 || mapping.State != "Disabled" {
		t.Fatalf("stored mapping = %#v, want configured values", mapping)
	}

	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() mapping error = %v", err)
	}
	if _, err := storage.GetEventSourceMapping(context.Background(), result.Identifier); err == nil {
		t.Fatal("GetEventSourceMapping() after Delete unexpectedly succeeded")
	}
}
