//nolint:goconst,wsl // Tests use CloudFormation property and type names as explicit examples.
package resource

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/sivchari/kumo/internal/service/dynamodb"
	"github.com/sivchari/kumo/internal/service/s3"
	"github.com/sivchari/kumo/internal/service/sns"
	"github.com/sivchari/kumo/internal/service/sqs"
)

const (
	attributeNameProperty         = "AttributeName"
	deadLetterTargetARNProperty   = "deadLetterTargetArn"
	indexNameProperty             = "IndexName"
	maxReceiveCountProperty       = "maxReceiveCount"
	maximumMessageSizeProperty    = "MaximumMessageSize"
	projectionProperty            = "Projection"
	projectionTypeProperty        = "ProjectionType"
	projectionTypeAll             = "ALL"
	provisionedThroughputProperty = "ProvisionedThroughput"
	readCapacityUnitsProperty     = "ReadCapacityUnits"
	receiveWaitProperty           = "ReceiveMessageWaitTimeSeconds"
	redrivePolicyProperty         = "RedrivePolicy"
	deadLetterTargetARN           = "arn:aws:sqs:us-east-1:000000000000:dead-letter"
	writeCapacityUnitsProperty    = "WriteCapacityUnits"
)

func TestEveryRegisteredAdapterResolvesItsBackendStorage(t *testing.T) {
	t.Parallel()

	for _, typeName := range DefaultRegistry().TypeNames() {
		if requiresExistingResource(typeName) {
			continue
		}
		adapter, ok := DefaultRegistry().Get(typeName)
		if !ok {
			t.Fatalf("%s adapter is not registered", typeName)
		}

		if adapter.Schema().TypeName != typeName {
			t.Fatalf("Schema().TypeName = %q, want %q", adapter.Schema().TypeName, typeName)
		}

		result, err := adapter.Create(context.Background(), validCreateRequest(typeName))
		if err != nil {
			t.Fatalf("%s failed to resolve its backend storage: %v", typeName, err)
		}
		assertExactAttributes(t, expectedAttributes(typeName), result)

		if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
			t.Fatalf("%s failed to resolve its backend storage for deletion: %v", typeName, err)
		}
	}
}

func TestAdaptersRejectUndeclaredProperties(t *testing.T) {
	t.Parallel()

	for _, typeName := range DefaultRegistry().TypeNames() {
		adapter, ok := DefaultRegistry().Get(typeName)
		if !ok {
			t.Fatalf("%s adapter is not registered", typeName)
		}

		req := validCreateRequest(typeName)
		req.Properties["Undeclared"] = true

		if _, err := adapter.Create(context.Background(), req); err == nil {
			t.Errorf("%s accepted an undeclared property", typeName)
		}
	}
}

//nolint:goconst // CloudFormation property names are repeated as explicit test fixtures.
func TestS3BucketProvisionsAndDeletesRealStorage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	name := "cfn-resource-s3-test"
	adapter := mustAdapter(t, "AWS::S3::Bucket")

	result, err := adapter.Create(ctx, CreateRequest{Properties: map[string]any{"BucketName": name}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	assertExactAttributes(t, expectedAttributes("AWS::S3::Bucket"), result)
	if websiteURL, _ := result.Attributes["WebsiteURL"].(string); websiteURL != "" {
		t.Fatalf("WebsiteURL = %q without website configuration, want empty", websiteURL)
	}

	storage, err := lookupStorage[s3.Storage]("s3")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}

	exists, err := storage.BucketExists(ctx, name)
	if err != nil || !exists {
		t.Fatalf("BucketExists() = %t, %v; want true, nil", exists, err)
	}

	if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	exists, err = storage.BucketExists(ctx, name)
	if err != nil || exists {
		t.Fatalf("BucketExists() after Delete = %t, %v; want false, nil", exists, err)
	}
}

