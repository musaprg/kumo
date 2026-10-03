//nolint:wsl // CloudFormation property translation is intentionally explicit.
package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"

	"github.com/sivchari/kumo/internal/service/iam"
)

// iamRoleAdapter implements AWS::IAM::Role.
type iamRoleAdapter struct{}

const iamRoleDescriptionProperty = "Description"

var iamRoleSchema = Schema{
	TypeName: "AWS::IAM::Role",
	PropertyUpdate: map[string]PropertyUpdate{
		"AssumeRolePolicyDocument": propertyUpdateNoInterruption,
		iamRoleDescriptionProperty: propertyUpdateNoInterruption,
		"ManagedPolicyArns":        propertyUpdateNoInterruption,
		"MaxSessionDuration":       propertyUpdateNoInterruption,
		"Path":                     propertyUpdateReplacement,
		"Policies":                 propertyUpdateNoInterruption,
		"RoleName":                 propertyUpdateReplacement,
		"Tags":                     propertyUpdateNoInterruption,
	},
	Attributes:           []string{attributeARN, "RoleId"},
	RequiredCapabilities: []string{"CAPABILITY_IAM"},
}

func (*iamRoleAdapter) Schema() Schema { return cloneSchema(iamRoleSchema) }

func (a *iamRoleAdapter) RequiredCapabilities(properties map[string]any) []string {
	if iamNamedResource(properties, "RoleName") {
		return []string{"CAPABILITY_NAMED_IAM"}
	}

	return append([]string(nil), a.Schema().RequiredCapabilities...)
}

