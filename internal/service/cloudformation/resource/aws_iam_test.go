//nolint:goconst,wsl // CloudFormation fixtures intentionally repeat AWS property names.
package resource

import (
	"context"
	"strings"
	"testing"

	"github.com/sivchari/kumo/internal/service/iam"
)

func TestIAMRoleCreatesAndDeletesThroughIAMStorage(t *testing.T) {
	adapter := mustAdapter(t, "AWS::IAM::Role")
	properties := map[string]any{
		"AssumeRolePolicyDocument": map[string]any{
			"Version": "2012-10-17",
			"Statement": []any{map[string]any{
				"Effect":    "Allow",
				"Principal": map[string]any{"Service": "lambda.amazonaws.com"},
				"Action":    "sts:AssumeRole",
			}},
		},
		"RoleName":    "cfn-iam-role",
		"Path":        "/service-role/",
		"Description": "CloudFormation test role",
		"Tags": []any{map[string]any{
			"Key": "environment", "Value": "test",
		}},
	}

	result, err := adapter.Create(context.Background(), CreateRequest{
		Properties:   properties,
		Capabilities: []string{"CAPABILITY_NAMED_IAM"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	assertExactAttributes(t, []string{"Arn", "RoleId"}, result)

	storage, err := lookupStorage[iam.Storage]("iam")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	role, err := storage.GetRole(context.Background(), result.Identifier)
	if err != nil {
		t.Fatalf("GetRole() error = %v", err)
	}
	if role.Arn != result.Attributes[attributeARN] || role.Description != properties["Description"] {
		t.Fatalf("stored role = %#v, result = %#v", role, result)
	}
	if len(role.Tags) != 1 || role.Tags[0].Key != "environment" {
		t.Fatalf("stored role tags = %#v, want environment tag", role.Tags)
	}

	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := storage.GetRole(context.Background(), result.Identifier); err == nil {
		t.Fatal("GetRole() after Delete unexpectedly succeeded")
	}
}

func TestIAMRoleUpdateResetsOmittedDesiredProperties(t *testing.T) {
	adapter := mustAdapter(t, "AWS::IAM::Role")
	result, err := adapter.Create(context.Background(), CreateRequest{
		Capabilities: []string{"CAPABILITY_NAMED_IAM"},
		Properties: map[string]any{
			"AssumeRolePolicyDocument": `{"Version":"2012-10-17"}`,
			"RoleName":                 "cfn-iam-reset-role", "Description": "old", "MaxSessionDuration": int64(7200),
			"Tags":     []any{map[string]any{"Key": "old", "Value": "tag"}},
			"Policies": []any{map[string]any{"PolicyName": "old-policy", "PolicyDocument": map[string]any{"Version": "2012-10-17"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}) })
	updater, ok := adapter.(UpdateCapable)
	if !ok {
		t.Fatal("IAM role does not implement Update")
	}
	if _, err := updater.Update(context.Background(), UpdateRequest{
		Identifier:   result.Identifier,
		Capabilities: []string{"CAPABILITY_NAMED_IAM"},
		Properties:   map[string]any{"AssumeRolePolicyDocument": `{"Version":"2012-10-17"}`},
		PreviousProperties: map[string]any{
			"AssumeRolePolicyDocument": `{"Version":"2012-10-17"}`, "Description": "old", "MaxSessionDuration": int64(7200),
			"Tags":     []any{map[string]any{"Key": "old", "Value": "tag"}},
			"Policies": []any{map[string]any{"PolicyName": "old-policy", "PolicyDocument": map[string]any{"Version": "2012-10-17"}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	storage, err := lookupStorage[iam.Storage]("iam")
	if err != nil {
		t.Fatal(err)
	}
	role, err := storage.GetRole(context.Background(), result.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	if role.Description != "" || role.MaxSessionDuration != 3600 || len(role.Tags) != 0 || len(role.InlinePolicies) != 0 {
		t.Fatalf("role after reset = %#v", role)
	}
}

func TestIAMRoleRequiresIAMCapabilitiesForNamedAndGeneratedRoles(t *testing.T) {
	adapter := mustAdapter(t, "AWS::IAM::Role")
	properties := map[string]any{"AssumeRolePolicyDocument": `{"Version":"2012-10-17"}`}

	if _, err := adapter.Create(context.Background(), CreateRequest{Properties: properties}); err == nil || !strings.Contains(err.Error(), "CAPABILITY_IAM") {
		t.Fatalf("unnamed role without capability error = %v, want CAPABILITY_IAM", err)
	}
	result, err := adapter.Create(context.Background(), CreateRequest{
		Properties:   properties,
		Capabilities: []string{"CAPABILITY_NAMED_IAM"},
	})
	if err != nil {
		t.Fatalf("unnamed role with CAPABILITY_NAMED_IAM error = %v", err)
	}
	t.Cleanup(func() { _ = adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}) })

	properties["RoleName"] = "cfn-named-iam-role"
	if _, err := adapter.Create(context.Background(), CreateRequest{
		Properties:   properties,
		Capabilities: []string{"CAPABILITY_IAM"},
	}); err == nil || !strings.Contains(err.Error(), "CAPABILITY_NAMED_IAM") {
		t.Fatalf("named role with only CAPABILITY_IAM error = %v, want CAPABILITY_NAMED_IAM", err)
	}
}

func TestIAMPolicyAttachesInlineDocumentOnlyToRoles(t *testing.T) {
	adapter := mustAdapter(t, "AWS::IAM::Policy")
	roleAdapter := mustAdapter(t, "AWS::IAM::Role")
	roleResult, err := roleAdapter.Create(context.Background(), CreateRequest{
		Capabilities: []string{"CAPABILITY_IAM"},
		Properties: map[string]any{
			"AssumeRolePolicyDocument": `{"Version":"2012-10-17"}`,
		},
	})
	if err != nil {
		t.Fatalf("Create() role error = %v", err)
	}
	t.Cleanup(func() { _ = roleAdapter.Delete(context.Background(), DeleteRequest{Identifier: roleResult.Identifier}) })

	result, err := adapter.Create(context.Background(), CreateRequest{
		Capabilities: []string{"CAPABILITY_NAMED_IAM"},
		Properties: map[string]any{
			"PolicyName":     "cfn-inline-policy",
			"PolicyDocument": map[string]any{"Version": "2012-10-17"},
			"Roles":          []any{roleResult.Identifier},
		},
	})
	if err != nil {
		t.Fatalf("Create() policy error = %v", err)
	}
	assertExactAttributes(t, []string{"Id"}, result)

	storage, err := lookupStorage[iam.Storage]("iam")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	if _, err := storage.GetPolicy(context.Background(), result.Identifier); err == nil {
		t.Fatal("GetPolicy() unexpectedly found a managed policy for AWS::IAM::Policy")
	}
	role, err := storage.GetRole(context.Background(), roleResult.Identifier)
	if err != nil {
		t.Fatalf("GetRole() error = %v", err)
	}
	if got := role.InlinePolicies["cfn-inline-policy"]; got != `{"Version":"2012-10-17"}` {
		t.Fatalf("inline policy document = %q, want stored JSON", got)
	}
	if len(role.AttachedPolicies) != 0 {
		t.Fatalf("attached policies = %#v, want no managed policy attachment", role.AttachedPolicies)
	}

	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	role, err = storage.GetRole(context.Background(), roleResult.Identifier)
	if err != nil {
		t.Fatalf("GetRole() after Delete error = %v", err)
	}
	if _, exists := role.InlinePolicies["cfn-inline-policy"]; exists {
		t.Fatal("inline policy still exists after Delete")
	}
}

func TestIAMPolicyRejectsUsersAndGroups(t *testing.T) {
	adapter := mustAdapter(t, "AWS::IAM::Policy")
	for _, target := range []string{"Users", "Groups"} {
		t.Run(target, func(t *testing.T) {
			_, err := adapter.Create(context.Background(), CreateRequest{
				Capabilities: []string{"CAPABILITY_NAMED_IAM"},
				Properties: map[string]any{
					"PolicyName":     "cfn-unsupported-policy-" + strings.ToLower(target),
					"PolicyDocument": `{"Version":"2012-10-17"}`,
					target:           []any{"unsupported"},
				},
			})
			if err == nil || !strings.Contains(err.Error(), target) {
				t.Fatalf("Create() error = %v, want explicit %s rejection", err, target)
			}
		})
	}
}

func TestIAMPolicyRequiresNamedIAMCapability(t *testing.T) {
	adapter := mustAdapter(t, "AWS::IAM::Policy")
	_, err := adapter.Create(context.Background(), CreateRequest{
		Properties: map[string]any{
			"PolicyName":     "cfn-named-policy",
			"PolicyDocument": `{"Version":"2012-10-17"}`,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "CAPABILITY_NAMED_IAM") {
		t.Fatalf("Create() error = %v, want CAPABILITY_NAMED_IAM", err)
	}
}

func TestIAMPolicyRequiresPolicyNameAndSupportedRoleTarget(t *testing.T) {
	adapter := mustAdapter(t, "AWS::IAM::Policy")
	for _, testCase := range []struct {
		name       string
		properties map[string]any
		contains   string
	}{
		{
			name: "missing policy name",
			properties: map[string]any{
				"PolicyDocument": `{"Version":"2012-10-17"}`,
				"Roles":          []any{"missing-role"},
			},
			contains: "PolicyName",
		},
		{
			name: "missing role target",
			properties: map[string]any{
				"PolicyName":     "cfn-required-target-policy",
				"PolicyDocument": `{"Version":"2012-10-17"}`,
			},
			contains: "roles",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := adapter.Create(context.Background(), CreateRequest{
				Capabilities: []string{"CAPABILITY_NAMED_IAM"},
				Properties:   testCase.properties,
			})
			if err == nil || !strings.Contains(err.Error(), testCase.contains) {
				t.Fatalf("Create() error = %v, want %s validation", err, testCase.contains)
			}
		})
	}
}

func TestIAMPolicyRejectsUndocumentedProperties(t *testing.T) {
	adapter := mustAdapter(t, "AWS::IAM::Policy")
	for _, property := range []string{"Description", "Path", "Tags"} {
		properties := map[string]any{
			"PolicyName":     "cfn-undocumented-" + strings.ToLower(property),
			"PolicyDocument": `{"Version":"2012-10-17"}`,
			"Roles":          []any{"role"},
			property:         "unsupported",
		}
		if _, err := adapter.Create(context.Background(), CreateRequest{
			Capabilities: []string{"CAPABILITY_NAMED_IAM"},
			Properties:   properties,
		}); err == nil || !strings.Contains(err.Error(), property) {
			t.Fatalf("Create() with %s error = %v, want property rejection", property, err)
		}
	}
}

func TestIAMNestedPropertiesRejectUnknownFields(t *testing.T) {
	tests := []struct {
		name       string
		adapter    string
		properties map[string]any
		contains   string
	}{
		{
			name:    "role tag",
			adapter: "AWS::IAM::Role",
			properties: map[string]any{
				"AssumeRolePolicyDocument": `{"Version":"2012-10-17"}`,
				"Tags":                     []any{map[string]any{"Key": "environment", "Value": "test", "Unsupported": true}},
			},
			contains: "Unsupported",
		},
		{
			name:    "policy tag",
			adapter: "AWS::IAM::Policy",
			properties: map[string]any{
				"PolicyDocument": `{"Version":"2012-10-17"}`,
				"Tags":           []any{map[string]any{"Key": "environment", "Value": "test", "Unsupported": true}},
			},
			contains: "Tags",
		},
		{
			name:    "inline policy",
			adapter: "AWS::IAM::Role",
			properties: map[string]any{
				"AssumeRolePolicyDocument": `{"Version":"2012-10-17"}`,
				"Policies": []any{map[string]any{
					"PolicyName":     "inline",
					"PolicyDocument": map[string]any{"Version": "2012-10-17"},
					"Unsupported":    true,
				}},
			},
			contains: "Unsupported",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := mustAdapter(t, test.adapter).Create(context.Background(), CreateRequest{
				Capabilities: []string{"CAPABILITY_IAM"},
				Properties:   test.properties,
			})
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("Create() error = %v, want nested property rejection", err)
			}
		})
	}
}
