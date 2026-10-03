//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/sivchari/golden"
)

const (
	multiServiceTemplate = `{
  "AWSTemplateFormatVersion": "2010-09-09",
  "Description": "CloudFormation execution engine multi-service test",
  "Resources": {
    "Bucket": {
      "Type": "AWS::S3::Bucket",
      "Properties": {"BucketName": "cfn-execution-multi-bucket"}
    },
    "Queue": {
      "Type": "AWS::SQS::Queue",
      "Properties": {"QueueName": "cfn-execution-multi-queue"}
    },
    "Table": {
      "Type": "AWS::DynamoDB::Table",
      "Properties": {
        "TableName": "cfn-execution-multi-table",
        "BillingMode": "PAY_PER_REQUEST",
        "AttributeDefinitions": [{"AttributeName": "id", "AttributeType": "S"}],
        "KeySchema": [{"AttributeName": "id", "KeyType": "HASH"}]
      }
    },
    "Topic": {
      "Type": "AWS::SNS::Topic",
      "Properties": {"TopicName": "cfn-execution-multi-topic"}
    }
  }
}`

	parameterUpdateTemplate = `{
  "Parameters": {
    "Delay": {"Type": "String", "Default": "1"}
  },
  "Resources": {
    "Queue": {
      "Type": "AWS::SQS::Queue",
      "Properties": {
        "QueueName": "cfn-execution-parameter-queue",
        "DelaySeconds": {"Ref": "Delay"}
      }
    }
  }
}`

	replacementTemplate = `{
  "Resources": {
    "Bucket": {"Type": "AWS::S3::Bucket"}
  }
}`

	replacementWithNameTemplate = `{
  "Resources": {
    "Bucket": {
      "Type": "AWS::S3::Bucket",
      "Properties": {"BucketName": "cfn-execution-replacement-bucket"}
    }
  }
}`

	customNameBaseTemplate = `{
  "Resources": {
    "Table": {
      "Type": "AWS::DynamoDB::Table",
      "Properties": {
        "TableName": "cfn-execution-fixed-table",
        "BillingMode": "PAY_PER_REQUEST",
        "AttributeDefinitions": [{"AttributeName": "id", "AttributeType": "S"}],
        "KeySchema": [{"AttributeName": "id", "KeyType": "HASH"}]
      }
    }
  }
}`

	customNameReplacementTemplate = `{
  "Resources": {
    "Table": {
      "Type": "AWS::DynamoDB::Table",
      "Properties": {
        "TableName": "cfn-execution-fixed-table",
        "BillingMode": "PAY_PER_REQUEST",
        "AttributeDefinitions": [{"AttributeName": "key", "AttributeType": "S"}],
        "KeySchema": [{"AttributeName": "key", "KeyType": "HASH"}]
      }
    }
  }
}`

	conditionalYAMLTemplate = `AWSTemplateFormatVersion: '2010-09-09'
Description: YAML and conditions execution test
Parameters:
  BucketName:
    Type: String
    Default: cfn-execution-yaml-bucket
  EnableQueue:
    Type: String
    Default: 'no'
    AllowedValues: ['yes', 'no']
Conditions:
  CreateQueue: !Equals [!Ref EnableQueue, 'yes']
Resources:
  Bucket:
    Type: AWS::S3::Bucket
    Properties:
      BucketName: !Ref BucketName
  Queue:
    Condition: CreateQueue
    Type: AWS::SQS::Queue
    Properties:
      QueueName: cfn-execution-yaml-queue
`

	iamRoleTemplate = `{
  "Resources": {
    "Role": {
      "Type": "AWS::IAM::Role",
      "Properties": {
        "AssumeRolePolicyDocument": {
          "Version": "2012-10-17",
          "Statement": [{
            "Effect": "Allow",
            "Principal": {"Service": ["lambda.amazonaws.com"]},
            "Action": ["sts:AssumeRole"]
          }]
        }
      }
    }
  }
}`

	transformTemplate = `{
  "Transform": "AWS::Serverless-2016-10-31",
  "Resources": {
    "Bucket": {"Type": "AWS::S3::Bucket"}
  }
}`

	changeSetBaseTemplate = `{
  "Resources": {
    "Bucket": {
      "Type": "AWS::S3::Bucket",
      "Properties": {"BucketName": "cfn-execution-change-set-bucket"}
    }
  }
}`

	changeSetUpdatedTemplate = `{
  "Resources": {
    "Bucket": {
      "Type": "AWS::S3::Bucket",
      "Properties": {"BucketName": "cfn-execution-change-set-bucket"}
    },
    "Topic": {
      "Type": "AWS::SNS::Topic",
      "Properties": {"TopicName": "cfn-execution-change-set-topic"}
    }
  }
}`

	exportTemplate = `{
  "Resources": {
    "Bucket": {"Type": "AWS::S3::Bucket", "Properties": {"BucketName": "cfn-execution-export-bucket"}}
  },
  "Outputs": {
    "ExportedValue": {
      "Value": "cfn-execution-export-value",
      "Export": {"Name": "cfn-execution-export-name"}
    }
  }
}`

	retainTemplate = `{
  "Resources": {
    "Bucket": {
      "Type": "AWS::S3::Bucket",
      "DeletionPolicy": "Retain",
      "Properties": {"BucketName": "cfn-execution-retain-bucket"}
    }
  }
}`

	crossStackExporterTemplate = `{
  "Resources": {"Bucket": {"Type": "AWS::S3::Bucket", "Properties": {"BucketName": "cfn-execution-cross-export-bucket"}}},
  "Outputs": {"Value": {"Value": "cross-stack-value", "Export": {"Name": "cfn-execution-cross-export"}}}
}`

	crossStackImporterTemplate = `{
  "Resources": {"Bucket": {"Type": "AWS::S3::Bucket", "Properties": {"BucketName": {"Fn::ImportValue": "cfn-execution-cross-export"}}}}
}`
)