//nolint:goconst // CloudFormation property names and document values are explicit test fixtures.
func TestS3BucketReturnsWebsiteURLForObservedWebsiteConfiguration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	name := "cfn-resource-s3-website-test"
	adapter := mustAdapter(t, "AWS::S3::Bucket")

	result, err := adapter.Create(ctx, CreateRequest{Properties: map[string]any{
		"BucketName": name,
		"WebsiteConfiguration": map[string]any{
			"IndexDocument": "index.html",
			"ErrorDocument": "error.html",
		},
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	assertExactAttributes(t, expectedAttributes("AWS::S3::Bucket"), result)

	want := "http://" + name + ".s3-website-us-east-1.amazonaws.com"
	if got := result.Attributes["WebsiteURL"]; got != want {
		t.Fatalf("WebsiteURL = %v, want %q", got, want)
	}

	storage, err := lookupStorage[s3.Storage]("s3")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	configuration, err := storage.GetBucketWebsite(ctx, name)
	if err != nil {
		t.Fatalf("GetBucketWebsite() error = %v", err)
	}
	if configuration.IndexDocument == nil || configuration.IndexDocument.Suffix != "index.html" {
		t.Fatalf("stored website configuration = %#v, want index.html", configuration)
	}
	if configuration.ErrorDocument == nil || configuration.ErrorDocument.Key != "error.html" {
		t.Fatalf("stored website configuration = %#v, want error.html", configuration)
	}

	if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestS3BucketTranslatesCloudFormationWebsiteRoutingRules(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	name := "cfn-resource-s3-routing-rules-test"
	adapter := mustAdapter(t, "AWS::S3::Bucket")
	result, err := adapter.Create(ctx, CreateRequest{Properties: map[string]any{
		"BucketName": name,
		"WebsiteConfiguration": map[string]any{
			"IndexDocument": "index.html",
			"RoutingRules": []any{
				map[string]any{
					"RoutingRuleCondition": map[string]any{
						"KeyPrefixEquals":             "docs/",
						"HttpErrorCodeReturnedEquals": "404",
					},
					"RedirectRule": map[string]any{
						"HostName":             "example.com",
						"Protocol":             "https",
						"ReplaceKeyPrefixWith": "documents/",
						"HttpRedirectCode":     "301",
					},
				},
			},
		},
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if got, want := result.Attributes["WebsiteURL"], "http://"+name+".s3-website-us-east-1.amazonaws.com"; got != want {
		t.Fatalf("WebsiteURL = %v, want %q", got, want)
	}

	storage, err := lookupStorage[s3.Storage]("s3")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	configuration, err := storage.GetBucketWebsite(ctx, name)
	if err != nil {
		t.Fatalf("GetBucketWebsite() error = %v", err)
	}
	if configuration.RoutingRules == nil || len(configuration.RoutingRules.Rules) != 1 {
		t.Fatalf("stored routing rules = %#v, want one rule", configuration.RoutingRules)
	}

	rule := configuration.RoutingRules.Rules[0]
	if rule.Condition == nil {
		t.Fatal("stored routing rule condition = nil, want condition")
	}
	if rule.Condition.KeyPrefixEquals != "docs/" || rule.Condition.HTTPErrorCodeReturnedEquals != "404" {
		t.Fatalf("stored routing rule condition = %#v, want docs/ and 404", rule.Condition)
	}
	if got := rule.Redirect; got.HostName != "example.com" || got.Protocol != "https" || got.ReplaceKeyPrefixWith != "documents/" || got.HTTPRedirectCode != "301" {
		t.Fatalf("stored routing rule redirect = %#v, want translated redirect", got)
	}

	if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

//nolint:goconst // CloudFormation property names are repeated as explicit test fixtures.
func TestDynamoDBTableProvisionsAndDeletesRealStorage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	name := "CfnResourceDynamoDBTest"
	adapter := mustAdapter(t, "AWS::DynamoDB::Table")

	request := CreateRequest{Properties: map[string]any{
		"TableName": name,
		"AttributeDefinitions": []any{
			map[string]any{attributeNameProperty: "ID", "AttributeType": "S"},
		},
		"KeySchema": []any{
			map[string]any{attributeNameProperty: "ID", "KeyType": "HASH"},
		},
		"BillingMode": "PAY_PER_REQUEST",
	}}

	result, err := adapter.Create(ctx, request)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	assertExactAttributes(t, expectedAttributes("AWS::DynamoDB::Table"), result)

	storage, err := lookupStorage[dynamodb.Storage]("dynamodb")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}

	if _, err := storage.DescribeTable(ctx, name); err != nil {
		t.Fatalf("DescribeTable() error = %v", err)
	}

	if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := storage.DescribeTable(ctx, name); err == nil {
		t.Fatal("DescribeTable() after Delete unexpectedly succeeded")
	}
}

//nolint:goconst // CloudFormation property names are repeated as explicit test fixtures.
func TestSQSQueueProvisionsAndDeletesRealStorage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	name := "cfn-resource-sqs-test"
	adapter := mustAdapter(t, "AWS::SQS::Queue")

	result, err := adapter.Create(ctx, CreateRequest{Properties: map[string]any{"QueueName": name}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	assertExactAttributes(t, expectedAttributes("AWS::SQS::Queue"), result)

	storage, err := lookupStorage[sqs.Storage]("sqs")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}

	if _, err := storage.GetQueue(ctx, result.Identifier); err != nil {
		t.Fatalf("GetQueue() error = %v", err)
	}

	if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := storage.GetQueue(ctx, result.Identifier); err == nil {
		t.Fatal("GetQueue() after Delete unexpectedly succeeded")
	}
}

//nolint:goconst // CloudFormation property names are repeated as explicit test fixtures.
func TestSNSTopicProvisionsAndDeletesRealStorage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	name := "cfn-resource-sns-test"
	adapter := mustAdapter(t, "AWS::SNS::Topic")

	result, err := adapter.Create(ctx, CreateRequest{Properties: map[string]any{"TopicName": name}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	assertExactAttributes(t, expectedAttributes("AWS::SNS::Topic"), result)

	storage, err := lookupStorage[sns.Storage]("sns")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}

	if _, err := storage.GetTopic(ctx, result.Identifier); err != nil {
		t.Fatalf("GetTopic() error = %v", err)
	}

	if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := storage.GetTopic(ctx, result.Identifier); err == nil {
		t.Fatal("GetTopic() after Delete unexpectedly succeeded")
	}
}

//nolint:goconst // Resource type names are explicit test fixtures for each adapter.
func TestAdaptersGeneratePhysicalNamesWhenNameIsOmitted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	testCases := []struct {
		typeName   string
		properties map[string]any
	}{
		{"AWS::S3::Bucket", map[string]any{}},
		{"AWS::DynamoDB::Table", map[string]any{
			"AttributeDefinitions": dynamoDBProperties("")["AttributeDefinitions"],
			"KeySchema":            dynamoDBProperties("")["KeySchema"],
			"BillingMode":          "PAY_PER_REQUEST",
		}},
		{"AWS::SQS::Queue", map[string]any{}},
		{"AWS::SNS::Topic", map[string]any{}},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.typeName, func(t *testing.T) {
			t.Parallel()

			adapter := mustAdapter(t, testCase.typeName)
			result, err := adapter.Create(ctx, CreateRequest{Properties: testCase.properties})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if result.Identifier == "" {
				t.Fatal("Create() returned an empty generated identifier")
			}

			if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err != nil {
				t.Fatalf("Delete() error = %v", err)
			}
		})
	}
}

//nolint:goconst // CloudFormation property names are repeated as explicit test fixtures.
func TestDynamoDBTableRejectsMissingOrInvalidKeyDefinitions(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::DynamoDB::Table")
	testCases := []struct {
		name       string
		properties map[string]any
	}{
		{"missing key schema", map[string]any{"AttributeDefinitions": dynamoDBProperties("")["AttributeDefinitions"]}},
		{"missing attribute definitions", map[string]any{"KeySchema": dynamoDBProperties("")["KeySchema"]}},
		{"key absent from attribute definitions", map[string]any{
			"AttributeDefinitions": []any{map[string]any{attributeNameProperty: "Other", "AttributeType": "S"}},
			"KeySchema":            []any{map[string]any{attributeNameProperty: "ID", "KeyType": "HASH"}},
		}},
		{"missing hash key", map[string]any{
			"AttributeDefinitions": dynamoDBProperties("")["AttributeDefinitions"],
			"KeySchema":            []any{map[string]any{attributeNameProperty: "ID", "KeyType": "RANGE"}},
		}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := adapter.Create(context.Background(), CreateRequest{Properties: testCase.properties}); err == nil {
				t.Fatal("Create() unexpectedly accepted invalid key definitions")
			}
		})
	}
}

func TestDynamoDBTableValidatesBillingModeThroughput(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::DynamoDB::Table")
	for _, properties := range []map[string]any{
		{
			"TableName":            "CfnDefaultProvisioned",
			"AttributeDefinitions": dynamoDBProperties("")["AttributeDefinitions"],
			"KeySchema":            dynamoDBProperties("")["KeySchema"],
		},
		{
			"TableName":            "CfnMissingThroughput",
			"AttributeDefinitions": dynamoDBProperties("")["AttributeDefinitions"],
			"KeySchema":            dynamoDBProperties("")["KeySchema"],
			"BillingMode":          "PROVISIONED",
		},
		{
			"TableName":            "CfnPayPerRequestThroughput",
			"AttributeDefinitions": dynamoDBProperties("")["AttributeDefinitions"],
			"KeySchema":            dynamoDBProperties("")["KeySchema"],
			"BillingMode":          "PAY_PER_REQUEST",
			provisionedThroughputProperty: map[string]any{
				readCapacityUnitsProperty:  1,
				writeCapacityUnitsProperty: 1,
			},
		},
	} {
		if _, err := adapter.Create(context.Background(), CreateRequest{Properties: properties}); err == nil {
			t.Fatal("Create() unexpectedly accepted an invalid billing mode throughput combination")
		}
	}
}

func TestDynamoDBTableAcceptsStrictProvisionedThroughputStrings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	properties := map[string]any{
		"TableName": "CfnStringProvisionedThroughput",
		"AttributeDefinitions": []any{
			map[string]any{attributeNameProperty: "ID", "AttributeType": "S"},
			map[string]any{attributeNameProperty: "GSIKey", "AttributeType": "S"},
		},
		"KeySchema": []any{
			map[string]any{attributeNameProperty: "ID", "KeyType": "HASH"},
		},
		"BillingMode": "PROVISIONED",
		provisionedThroughputProperty: map[string]any{
			readCapacityUnitsProperty:  "5",
			writeCapacityUnitsProperty: "7",
		},
		"GlobalSecondaryIndexes": []any{
			map[string]any{
				indexNameProperty: "by-gsi-key",
				"KeySchema": []any{
					map[string]any{attributeNameProperty: "GSIKey", "KeyType": "HASH"},
				},
				projectionProperty: map[string]any{projectionTypeProperty: projectionTypeAll},
				provisionedThroughputProperty: map[string]any{
					readCapacityUnitsProperty:  "11",
					writeCapacityUnitsProperty: "13",
				},
			},
		},
	}

	adapter := mustAdapter(t, "AWS::DynamoDB::Table")
	result, err := adapter.Create(ctx, CreateRequest{Properties: properties})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	storage, err := lookupStorage[dynamodb.Storage]("dynamodb")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	table, err := storage.DescribeTable(ctx, result.Identifier)
	if err != nil {
		t.Fatalf("DescribeTable() error = %v", err)
	}
	if got := table.ProvisionedThroughput; got == nil || got.ReadCapacityUnits != 5 || got.WriteCapacityUnits != 7 {
		t.Fatalf("table ProvisionedThroughput = %#v, want 5/7", got)
	}
	if got := table.GlobalSecondaryIndexes[0].ProvisionedThroughput; got == nil || got.ReadCapacityUnits != 11 || got.WriteCapacityUnits != 13 {
		t.Fatalf("GSI ProvisionedThroughput = %#v, want 11/13", got)
	}

	if _, err := storage.DeleteTable(ctx, result.Identifier); err != nil {
		t.Fatalf("test cleanup DeleteTable() error = %v", err)
	}
}

func TestDynamoDBTableRejectsInvalidProvisionedThroughputStrings(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::DynamoDB::Table")
	for _, testCase := range []struct {
		name  string
		value any
	}{
		{name: "zero", value: "0"},
		{name: "fractional", value: "1.5"},
		{name: "whitespace", value: " 1"},
		{name: "overflow", value: "9223372036854775808"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			properties := dynamoDBProperties("")
			properties["TableName"] = "CfnInvalidStringThroughput" + strings.ReplaceAll(testCase.name, " ", "")
			properties[provisionedThroughputProperty] = map[string]any{
				readCapacityUnitsProperty:  testCase.value,
				writeCapacityUnitsProperty: "1",
			}

			if _, err := adapter.Create(context.Background(), CreateRequest{Properties: properties}); err == nil {
				t.Fatal("Create() unexpectedly accepted invalid ProvisionedThroughput")
			}
		})
	}
}

