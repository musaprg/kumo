//nolint:wsl // CloudFormation property translation is intentionally explicit.
package resource

import (
	"context"
	"fmt"

	"github.com/sivchari/kumo/internal/service/iam"
)

// iamPolicyAdapter implements AWS::IAM::Policy.
type iamPolicyAdapter struct{}

var iamPolicySchema = Schema{
	TypeName: "AWS::IAM::Policy",
	PropertyUpdate: map[string]PropertyUpdate{
		"Groups":         propertyUpdateReplacement,
		"PolicyDocument": propertyUpdateNoInterruption,
		"PolicyName":     propertyUpdateReplacement,
		"Roles":          propertyUpdateReplacement,
		"Users":          propertyUpdateReplacement,
	},
	Attributes:           []string{"Id"},
	RequiredCapabilities: []string{"CAPABILITY_NAMED_IAM"},
}

func (*iamPolicyAdapter) Schema() Schema { return cloneSchema(iamPolicySchema) }

func (a *iamPolicyAdapter) RequiredCapabilities(_ map[string]any) []string {
	return append([]string(nil), a.Schema().RequiredCapabilities...)
}

func (a *iamPolicyAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(iamPolicySchema, req.Properties); err != nil {
		return CreateResult{}, err
	}
	if err := requireCapabilities(CapabilitiesFor(a, req.Properties), req.Capabilities); err != nil {
		return CreateResult{}, err
	}
	if _, ok := req.Properties["Users"]; ok {
		return CreateResult{}, fmt.Errorf("unsupported IAM policy target Users: IAM storage cannot attach policies to users")
	}
	if _, ok := req.Properties["Groups"]; ok {
		return CreateResult{}, fmt.Errorf("unsupported IAM policy target Groups: IAM storage cannot attach policies to groups")
	}

	policyName, err := requiredString(req.Properties, "PolicyName")
	if err != nil {
		return CreateResult{}, err
	}
	policyDocument, err := jsonDocument("PolicyDocument", req.Properties["PolicyDocument"])
	if err != nil {
		return CreateResult{}, err
	}
	roles, err := stringListProperty(req.Properties, "Roles")
	if err != nil {
		return CreateResult{}, err
	}
	if len(roles) == 0 {
		return CreateResult{}, fmt.Errorf("roles must contain at least one supported role target")
	}

	storage, err := lookupStorage[iam.Storage]("iam")
	if err != nil {
		return CreateResult{}, err
	}
	policy, err := createIAMPolicy(ctx, storage, iamPolicyProvisionRequest{
		PolicyName:     policyName,
		PolicyDocument: policyDocument,
		Roles:          roles,
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("create IAM inline policy: %w", err)
	}

	return CreateResult{Identifier: policy.PolicyName, Attributes: map[string]any{
		"Id": policy.PolicyID,
	}}, nil
}

func (a *iamPolicyAdapter) Update(ctx context.Context, req UpdateRequest) (UpdateResult, error) {
	if err := validateProperties(iamPolicySchema, req.Properties); err != nil {
		return UpdateResult{}, err
	}
	if err := requireCapabilities(CapabilitiesFor(a, req.Properties), req.Capabilities); err != nil {
		return UpdateResult{}, err
	}
	policyName, err := requiredString(req.Properties, "PolicyName")
	if err != nil {
		return UpdateResult{}, err
	}
	document, err := jsonDocument("PolicyDocument", req.Properties["PolicyDocument"])
	if err != nil {
		return UpdateResult{}, err
	}
	roles, err := stringListProperty(req.Properties, "Roles")
	if err != nil {
		return UpdateResult{}, err
	}
	storage, err := lookupStorage[iam.Storage]("iam")
	if err != nil {
		return UpdateResult{}, err
	}
	for _, role := range roles {
		if err := storage.PutRolePolicy(ctx, role, policyName, document); err != nil {
			return UpdateResult{}, fmt.Errorf("update IAM inline policy: %w", err)
		}
	}

	return UpdateResult{}, nil
}

func (*iamPolicyAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	storage, err := lookupStorage[iam.Storage]("iam")
	if err != nil {
		return err
	}
	roles, err := storage.ListRoles(ctx, "", 0)
	if err != nil {
		return fmt.Errorf("list IAM roles before policy deletion: %w", err)
	}
	for index := range roles {
		role := roles[index]
		inlinePolicies, listErr := storage.ListRolePolicies(ctx, role.RoleName)
		if listErr != nil {
			return fmt.Errorf("list IAM inline policies: %w", listErr)
		}
		for policyIndex := range inlinePolicies {
			if inlinePolicies[policyIndex] == req.Identifier {
				if err := storage.DeleteRolePolicy(ctx, role.RoleName, req.Identifier); err != nil {
					return fmt.Errorf("delete IAM inline policy: %w", err)
				}
			}
		}
	}

	return nil
}