func newS3IntegrationClient(t *testing.T) *s3.Client {
	t.Helper()

	return s3.NewFromConfig(awsConfig(t), func(o *s3.Options) {
		o.BaseEndpoint = aws.String(testEndpoint())
		o.UsePathStyle = true
	})
}

func newSQSIntegrationClient(t *testing.T) *sqs.Client {
	t.Helper()

	return sqs.NewFromConfig(awsConfig(t), func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(testEndpoint())
	})
}

func newDynamoDBIntegrationClient(t *testing.T) *dynamodb.Client {
	t.Helper()

	return dynamodb.NewFromConfig(awsConfig(t), func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(testEndpoint())
	})
}

func newSNSIntegrationClient(t *testing.T) *sns.Client {
	t.Helper()

	return sns.NewFromConfig(awsConfig(t), func(o *sns.Options) {
		o.BaseEndpoint = aws.String(testEndpoint())
	})
}

func newIAMIntegrationClient(t *testing.T) *iam.Client {
	t.Helper()

	return iam.NewFromConfig(awsConfig(t), func(o *iam.Options) {
		o.BaseEndpoint = aws.String(testEndpoint())
	})
}

func stackResource(t *testing.T, client *cloudformation.Client, stackName, logicalID string) *cfntypes.StackResource {
	t.Helper()

	output, err := client.DescribeStackResources(t.Context(), &cloudformation.DescribeStackResourcesInput{StackName: aws.String(stackName)})
	if err != nil {
		t.Fatal(err)
	}
	for index := range output.StackResources {
		resource := &output.StackResources[index]
		if aws.ToString(resource.LogicalResourceId) == logicalID {
			return resource
		}
	}
	t.Fatalf("logical resource %q was not found", logicalID)

	return nil
}

func requireStackStatus(t *testing.T, client *cloudformation.Client, stackName, want string) *cfntypes.Stack {
	t.Helper()

	output, err := client.DescribeStacks(t.Context(), &cloudformation.DescribeStacksInput{StackName: aws.String(stackName)})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Stacks) != 1 {
		t.Fatalf("DescribeStacks returned %d stacks, want 1", len(output.Stacks))
	}
	if got := string(output.Stacks[0].StackStatus); got != want {
		t.Fatalf("stack status = %q, want %q", got, want)
	}

	return &output.Stacks[0]
}

