//nolint:goconst // SQS attribute names intentionally mirror the AWS API.
package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/sivchari/kumo/internal/service/sqs"
)

// sqsQueueAdapter implements AWS::SQS::Queue.
// Ref and Fn::GetAtt: https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-sqs-queue.html
type sqsQueueAdapter struct{}

var sqsQueueSchema = Schema{
	TypeName: "AWS::SQS::Queue",
	PropertyUpdate: map[string]PropertyUpdate{
		"ContentBasedDeduplication":     propertyUpdateNoInterruption,
		"DelaySeconds":                  propertyUpdateNoInterruption,
		"FifoQueue":                     propertyUpdateReplacement,
		"MaximumMessageSize":            propertyUpdateNoInterruption,
		"MessageRetentionPeriod":        propertyUpdateNoInterruption,
		policyProperty:                  propertyUpdateNoInterruption,
		queueNameProperty:               propertyUpdateReplacement,
		"ReceiveMessageWaitTimeSeconds": propertyUpdateNoInterruption,
		sqsRedrivePolicyProperty:        propertyUpdateNoInterruption,
		"VisibilityTimeout":             propertyUpdateNoInterruption,
	},
	Attributes: []string{attributeARN, "QueueName", "QueueUrl"},
}

func (*sqsQueueAdapter) Schema() Schema { return cloneSchema(sqsQueueSchema) }

func (*sqsQueueAdapter) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	if err := validateProperties(sqsQueueSchema, req.Properties); err != nil {
		return CreateResult{}, err
	}

	name, provided := req.Properties[queueNameProperty]

	queueName, ok := name.(string)
	if provided && !ok {
		return CreateResult{}, fmt.Errorf("QueueName must be a string")
	}

	if provided && queueName == "" {
		return CreateResult{}, fmt.Errorf("QueueName must not be empty")
	}

	if queueName == "" {
		queueName = generatedPhysicalName("cfn-sqs")
		if fifo, _ := req.Properties["FifoQueue"].(bool); fifo {
			queueName += ".fifo"
		}
	}

	attributes, err := sqsAttributes(req.Properties, queueName)
	if err != nil {
		return CreateResult{}, err
	}

	storage, err := lookupStorage[sqs.Storage]("sqs")
	if err != nil {
		return CreateResult{}, err
	}

	queue, err := storage.CreateQueue(ctx, queueName, attributes, nil)
	if err != nil {
		return CreateResult{}, fmt.Errorf("create SQS queue: %w", err)
	}

	return CreateResult{Identifier: queue.URL, Attributes: map[string]any{attributeARN: queue.ARN, "QueueName": queue.Name, "QueueUrl": queue.URL}}, nil
}

//nolint:wsl // SQS queue updates validate and apply one attribute set.
func (*sqsQueueAdapter) Update(ctx context.Context, req UpdateRequest) (UpdateResult, error) {
	if err := validateProperties(sqsQueueSchema, req.Properties); err != nil {
		return UpdateResult{}, err
	}
	storage, err := lookupStorage[sqs.Storage]("sqs")
	if err != nil {
		return UpdateResult{}, err
	}
	attributes, err := sqsAttributes(req.Properties, "")
	if err != nil {
		return UpdateResult{}, err
	}
	if len(attributes) == 0 {
		return UpdateResult{}, nil
	}
	if err := storage.SetQueueAttributes(ctx, req.Identifier, attributes); err != nil {
		return UpdateResult{}, fmt.Errorf("update SQS queue: %w", err)
	}

	return UpdateResult{}, nil
}