//nolint:cyclop,funlen // The adapter validates and translates each supported IAM property.
func (a *iamRoleAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(iamRoleSchema, req.Properties); err != nil {
		return CreateResult{}, err
	}
	if err := requireCapabilities(CapabilitiesFor(a, req.Properties), req.Capabilities); err != nil {
		return CreateResult{}, err
	}

	policyDocument, err := jsonDocument("AssumeRolePolicyDocument", req.Properties["AssumeRolePolicyDocument"])
	if err != nil {
		return CreateResult{}, err
	}
	roleName, err := optionalString(req.Properties, "RoleName")
	if err != nil {
		return CreateResult{}, err
	}
	if roleName == "" {
		roleName = generatedPhysicalName("cfn-iam-role")
	}
	path, err := optionalString(req.Properties, "Path")
	if err != nil {
		return CreateResult{}, err
	}
	description, err := optionalString(req.Properties, iamRoleDescriptionProperty)
	if err != nil {
		return CreateResult{}, err
	}
	maxSessionDuration, err := optionalInt(req.Properties, "MaxSessionDuration")
	if err != nil || maxSessionDuration < 0 || maxSessionDuration > math.MaxInt32 {
		if err == nil {
			err = fmt.Errorf("MaxSessionDuration is out of range")
		}

		return CreateResult{}, err
	}
	tags, err := iamTags(req.Properties["Tags"])
	if err != nil {
		return CreateResult{}, err
	}
	managedPolicyARNs, err := stringListProperty(req.Properties, "ManagedPolicyArns")
	if err != nil {
		return CreateResult{}, err
	}
	inlinePolicies, err := inlineRolePolicies(req.Properties["Policies"])
	if err != nil {
		return CreateResult{}, err
	}

	storage, err := lookupStorage[iam.Storage]("iam")
	if err != nil {
		return CreateResult{}, err
	}
	role, err := createIAMRole(ctx, storage, iamRoleProvisionRequest{
		Create: &iam.CreateRoleRequest{
			RoleName:                 roleName,
			AssumeRolePolicyDocument: policyDocument,
			Path:                     path,
			Description:              description,
			MaxSessionDuration:       int(maxSessionDuration),
			Tags:                     tags,
		},
		ManagedPolicyARNs: managedPolicyARNs,
		InlinePolicies:    inlinePolicies,
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("create IAM role: %w", err)
	}

	return CreateResult{Identifier: role.RoleName, Attributes: map[string]any{
		attributeARN: role.Arn,
		"RoleId":     role.RoleID,
	}}, nil
}

//nolint:gocognit,gocyclo,cyclop,funlen,nlreturn // IAM role properties map to several independent AWS APIs.
func (a *iamRoleAdapter) Update(ctx context.Context, req UpdateRequest) (UpdateResult, error) {
	if err := validateProperties(iamRoleSchema, req.Properties); err != nil {
		return UpdateResult{}, err
	}
	storage, err := lookupStorage[iam.Storage]("iam")
	if err != nil {
		return UpdateResult{}, err
	}
	if value, ok := req.Properties["AssumeRolePolicyDocument"]; ok {
		document, err := jsonDocument("AssumeRolePolicyDocument", value)
		if err != nil {
			return UpdateResult{}, err
		}
		if err := storage.UpdateAssumeRolePolicy(ctx, req.Identifier, document); err != nil {
			return UpdateResult{}, fmt.Errorf("update IAM trust policy: %w", err)
		}
	}
	descriptionText, err := optionalString(req.Properties, iamRoleDescriptionProperty)
	if err != nil {
		return UpdateResult{}, err
	}
	description := &descriptionText
	maxSessionDurationValue := 3600
	if value, ok := req.Properties["MaxSessionDuration"]; ok {
		parsed, err := optionalInt(map[string]any{"MaxSessionDuration": value}, "MaxSessionDuration")
		if err != nil || parsed < 0 || parsed > math.MaxInt32 {
			if err == nil {
				err = fmt.Errorf("MaxSessionDuration is out of range")
			}
			return UpdateResult{}, err
		}
		maxSessionDurationValue = int(parsed)
	}
	if err := storage.UpdateRole(ctx, req.Identifier, description, &maxSessionDurationValue); err != nil {
		return UpdateResult{}, fmt.Errorf("update IAM role: %w", err)
	}

	newManaged, err := stringListProperty(req.Properties, "ManagedPolicyArns")
	if err != nil {
		return UpdateResult{}, err
	}
	oldManaged, err := stringListProperty(req.PreviousProperties, "ManagedPolicyArns")
	if err != nil {
		return UpdateResult{}, err
	}
	oldManagedSet := make(map[string]struct{}, len(oldManaged))
	for _, policyARN := range oldManaged {
		oldManagedSet[policyARN] = struct{}{}
	}
	newManagedSet := make(map[string]struct{}, len(newManaged))
	for _, policyARN := range newManaged {
		newManagedSet[policyARN] = struct{}{}
		if _, exists := oldManagedSet[policyARN]; !exists {
			if err := storage.AttachRolePolicy(ctx, req.Identifier, policyARN); err != nil {
				return UpdateResult{}, fmt.Errorf("attach IAM managed policy: %w", err)
			}
		}
	}
	for policyARN := range oldManagedSet {
		if _, exists := newManagedSet[policyARN]; !exists {
			if err := storage.DetachRolePolicy(ctx, req.Identifier, policyARN); err != nil {
				return UpdateResult{}, fmt.Errorf("detach IAM managed policy: %w", err)
			}
		}
	}
	newPolicies, err := inlineRolePolicies(req.Properties["Policies"])
	if err != nil {
		return UpdateResult{}, err
	}
	oldPolicies, err := inlineRolePolicies(req.PreviousProperties["Policies"])
	if err != nil {
		return UpdateResult{}, err
	}
	oldByName := make(map[string]struct{}, len(oldPolicies))
	for _, policy := range oldPolicies {
		oldByName[policy.Name] = struct{}{}
	}
	newByName := make(map[string]struct{}, len(newPolicies))
	for _, policy := range newPolicies {
		newByName[policy.Name] = struct{}{}
		if err := storage.PutRolePolicy(ctx, req.Identifier, policy.Name, policy.Document); err != nil {
			return UpdateResult{}, fmt.Errorf("update IAM inline policy: %w", err)
		}
	}
	for name := range oldByName {
		if _, ok := newByName[name]; !ok {
			if err := storage.DeleteRolePolicy(ctx, req.Identifier, name); err != nil {
				return UpdateResult{}, fmt.Errorf("delete IAM inline policy: %w", err)
			}
		}
	}
	newTags, err := iamTags(req.Properties[tagsProperty])
	if err != nil {
		return UpdateResult{}, err
	}
	oldTags, err := iamTags(req.PreviousProperties[tagsProperty])
	if err != nil {
		return UpdateResult{}, err
	}
	oldTagKeys := make([]string, 0, len(oldTags))
	newTagKeys := make(map[string]struct{}, len(newTags))
	for _, tag := range newTags {
		newTagKeys[tag.Key] = struct{}{}
	}
	for _, tag := range oldTags {
		if _, exists := newTagKeys[tag.Key]; !exists {
			oldTagKeys = append(oldTagKeys, tag.Key)
		}
	}
	if len(newTags) > 0 {
		if err := storage.TagRole(ctx, req.Identifier, newTags); err != nil {
			return UpdateResult{}, fmt.Errorf("update IAM role tags: %w", err)
		}
	}
	if len(oldTagKeys) > 0 {
		if err := storage.UntagRole(ctx, req.Identifier, oldTagKeys); err != nil {
			return UpdateResult{}, fmt.Errorf("remove IAM role tags: %w", err)
		}
	}
	return UpdateResult{}, nil
}

func (*iamRoleAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	storage, err := lookupStorage[iam.Storage]("iam")
	if err != nil {
		return err
	}

	attached, err := storage.ListAttachedRolePolicies(ctx, req.Identifier)
	if err != nil {
		return fmt.Errorf("list IAM role policies: %w", err)
	}
	for _, policy := range attached {
		if err := storage.DetachRolePolicy(ctx, req.Identifier, policy.PolicyArn); err != nil {
			return fmt.Errorf("detach IAM role policy: %w", err)
		}
	}
	inlinePolicies, err := storage.ListRolePolicies(ctx, req.Identifier)
	if err != nil {
		return fmt.Errorf("list IAM inline policies: %w", err)
	}
	for _, policyName := range inlinePolicies {
		if err := storage.DeleteRolePolicy(ctx, req.Identifier, policyName); err != nil {
			return fmt.Errorf("delete IAM inline policy: %w", err)
		}
	}
	if err := storage.DeleteRole(ctx, req.Identifier); err != nil {
		return fmt.Errorf("delete IAM role: %w", err)
	}

	return nil
}

func inlineRolePolicies(value any) ([]iamInlineRolePolicy, error) {
	if value == nil {
		return nil, nil
	}
	policies, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("policies must be a list")
	}
	result := make([]iamInlineRolePolicy, 0, len(policies))
	for _, value := range policies {
		policy, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("policies must contain objects")
		}
		if err := validateNestedProperties("Policies", policy, "PolicyName", "PolicyDocument"); err != nil {
			return nil, err
		}
		name, err := requiredString(policy, "PolicyName")
		if err != nil {
			return nil, err
		}
		document, err := jsonDocument("PolicyDocument", policy["PolicyDocument"])
		if err != nil {
			return nil, err
		}
		result = append(result, iamInlineRolePolicy{Name: name, Document: document})
	}

	return result, nil
}