func deleteIntegrationStack(t *testing.T, client *cloudformation.Client, stackName string) {
	t.Helper()

	if _, err := client.DeleteStack(context.Background(), &cloudformation.DeleteStackInput{StackName: aws.String(stackName)}); err != nil {
		if strings.Contains(err.Error(), "StackNotFoundException") || strings.Contains(err.Error(), "Stack not found") {
			return
		}
		t.Fatalf("cleanup stack %q: %v", stackName, err)
	}
}

func TestCloudFormationExecution_MultiServiceCreateAndDelete(t *testing.T) {
	client := newCloudFormationClient(t)
	stackName := "cfn-execution-multi-service"

	createOutput, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{
		StackName:          aws.String(stackName),
		TemplateBody:       aws.String(multiServiceTemplate),
		ClientRequestToken: aws.String("multi-create-token"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteIntegrationStack(t, client, stackName) })

	golden.New(t, golden.WithIgnoreFields("StackId", "ResultMetadata")).Assert(t.Name()+"_create", createOutput)
	stack := requireStackStatus(t, client, stackName, "CREATE_COMPLETE")
	resourcesOutput, err := client.DescribeStackResources(t.Context(), &cloudformation.DescribeStackResourcesInput{StackName: aws.String(stackName)})
	if err != nil {
		t.Fatal(err)
	}
	golden.New(t, golden.WithIgnoreFields("StackId", "Timestamp", "PhysicalResourceId", "ResultMetadata")).Assert(t.Name()+"_resources", resourcesOutput)

	bucket := stackResource(t, client, stackName, "Bucket")
	queue := stackResource(t, client, stackName, "Queue")
	table := stackResource(t, client, stackName, "Table")
	topic := stackResource(t, client, stackName, "Topic")

	if _, err := newS3IntegrationClient(t).HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("cfn-execution-multi-bucket")}); err != nil {
		t.Fatalf("HeadBucket() error = %v", err)
	}
	if _, err := newSQSIntegrationClient(t).GetQueueUrl(t.Context(), &sqs.GetQueueUrlInput{QueueName: aws.String("cfn-execution-multi-queue")}); err != nil {
		t.Fatalf("GetQueueUrl() error = %v", err)
	}
	if _, err := newDynamoDBIntegrationClient(t).DescribeTable(t.Context(), &dynamodb.DescribeTableInput{TableName: aws.String("cfn-execution-multi-table")}); err != nil {
		t.Fatalf("DescribeTable() error = %v", err)
	}
	if _, err := newSNSIntegrationClient(t).GetTopicAttributes(t.Context(), &sns.GetTopicAttributesInput{TopicArn: topic.PhysicalResourceId}); err != nil {
		t.Fatalf("GetTopicAttributes(%q) error = %v", aws.ToString(topic.PhysicalResourceId), err)
	}
	if aws.ToString(bucket.PhysicalResourceId) == "" || aws.ToString(queue.PhysicalResourceId) == "" || aws.ToString(table.PhysicalResourceId) == "" {
		t.Fatal("expected all multi-service resources to have physical IDs")
	}

	if _, err := client.DeleteStack(t.Context(), &cloudformation.DeleteStackInput{
		StackName:          aws.String(stackName),
		ClientRequestToken: aws.String("multi-delete-token"),
	}); err != nil {
		t.Fatal(err)
	}
	deleted := requireStackStatus(t, client, aws.ToString(createOutput.StackId), "DELETE_COMPLETE")
	if aws.ToString(deleted.StackId) != aws.ToString(stack.StackId) {
		t.Fatalf("deleted stack id = %q, want %q", aws.ToString(deleted.StackId), aws.ToString(stack.StackId))
	}

	if _, err := newS3IntegrationClient(t).HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("cfn-execution-multi-bucket")}); err == nil {
		t.Error("HeadBucket() succeeded after stack deletion")
	}
	if _, err := newSQSIntegrationClient(t).GetQueueUrl(t.Context(), &sqs.GetQueueUrlInput{QueueName: aws.String("cfn-execution-multi-queue")}); err == nil {
		t.Error("GetQueueUrl() succeeded after stack deletion")
	}
	if _, err := newDynamoDBIntegrationClient(t).DescribeTable(t.Context(), &dynamodb.DescribeTableInput{TableName: aws.String("cfn-execution-multi-table")}); err == nil {
		t.Error("DescribeTable() succeeded after stack deletion")
	}
	if _, err := newSNSIntegrationClient(t).GetTopicAttributes(t.Context(), &sns.GetTopicAttributesInput{TopicArn: topic.PhysicalResourceId}); err == nil {
		t.Error("GetTopicAttributes() succeeded after stack deletion")
	}
}

