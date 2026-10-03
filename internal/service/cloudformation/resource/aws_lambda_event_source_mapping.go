//nolint:goconst,wsl // CloudFormation property translation is intentionally explicit.
package resource

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/sivchari/kumo/internal/service/lambda"
)

// lambdaEventSourceMappingAdapter implements AWS::Lambda::EventSourceMapping.
type lambdaEventSourceMappingAdapter struct{}

var lambdaEventSourceMappingSchema = Schema{
	TypeName: "AWS::Lambda::EventSourceMapping",
	PropertyUpdate: map[string]PropertyUpdate{
		"BatchSize":                      propertyUpdateNoInterruption,
		"Enabled":                        propertyUpdateNoInterruption,
		"EventSourceArn":                 propertyUpdateReplacement,
		"FunctionName":                   propertyUpdateNoInterruption,
		"MaximumBatchingWindowInSeconds": propertyUpdateNoInterruption,
	},
	Attributes: []string{"EventSourceMappingArn", "Id"},
}

func (*lambdaEventSourceMappingAdapter) Schema() Schema {
	return cloneSchema(lambdaEventSourceMappingSchema)
}

//nolint:cyclop,funlen // The adapter validates and translates each supported mapping property.
func (*lambdaEventSourceMappingAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(lambdaEventSourceMappingSchema, req.Properties); err != nil {
		return CreateResult{}, err
	}
	functionName, err := requiredString(req.Properties, "FunctionName")
	if err != nil {
		return CreateResult{}, err
	}
	eventSourceARN, err := requiredString(req.Properties, "EventSourceArn")
	if err != nil {
		return CreateResult{}, err
	}
	batchSize, err := optionalInt(req.Properties, "BatchSize")
	if err != nil || batchSize < 0 || batchSize > math.MaxInt32 {
		if err == nil {
			err = fmt.Errorf("BatchSize is out of range")
		}

		return CreateResult{}, err
	}
	window, err := optionalInt(req.Properties, "MaximumBatchingWindowInSeconds")
	if err != nil || window < 0 || window > math.MaxInt32 {
		if err == nil {
			err = fmt.Errorf("MaximumBatchingWindowInSeconds is out of range")
		}

		return CreateResult{}, err
	}
	enabled := true
	if value, ok := req.Properties["Enabled"]; ok {
		var valid bool
		enabled, valid = value.(bool)
		if !valid {
			return CreateResult{}, fmt.Errorf("enabled must be a boolean")
		}
	}

	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		return CreateResult{}, err
	}
	functionName, err = lambdaFunctionName(ctx, storage, functionName)
	if err != nil {
		return CreateResult{}, err
	}
	mapping, err := storage.CreateEventSourceMapping(ctx, &lambda.CreateEventSourceMappingRequest{
		FunctionName:                   functionName,
		EventSourceArn:                 eventSourceARN,
		BatchSize:                      int(batchSize),
		MaximumBatchingWindowInSeconds: int(window),
		Enabled:                        &enabled,
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("create Lambda event source mapping: %w", err)
	}
	mappingARN, err := lambdaEventSourceMappingARN(mapping.FunctionArn, mapping.UUID)
	if err != nil {
		return CreateResult{}, err
	}

	return CreateResult{Identifier: mapping.UUID, Attributes: map[string]any{
		"EventSourceMappingArn": mappingARN,
		"Id":                    mapping.UUID,
	}}, nil
}

func lambdaEventSourceMappingARN(functionARN, mappingID string) (string, error) {
	functionMarker := strings.Index(functionARN, ":function:")
	if functionMarker < 0 {
		return "", fmt.Errorf("construct Lambda event source mapping ARN from %q", functionARN)
	}

	return functionARN[:functionMarker] + ":event-source-mapping:" + mappingID, nil
}

//nolint:cyclop,nlreturn // Each optional mapping property has an independent default and validation path.
func (*lambdaEventSourceMappingAdapter) Update(ctx context.Context, req UpdateRequest) (UpdateResult, error) {
	if err := validateProperties(lambdaEventSourceMappingSchema, req.Properties); err != nil {
		return UpdateResult{}, err
	}
	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		return UpdateResult{}, err
	}
	batchSize := 10
	if value, ok := req.Properties["BatchSize"]; ok {
		parsed, parseErr := optionalInt(map[string]any{"BatchSize": value}, "BatchSize")
		if parseErr != nil || parsed < 0 || parsed > math.MaxInt32 {
			if parseErr == nil {
				parseErr = fmt.Errorf("BatchSize is out of range")
			}
			return UpdateResult{}, parseErr
		}
		batchSize = int(parsed)
	}
	window := 0
	if value, ok := req.Properties["MaximumBatchingWindowInSeconds"]; ok {
		parsed, parseErr := optionalInt(map[string]any{"MaximumBatchingWindowInSeconds": value}, "MaximumBatchingWindowInSeconds")
		if parseErr != nil || parsed < 0 || parsed > math.MaxInt32 {
			if parseErr == nil {
				parseErr = fmt.Errorf("MaximumBatchingWindowInSeconds is out of range")
			}
			return UpdateResult{}, parseErr
		}
		window = int(parsed)
	}
	enabled := true
	if value, ok := req.Properties["Enabled"]; ok {
		var valid bool
		enabled, valid = value.(bool)
		if !valid {
			return UpdateResult{}, fmt.Errorf("enabled must be a boolean")
		}
	}
	var functionName *string
	if value, ok := req.Properties["FunctionName"]; ok {
		name, valid := value.(string)
		if !valid {
			return UpdateResult{}, fmt.Errorf("FunctionName must be a string")
		}
		name, err = lambdaFunctionName(ctx, storage, name)
		if err != nil {
			return UpdateResult{}, err
		}
		functionName = &name
	}
	if _, err := storage.UpdateEventSourceMapping(ctx, req.Identifier, &lambda.UpdateEventSourceMappingRequest{
		FunctionName: functionName, BatchSize: &batchSize, MaximumBatchingWindowInSeconds: &window, Enabled: &enabled,
	}); err != nil {
		return UpdateResult{}, fmt.Errorf("update Lambda event source mapping: %w", err)
	}

	return UpdateResult{}, nil
}

func (*lambdaEventSourceMappingAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		return err
	}
	if err := storage.DeleteEventSourceMapping(ctx, req.Identifier); err != nil {
		return fmt.Errorf("delete Lambda event source mapping: %w", err)
	}

	return nil
}
