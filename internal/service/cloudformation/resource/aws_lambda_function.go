//nolint:goconst,wsl // CloudFormation property translation is intentionally explicit.
package resource

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/sivchari/kumo/internal/service/lambda"
)

// lambdaFunctionAdapter implements AWS::Lambda::Function.
type lambdaFunctionAdapter struct{}

var lambdaFunctionSchema = Schema{
	TypeName: "AWS::Lambda::Function",
	PropertyUpdate: map[string]PropertyUpdate{
		"Architectures": propertyUpdateNoInterruption,
		"Code":          propertyUpdateNoInterruption,
		"Description":   propertyUpdateNoInterruption,
		"Environment":   propertyUpdateNoInterruption,
		"FunctionName":  propertyUpdateReplacement,
		"Handler":       propertyUpdateNoInterruption,
		"MemorySize":    propertyUpdateNoInterruption,
		"PackageType":   propertyUpdateReplacement,
		"Role":          propertyUpdateNoInterruption,
		"Runtime":       propertyUpdateNoInterruption,
		"Tags":          propertyUpdateNoInterruption,
		"Timeout":       propertyUpdateNoInterruption,
	},
	Attributes: []string{attributeARN},
}

func (*lambdaFunctionAdapter) Schema() Schema { return cloneSchema(lambdaFunctionSchema) }

//nolint:cyclop,funlen // The adapter validates and translates each supported Lambda property.
func (*lambdaFunctionAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(lambdaFunctionSchema, req.Properties); err != nil {
		return CreateResult{}, err
	}
	role, err := requiredString(req.Properties, "Role")
	if err != nil {
		return CreateResult{}, err
	}
	code, err := lambdaCode(req.Properties["Code"])
	if err != nil {
		return CreateResult{}, err
	}
	functionName, err := optionalString(req.Properties, "FunctionName")
	if err != nil {
		return CreateResult{}, err
	}
	if functionName == "" {
		functionName = generatedPhysicalName("cfn-lambda")
	}
	runtime, err := optionalString(req.Properties, "Runtime")
	if err != nil {
		return CreateResult{}, err
	}
	handler, err := optionalString(req.Properties, "Handler")
	if err != nil {
		return CreateResult{}, err
	}
	description, err := optionalString(req.Properties, "Description")
	if err != nil {
		return CreateResult{}, err
	}
	packageType, err := optionalString(req.Properties, "PackageType")
	if err != nil {
		return CreateResult{}, err
	}
	architectures, err := stringListProperty(req.Properties, "Architectures")
	if err != nil {
		return CreateResult{}, err
	}
	timeout, err := optionalInt(req.Properties, "Timeout")
	if err != nil || timeout < 0 || timeout > math.MaxInt32 {
		if err == nil {
			err = fmt.Errorf("timeout is out of range")
		}

		return CreateResult{}, err
	}
	memorySize, err := optionalInt(req.Properties, "MemorySize")
	if err != nil || memorySize < 0 || memorySize > math.MaxInt32 {
		if err == nil {
			err = fmt.Errorf("memory size is out of range")
		}

		return CreateResult{}, err
	}
	environment, err := lambdaEnvironment(req.Properties["Environment"])
	if err != nil {
		return CreateResult{}, err
	}
	tags, err := lambdaTags(req.Properties["Tags"])
	if err != nil {
		return CreateResult{}, err
	}

	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		return CreateResult{}, err
	}
	function, err := storage.CreateFunction(ctx, &lambda.CreateFunctionRequest{
		FunctionName:  functionName,
		Runtime:       runtime,
		Role:          role,
		Handler:       handler,
		Code:          code,
		Description:   description,
		Timeout:       int(timeout),
		MemorySize:    int(memorySize),
		PackageType:   packageType,
		Architectures: architectures,
		Environment:   environment,
		Tags:          tags,
	})
	if err != nil {
		return CreateResult{}, fmt.Errorf("create Lambda function: %w", err)
	}

	return CreateResult{Identifier: function.FunctionName, Attributes: map[string]any{attributeARN: function.FunctionArn}}, nil
}