func TestCloudFormationExecution_RollbackOptions(t *testing.T) {
	client := newCloudFormationClient(t)
	s3Client := newS3IntegrationClient(t)

	templateBody := `{
  "Resources": {
    "First": {"Type": "AWS::S3::Bucket", "Properties": {"BucketName": "cfn-execution-retained-failure-bucket"}},
    "Second": {"Type": "AWS::S3::Bucket", "Properties": {"BucketName": "cfn-execution-retained-failure-bucket"}}
  }
}`
	retainedStack := "cfn-execution-on-failure-retain"
	_, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{
		StackName:       aws.String(retainedStack),
		TemplateBody:    aws.String(templateBody),
		DisableRollback: aws.Bool(true),
	})
	retainedDeleted := false
	t.Cleanup(func() {
		if !retainedDeleted {
			deleteIntegrationStack(t, client, retainedStack)
		}
	})
	if err == nil {
		t.Fatal("CreateStack() unexpectedly succeeded")
	}
	if _, headErr := s3Client.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("cfn-execution-retained-failure-bucket")}); headErr != nil {
		t.Fatalf("retained bucket was deleted: %v", headErr)
	}
	requireStackStatus(t, client, retainedStack, "CREATE_FAILED")
	if _, err := client.DeleteStack(t.Context(), &cloudformation.DeleteStackInput{StackName: aws.String(retainedStack)}); err != nil {
		t.Fatal(err)
	}
	retainedDeleted = true
	if _, err := s3Client.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("cfn-execution-retained-failure-bucket")}); err == nil {
		if _, err := s3Client.DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String("cfn-execution-retained-failure-bucket")}); err != nil {
			t.Fatal(err)
		}
	}

	rolledBackStack := "cfn-execution-on-failure-rollback"
	_, err = client.CreateStack(t.Context(), &cloudformation.CreateStackInput{
		StackName:    aws.String(rolledBackStack),
		TemplateBody: aws.String(strings.ReplaceAll(templateBody, "retained-failure", "rollback")),
		OnFailure:    cfntypes.OnFailureRollback,
	})
	t.Cleanup(func() { deleteIntegrationStack(t, client, rolledBackStack) })
	if err == nil {
		t.Fatal("CreateStack() unexpectedly succeeded")
	}
	if _, headErr := s3Client.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("cfn-execution-rollback-bucket")}); headErr == nil {
		t.Error("rollback bucket still exists after failed create")
	}
	requireStackStatus(t, client, rolledBackStack, "CREATE_FAILED")

	deletedStack := "cfn-execution-on-failure-delete"
	_, err = client.CreateStack(t.Context(), &cloudformation.CreateStackInput{
		StackName:    aws.String(deletedStack),
		TemplateBody: aws.String(strings.ReplaceAll(templateBody, "retained-failure", "delete")),
		OnFailure:    cfntypes.OnFailureDelete,
	})
	t.Cleanup(func() { deleteIntegrationStack(t, client, deletedStack) })
	if err == nil {
		t.Fatal("CreateStack() unexpectedly succeeded")
	}
	deletedStacks, err := client.ListStacks(t.Context(), &cloudformation.ListStacksInput{
		StackStatusFilter: []cfntypes.StackStatus{cfntypes.StackStatusDeleteComplete},
	})
	if err != nil {
		t.Fatal(err)
	}
	deletedStackFound := false
	for _, stack := range deletedStacks.StackSummaries {
		if aws.ToString(stack.StackName) == deletedStack {
			if stack.StackStatus != cfntypes.StackStatusDeleteComplete {
				t.Fatalf("deleted stack status = %s, want DELETE_COMPLETE", stack.StackStatus)
			}
			deletedStackFound = true
			break
		}
	}
	if !deletedStackFound {
		t.Fatalf("deleted stack %q was not listed as DELETE_COMPLETE", deletedStack)
	}
}

