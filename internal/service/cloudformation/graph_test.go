//nolint:goconst // Dependency graph fixtures intentionally reuse logical IDs and attributes.
package cloudformation

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sivchari/kumo/internal/service/cloudformation/resource"
	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

const (
	testBucketLogicalID = "Bucket"
	testSecondEventID   = "event-2"
	testRefKey          = "Ref"
	testConsumerValue   = "consumer"
	testProducerValue   = "producer"
	testProducer        = "Producer"
	testAlternate       = "Alternate"
	testConsumer        = "Consumer"
	testZulu            = "Zulu"
	testAlpha           = "Alpha"
	testMiddle          = "Middle"
	testParameterProd   = "prod"
)

func TestDependencyGraphUsesSelectedIntrinsicBranchesAndAdapterSchemas(t *testing.T) {
	t.Parallel()

	body := `
Parameters:
  UseProducer:
    Type: String
    Default: "true"
Conditions:
  UseProducerCondition: !Equals [!Ref UseProducer, "true"]
Resources:
  Producer:
    Type: AWS::S3::Bucket
    Properties:
      BucketName: producer
  Alternate:
    Type: AWS::SNS::Topic
    Properties:
      TopicName: alternate
  Consumer:
    Type: AWS::SQS::Queue
    DependsOn: Alternate
    Properties:
      QueueName: !Sub
        - "${Local}-${Producer.Arn}-${!Escaped}"
        - Local: !If [UseProducerCondition, !Ref Producer, !Ref Alternate]
`

	graph, err := BuildDependencyGraph(body, template.EvaluationInput{
		ResourceAttributes: map[string]map[string]string{
			testProducer:  {testRefKey: testProducerValue, "Arn": "arn:aws:s3:::producer"},
			testAlternate: {testRefKey: "arn:aws:sns:us-east-1:123456789012:alternate"},
			testConsumer:  {testRefKey: testConsumerValue},
		},
	}, resource.DefaultRegistry())
	if err != nil {
		t.Fatalf("BuildDependencyGraph() error = %v", err)
	}

	if got, want := graph.Dependencies(testConsumer), []string{testAlternate, testProducer}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Dependencies(Consumer) = %v, want %v", got, want)
	}

	if strings.Contains(strings.Join(graph.Dependencies(testConsumer), " "), "Escaped") {
		t.Fatal("escaped Fn::Sub variable became a dependency")
	}
}

func TestDependencyGraphIgnoresUnselectedIfBranch(t *testing.T) {
	t.Parallel()

	body := `
Parameters:
  UseProducer:
    Type: String
    Default: "false"
Conditions:
  UseProducerCondition: !Equals [!Ref UseProducer, "true"]
Resources:
  Producer:
    Type: AWS::S3::Bucket
    Properties: {BucketName: producer}
  Alternate:
    Type: AWS::SNS::Topic
    Properties: {TopicName: alternate}
  Consumer:
    Type: AWS::SQS::Queue
    Properties:
      QueueName: !If [UseProducerCondition, !Ref Producer, !Ref Alternate]
`

	graph, err := BuildDependencyGraph(body, template.EvaluationInput{
		ResourceAttributes: map[string]map[string]string{
			testProducer:  {testRefKey: testProducerValue},
			testAlternate: {testRefKey: "arn:aws:sns:us-east-1:123456789012:alternate"},
			testConsumer:  {testRefKey: testConsumerValue},
		},
	}, resource.DefaultRegistry())
	if err != nil {
		t.Fatalf("BuildDependencyGraph() error = %v", err)
	}

	if got, want := graph.Dependencies(testConsumer), []string{testAlternate}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Dependencies(Consumer) = %v, want %v", got, want)
	}
}

func TestDependencyGraphEvaluatesFindInMapInConditions(t *testing.T) {
	t.Parallel()

	body := `
Parameters:
  Environment:
    Type: String
Mappings:
  EnvironmentValues:
    prod:
      Enabled: "true"
Conditions:
  IsEnabled: !Equals [!FindInMap [EnvironmentValues, !Ref Environment, Enabled], "true"]
Resources:
  Producer:
    Type: AWS::S3::Bucket
    Properties: {BucketName: producer}
  Alternate:
    Type: AWS::SNS::Topic
    Properties: {TopicName: alternate}
  Consumer:
    Type: AWS::SQS::Queue
    Properties:
      QueueName: !If [IsEnabled, !Ref Producer, !Ref Alternate]
`

	graph, err := BuildDependencyGraph(body, template.EvaluationInput{
		Parameters: map[string]string{"Environment": testParameterProd},
		ResourceAttributes: map[string]map[string]string{
			testProducer:  {testRefKey: testProducerValue},
			testAlternate: {testRefKey: "alternate"},
			testConsumer:  {testRefKey: testConsumerValue},
		},
	}, resource.DefaultRegistry())
	if err != nil {
		t.Fatalf("BuildDependencyGraph() error = %v", err)
	}

	if got, want := graph.Dependencies(testConsumer), []string{testProducer}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Dependencies(Consumer) = %v, want %v", got, want)
	}
}

