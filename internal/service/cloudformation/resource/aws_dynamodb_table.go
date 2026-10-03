package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/sivchari/kumo/internal/service/dynamodb"
)

// dynamoDBTableAdapter implements AWS::DynamoDB::Table.
// Ref and Fn::GetAtt: https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-dynamodb-table.html
type dynamoDBTableAdapter struct{}

var dynamoDBTableSchema = Schema{
	TypeName: dynamoDBTableTypeName,
	PropertyUpdate: map[string]PropertyUpdate{
		attributeDefinitionsProperty: propertyUpdateReplacement,
		billingModeProperty:          propertyUpdateReplacement,
		"DeletionProtectionEnabled":  propertyUpdateReplacement,
		"GlobalSecondaryIndexes":     propertyUpdateReplacement,
		keySchemaProperty:            propertyUpdateReplacement,
		"LocalSecondaryIndexes":      propertyUpdateReplacement,
		"ProvisionedThroughput":      propertyUpdateReplacement,
		"StreamSpecification":        propertyUpdateReplacement,
		"TableClass":                 propertyUpdateNoInterruption,
		tableNameProperty:            propertyUpdateReplacement,
		tagsProperty:                 propertyUpdateReplacement,
		"WarmThroughput":             propertyUpdateReplacement,
	},
	Attributes: []string{attributeARN, "StreamArn"},
}

func (*dynamoDBTableAdapter) Schema() Schema { return cloneSchema(dynamoDBTableSchema) }

func (*dynamoDBTableAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(dynamoDBTableSchema, req.Properties); err != nil {
		return CreateResult{}, err
	}

	normalizedProperties, err := normalizeDynamoDBProperties(req.Properties)
	if err != nil {
		return CreateResult{}, err
	}

	properties, err := json.Marshal(normalizedProperties)
	if err != nil {
		return CreateResult{}, fmt.Errorf("marshal DynamoDB properties: %w", err)
	}

	var createRequest dynamodb.CreateTableRequest

	if err := json.Unmarshal(properties, &createRequest); err != nil {
		return CreateResult{}, fmt.Errorf("decode DynamoDB properties: %w", err)
	}

	_, tableNameProvided := req.Properties[tableNameProperty]
	if tableNameProvided && createRequest.TableName == "" {
		return CreateResult{}, fmt.Errorf("TableName must not be empty")
	}

	if !tableNameProvided {
		createRequest.TableName = "CfnDynamoDB" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}

	if err := validateDynamoDBTable(&createRequest); err != nil {
		return CreateResult{}, err
	}

	storage, err := lookupStorage[dynamodb.Storage]("dynamodb")
	if err != nil {
		return CreateResult{}, err
	}

	table, err := storage.CreateTable(ctx, &createRequest)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create DynamoDB table: %w", err)
	}

	return CreateResult{Identifier: createRequest.TableName, Attributes: map[string]any{
		attributeARN: table.TableARN,
		"StreamArn":  table.LatestStreamArn,
	}}, nil
}

//nolint:wsl // DynamoDB table class updates preserve the explicit AWS default.
func (*dynamoDBTableAdapter) Update(ctx context.Context, req UpdateRequest) (UpdateResult, error) {
	if err := validateProperties(dynamoDBTableSchema, req.Properties); err != nil {
		return UpdateResult{}, err
	}
	class := "STANDARD"
	if value, ok := req.Properties["TableClass"]; ok {
		var valid bool
		class, valid = value.(string)
		if !valid {
			return UpdateResult{}, fmt.Errorf("TableClass must be a string")
		}
	}
	storage, err := lookupStorage[dynamodb.Storage]("dynamodb")
	if err != nil {
		return UpdateResult{}, err
	}
	if _, err := storage.UpdateTable(ctx, &dynamodb.UpdateTableRequest{TableName: req.Identifier, TableClass: class}); err != nil {
		return UpdateResult{}, fmt.Errorf("update DynamoDB table: %w", err)
	}

	return UpdateResult{}, nil
}