func TestDynamoDBTableRejectsUnknownBillingMode(t *testing.T) {
	t.Parallel()

	properties := dynamoDBProperties("")
	properties["TableName"] = "CfnInvalidBillingMode"
	properties["BillingMode"] = "UNKNOWN"
	properties[provisionedThroughputProperty] = map[string]any{
		readCapacityUnitsProperty:  1,
		writeCapacityUnitsProperty: 1,
	}

	if _, err := mustAdapter(t, "AWS::DynamoDB::Table").Create(context.Background(), CreateRequest{Properties: properties}); err == nil {
		t.Fatal("Create() unexpectedly accepted an unknown BillingMode")
	}
}

func TestDynamoDBTableValidatesSecondaryIndexKeyDefinitions(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::DynamoDB::Table")
	for _, properties := range []map[string]any{
		{
			"TableName":            "CfnInvalidGlobalIndex",
			"AttributeDefinitions": dynamoDBProperties("")["AttributeDefinitions"],
			"KeySchema":            dynamoDBProperties("")["KeySchema"],
			"BillingMode":          "PAY_PER_REQUEST",
			"GlobalSecondaryIndexes": []any{map[string]any{
				indexNameProperty: "by-other",
				"KeySchema": []any{map[string]any{
					"AttributeName": "Other",
					"KeyType":       "HASH",
				}},
				projectionProperty: map[string]any{projectionTypeProperty: projectionTypeAll},
			}},
		},
		{
			"TableName":            "CfnInvalidLocalIndex",
			"AttributeDefinitions": dynamoDBProperties("")["AttributeDefinitions"],
			"KeySchema":            dynamoDBProperties("")["KeySchema"],
			"BillingMode":          "PAY_PER_REQUEST",
			"LocalSecondaryIndexes": []any{map[string]any{
				indexNameProperty: "by-other",
				"KeySchema": []any{map[string]any{
					"AttributeName": "Other",
					"KeyType":       "HASH",
				}},
				projectionProperty: map[string]any{projectionTypeProperty: projectionTypeAll},
			}},
		},
	} {
		if _, err := adapter.Create(context.Background(), CreateRequest{Properties: properties}); err == nil {
			t.Fatal("Create() unexpectedly accepted an undeclared secondary index key attribute")
		}
	}
}