func TestCloudFormationExecution_ParameterOnlyNoInterruptionUpdate(t *testing.T) {
	client := newCloudFormationClient(t)
	stackName := "cfn-execution-parameter-update"
	_, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(stackName), TemplateBody: aws.String(parameterUpdateTemplate)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteIntegrationStack(t, client, stackName) })
	initial := stackResource(t, client, stackName, "Queue")
	sqsClient := newSQSIntegrationClient(t)
	initialAttributes, err := sqsClient.GetQueueAttributes(t.Context(), &sqs.GetQueueAttributesInput{QueueUrl: initial.PhysicalResourceId, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameDelaySeconds}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.UpdateStack(t.Context(), &cloudformation.UpdateStackInput{
		StackName:           aws.String(stackName),
		UsePreviousTemplate: aws.Bool(true),
		Parameters:          []cfntypes.Parameter{{ParameterKey: aws.String("Delay"), ParameterValue: aws.String("7")}},
		ClientRequestToken:  aws.String("parameter-update-token"),
	}); err != nil {
		t.Fatal(err)
	}
	updated := requireStackStatus(t, client, stackName, "UPDATE_COMPLETE")
	if aws.ToString(updated.Parameters[0].ParameterValue) != "7" {
		t.Fatalf("updated parameter = %q, want 7", aws.ToString(updated.Parameters[0].ParameterValue))
	}
	updatedResource := stackResource(t, client, stackName, "Queue")
	if aws.ToString(initial.PhysicalResourceId) != aws.ToString(updatedResource.PhysicalResourceId) {
		t.Fatalf("no-interruption update replaced queue: %q -> %q", aws.ToString(initial.PhysicalResourceId), aws.ToString(updatedResource.PhysicalResourceId))
	}
	if initialAttributes.Attributes["DelaySeconds"] == "7" {
		t.Fatal("initial backend DelaySeconds already equals updated value")
	}
	queueAttributes, err := sqsClient.GetQueueAttributes(t.Context(), &sqs.GetQueueAttributesInput{QueueUrl: updatedResource.PhysicalResourceId, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameDelaySeconds}})
	if err != nil {
		t.Fatal(err)
	}
	if queueAttributes.Attributes["DelaySeconds"] != "7" {
		t.Fatalf("backend DelaySeconds = %q, want 7", queueAttributes.Attributes["DelaySeconds"])
	}
}

func TestCloudFormationExecution_ReplacementAndCustomNameFailure(t *testing.T) {
	client := newCloudFormationClient(t)
	s3Client := newS3IntegrationClient(t)

	stackName := "cfn-execution-replacement"
	_, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(stackName), TemplateBody: aws.String(replacementTemplate)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteIntegrationStack(t, client, stackName) })
	oldResource := stackResource(t, client, stackName, "Bucket")
	oldID := aws.ToString(oldResource.PhysicalResourceId)
	if _, err := client.UpdateStack(t.Context(), &cloudformation.UpdateStackInput{StackName: aws.String(stackName), TemplateBody: aws.String(replacementWithNameTemplate)}); err != nil {
		t.Fatal(err)
	}
	newResource := stackResource(t, client, stackName, "Bucket")
	if aws.ToString(newResource.PhysicalResourceId) == oldID {
		t.Fatal("replacement update retained the old physical resource")
	}
	if _, err := s3Client.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("cfn-execution-replacement-bucket")}); err != nil {
		t.Fatalf("replacement bucket does not exist: %v", err)
	}
	if _, err := s3Client.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String(oldID)}); err == nil {
		t.Error("old generated bucket still exists after replacement")
	}

	customNameStack := "cfn-execution-custom-name-failure"
	_, err = client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(customNameStack), TemplateBody: aws.String(customNameBaseTemplate)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteIntegrationStack(t, client, customNameStack) })
	_, err = client.UpdateStack(t.Context(), &cloudformation.UpdateStackInput{StackName: aws.String(customNameStack), TemplateBody: aws.String(customNameReplacementTemplate)})
	if err == nil {
		t.Fatalf("custom-name replacement error = %v", err)
	}
	requireStackStatus(t, client, customNameStack, "UPDATE_FAILED")
}