func TestDependencyGraphUsesCanonicalConditionIntrinsics(t *testing.T) {
	t.Parallel()

	body := `
Parameters:
  Environment:
    Type: String
    Default: prod
Conditions:
  Joined: !Equals [!Join ["-", [!Ref Environment, app]], prod-app]
  Subbed: !Equals [!Sub ["${AWS::Region}-${Suffix}", {Suffix: !Ref Environment}], us-east-1-prod]
  Imported: !Equals [!ImportValue FeatureFlag, enabled]
Resources:
  JoinedProducer:
    Type: AWS::S3::Bucket
    Properties: {BucketName: joined}
  SubProducer:
    Type: AWS::S3::Bucket
    Properties: {BucketName: subbed}
  ImportedProducer:
    Type: AWS::S3::Bucket
    Properties: {BucketName: imported}
  Alternate:
    Type: AWS::SNS::Topic
    Properties: {TopicName: alternate}
  Consumer:
    Type: AWS::SQS::Queue
    Properties:
      Joined: !If [Joined, !Ref JoinedProducer, !Ref Alternate]
      Subbed: !If [Subbed, !Ref SubProducer, !Ref Alternate]
      Imported: !If [Imported, !Ref ImportedProducer, !Ref Alternate]
`

	graph, err := BuildDependencyGraph(body, template.EvaluationInput{
		PseudoParameters: map[string]string{"AWS::Region": testRegion},
		Imports:          map[string]string{"FeatureFlag": "enabled"},
		ResourceAttributes: map[string]map[string]string{
			"JoinedProducer":   {testRefKey: "joined"},
			"SubProducer":      {testRefKey: "subbed"},
			"ImportedProducer": {testRefKey: "imported"},
			"Alternate":        {testRefKey: "alternate"},
			"Consumer":         {testRefKey: "consumer"},
		},
	}, resource.DefaultRegistry())
	if err != nil {
		t.Fatalf("BuildDependencyGraph() error = %v", err)
	}

	if got, want := graph.Dependencies("Consumer"), []string{"ImportedProducer", "JoinedProducer", "SubProducer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Dependencies(Consumer) = %v, want %v", got, want)
	}
}

func TestDependencyGraphOrdersCreateAndDeleteDeterministically(t *testing.T) {
	t.Parallel()

	body := `
Resources:
  Zulu:
    Type: AWS::S3::Bucket
    Properties: {BucketName: zulu}
  Alpha:
    Type: AWS::S3::Bucket
    Properties: {BucketName: alpha}
  Middle:
    Type: AWS::SQS::Queue
    DependsOn: [Zulu, Alpha]
    Properties: {QueueName: middle}
`
	graph, err := BuildDependencyGraph(body, template.EvaluationInput{ResourceAttributes: map[string]map[string]string{testZulu: {testRefKey: "zulu"}, testAlpha: {testRefKey: "alpha"}, testMiddle: {testRefKey: "middle"}}}, resource.DefaultRegistry())
	// Check graph construction before inspecting its order.
	if err != nil {
		t.Fatalf("BuildDependencyGraph() error = %v", err)
	}

	if got, want := graph.CreateOrder(), []string{"Alpha", "Zulu", "Middle"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateOrder() = %v, want %v", got, want)
	}

	if got, want := graph.DeleteOrder(), []string{"Middle", "Zulu", "Alpha"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DeleteOrder() = %v, want %v", got, want)
	}
}

func TestDependencyGraphRejectsUnknownGetAttAttributeAndCycles(t *testing.T) {
	t.Parallel()

	unknownAttribute := `
Resources:
  Producer:
    Type: AWS::S3::Bucket
    Properties: {BucketName: producer}
  Consumer:
    Type: AWS::SQS::Queue
    Properties: {QueueName: {Fn::Sub: "${Producer.NotDeclared}"}}
`
	_, err := BuildDependencyGraph(unknownAttribute, template.EvaluationInput{
		ResourceAttributes: map[string]map[string]string{
			testProducer: {testRefKey: testProducerValue, "NotDeclared": "value"},
			testConsumer: {testRefKey: testConsumerValue},
		},
	}, resource.DefaultRegistry())

	if err == nil || !strings.Contains(err.Error(), "NotDeclared") {
		t.Fatalf("unknown GetAtt error = %v, want attribute error", err)
	}

	cycle := `
Resources:
  A:
    Type: AWS::S3::Bucket
    DependsOn: B
    Properties: {BucketName: a}
  B:
    Type: AWS::S3::Bucket
    DependsOn: A
    Properties: {BucketName: b}
`
	_, err = BuildDependencyGraph(cycle, template.EvaluationInput{
		ResourceAttributes: map[string]map[string]string{
			"A": {testRefKey: "a"}, "B": {testRefKey: "b"},
		},
	}, resource.DefaultRegistry())

	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v, want cycle error", err)
	}
}

func TestDependencyGraphUsesReferencedResourceSchemaForGetAtt(t *testing.T) {
	t.Parallel()

	body := `
Resources:
  Bucket:
    Type: AWS::S3::Bucket
    Properties: {BucketName: producer}
  Consumer:
    Type: AWS::SNS::Topic
    Properties: {DisplayName: {Fn::GetAtt: [Bucket, DomainName]}}
`
	graph, err := BuildDependencyGraph(body, template.EvaluationInput{ResourceAttributes: map[string]map[string]string{testBucketLogicalID: {testRefKey: testProducerValue, "DomainName": "producer.s3.amazonaws.com"}, testConsumer: {testRefKey: testConsumerValue}}}, resource.DefaultRegistry())
	// Check graph construction before inspecting its dependencies.
	if err != nil {
		t.Fatalf("BuildDependencyGraph() error = %v", err)
	}

	if got, want := graph.Dependencies(testConsumer), []string{testBucketLogicalID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Dependencies(Consumer) = %v, want %v", got, want)
	}
}