//nolint:goconst // CloudFormation property names are repeated as explicit test fixtures.
func TestSQSQueueRejectsInvalidPropertyValues(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::SQS::Queue")
	testCases := []struct {
		name       string
		properties map[string]any
	}{
		{"fractional delay", map[string]any{"QueueName": "cfn-invalid-delay", "DelaySeconds": 1.5}},
		{"numeric queue name", map[string]any{"QueueName": 1}},
		{"fractional string visibility timeout", map[string]any{"QueueName": "cfn-invalid-visibility", "VisibilityTimeout": "30.0"}},
		{"out of range retention", map[string]any{"QueueName": "cfn-invalid-retention", "MessageRetentionPeriod": 1}},
		{"invalid fifo queue type", map[string]any{"QueueName": "cfn-invalid-fifo", "FifoQueue": "true"}},
		{"deduplication without fifo", map[string]any{"QueueName": "cfn-invalid-dedup", "ContentBasedDeduplication": true}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := adapter.Create(context.Background(), CreateRequest{Properties: testCase.properties}); err == nil {
				t.Fatal("Create() unexpectedly accepted an invalid SQS property")
			}
		})
	}
}

func TestSQSQueueAcceptsInt64Properties(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	adapter := mustAdapter(t, "AWS::SQS::Queue")
	result, err := adapter.Create(ctx, CreateRequest{Properties: map[string]any{
		"QueueName":                "cfn-int64-queue",
		"DelaySeconds":             int64(5),
		maximumMessageSizeProperty: int64(1024),
		"MessageRetentionPeriod":   int64(60),
		receiveWaitProperty:        int64(1),
		"VisibilityTimeout":        int64(30),
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestSQSQueueAcceptsStrictIntegerStringsFromNumberRefs(t *testing.T) {
	t.Parallel()

	attributes, err := sqsAttributes(map[string]any{
		"DelaySeconds":             "5",
		maximumMessageSizeProperty: "1024",
		"MessageRetentionPeriod":   "60",
		receiveWaitProperty:        "1",
		"VisibilityTimeout":        "30",
		redrivePolicyProperty: map[string]any{
			deadLetterTargetARNProperty: deadLetterTargetARN,
			maxReceiveCountProperty:     "3",
		},
	}, "cfn-string-number-ref")
	if err != nil {
		t.Fatalf("sqsAttributes() error = %v", err)
	}

	for property, want := range map[string]string{
		"DelaySeconds":             "5",
		maximumMessageSizeProperty: "1024",
		"MessageRetentionPeriod":   "60",
		receiveWaitProperty:        "1",
		"VisibilityTimeout":        "30",
	} {
		if got := attributes[property]; got != want {
			t.Errorf("attributes[%q] = %q, want %q", property, got, want)
		}
	}
	if !strings.Contains(attributes[redrivePolicyProperty], `"maxReceiveCount":"3"`) {
		t.Errorf("RedrivePolicy = %q, want string value converted from Number Ref", attributes[redrivePolicyProperty])
	}
}

func TestSQSQueueRejectsMalformedOrOutOfRangeIntegerStrings(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name       string
		properties map[string]any
	}{
		{name: "fractional", properties: map[string]any{"DelaySeconds": "1.5"}},
		{name: "whitespace", properties: map[string]any{"DelaySeconds": " 1"}},
		{name: "overflow", properties: map[string]any{"DelaySeconds": "9223372036854775808"}},
		{name: "out of range", properties: map[string]any{"DelaySeconds": "901"}},
		{name: "nested zero", properties: map[string]any{redrivePolicyProperty: map[string]any{
			deadLetterTargetARNProperty: deadLetterTargetARN,
			maxReceiveCountProperty:     "0",
		}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := sqsAttributes(testCase.properties, "cfn-invalid-number-ref"); err == nil {
				t.Fatal("sqsAttributes() unexpectedly accepted invalid integer string")
			}
		})
	}
}

func TestSQSQueueGeneratesFIFONameWhenNameIsOmitted(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::SQS::Queue")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{"FifoQueue": true}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if queueName, _ := result.Attributes["QueueName"].(string); !strings.HasSuffix(queueName, ".fifo") {
		t.Fatalf("generated QueueName = %q, want .fifo suffix", queueName)
	}

	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestSQSQueueRejectsExplicitFIFONameWithoutFifoQueue(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::SQS::Queue")
	for _, properties := range []map[string]any{
		{"QueueName": "cfn-explicit.fifo"},
		{"QueueName": "cfn-explicit.fifo", "FifoQueue": false},
	} {
		if _, err := adapter.Create(context.Background(), CreateRequest{Properties: properties}); err == nil {
			t.Fatal("Create() unexpectedly accepted a FIFO queue name without FifoQueue")
		}
	}
}

func TestAdaptersRejectExplicitEmptyNames(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		typeName   string
		properties map[string]any
	}{
		{"AWS::S3::Bucket", map[string]any{"BucketName": ""}},
		{"AWS::DynamoDB::Table", map[string]any{
			"TableName":            "",
			"AttributeDefinitions": dynamoDBProperties("")["AttributeDefinitions"],
			"KeySchema":            dynamoDBProperties("")["KeySchema"],
		}},
		{"AWS::SQS::Queue", map[string]any{"QueueName": ""}},
		{"AWS::SNS::Topic", map[string]any{"TopicName": ""}},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.typeName, func(t *testing.T) {
			if _, err := mustAdapter(t, testCase.typeName).Create(context.Background(), CreateRequest{Properties: testCase.properties}); err == nil {
				t.Fatal("Create() unexpectedly generated a name for an explicitly empty name")
			}
		})
	}
}

//nolint:goconst // CloudFormation policy property names are explicit test fixtures.
func TestSQSQueueSerializesObjectPolicies(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	adapter := mustAdapter(t, "AWS::SQS::Queue")
	result, err := adapter.Create(ctx, CreateRequest{Properties: map[string]any{
		"QueueName": "cfn-object-policy-queue",
		"Policy": map[string]any{
			"Version": "2012-10-17",
		},
		redrivePolicyProperty: map[string]any{
			deadLetterTargetARNProperty: deadLetterTargetARN,
			maxReceiveCountProperty:     3,
		},
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	storage, err := lookupStorage[sqs.Storage]("sqs")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	attributes, err := storage.GetQueueAttributes(ctx, result.Identifier, []string{"Policy", redrivePolicyProperty})
	if err != nil {
		t.Fatalf("GetQueueAttributes() error = %v", err)
	}
	if !strings.HasPrefix(attributes["Policy"], "{") || !strings.HasPrefix(attributes[redrivePolicyProperty], "{") {
		t.Fatalf("serialized queue attributes = %v, want JSON objects", attributes)
	}

	if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestSQSQueueRejectsInvalidPolicyShapes(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::SQS::Queue")
	for _, properties := range []map[string]any{
		{"QueueName": "cfn-invalid-policy", "Policy": []any{}},
		{"QueueName": "cfn-invalid-redrive", redrivePolicyProperty: map[string]any{maxReceiveCountProperty: "nope"}},
	} {
		if _, err := adapter.Create(context.Background(), CreateRequest{Properties: properties}); err == nil {
			t.Fatal("Create() unexpectedly accepted an invalid policy shape")
		}
	}
}

func TestS3BucketRejectsNonStringBucketName(t *testing.T) {
	t.Parallel()

	if _, err := mustAdapter(t, "AWS::S3::Bucket").Create(context.Background(), CreateRequest{Properties: map[string]any{"BucketName": 1}}); err == nil {
		t.Fatal("Create() unexpectedly accepted a non-string BucketName")
	}
}

//nolint:goconst // CloudFormation website property names are explicit test fixtures.
func TestS3BucketRejectsBackendWebsiteDocumentShapes(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::S3::Bucket")
	for _, testCase := range []struct {
		name          string
		configuration map[string]any
	}{
		{
			name: "index document object",
			configuration: map[string]any{
				"IndexDocument": map[string]any{"Suffix": "index.html"},
			},
		},
		{
			name: "error document object",
			configuration: map[string]any{
				"ErrorDocument": map[string]any{"Key": "error.html"},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			name := "cfn-invalid-website-" + strings.ReplaceAll(testCase.name, " ", "-")
			result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
				"BucketName":           name,
				"WebsiteConfiguration": testCase.configuration,
			}})
			if err == nil {
				storage, lookupErr := lookupStorage[s3.Storage]("s3")
				if lookupErr != nil {
					t.Fatalf("lookupStorage() error = %v", lookupErr)
				}
				if deleteErr := storage.DeleteBucket(context.Background(), result.Identifier); deleteErr != nil {
					t.Fatalf("cleanup DeleteBucket() error = %v", deleteErr)
				}
				t.Fatal("Create() unexpectedly accepted a backend website document shape")
			}
		})
	}
}

func TestS3BucketRejectsNullWebsiteRoutingRules(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::S3::Bucket")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"BucketName": "cfn-invalid-website-routing-rules",
		"WebsiteConfiguration": map[string]any{
			"RoutingRules": nil,
		},
	}})
	if err == nil {
		storage, lookupErr := lookupStorage[s3.Storage]("s3")
		if lookupErr != nil {
			t.Fatalf("lookupStorage() error = %v", lookupErr)
		}
		if deleteErr := storage.DeleteBucket(context.Background(), result.Identifier); deleteErr != nil {
			t.Fatalf("cleanup DeleteBucket() error = %v", deleteErr)
		}
		t.Fatal("Create() unexpectedly accepted null WebsiteConfiguration.RoutingRules")
	}
}