//nolint:cyclop,funlen // Each supported SQS property has a distinct AWS range.
func sqsAttributes(properties map[string]any, name string) (map[string]string, error) {
	attributes := map[string]string{
		"DelaySeconds":                  "0",
		"MaximumMessageSize":            "262144",
		"MessageRetentionPeriod":        "345600",
		"ReceiveMessageWaitTimeSeconds": "0",
		"VisibilityTimeout":             "30",
		"FifoQueue":                     "false",
		"ContentBasedDeduplication":     "false",
		policyProperty:                  "",
		sqsRedrivePolicyProperty:        "",
	}
	if err := copySQSIntegerProperty(properties, attributes, "DelaySeconds", 0, 900); err != nil {
		return nil, err
	}

	if err := copySQSIntegerProperty(properties, attributes, "MaximumMessageSize", 1024, 1_048_576); err != nil {
		return nil, err
	}

	if err := copySQSIntegerProperty(properties, attributes, "MessageRetentionPeriod", 60, 1_209_600); err != nil {
		return nil, err
	}

	if err := copySQSIntegerProperty(properties, attributes, "ReceiveMessageWaitTimeSeconds", 0, 20); err != nil {
		return nil, err
	}

	if err := copySQSIntegerProperty(properties, attributes, "VisibilityTimeout", 0, 43_200); err != nil {
		return nil, err
	}

	fifo, err := optionalSQSBool(properties, "FifoQueue")
	if err != nil {
		return nil, err
	}

	if fifo {
		if !strings.HasSuffix(name, ".fifo") {
			return nil, fmt.Errorf("QueueName must end in .fifo when FifoQueue is true")
		}

		attributes["FifoQueue"] = strconv.FormatBool(true)
	} else if strings.HasSuffix(name, ".fifo") {
		return nil, fmt.Errorf("FifoQueue must be true when QueueName ends in .fifo")
	}

	deduplication, err := optionalSQSBool(properties, "ContentBasedDeduplication")
	if err != nil {
		return nil, err
	}

	if deduplication && !fifo {
		return nil, fmt.Errorf("ContentBasedDeduplication requires FifoQueue")
	}

	attributes["ContentBasedDeduplication"] = strconv.FormatBool(deduplication)

	if value, ok := properties[policyProperty]; ok {
		policy, err := jsonObject(policyProperty, value)
		if err != nil {
			return nil, err
		}

		attributes[policyProperty] = policy
	}

	if value, ok := properties[sqsRedrivePolicyProperty]; ok {
		redrivePolicy, err := redrivePolicyJSON(value)
		if err != nil {
			return nil, err
		}

		attributes[sqsRedrivePolicyProperty] = redrivePolicy
	}

	return attributes, nil
}

func jsonObject(property string, value any) (string, error) {
	if text, ok := value.(string); ok {
		var object map[string]any
		if err := json.Unmarshal([]byte(text), &object); err != nil || object == nil {
			return "", fmt.Errorf("%s must be a JSON object", property)
		}

		return text, nil
	}

	object, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s must be a JSON object", property)
	}

	encoded, err := json.Marshal(object)
	if err != nil {
		return "", fmt.Errorf("serialize %s: %w", property, err)
	}

	return string(encoded), nil
}

func redrivePolicyJSON(value any) (string, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s must be a JSON object", sqsRedrivePolicyProperty)
	}

	if _, ok := object["deadLetterTargetArn"].(string); !ok {
		return "", fmt.Errorf("%s.deadLetterTargetArn must be a string", sqsRedrivePolicyProperty)
	}

	if !positiveInteger(object["maxReceiveCount"]) {
		return "", fmt.Errorf("%s.maxReceiveCount must be a positive integer", sqsRedrivePolicyProperty)
	}

	encoded, err := json.Marshal(object)
	if err != nil {
		return "", fmt.Errorf("serialize %s: %w", sqsRedrivePolicyProperty, err)
	}

	return string(encoded), nil
}

func positiveInteger(value any) bool {
	switch value := value.(type) {
	case int:
		return value > 0
	case int64:
		return value > 0
	case float64:
		return value > 0 && math.Trunc(value) == value
	case string:
		integer, ok := strictIntegerString(value)

		return ok && integer > 0
	default:
		return false
	}
}

func copySQSIntegerProperty(properties map[string]any, attributes map[string]string, property string, minimum, maximum int64) error {
	value, ok := properties[property]
	if !ok {
		return nil
	}

	integer, ok := sqsInteger(value)
	if !ok || integer < minimum || integer > maximum {
		return fmt.Errorf("%s must be an integer from %d through %d", property, minimum, maximum)
	}

	attributes[property] = strconv.FormatInt(integer, 10)

	return nil
}

func sqsInteger(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), true
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		if uint64(value) > math.MaxInt64 {
			return 0, false
		}

		return int64(value), true
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		if value > math.MaxInt64 {
			return 0, false
		}

		return int64(value), true
	case string:
		return strictIntegerString(value)
	default:
		return 0, false
	}
}

func optionalSQSBool(properties map[string]any, property string) (bool, error) {
	value, ok := properties[property]
	if !ok {
		return false, nil
	}

	boolean, ok := value.(bool)

	if !ok {
		return false, fmt.Errorf("%s must be a boolean", property)
	}

	return boolean, nil
}

func (*sqsQueueAdapter) Delete(ctx context.Context, req DeleteRequest) error {
	storage, err := lookupStorage[sqs.Storage]("sqs")
	if err != nil {
		return err
	}

	if err := storage.DeleteQueue(ctx, req.Identifier); err != nil {
		return fmt.Errorf("delete SQS queue: %w", err)
	}

	return nil
}
