//nolint:goconst,wsl // CloudFormation property translation is intentionally explicit.
package resource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sivchari/kumo/internal/service/lambda"
)

// lambdaPermissionAdapter implements AWS::Lambda::Permission.
type lambdaPermissionAdapter struct{}

var lambdaPermissionSchema = Schema{
	TypeName: "AWS::Lambda::Permission",
	PropertyUpdate: map[string]PropertyUpdate{
		"Action":                propertyUpdateReplacement,
		"EventSourceToken":      propertyUpdateReplacement,
		"FunctionName":          propertyUpdateReplacement,
		"FunctionUrlAuthType":   propertyUpdateReplacement,
		"InvokedViaFunctionUrl": propertyUpdateReplacement,
		"Principal":             propertyUpdateReplacement,
		"PrincipalOrgID":        propertyUpdateReplacement,
		"SourceAccount":         propertyUpdateReplacement,
		"SourceArn":             propertyUpdateReplacement,
	},
}

func (*lambdaPermissionAdapter) Schema() Schema { return cloneSchema(lambdaPermissionSchema) }

//nolint:cyclop,funlen // The adapter validates and translates each supported permission property.
func (*lambdaPermissionAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(lambdaPermissionSchema, req.Properties); err != nil {
		return CreateResult{}, err
	}
	action, err := requiredString(req.Properties, "Action")
	if err != nil {
		return CreateResult{}, err
	}
	principal, err := requiredString(req.Properties, "Principal")
	if err != nil {
		return CreateResult{}, err
	}
	functionValue, err := requiredString(req.Properties, "FunctionName")
	if err != nil {
		return CreateResult{}, err
	}

	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		return CreateResult{}, err
	}
	functionName, err := lambdaFunctionName(ctx, storage, functionValue)
	if err != nil {
		return CreateResult{}, err
	}
	function, err := storage.GetFunction(ctx, functionName)
	if err != nil {
		return CreateResult{}, fmt.Errorf("get Lambda function for permission: %w", err)
	}
	for _, property := range []string{"EventSourceToken", "FunctionUrlAuthType", "InvokedViaFunctionUrl"} {
		if _, ok := req.Properties[property]; ok {
			return CreateResult{}, fmt.Errorf("%s is documented by Lambda but unobservable by Lambda storage", property)
		}
	}
	statementID := lambdaPermissionStatementID(function.FunctionArn, req.Properties)
	principalKey := "Service"
	if principal == "*" || strings.HasPrefix(principal, "arn:") || isAWSAccountID(principal) {
		principalKey = "AWS"
	}
	statement := &lambda.PolicyStatement{
		Sid:       statementID,
		Effect:    "Allow",
		Principal: map[string]string{principalKey: principal},
		Action:    action,
		Resource:  function.FunctionArn,
	}
	condition := map[string]any{}
	stringEquals := map[string]string{}
	for property, key := range map[string]string{"SourceArn": "AWS:SourceArn", "SourceAccount": "AWS:SourceAccount", "PrincipalOrgID": "aws:PrincipalOrgID"} {
		if value, ok := req.Properties[property]; ok {
			text, ok := value.(string)
			if !ok {
				return CreateResult{}, fmt.Errorf("%s must be a string", property)
			}
			if property == "SourceArn" {
				condition["ArnLike"] = map[string]string{key: text}
			} else {
				stringEquals[key] = text
			}
		}
	}
	if len(stringEquals) > 0 {
		condition["StringEquals"] = stringEquals
	}
	if len(condition) > 0 {
		statement.Condition = condition
	}
	if err := storage.AddPermission(ctx, functionName, statement); err != nil {
		return CreateResult{}, fmt.Errorf("add Lambda permission: %w", err)
	}

	identifier, err := json.Marshal([]string{functionName, statementID})
	if err != nil {
		return CreateResult{}, fmt.Errorf("encode Lambda permission identifier: %w", err)
	}

	return CreateResult{Identifier: string(identifier)}, nil
}

func lambdaPermissionStatementID(functionARN string, properties map[string]any) string {
	identity := struct {
		FunctionARN    string
		Action         any
		Principal      any
		PrincipalOrgID any
		SourceAccount  any
		SourceArn      any
	}{
		FunctionARN:    functionARN,
		Action:         properties["Action"],
		Principal:      properties["Principal"],
		PrincipalOrgID: properties["PrincipalOrgID"],
		SourceAccount:  properties["SourceAccount"],
		SourceArn:      properties["SourceArn"],
	}
	encoded, _ := json.Marshal(identity)
	digest := sha256.Sum256(encoded)

	return "cfn-" + hex.EncodeToString(digest[:12])
}

func isAWSAccountID(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}

	return true
}

func (*lambdaPermissionAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	var parts []string
	if err := json.Unmarshal([]byte(req.Identifier), &parts); err != nil || len(parts) != 2 {
		return fmt.Errorf("invalid Lambda permission identifier")
	}
	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		return err
	}
	if err := storage.RemovePermission(ctx, parts[0], parts[1]); err != nil {
		return fmt.Errorf("remove Lambda permission: %w", err)
	}

	return nil
}