//nolint:goconst // CloudFormation property names are repeated as explicit test fixtures.
func TestSNSTopicRejectsInvalidProperties(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::SNS::Topic")
	for _, properties := range []map[string]any{
		{"TopicName": 1},
		{"DisplayName": 1},
		{"Tags": "invalid"},
		{"Tags": []any{map[string]any{"Key": "key", "Value": 1}}},
	} {
		if _, err := adapter.Create(context.Background(), CreateRequest{Properties: properties}); err == nil {
			t.Fatal("Create() unexpectedly accepted an invalid SNS property")
		}
	}
}

func TestSNSTopicProvisionsValidTags(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	adapter := mustAdapter(t, "AWS::SNS::Topic")
	result, err := adapter.Create(ctx, CreateRequest{Properties: map[string]any{
		"TopicName": "cfn-tagged-topic",
		"Tags":      []any{map[string]any{"Key": "environment", "Value": "test"}},
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	storage, err := lookupStorage[sns.Storage]("sns")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	tags, err := storage.ListTagsForResource(ctx, result.Identifier)
	if err != nil {
		t.Fatalf("ListTagsForResource() error = %v", err)
	}
	if !reflect.DeepEqual(tags, []sns.Tag{{Key: "environment", Value: "test"}}) {
		t.Fatalf("tags = %#v, want provisioned tag", tags)
	}

	if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestAdaptersExposeOnlySupportedGetAttAttributes(t *testing.T) {
	t.Parallel()

	assertAttributes(t, mustAdapter(t, "AWS::S3::Bucket").Schema(), []string{attributeARN, "DomainName", "DualStackDomainName", "RegionalDomainName", "WebsiteURL"})
	assertAttributes(t, mustAdapter(t, "AWS::DynamoDB::Table").Schema(), []string{attributeARN, "StreamArn"})
	assertAttributes(t, mustAdapter(t, "AWS::SQS::Queue").Schema(), []string{attributeARN, "QueueName", "QueueUrl"})
	assertAttributes(t, mustAdapter(t, "AWS::SNS::Topic").Schema(), []string{"TopicArn", "TopicName"})
	assertAttributes(t, mustAdapter(t, "AWS::IAM::Role").Schema(), []string{"Arn", "RoleId"})
	assertAttributes(t, mustAdapter(t, "AWS::IAM::Policy").Schema(), []string{"Id"})
	assertAttributes(t, mustAdapter(t, "AWS::Lambda::Function").Schema(), []string{attributeARN})
	assertAttributes(t, mustAdapter(t, "AWS::SNS::Subscription").Schema(), []string{"Arn"})
	assertAttributes(t, mustAdapter(t, "AWS::SQS::QueuePolicy").Schema(), []string{"Id"})
	assertAttributes(t, mustAdapter(t, "AWS::Lambda::EventSourceMapping").Schema(), []string{"EventSourceMappingArn", "Id"})
}

func TestLambdaFunctionExposesOnlyArnAttribute(t *testing.T) {
	adapter := mustAdapter(t, "AWS::Lambda::Function")
	assertAttributes(t, adapter.Schema(), []string{attributeARN})

	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"FunctionName": "cfn-lambda-arn-only-function",
		"Role":         "arn:aws:iam::123456789012:role/cfn-role",
		"Code":         map[string]any{"ZipFile": "test"},
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	t.Cleanup(func() { _ = adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}) })
	assertExactAttributes(t, []string{attributeARN}, result)
}

func TestSpecReviewedPropertyUpdateMetadata(t *testing.T) {
	if got := mustAdapter(t, "AWS::Lambda::Function").Schema().PropertyUpdate["Architectures"]; got != propertyUpdateNoInterruption {
		t.Errorf("Lambda Architectures update = %q, want %q", got, propertyUpdateNoInterruption)
	}
	if got := mustAdapter(t, "AWS::Lambda::Function").Schema().PropertyUpdate["Code"]; got != propertyUpdateNoInterruption {
		t.Errorf("Lambda Code update = %q, want %q", got, propertyUpdateNoInterruption)
	}
	if got := mustAdapter(t, "AWS::SNS::Subscription").Schema().PropertyUpdate["Region"]; got != propertyUpdateReplacement {
		t.Errorf("SNS Subscription Region update = %q, want %q", got, propertyUpdateReplacement)
	}
	if got := mustAdapter(t, "AWS::IAM::Policy").Schema().PropertyUpdate["PolicyName"]; got != propertyUpdateReplacement {
		t.Errorf("IAM Policy PolicyName update = %q, want %q", got, propertyUpdateReplacement)
	}
	if got := mustAdapter(t, "AWS::Lambda::EventSourceMapping").Schema().PropertyUpdate["FunctionName"]; got != propertyUpdateNoInterruption {
		t.Errorf("Lambda EventSourceMapping FunctionName update = %q, want %q", got, propertyUpdateNoInterruption)
	}
}

func TestNoInterruptionPropertiesHaveUpdateImplementations(t *testing.T) {
	for _, typeName := range DefaultRegistry().TypeNames() {
		adapter, ok := DefaultRegistry().Get(typeName)
		if !ok {
			t.Fatal("adapter disappeared from default registry")
		}
		for property, update := range adapter.Schema().PropertyUpdate {
			if update != propertyUpdateNoInterruption {
				continue
			}
			if _, ok := adapter.(UpdateCapable); !ok {
				t.Errorf("%s property %s advertises NoInterruption without Update", typeName, property)
			}
		}
	}
}

func TestIAMSchemasDeclareDocumentedPropertiesAndCapabilities(t *testing.T) {
	assertPropertyNames(t, mustAdapter(t, "AWS::IAM::Role").Schema(), []string{
		"AssumeRolePolicyDocument", "Description", "ManagedPolicyArns", "MaxSessionDuration",
		"Path", "Policies", "RoleName", "Tags",
	})
	assertPropertyNames(t, mustAdapter(t, "AWS::IAM::Policy").Schema(), []string{
		"Groups", "PolicyDocument", "PolicyName", "Roles", "Users",
	})

	role := mustAdapter(t, "AWS::IAM::Role")
	policy := mustAdapter(t, "AWS::IAM::Policy")
	if got := CapabilitiesFor(role, map[string]any{}); !reflect.DeepEqual(got, []string{"CAPABILITY_IAM"}) {
		t.Fatalf("unnamed IAM role capabilities = %v, want CAPABILITY_IAM", got)
	}
	if got := CapabilitiesFor(role, map[string]any{"RoleName": "named-role"}); !reflect.DeepEqual(got, []string{"CAPABILITY_NAMED_IAM"}) {
		t.Fatalf("named IAM role capabilities = %v, want CAPABILITY_NAMED_IAM", got)
	}
	if got := CapabilitiesFor(policy, map[string]any{"PolicyName": "named-policy"}); !reflect.DeepEqual(got, []string{"CAPABILITY_NAMED_IAM"}) {
		t.Fatalf("IAM policy capabilities = %v, want CAPABILITY_NAMED_IAM", got)
	}
}

func assertPropertyNames(t *testing.T, schema Schema, want []string) {
	t.Helper()
	got := make([]string, 0, len(schema.PropertyUpdate))
	for property := range schema.PropertyUpdate {
		got = append(got, property)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema properties = %v, want %v", got, want)
	}
}

func TestAdapterSchemaReturnsDefensiveCopies(t *testing.T) {
	t.Parallel()

	for _, typeName := range DefaultRegistry().TypeNames() {
		t.Run(typeName, func(t *testing.T) {
			adapter := mustAdapter(t, typeName)
			first := adapter.Schema()
			first.PropertyUpdate["Mutated"] = propertyUpdateReplacement
			if len(first.Attributes) > 0 {
				first.Attributes[0] = "Mutated"
			}

			second := adapter.Schema()
			if _, exists := second.PropertyUpdate["Mutated"]; exists {
				t.Fatal("Schema() returned a shared PropertyUpdate map")
			}
			if len(second.Attributes) > 0 && second.Attributes[0] == "Mutated" {
				t.Fatal("Schema() returned a shared Attributes slice")
			}
		})
	}
}

func TestDynamoDBTableExposesStreamArnWhenStreamIsEnabled(t *testing.T) {
	t.Parallel()

	adapter := mustAdapter(t, "AWS::DynamoDB::Table")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"TableName":            "CfnResourceStreamTable",
		"AttributeDefinitions": dynamoDBProperties("")["AttributeDefinitions"],
		"KeySchema":            dynamoDBProperties("")["KeySchema"],
		"BillingMode":          "PAY_PER_REQUEST",
		"StreamSpecification":  map[string]any{"StreamEnabled": true, "StreamViewType": "NEW_IMAGE"},
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if streamARN, _ := result.Attributes["StreamArn"].(string); streamARN == "" {
		t.Fatal("StreamArn is empty for an enabled stream")
	}

	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestDynamoDBTableDeleteRejectsDeletionProtectedTable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	adapter := mustAdapter(t, "AWS::DynamoDB::Table")
	result, err := adapter.Create(ctx, CreateRequest{Properties: map[string]any{
		"TableName":                 "CfnDeletionProtectedTable",
		"AttributeDefinitions":      dynamoDBProperties("")["AttributeDefinitions"],
		"KeySchema":                 dynamoDBProperties("")["KeySchema"],
		"BillingMode":               "PAY_PER_REQUEST",
		"DeletionProtectionEnabled": true,
	}})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := adapter.Delete(ctx, DeleteRequest{Identifier: result.Identifier}); err == nil {
		t.Fatal("Delete() unexpectedly deleted a protected table")
	}

	storage, err := lookupStorage[dynamodb.Storage]("dynamodb")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	if _, err := storage.DescribeTable(ctx, result.Identifier); err != nil {
		t.Fatalf("DescribeTable() error = %v", err)
	}

	if _, err := storage.DeleteTable(ctx, result.Identifier); err != nil {
		t.Fatalf("test cleanup DeleteTable() error = %v", err)
	}
}

func TestRegistrySupportsConcurrentRegistrationAndLookup(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()

			typeName := fmt.Sprintf("Test::Resource::%d", i)
			registry.Register(testAdapter{typeName: typeName})
			_, _ = registry.Get(typeName)
			_ = registry.TypeNames()
		}(i)
	}
	group.Wait()
}