func TestCloudFormationExecution_YAMLConditions(t *testing.T) {
	client := newCloudFormationClient(t)
	stackName := "cfn-execution-yaml-conditions"
	_, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(stackName), TemplateBody: aws.String(conditionalYAMLTemplate)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteIntegrationStack(t, client, stackName) })
	if resources, err := client.DescribeStackResources(t.Context(), &cloudformation.DescribeStackResourcesInput{StackName: aws.String(stackName)}); err != nil || len(resources.StackResources) != 1 {
		t.Fatalf("conditional create resources = %#v, error = %v", resources, err)
	}
	if _, err := client.UpdateStack(t.Context(), &cloudformation.UpdateStackInput{
		StackName:    aws.String(stackName),
		TemplateBody: aws.String(conditionalYAMLTemplate),
		Parameters:   []cfntypes.Parameter{{ParameterKey: aws.String("EnableQueue"), ParameterValue: aws.String("yes")}},
	}); err != nil {
		t.Fatal(err)
	}
	resources, err := client.DescribeStackResources(t.Context(), &cloudformation.DescribeStackResourcesInput{StackName: aws.String(stackName)})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.StackResources) != 2 {
		t.Fatalf("conditional update resources = %d, want 2", len(resources.StackResources))
	}
}

func TestCloudFormationExecution_CapabilitiesAndTransform(t *testing.T) {
	client := newCloudFormationClient(t)
	roleStack := "cfn-execution-capability-role"
	_, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(roleStack), TemplateBody: aws.String(iamRoleTemplate)})
	if err == nil || !strings.Contains(err.Error(), "InsufficientCapabilities") {
		t.Fatalf("missing capability error = %v", err)
	}
	requireStackStatus(t, client, roleStack, "CREATE_FAILED")
	deleteIntegrationStack(t, client, roleStack)

	roleStack = "cfn-execution-capability-role-allowed"
	_, err = client.CreateStack(t.Context(), &cloudformation.CreateStackInput{
		StackName:    aws.String(roleStack),
		TemplateBody: aws.String(iamRoleTemplate),
		Capabilities: []cfntypes.Capability{cfntypes.Capability("CAPABILITY_IAM")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteIntegrationStack(t, client, roleStack) })
	role := stackResource(t, client, roleStack, "Role")
	if _, err := newIAMIntegrationClient(t).GetRole(t.Context(), &iam.GetRoleInput{RoleName: role.PhysicalResourceId}); err != nil {
		t.Fatalf("GetRole() error = %v", err)
	}

	summary, err := client.GetTemplateSummary(t.Context(), &cloudformation.GetTemplateSummaryInput{TemplateBody: aws.String(transformTemplate)})
	if err != nil {
		t.Fatal(err)
	}
	if !containsCapability(summary.Capabilities, "CAPABILITY_AUTO_EXPAND") {
		t.Fatalf("transform capabilities = %v", summary.Capabilities)
	}
	transformStack := "cfn-execution-transform-rejected"
	transformDeleted := false
	t.Cleanup(func() {
		if !transformDeleted {
			deleteIntegrationStack(t, client, transformStack)
		}
	})
	_, err = client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(transformStack), TemplateBody: aws.String(transformTemplate)})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "transform") {
		t.Fatalf("transform create error = %v", err)
	}
	requireStackStatus(t, client, transformStack, "CREATE_FAILED")
	deleteIntegrationStack(t, client, transformStack)
	transformDeleted = true
}