func iamNamedResource(properties map[string]any, property string) bool {
	value, ok := properties[property]
	name, nameOK := value.(string)

	return ok && nameOK && name != ""
}

func requireCapabilities(required, provided []string) error {
	for _, capability := range required {
		if slices.Contains(provided, capability) || capability == "CAPABILITY_IAM" && slices.Contains(provided, "CAPABILITY_NAMED_IAM") {
			continue
		}

		return fmt.Errorf("resource requires %s", capability)
	}

	return nil
}

func jsonDocument(property string, value any) (string, error) {
	if value == nil {
		return "", fmt.Errorf("%s is required", property)
	}
	if text, ok := value.(string); ok {
		var document any
		if err := json.Unmarshal([]byte(text), &document); err != nil || document == nil {
			return "", fmt.Errorf("%s must be valid JSON", property)
		}

		return text, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("serialize %s: %w", property, err)
	}

	return string(encoded), nil
}

func optionalString(properties map[string]any, property string) (string, error) {
	value, ok := properties[property]
	if !ok {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", property)
	}

	return text, nil
}

func requiredString(properties map[string]any, property string) (string, error) {
	text, err := optionalString(properties, property)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", fmt.Errorf("%s is required", property)
	}

	return text, nil
}

func optionalInt(properties map[string]any, property string) (int64, error) {
	value, ok := properties[property]
	if !ok {
		return 0, nil
	}
	switch value := value.(type) {
	case int:
		return int64(value), nil
	case int64:
		return value, nil
	case float64:
		if value == math.Trunc(value) {
			return int64(value), nil
		}
	case string:
		if integer, valid := strictIntegerString(value); valid {
			return integer, nil
		}
	}

	return 0, fmt.Errorf("%s must be an integer", property)
}

func stringListProperty(properties map[string]any, property string) ([]string, error) {
	value, ok := properties[property]
	if !ok {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list", property)
	}
	result := make([]string, len(values))
	for index := range values {
		value := values[index]
		text, ok := value.(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("%s[%d] must be a non-empty string", property, index)
		}
		result[index] = text
	}

	return result, nil
}

func iamTags(value any) ([]iam.Tag, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("tags must be a list")
	}
	tags := make([]iam.Tag, len(values))
	for index, value := range values {
		tag, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tags[%d] must be an object", index)
		}
		if err := validateNestedProperties("Tags", tag, "Key", "Value"); err != nil {
			return nil, err
		}
		key, err := requiredString(tag, "Key")
		if err != nil {
			return nil, err
		}
		valueText, err := requiredString(tag, "Value")
		if err != nil {
			return nil, err
		}
		tags[index] = iam.Tag{Key: key, Value: valueText}
	}

	return tags, nil
}