func mustAdapter(t *testing.T, typeName string) Adapter {
	t.Helper()

	adapter, ok := DefaultRegistry().Get(typeName)
	if !ok {
		t.Fatalf("%s adapter is not registered", typeName)
	}

	return adapter
}

func assertExactAttributes(t *testing.T, want []string, result CreateResult) {
	t.Helper()

	got := make([]string, 0, len(result.Attributes))
	for name := range result.Attributes {
		got = append(got, name)
	}

	sort.Strings(got)

	want = append([]string(nil), want...)

	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result attribute names = %v, want %v", got, want)
	}
}

func expectedAttributes(typeName string) []string {
	switch typeName {
	case "AWS::S3::Bucket":
		return []string{attributeARN, "DomainName", "DualStackDomainName", "RegionalDomainName", "WebsiteURL"}
	case "AWS::DynamoDB::Table":
		return []string{attributeARN, "StreamArn"}
	case "AWS::SQS::Queue":
		return []string{attributeARN, "QueueName", "QueueUrl"}
	case "AWS::SNS::Topic":
		return []string{"TopicArn", "TopicName"}
	case "AWS::IAM::Role":
		return []string{"Arn", "RoleId"}
	case "AWS::IAM::Policy":
		return []string{"Id"}
	case "AWS::Lambda::Function":
		return []string{attributeARN}
	default:
		panic(fmt.Sprintf("no expected attributes for %s", typeName))
	}
}

