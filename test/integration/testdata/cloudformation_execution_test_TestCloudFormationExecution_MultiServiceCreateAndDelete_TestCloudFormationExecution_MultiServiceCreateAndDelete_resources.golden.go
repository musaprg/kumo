{
  "StackResources": [
    {
      "LogicalResourceId": "Bucket",
      "ResourceStatus": "CREATE_COMPLETE",
      "ResourceType": "AWS::S3::Bucket",
      "Timestamp": "2026-10-03T11:26:21.285Z",
      "Description": null,
      "DriftInformation": null,
      "ModuleInfo": null,
      "PhysicalResourceId": "cfn-execution-multi-bucket",
      "ResourceStatusReason": null,
      "StackId": "arn:aws:cloudformation:us-east-1:123456789012:stack/cfn-execution-multi-service/d8cd7db4-9f5d-40e7-b26a-894344b889d0",
      "StackName": "cfn-execution-multi-service"
    },
    {
      "LogicalResourceId": "Queue",
      "ResourceStatus": "CREATE_COMPLETE",
      "ResourceType": "AWS::SQS::Queue",
      "Timestamp": "2026-10-03T11:26:21.286Z",
      "Description": null,
      "DriftInformation": null,
      "ModuleInfo": null,
      "PhysicalResourceId": "http://localhost:4566/000000000000/cfn-execution-multi-queue",
      "ResourceStatusReason": null,
      "StackId": "arn:aws:cloudformation:us-east-1:123456789012:stack/cfn-execution-multi-service/d8cd7db4-9f5d-40e7-b26a-894344b889d0",
      "StackName": "cfn-execution-multi-service"
    },
    {
      "LogicalResourceId": "Table",
      "ResourceStatus": "CREATE_COMPLETE",
      "ResourceType": "AWS::DynamoDB::Table",
      "Timestamp": "2026-10-03T11:26:21.286Z",
      "Description": null,
      "DriftInformation": null,
      "ModuleInfo": null,
      "PhysicalResourceId": "cfn-execution-multi-table",
      "ResourceStatusReason": null,
      "StackId": "arn:aws:cloudformation:us-east-1:123456789012:stack/cfn-execution-multi-service/d8cd7db4-9f5d-40e7-b26a-894344b889d0",
      "StackName": "cfn-execution-multi-service"
    },
    {
      "LogicalResourceId": "Topic",
      "ResourceStatus": "CREATE_COMPLETE",
      "ResourceType": "AWS::SNS::Topic",
      "Timestamp": "2026-10-03T11:26:21.291Z",
      "Description": null,
      "DriftInformation": null,
      "ModuleInfo": null,
      "PhysicalResourceId": "arn:aws:sns:us-east-1:000000000000:cfn-execution-multi-topic",
      "ResourceStatusReason": null,
      "StackId": "arn:aws:cloudformation:us-east-1:123456789012:stack/cfn-execution-multi-service/d8cd7db4-9f5d-40e7-b26a-894344b889d0",
      "StackName": "cfn-execution-multi-service"
    }
  ],
  "ResultMetadata": {}
}