//nolint:cyclop // Validation mirrors the related DynamoDB schema invariants.
func validateDynamoDBTable(req *dynamodb.CreateTableRequest) error {
	if len(req.KeySchema) == 0 {
		return fmt.Errorf("KeySchema is required")
	}

	if len(req.AttributeDefinitions) == 0 {
		return fmt.Errorf("AttributeDefinitions is required")
	}

	definitions := make(map[string]struct{}, len(req.AttributeDefinitions))

	for _, definition := range req.AttributeDefinitions {
		if definition.AttributeName == "" || !isDynamoDBAttributeType(definition.AttributeType) {
			return fmt.Errorf("AttributeDefinitions contains an invalid attribute")
		}

		if _, exists := definitions[definition.AttributeName]; exists {
			return fmt.Errorf("AttributeDefinitions contains duplicate attribute %q", definition.AttributeName)
		}

		definitions[definition.AttributeName] = struct{}{}
	}

	if err := validateDynamoDBKeySchema(req.KeySchema, definitions); err != nil {
		return fmt.Errorf("validate table KeySchema: %w", err)
	}

	for _, index := range req.GlobalSecondaryIndexes {
		if err := validateDynamoDBKeySchema(index.KeySchema, definitions); err != nil {
			return fmt.Errorf("validate global secondary index %q KeySchema: %w", index.IndexName, err)
		}

		if err := validateDynamoDBProvisionedThroughput("GlobalSecondaryIndexes ProvisionedThroughput", index.ProvisionedThroughput); err != nil {
			return fmt.Errorf("validate global secondary index %q ProvisionedThroughput: %w", index.IndexName, err)
		}
	}

	for _, index := range req.LocalSecondaryIndexes {
		if err := validateDynamoDBKeySchema(index.KeySchema, definitions); err != nil {
			return fmt.Errorf("validate local secondary index %q KeySchema: %w", index.IndexName, err)
		}
	}

	switch req.BillingMode {
	case "", "PROVISIONED":
		if req.ProvisionedThroughput == nil {
			return fmt.Errorf("ProvisionedThroughput is required for PROVISIONED billing mode")
		}

		if err := validateDynamoDBProvisionedThroughput("ProvisionedThroughput", req.ProvisionedThroughput); err != nil {
			return err
		}
	case "PAY_PER_REQUEST":
		if req.ProvisionedThroughput != nil {
			return fmt.Errorf("ProvisionedThroughput must not be specified for PAY_PER_REQUEST billing mode")
		}
	default:
		return fmt.Errorf("BillingMode must be PROVISIONED or PAY_PER_REQUEST")
	}

	return nil
}

func normalizeDynamoDBProperties(properties map[string]any) (map[string]any, error) {
	normalized := make(map[string]any, len(properties))

	for property, value := range properties {
		normalizedValue, err := normalizeDynamoDBProperty(property, value)
		if err != nil {
			return nil, err
		}

		normalized[property] = normalizedValue
	}

	return normalized, nil
}

func normalizeDynamoDBProperty(property string, value any) (any, error) {
	if property == "ReadCapacityUnits" || property == "WriteCapacityUnits" {
		if stringValue, ok := value.(string); ok {
			integer, valid := strictIntegerString(stringValue)
			if !valid {
				return nil, fmt.Errorf("%s must be a strict integer string", property)
			}

			return integer, nil
		}
	}

	switch value := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(value))

		for nestedProperty, nestedValue := range value {
			normalizedValue, err := normalizeDynamoDBProperty(nestedProperty, nestedValue)
			if err != nil {
				return nil, err
			}

			normalized[nestedProperty] = normalizedValue
		}

		return normalized, nil
	case []any:
		normalized := make([]any, len(value))

		for index, nestedValue := range value {
			normalizedValue, err := normalizeDynamoDBProperty(property, nestedValue)
			if err != nil {
				return nil, err
			}

			normalized[index] = normalizedValue
		}

		return normalized, nil
	default:
		return value, nil
	}
}

func validateDynamoDBProvisionedThroughput(property string, throughput *dynamodb.ProvisionedThroughput) error {
	if throughput == nil {
		return nil
	}

	if throughput.ReadCapacityUnits <= 0 || throughput.WriteCapacityUnits <= 0 {
		return fmt.Errorf("%s must contain positive capacity units", property)
	}

	return nil
}

func validateDynamoDBKeySchema(keySchema []dynamodb.KeySchemaElement, definitions map[string]struct{}) error {
	if len(keySchema) == 0 {
		return fmt.Errorf("KeySchema is required")
	}

	keys := make(map[string]struct{}, len(keySchema))
	hashKeys := 0
	rangeKeys := 0

	for _, key := range keySchema {
		if key.AttributeName == "" {
			return fmt.Errorf("KeySchema contains an empty AttributeName")
		}

		if _, exists := definitions[key.AttributeName]; !exists {
			return fmt.Errorf("KeySchema attribute %q is not declared", key.AttributeName)
		}

		if _, exists := keys[key.AttributeName]; exists {
			return fmt.Errorf("KeySchema contains duplicate attribute %q", key.AttributeName)
		}

		keys[key.AttributeName] = struct{}{}

		switch key.KeyType {
		case hashKeyType:
			hashKeys++
		case "RANGE":
			rangeKeys++
		default:
			return fmt.Errorf("KeySchema contains an invalid KeyType")
		}
	}

	if hashKeys != 1 || rangeKeys > 1 {
		return fmt.Errorf("KeySchema must contain one HASH key and at most one RANGE key")
	}

	return nil
}

func isDynamoDBAttributeType(attributeType string) bool {
	return attributeType == "S" || attributeType == "N" || attributeType == "B"
}

func (*dynamoDBTableAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	storage, err := lookupStorage[dynamodb.Storage]("dynamodb")
	if err != nil {
		return err
	}

	table, err := storage.DescribeTable(ctx, req.Identifier)
	if err != nil {
		return fmt.Errorf("describe DynamoDB table before deletion: %w", err)
	}

	if table.DeletionProtection {
		return fmt.Errorf("DynamoDB table %q has deletion protection enabled", req.Identifier)
	}

	_, err = storage.DeleteTable(ctx, req.Identifier)
	if err != nil {
		return fmt.Errorf("delete DynamoDB table: %w", err)
	}

	return nil
}
