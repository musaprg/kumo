//nolint:goconst,wsl // CloudFormation fixtures intentionally repeat AWS property names.
package resource

import (
	"context"
	"strings"
	"testing"

	"github.com/sivchari/kumo/internal/service/sns"
	"github.com/sivchari/kumo/internal/service/sqs"
)

func TestSNSSubscriptionCreatesAndDeletesThroughSNSStorage(t *testing.T) {
	topicAdapter := mustAdapter(t, "AWS::SNS::Topic")
	topicResult, err := topicAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"TopicName": "cfn-subscription-topic",
	}})
	if err != nil {
		t.Fatalf("Create() topic error = %v", err)
	}
	t.Cleanup(func() {
		_ = topicAdapter.Delete(context.Background(), DeleteRequest{Identifier: topicResult.Identifier})
	})

	adapter := mustAdapter(t, "AWS::SNS::Subscription")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"TopicArn":           topicResult.Identifier,
		"Protocol":           "lambda",
		"Endpoint":           "arn:aws:lambda:us-east-1:000000000000:function:cfn-function",
		"RawMessageDelivery": true,
		"FilterPolicy":       map[string]any{"event": []any{"created"}},
	}})
	if err != nil {
		t.Fatalf("Create() subscription error = %v", err)
	}
	assertExactAttributes(t, []string{"Arn"}, result)

	storage, err := lookupStorage[sns.Storage]("sns")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	subscription, err := storage.GetSubscription(context.Background(), result.Identifier)
	if err != nil {
		t.Fatalf("GetSubscription() error = %v", err)
	}
	if subscription.TopicARN != topicResult.Identifier || subscription.Protocol != "lambda" {
		t.Fatalf("stored subscription = %#v, want topic and protocol", subscription)
	}
	if subscription.SubscriptionAttributes["RawMessageDelivery"] != "true" || !strings.Contains(subscription.SubscriptionAttributes["FilterPolicy"], "created") {
		t.Fatalf("subscription attributes = %#v, want configured attributes", subscription.SubscriptionAttributes)
	}

	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() subscription error = %v", err)
	}
	if _, err := storage.GetSubscription(context.Background(), result.Identifier); err == nil {
		t.Fatal("GetSubscription() after Delete unexpectedly succeeded")
	}
}

func TestSQSQueuePolicySetsAndClearsPolicyThroughSQSStorage(t *testing.T) {
	queueAdapter := mustAdapter(t, "AWS::SQS::Queue")
	queueResult, err := queueAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"QueueName": "cfn-queue-policy-queue",
	}})
	if err != nil {
		t.Fatalf("Create() queue error = %v", err)
	}
	t.Cleanup(func() {
		_ = queueAdapter.Delete(context.Background(), DeleteRequest{Identifier: queueResult.Identifier})
	})

	adapter := mustAdapter(t, "AWS::SQS::QueuePolicy")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"Queues": []any{queueResult.Identifier},
		"PolicyDocument": map[string]any{
			"Version": "2012-10-17",
			"Statement": []any{map[string]any{
				"Effect": "Allow", "Principal": map[string]any{"AWS": "*"},
				"Action": "sqs:SendMessage", "Resource": "*",
			}},
		},
	}})
	if err != nil {
		t.Fatalf("Create() queue policy error = %v", err)
	}
	if result.Attributes["Id"] != result.Identifier {
		t.Fatalf("queue policy Id = %v, want identifier %q", result.Attributes["Id"], result.Identifier)
	}
	assertExactAttributes(t, []string{"Id"}, result)

	storage, err := lookupStorage[sqs.Storage]("sqs")
	if err != nil {
		t.Fatalf("lookupStorage() error = %v", err)
	}
	attributes, err := storage.GetQueueAttributes(context.Background(), queueResult.Identifier, []string{"Policy"})
	if err != nil {
		t.Fatalf("GetQueueAttributes() error = %v", err)
	}
	if !strings.Contains(attributes["Policy"], "sqs:SendMessage") {
		t.Fatalf("stored queue policy = %q, want SendMessage", attributes["Policy"])
	}

	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatalf("Delete() queue policy error = %v", err)
	}
	attributes, err = storage.GetQueueAttributes(context.Background(), queueResult.Identifier, []string{"Policy"})
	if err != nil {
		t.Fatalf("GetQueueAttributes() after delete error = %v", err)
	}
	if attributes["Policy"] != "" {
		t.Fatalf("stored queue policy after delete = %q, want empty", attributes["Policy"])
	}
}