func assertAttributes(t *testing.T, schema Schema, want []string) {
	t.Helper()

	got := append([]string(nil), schema.Attributes...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema attributes = %v, want %v", got, want)
	}
}

func dynamoDBProperties(_ ...string) map[string]any {
	return map[string]any{
		"AttributeDefinitions": []any{map[string]any{attributeNameProperty: "ID", "AttributeType": "S"}},
		"KeySchema":            []any{map[string]any{attributeNameProperty: "ID", "KeyType": "HASH"}},
	}
}

type testAdapter struct{ typeName string }

func (a testAdapter) Schema() Schema { return Schema{TypeName: a.typeName} }

func (testAdapter) Create(context.Context, CreateRequest) (CreateResult, error) {
	return CreateResult{}, nil
}

func (testAdapter) Delete(context.Context, DeleteRequest) error { return nil }

func validCreateRequest(typeName string) CreateRequest {
	switch typeName {
	case "AWS::S3::Bucket":
		return CreateRequest{Properties: map[string]any{"BucketName": "cfn-resource-schema-s3"}}
	case "AWS::DynamoDB::Table":
		return CreateRequest{Properties: map[string]any{
			"TableName":            "CfnResourceSchemaDynamoDB",
			"AttributeDefinitions": []any{map[string]any{attributeNameProperty: "ID", "AttributeType": "S"}},
			"KeySchema":            []any{map[string]any{attributeNameProperty: "ID", "KeyType": "HASH"}},
			"BillingMode":          "PAY_PER_REQUEST",
		}}
	case "AWS::SQS::Queue":
		return CreateRequest{Properties: map[string]any{"QueueName": "cfn-resource-schema-sqs"}}
	case "AWS::SNS::Topic":
		return CreateRequest{Properties: map[string]any{"TopicName": "cfn-resource-schema-sns"}}
	case "AWS::IAM::Role":
		return CreateRequest{
			Capabilities: []string{"CAPABILITY_IAM"},
			Properties: map[string]any{
				"AssumeRolePolicyDocument": `{"Version":"2012-10-17"}`,
			},
		}
	case "AWS::IAM::Policy":
		return CreateRequest{
			Capabilities: []string{"CAPABILITY_IAM"},
			Properties: map[string]any{
				"PolicyDocument": `{"Version":"2012-10-17"}`,
			},
		}
	case "AWS::Lambda::Function":
		return CreateRequest{Properties: map[string]any{
			"Role": "arn:aws:iam::123456789012:role/cfn-role",
			"Code": map[string]any{"ZipFile": "test"},
		}}
	case "AWS::Lambda::Permission":
		return CreateRequest{Properties: map[string]any{
			"Action":       "lambda:InvokeFunction",
			"FunctionName": "cfn-function",
			"Principal":    "sns.amazonaws.com",
		}}
	case "AWS::Lambda::EventSourceMapping":
		return CreateRequest{Properties: map[string]any{
			"FunctionName":   "cfn-function",
			"EventSourceArn": "arn:aws:sqs:us-east-1:000000000000:cfn-queue",
		}}
	case "AWS::SNS::Subscription":
		return CreateRequest{Properties: map[string]any{
			"TopicArn": "arn:aws:sns:us-east-1:000000000000:cfn-topic",
			"Protocol": "lambda",
		}}
	case "AWS::SQS::QueuePolicy":
		return CreateRequest{Properties: map[string]any{
			"PolicyDocument": `{"Version":"2012-10-17"}`,
			"Queues":         []any{"http://localhost:4566/000000000000/cfn-queue"},
		}}
	default:
		panic(fmt.Sprintf("no valid request for %s", typeName))
	}
}