func TestCloudFormationExecution_ChangeSetReviewAndExecution(t *testing.T) {
	client := newCloudFormationClient(t)
	stackName := "cfn-execution-change-set"
	_, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(stackName), TemplateBody: aws.String(changeSetBaseTemplate)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteIntegrationStack(t, client, stackName) })

	createOutput, err := client.CreateChangeSet(t.Context(), &cloudformation.CreateChangeSetInput{
		StackName:     aws.String(stackName),
		ChangeSetName: aws.String("add-topic"),
		ChangeSetType: cfntypes.ChangeSetTypeUpdate,
		TemplateBody:  aws.String(changeSetUpdatedTemplate),
		ClientToken:   aws.String("change-set-create-token"),
	})
	if err != nil {
		t.Fatal(err)
	}
	changeSet, err := client.DescribeChangeSet(t.Context(), &cloudformation.DescribeChangeSetInput{StackName: aws.String(stackName), ChangeSetName: createOutput.Id})
	if err != nil {
		t.Fatal(err)
	}
	golden.New(t, golden.WithIgnoreFields("ChangeSetId", "StackId", "CreationTime", "ResultMetadata")).Assert(t.Name()+"_describe", changeSet)
	if string(changeSet.Status) != "CREATE_COMPLETE" || string(changeSet.ExecutionStatus) != "AVAILABLE" {
		t.Fatalf("change set status = %s/%s", changeSet.Status, changeSet.ExecutionStatus)
	}
	if len(changeSet.Changes) != 1 || aws.ToString(changeSet.Changes[0].ResourceChange.LogicalResourceId) != "Topic" {
		t.Fatalf("change set changes = %#v", changeSet.Changes)
	}

	if _, err := client.ExecuteChangeSet(t.Context(), &cloudformation.ExecuteChangeSetInput{
		StackName:          aws.String(stackName),
		ChangeSetName:      createOutput.Id,
		ClientRequestToken: aws.String("change-set-execute-token"),
	}); err != nil {
		t.Fatal(err)
	}
	executedChangeSet, err := client.DescribeChangeSet(t.Context(), &cloudformation.DescribeChangeSetInput{StackName: aws.String(stackName), ChangeSetName: createOutput.Id})
	if err != nil {
		t.Fatal(err)
	}
	if string(executedChangeSet.Status) != "CREATE_COMPLETE" || string(executedChangeSet.ExecutionStatus) != "EXECUTE_COMPLETE" {
		t.Fatalf("executed change-set status = %s/%s, want CREATE_COMPLETE/EXECUTE_COMPLETE", executedChangeSet.Status, executedChangeSet.ExecutionStatus)
	}
	resources, err := client.DescribeStackResources(t.Context(), &cloudformation.DescribeStackResourcesInput{StackName: aws.String(stackName)})
	if err != nil {
		t.Fatal(err)
	}
	topicCount := 0
	for _, resource := range resources.StackResources {
		if aws.ToString(resource.LogicalResourceId) == "Topic" {
			topicCount++
		}
	}
	if topicCount != 1 {
		t.Fatalf("change-set Topic resources = %d, want exactly 1", topicCount)
	}
	requireStackStatus(t, client, stackName, "UPDATE_COMPLETE")
	if resource := stackResource(t, client, stackName, "Topic"); string(resource.ResourceStatus) != "UPDATE_COMPLETE" {
		t.Fatalf("change-set resource status = %s, want UPDATE_COMPLETE", resource.ResourceStatus)
	}
}

func TestCloudFormationExecution_CrossStackImportEdgesBlockExporterMutation(t *testing.T) {
	client := newCloudFormationClient(t)
	exporter := "cfn-execution-cross-exporter"
	importer := "cfn-execution-cross-importer"
	importerDeleted := false
	exporterDeleted := false
	if _, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(exporter), TemplateBody: aws.String(crossStackExporterTemplate)}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(importer), TemplateBody: aws.String(crossStackImporterTemplate)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !importerDeleted {
			if _, err := client.DeleteStack(context.Background(), &cloudformation.DeleteStackInput{StackName: aws.String(importer)}); err != nil {
				t.Errorf("cleanup importer: %v", err)
			}
		}
		if !exporterDeleted {
			if _, err := client.DeleteStack(context.Background(), &cloudformation.DeleteStackInput{StackName: aws.String(exporter)}); err != nil {
				t.Errorf("cleanup exporter: %v", err)
			}
		}
	})

	if _, err := client.DeleteStack(t.Context(), &cloudformation.DeleteStackInput{StackName: aws.String(exporter)}); err == nil {
		t.Fatal("DeleteStack(exporter) succeeded with an active importer")
	}
	if _, err := client.UpdateStack(t.Context(), &cloudformation.UpdateStackInput{StackName: aws.String(exporter), TemplateBody: aws.String(strings.ReplaceAll(crossStackExporterTemplate, "cross-stack-value", "changed-value"))}); err == nil {
		t.Fatal("UpdateStack(exporter) succeeded while its export was imported")
	}
	if _, err := client.DeleteStack(t.Context(), &cloudformation.DeleteStackInput{StackName: aws.String(importer)}); err != nil {
		t.Fatal(err)
	}
	importerDeleted = true
	if _, err := client.DeleteStack(t.Context(), &cloudformation.DeleteStackInput{StackName: aws.String(exporter)}); err != nil {
		t.Fatal(err)
	}
	exporterDeleted = true
}