//nolint:gocognit,cyclop,funlen,nestif // Lambda code, configuration, environment, and tags use separate AWS APIs.
func (*lambdaFunctionAdapter) Update(ctx context.Context, req UpdateRequest) (UpdateResult, error) {
	if err := validateProperties(lambdaFunctionSchema, req.Properties); err != nil {
		return UpdateResult{}, err
	}
	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		return UpdateResult{}, err
	}
	_, codeChanged := req.Properties["Code"]
	_, architecturesChanged := req.Properties["Architectures"]
	if codeChanged || architecturesChanged {
		code := lambda.FunctionCode{}
		if codeChanged {
			code, err = lambdaCode(req.Properties["Code"])
			if err != nil {
				return UpdateResult{}, err
			}
		}
		architectures, architectureErr := stringListProperty(req.Properties, "Architectures")
		if architectureErr != nil {
			return UpdateResult{}, architectureErr
		}
		if len(architectures) == 0 {
			architectures = []string{"x86_64"}
		}
		_, err = storage.UpdateFunctionCode(ctx, req.Identifier, &lambda.UpdateFunctionCodeRequest{
			ZipFile: code.ZipFile, S3Bucket: code.S3Bucket, S3Key: code.S3Key, S3ObjectVersion: code.S3ObjectVersion, ImageURI: code.ImageURI, Architectures: &architectures,
		})
		if err != nil {
			return UpdateResult{}, fmt.Errorf("update Lambda code: %w", err)
		}
	}
	description, err := lambdaStringProperty(req.Properties, "Description")
	if err != nil {
		return UpdateResult{}, err
	}
	handler, err := lambdaStringProperty(req.Properties, "Handler")
	if err != nil {
		return UpdateResult{}, err
	}
	role, err := lambdaStringProperty(req.Properties, "Role")
	if err != nil {
		return UpdateResult{}, err
	}
	runtime, err := lambdaStringProperty(req.Properties, "Runtime")
	if err != nil {
		return UpdateResult{}, err
	}
	timeout, err := lambdaIntegerProperty(req.Properties, "Timeout", 3, 0, math.MaxInt32)
	if err != nil {
		return UpdateResult{}, err
	}
	memorySize, err := lambdaIntegerProperty(req.Properties, "MemorySize", 128, 0, math.MaxInt32)
	if err != nil {
		return UpdateResult{}, err
	}
	environment, err := lambdaEnvironment(req.Properties["Environment"])
	if err != nil {
		return UpdateResult{}, err
	}
	configuration := &lambda.UpdateFunctionConfigurationRequest{
		Description: &description, Handler: &handler, MemorySize: &memorySize, Role: &role,
		Runtime: &runtime, Timeout: &timeout, Environment: &environment,
	}
	if _, err := storage.UpdateFunctionConfiguration(ctx, req.Identifier, configuration); err != nil {
		return UpdateResult{}, fmt.Errorf("update Lambda configuration: %w", err)
	}
	tags, err := lambdaTags(req.Properties["Tags"])
	if err != nil {
		return UpdateResult{}, err
	}
	function, err := storage.GetFunction(ctx, req.Identifier)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("get Lambda function: %w", err)
	}
	if err := storage.TagResource(ctx, function.FunctionArn, tags); err != nil {
		return UpdateResult{}, fmt.Errorf("update Lambda tags: %w", err)
	}
	oldTags, err := lambdaTags(req.PreviousProperties["Tags"])
	if err != nil {
		return UpdateResult{}, err
	}
	removed := make([]string, 0)
	for key := range oldTags {
		if _, exists := tags[key]; !exists {
			removed = append(removed, key)
		}
	}
	if len(removed) > 0 {
		if err := storage.UntagResource(ctx, function.FunctionArn, removed); err != nil {
			return UpdateResult{}, fmt.Errorf("remove Lambda tags: %w", err)
		}
	}

	return UpdateResult{}, nil
}

func lambdaStringProperty(properties map[string]any, name string) (string, error) {
	value, ok := properties[name]
	if !ok {
		return "", nil
	}
	text, valid := value.(string)
	if !valid {
		return "", fmt.Errorf("%s must be a string", name)
	}

	return text, nil
}

func lambdaIntegerProperty(properties map[string]any, name string, defaultValue, minimum, maximum int64) (int, error) {
	value, ok := properties[name]
	if !ok {
		return int(defaultValue), nil
	}
	parsed, err := optionalInt(map[string]any{name: value}, name)
	if err != nil || parsed < minimum || parsed > maximum {
		if err == nil {
			err = fmt.Errorf("%s is out of range", name)
		}

		return 0, err
	}

	return int(parsed), nil
}

func (*lambdaFunctionAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	storage, err := lookupStorage[lambda.Storage]("lambda")
	if err != nil {
		return err
	}
	if err := storage.DeleteFunction(ctx, req.Identifier); err != nil {
		return fmt.Errorf("delete Lambda function: %w", err)
	}

	return nil
}