func requiresExistingResource(typeName string) bool {
	switch typeName {
	case "AWS::Lambda::Permission", "AWS::Lambda::EventSourceMapping", "AWS::SNS::Subscription", "AWS::SQS::QueuePolicy":
		return true
	case "AWS::IAM::Policy":
		return true
	default:
		return false
	}
}

func TestCreateS3BucketCleansUpWhenWebsiteSetupAndCleanupFail(t *testing.T) {
	t.Parallel()

	websiteErr := errors.New("website setup failed")
	cleanupErr := errors.New("bucket cleanup failed")
	storage := &testS3BucketStorage{websiteErr: websiteErr, deleteErr: cleanupErr}

	_, err := createS3Bucket(context.Background(), storage, "cfn-failing-website", &s3.WebsiteConfiguration{})
	if err == nil {
		t.Fatal("createS3Bucket() unexpectedly succeeded")
	}
	if !errors.Is(err, websiteErr) {
		t.Fatalf("createS3Bucket() error = %v, want website setup error context", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("createS3Bucket() error = %v, want cleanup error context", err)
	}
	if !storage.created || !storage.deleted {
		t.Fatalf("bucket lifecycle = created %t, deleted %t; want both true", storage.created, storage.deleted)
	}
}

type testS3BucketStorage struct {
	created    bool
	deleted    bool
	websiteErr error
	deleteErr  error
}

func (s *testS3BucketStorage) CreateBucket(context.Context, string) error {
	s.created = true

	return nil
}

func (s *testS3BucketStorage) DeleteBucket(context.Context, string) error {
	s.deleted = true

	return s.deleteErr
}

func (s *testS3BucketStorage) PutBucketWebsite(context.Context, string, *s3.WebsiteConfiguration) error {
	return s.websiteErr
}

func (*testS3BucketStorage) GetBucketWebsite(context.Context, string) (*s3.WebsiteConfiguration, error) {
	return &s3.WebsiteConfiguration{}, nil
}