func TestCloudFormationExecution_ExportsAndRetain(t *testing.T) {
	client := newCloudFormationClient(t)
	stackName := "cfn-execution-export"
	_, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(stackName), TemplateBody: aws.String(exportTemplate)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteIntegrationStack(t, client, stackName) })
	exports, err := client.ListExports(t.Context(), &cloudformation.ListExportsInput{})
	if err != nil {
		t.Fatal(err)
	}
	golden.New(t, golden.WithIgnoreFields("ExportingStackId", "ResultMetadata")).Assert(t.Name()+"_exports", exports)
	if len(exports.Exports) != 1 || aws.ToString(exports.Exports[0].Name) != "cfn-execution-export-name" || aws.ToString(exports.Exports[0].Value) != "cfn-execution-export-value" {
		t.Fatalf("exports = %#v", exports.Exports)
	}
	if _, err := client.DeleteStack(t.Context(), &cloudformation.DeleteStackInput{StackName: aws.String(stackName)}); err != nil {
		t.Fatal(err)
	}
	exports, err = client.ListExports(t.Context(), &cloudformation.ListExportsInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, export := range exports.Exports {
		if aws.ToString(export.Name) == "cfn-execution-export-name" {
			t.Fatal("export remains after stack deletion")
		}
	}

	retainClient := newS3IntegrationClient(t)
	retainStack := "cfn-execution-retain"
	_, err = client.CreateStack(t.Context(), &cloudformation.CreateStackInput{StackName: aws.String(retainStack), TemplateBody: aws.String(retainTemplate)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteStack(t.Context(), &cloudformation.DeleteStackInput{StackName: aws.String(retainStack)}); err != nil {
		t.Fatal(err)
	}
	if _, err := retainClient.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("cfn-execution-retain-bucket")}); err != nil {
		t.Fatalf("retained bucket was deleted: %v", err)
	}
	if _, err := retainClient.DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String("cfn-execution-retain-bucket")}); err != nil {
		t.Fatal(err)
	}
}

func TestCloudFormationExecution_EventRequestTokens(t *testing.T) {
	client := newCloudFormationClient(t)
	stackName := "cfn-execution-event-tokens"
	createOutput, err := client.CreateStack(t.Context(), &cloudformation.CreateStackInput{
		StackName:          aws.String(stackName),
		TemplateBody:       aws.String(parameterUpdateTemplate),
		ClientRequestToken: aws.String("events-create-token"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateStack(t.Context(), &cloudformation.UpdateStackInput{
		StackName:           aws.String(stackName),
		UsePreviousTemplate: aws.Bool(true),
		Parameters:          []cfntypes.Parameter{{ParameterKey: aws.String("Delay"), ParameterValue: aws.String("9")}},
		ClientRequestToken:  aws.String("events-update-token"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteStack(t.Context(), &cloudformation.DeleteStackInput{StackName: aws.String(stackName), ClientRequestToken: aws.String("events-delete-token")}); err != nil {
		t.Fatal(err)
	}
	events, err := client.DescribeStackEvents(t.Context(), &cloudformation.DescribeStackEventsInput{StackName: createOutput.StackId})
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"events-create-token", "events-update-token", "events-delete-token"} {
		if !hasEventToken(events.StackEvents, token) {
			t.Errorf("event token %q was not recorded in %#v", token, events.StackEvents)
		}
	}
}

func containsCapability(values []cfntypes.Capability, want string) bool {
	for _, value := range values {
		if string(value) == want {
			return true
		}
	}

	return false
}

func hasEventToken(events []cfntypes.StackEvent, want string) bool {
	for _, event := range events {
		if aws.ToString(event.ClientRequestToken) == want {
			return true
		}
	}

	return false
}