//nolint:cyclop // Each supported Lambda code source has a distinct storage field.
func lambdaCode(value any) (lambda.FunctionCode, error) {
	properties, ok := value.(map[string]any)
	if !ok {
		return lambda.FunctionCode{}, fmt.Errorf("code must be an object")
	}
	code := lambda.FunctionCode{}
	for property, value := range properties {
		switch property {
		case "ZipFile":
			text, ok := value.(string)
			if !ok {
				return lambda.FunctionCode{}, fmt.Errorf("Code.ZipFile must be a string")
			}
			code.ZipFile = []byte(text)
		case "S3Bucket":
			code.S3Bucket, ok = value.(string)
			if !ok {
				return lambda.FunctionCode{}, fmt.Errorf("Code.S3Bucket must be a string")
			}
		case "S3Key":
			code.S3Key, ok = value.(string)
			if !ok {
				return lambda.FunctionCode{}, fmt.Errorf("Code.S3Key must be a string")
			}
		case "S3ObjectVersion":
			code.S3ObjectVersion, ok = value.(string)
			if !ok {
				return lambda.FunctionCode{}, fmt.Errorf("Code.S3ObjectVersion must be a string")
			}
		case "ImageUri":
			code.ImageURI, ok = value.(string)
			if !ok {
				return lambda.FunctionCode{}, fmt.Errorf("Code.ImageUri must be a string")
			}
		default:
			return lambda.FunctionCode{}, fmt.Errorf("code.%s is unobservable by Lambda storage", property)
		}
	}
	if len(code.ZipFile) == 0 && code.S3Bucket == "" && code.ImageURI == "" {
		return lambda.FunctionCode{}, fmt.Errorf("code must contain ZipFile, S3Bucket, or ImageUri")
	}

	return code, nil
}

func lambdaEnvironment(value any) (*lambda.Environment, error) {
	if value == nil {
		return nil, nil //nolint:nilnil // An absent optional environment is represented by nil.
	}
	properties, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("environment must be an object")
	}
	if err := validateNestedProperties("Environment", properties, "Variables"); err != nil {
		return nil, err
	}
	variablesValue, ok := properties["Variables"]
	if !ok {
		return nil, fmt.Errorf("Environment.Variables is required")
	}
	variables, ok := variablesValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Environment.Variables must be an object")
	}
	result := &lambda.Environment{Variables: make(map[string]string, len(variables))}
	for key, value := range variables {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("Environment.Variables.%s must be a string", key)
		}
		result.Variables[key] = text
	}

	return result, nil
}

func lambdaTags(value any) (map[string]string, error) {
	if value == nil {
		return nil, nil //nolint:nilnil // An absent optional tag set is represented by nil.
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("tags must be a list")
	}
	result := make(map[string]string, len(values))
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
		text, err := requiredString(tag, "Value")
		if err != nil {
			return nil, err
		}
		result[key] = text
	}

	return result, nil
}

func lambdaFunctionName(ctx context.Context, storage lambda.Storage, value string) (string, error) {
	if !strings.Contains(value, ":") {
		return value, nil
	}

	if strings.HasPrefix(value, "arn:") {
		return lambdaFunctionNameFromARN(ctx, storage, value)
	}

	return lambdaFunctionNameFromPartialARN(ctx, storage, value)
}

func lambdaFunctionNameFromARN(ctx context.Context, storage lambda.Storage, value string) (string, error) {
	parts := strings.Split(value, ":")
	if len(parts) < 7 || parts[2] != "lambda" || parts[5] != "function" || parts[6] == "" {
		return "", fmt.Errorf("unsupported Lambda function reference %q", value)
	}
	if len(parts) > 7 {
		return "", fmt.Errorf("qualified Lambda function reference %q is unsupported by Lambda storage", value)
	}
	function, err := storage.GetFunctionByARN(ctx, value)
	if err != nil {
		return "", fmt.Errorf("resolve Lambda function ARN: %w", err)
	}

	return function.FunctionName, nil
}

func lambdaFunctionNameFromPartialARN(ctx context.Context, storage lambda.Storage, value string) (string, error) {
	functionMarker := strings.Index(value, ":function:")
	if functionMarker >= 0 {
		return resolveLambdaFunctionName(ctx, storage, value[functionMarker+len(":function:"):], value)
	}
	if strings.HasPrefix(value, "function:") {
		return resolveLambdaFunctionName(ctx, storage, strings.TrimPrefix(value, "function:"), value)
	}

	return "", fmt.Errorf("qualified Lambda function reference %q is unsupported by Lambda storage", value)
}

func resolveLambdaFunctionName(ctx context.Context, storage lambda.Storage, name, value string) (string, error) {
	if name == "" || strings.Contains(name, ":") {
		return "", fmt.Errorf("qualified Lambda function reference %q is unsupported by Lambda storage", value)
	}
	if _, err := storage.GetFunction(ctx, name); err != nil {
		return "", fmt.Errorf("resolve Lambda partial ARN: %w", err)
	}

	return name, nil
}