func TestSQSQueueUpdateResetsOmittedDesiredProperties(t *testing.T) {
	adapter := mustAdapter(t, "AWS::SQS::Queue")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"QueueName":                     "cfn-sqs-reset-queue",
		"DelaySeconds":                  int64(42),
		"MaximumMessageSize":            int64(1024),
		"MessageRetentionPeriod":        int64(60),
		"ReceiveMessageWaitTimeSeconds": int64(20),
		"VisibilityTimeout":             int64(120),
		"Policy":                        map[string]any{"Version": "2012-10-17"},
		"RedrivePolicy":                 map[string]any{"deadLetterTargetArn": "arn:aws:sqs:us-east-1:000000000000:dlq", "maxReceiveCount": int64(2)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}) })

	updater, ok := adapter.(UpdateCapable)
	if !ok {
		t.Fatal("SQS queue does not implement Update")
	}
	if _, err := updater.Update(context.Background(), UpdateRequest{Identifier: result.Identifier, Properties: map[string]any{"QueueName": "cfn-sqs-reset-queue"}}); err != nil {
		t.Fatal(err)
	}
	storage, err := lookupStorage[sqs.Storage]("sqs")
	if err != nil {
		t.Fatal(err)
	}
	attributes, err := storage.GetQueueAttributes(context.Background(), result.Identifier, []string{"All"})
	if err != nil {
		t.Fatal(err)
	}
	for property, want := range map[string]string{
		"DelaySeconds": "0", "MaximumMessageSize": "262144", "MessageRetentionPeriod": "345600",
		"ReceiveMessageWaitTimeSeconds": "0", "VisibilityTimeout": "30", "Policy": "", "RedrivePolicy": "",
	} {
		if got := attributes[property]; got != want {
			t.Errorf("%s after reset = %q, want %q", property, got, want)
		}
	}
}

func TestSNSSubscriptionUpdateResetsOmittedDesiredAttributes(t *testing.T) {
	topicAdapter := mustAdapter(t, "AWS::SNS::Topic")
	topic, err := topicAdapter.Create(context.Background(), CreateRequest{Properties: map[string]any{"TopicName": "cfn-subscription-reset-topic"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = topicAdapter.Delete(context.Background(), DeleteRequest{Identifier: topic.Identifier}) })
	adapter := mustAdapter(t, "AWS::SNS::Subscription")
	result, err := adapter.Create(context.Background(), CreateRequest{Properties: map[string]any{
		"TopicArn": topic.Identifier, "Protocol": "lambda", "Endpoint": "arn:aws:lambda:us-east-1:000000000000:function:reset",
		"FilterPolicy": map[string]any{"event": []any{"old"}}, "RawMessageDelivery": true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	updater, ok := adapter.(UpdateCapable)
	if !ok {
		t.Fatal("SNS subscription does not implement Update")
	}
	if _, err := updater.Update(context.Background(), UpdateRequest{Identifier: result.Identifier, Properties: map[string]any{
		"TopicArn": topic.Identifier, "Protocol": "lambda", "Endpoint": "arn:aws:lambda:us-east-1:000000000000:function:reset",
	}}); err != nil {
		t.Fatal(err)
	}
	storage, err := lookupStorage[sns.Storage]("sns")
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := storage.GetSubscription(context.Background(), result.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	if subscription.SubscriptionAttributes["FilterPolicy"] != "" || subscription.SubscriptionAttributes["RawMessageDelivery"] != "false" {
		t.Fatalf("subscription attributes after reset = %#v", subscription.SubscriptionAttributes)
	}
	if err := adapter.Delete(context.Background(), DeleteRequest{Identifier: result.Identifier}); err != nil {
		t.Fatal(err)
	}
}
