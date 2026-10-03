package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const (
	queryActionField      = "Action"
	queryStackNameField   = "StackName"
	queryEnvironmentValue = "Environment"
	queryProductionValue  = "prod"
)

func TestParseServiceFromUserAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		userAgent string
		want      string
	}{
		{
			name:      "standard AWS SDK Go v2 with rds",
			userAgent: "aws-sdk-go-v2/1.36.3 ua/2.1 api/rds#1.5.0 os/linux lang/go#1.25.0",
			want:      "rds",
		},
		{
			name:      "neptune",
			userAgent: "aws-sdk-go-v2/1.36.3 ua/2.1 api/neptune#1.48.11 os/linux lang/go#1.25.0",
			want:      "neptune",
		},
		{
			name:      "docdb",
			userAgent: "aws-sdk-go-v2/1.36.3 api/docdb#1.48.11",
			want:      "docdb",
		},
		{
			name:      "no api token",
			userAgent: "aws-sdk-go-v2/1.36.3 ua/2.1",
			want:      "",
		},
		{
			name:      "empty string",
			userAgent: "",
			want:      "",
		},
		{
			name:      "Java sdk sns upper case",
			userAgent: "aws-sdk-java/2.44.12 md/io#sync md/http#Apache ua/2.1 api/SNS#2.44.x os/Linux#6.1 lang/java#17 md/OpenJDK_64-Bit_Server_VM#17 md/vendor#OpenJDK md/en_US",
			want:      "sns",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := parseServiceFromUserAgent(tt.userAgent)
			if got != tt.want {
				t.Errorf("parseServiceFromUserAgent(%q) = %q, want %q", tt.userAgent, got, tt.want)
			}
		})
	}
}

func TestQueryDispatcher_RoutesViaUserAgent(t *testing.T) {
	t.Parallel()

	d := NewQueryProtocolDispatcher()

	called := false

	d.RegisterAction("DescribeInstances", "EC2", "ec2", func(w http.ResponseWriter, _ *http.Request) {
		called = true

		w.WriteHeader(http.StatusOK)
	})

	body := strings.NewReader("Action=DescribeInstances&Version=2016-11-15")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", body)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "aws-sdk-go-v2/1.36.3 api/ec2#1.0.0 os/linux")

	rec := httptest.NewRecorder()

	d.ServeHTTP(rec, req)

	if !called {
		t.Error("expected handler to be called")
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestQueryDispatcher_DisambiguatesOverlappingActions(t *testing.T) {
	t.Parallel()

	d := NewQueryProtocolDispatcher()

	rdsHandler := false
	neptuneHandler := false

	d.RegisterAction("CreateDBCluster", "AmazonRDSv19", "rds", func(w http.ResponseWriter, _ *http.Request) {
		rdsHandler = true

		w.WriteHeader(http.StatusOK)
	})
	d.RegisterAction("CreateDBCluster", "AmazonNeptuneDataService", "neptune", func(w http.ResponseWriter, _ *http.Request) {
		neptuneHandler = true

		w.WriteHeader(http.StatusOK)
	})

	body := strings.NewReader("Action=CreateDBCluster&Version=2014-10-31")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", body)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "aws-sdk-go-v2/1.36.3 api/neptune#1.48.11 os/linux")

	rec := httptest.NewRecorder()

	d.ServeHTTP(rec, req)

	if rdsHandler {
		t.Error("RDS handler should not have been called")
	}

	if !neptuneHandler {
		t.Error("Neptune handler should have been called")
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestQueryDispatcher_MissingUserAgent_UniqueAction(t *testing.T) {
	t.Parallel()

	d := NewQueryProtocolDispatcher()

	called := false

	d.RegisterAction("CreateDBCluster", "AmazonRDSv19", "rds", func(w http.ResponseWriter, _ *http.Request) {
		called = true

		w.WriteHeader(http.StatusOK)
	})

	body := strings.NewReader("Action=CreateDBCluster&Version=2014-10-31")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", body)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	d.ServeHTTP(rec, req)

	// When the action is unique across all services, the dispatcher should
	// fall back to action-based lookup even without a User-Agent header.
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	if !called {
		t.Error("expected handler to be called via fallback")
	}
}

func TestQueryDispatcher_MissingUserAgent_AmbiguousAction(t *testing.T) {
	t.Parallel()

	d := NewQueryProtocolDispatcher()

	d.RegisterAction("CreateDBCluster", "AmazonRDSv19", "rds", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	d.RegisterAction("CreateDBCluster", "AmazonNeptuneDataService", "neptune", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	body := strings.NewReader("Action=CreateDBCluster&Version=2014-10-31")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", body)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	d.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "AmbiguousAction") {
		t.Errorf("expected AmbiguousAction error, got %s", rec.Body.String())
	}
}

func TestQueryDispatcher_UnknownService(t *testing.T) {
	t.Parallel()

	d := NewQueryProtocolDispatcher()

	d.RegisterAction("CreateDBCluster", "AmazonRDSv19", "rds", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	body := strings.NewReader("Action=CreateDBCluster&Version=2014-10-31")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", body)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "aws-sdk-go-v2/1.36.3 api/unknown#1.0.0 os/linux")

	rec := httptest.NewRecorder()

	d.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "UnknownService") {
		t.Errorf("expected UnknownService error, got %s", rec.Body.String())
	}
}

func TestQueryDispatcher_UnknownAction(t *testing.T) {
	t.Parallel()

	d := NewQueryProtocolDispatcher()

	d.RegisterAction("DescribeInstances", "EC2", "ec2", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	body := strings.NewReader("Action=NonExistentAction&Version=2016-11-15")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", body)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "aws-sdk-go-v2/1.36.3 api/ec2#1.0.0 os/linux")

	rec := httptest.NewRecorder()

	d.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "UnknownAction") {
		t.Errorf("expected UnknownAction error, got %s", rec.Body.String())
	}
}

//nolint:wsl // The test keeps request setup and XML assertions together.
func TestQueryDispatcherCloudFormationPreDispatchErrorsUseXML(t *testing.T) {
	t.Parallel()

	d := NewQueryProtocolDispatcher()
	d.RegisterAction("CreateStack", "CloudFormation", "cloudformation", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader("Action=UnknownAction&Version=2010-05-15"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "aws-sdk-go-v2/1.0 api/cloudformation#1.0.0")
	rec := httptest.NewRecorder()

	d.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Header().Get("Content-Type"), "text/xml") {
		t.Fatalf("response status/content-type = %d/%q, want CloudFormation XML error", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "<ErrorResponse") || !strings.Contains(rec.Body.String(), "<Code>UnknownAction</Code>") {
		t.Fatalf("response body = %s, want Query XML error", rec.Body.String())
	}
}

// formToJSONTests covers the form-to-JSON conversions that
// QueryProtocolDispatcher relies on before dispatching to a JSON protocol
// handler.
var formToJSONTests = []struct {
	name string
	form url.Values
	want map[string]any
}{
	{
		name: "scalar member list",
		form: url.Values{
			"Subnets.member.1": {"subnet-1"},
			"Subnets.member.2": {"subnet-2"},
		},
		want: map[string]any{
			"Subnets": []any{"subnet-1", "subnet-2"},
		},
	},
	{
		name: "struct member list (Tags)",
		form: url.Values{
			queryActionField:      {"CreateTopic"},
			"Tags.member.1.Key":   {"env"},
			"Tags.member.1.Value": {"terraform"},
			"Tags.member.2.Key":   {"team"},
			"Tags.member.2.Value": {"platform"},
		},
		want: map[string]any{
			"Tags": []any{
				map[string]any{"Key": "env", "Value": "terraform"},
				map[string]any{"Key": "team", "Value": "platform"},
			},
		},
	},
	{
		name: "Attributes.entry still works",
		form: url.Values{
			"Attributes.entry.1.key":   {"VisibilityTimeout"},
			"Attributes.entry.1.value": {"30"},
		},
		want: map[string]any{
			"Attributes": map[string]any{"VisibilityTimeout": "30"},
		},
	},
}

func TestFormToJSON(t *testing.T) {
	t.Parallel()

	for _, tt := range formToJSONTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := formToJSON(tt.form)

			var gotMap map[string]any
			if err := json.Unmarshal(got, &gotMap); err != nil {
				t.Fatalf("failed to unmarshal formToJSON output %s: %v", got, err)
			}

			if !reflect.DeepEqual(gotMap, tt.want) {
				t.Errorf("formToJSON(%v) = %v, want %v", tt.form, gotMap, tt.want)
			}
		})
	}
}

//nolint:wsl // The fixture registers a handler before decoding its request.
func TestQueryDispatcherCloudFormationDecodesNestedIndexedForms(t *testing.T) {
	t.Parallel()

	d := NewQueryProtocolDispatcher()
	var got map[string]any
	d.RegisterAction("CreateStack", "CloudFormation", "cloudformation", func(_ http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode dispatched body: %v", err)
		}
	})

	form := url.Values{
		queryActionField:                                       {"CreateStack"},
		queryStackNameField:                                    {"query-stack"},
		"Parameters.member.1.ParameterKey":                     {queryEnvironmentValue},
		"Parameters.member.1.ParameterValue":                   {queryProductionValue},
		"Parameters.member.1.UsePreviousValue":                 {"true"},
		"RollbackConfiguration.MonitoringTimeInMinutes":        {"10"},
		"RollbackConfiguration.RollbackTriggers.member.1.Arn":  {"arn:aws:cloudwatch:::alarm/a"},
		"RollbackConfiguration.RollbackTriggers.member.1.Type": {"AWS::CloudWatch::Alarm"},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "aws-sdk-go-v2/1.36.3 api/cloudformation#1.0.0")

	d.ServeHTTP(httptest.NewRecorder(), req)

	want := map[string]any{
		queryStackNameField: "query-stack",
		"Parameters": []any{map[string]any{
			"ParameterKey":     queryEnvironmentValue,
			"ParameterValue":   queryProductionValue,
			"UsePreviousValue": true,
		}},
		"RollbackConfiguration": map[string]any{
			"MonitoringTimeInMinutes": float64(10),
			"RollbackTriggers": []any{map[string]any{
				"Arn":  "arn:aws:cloudwatch:::alarm/a",
				"Type": "AWS::CloudWatch::Alarm",
			}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded CloudFormation form = %#v, want %#v", got, want)
	}
}

//nolint:wsl // The fixture keeps each CloudFormation wire field visible.
func TestCloudFormationQueryDecoderSupportsDirectIndexedMembers(t *testing.T) {
	t.Parallel()

	form := url.Values{
		queryActionField:                {"CreateStack"},
		queryStackNameField:             {"direct-index-stack"},
		"Parameters.1.ParameterKey":     {queryEnvironmentValue},
		"Parameters.1.ParameterValue":   {"123"},
		"Parameters.1.UsePreviousValue": {"false"},
		"Capabilities.1":                {"CAPABILITY_IAM"},
		"NotificationARNs.1":            {"arn:aws:sns:us-east-1:123456789012:topic"},
		"ResourceTypes.1":               {"AWS::S3::Bucket"},
		"Tags.1.Key":                    {queryEnvironmentValue},
		"Tags.1.Value":                  {queryProductionValue},
	}

	//nolint:tagliatelle // The fixture mirrors CloudFormation's wire casing.
	var got struct {
		StackName  string `json:"StackName"`
		Parameters []struct {
			ParameterKey     string `json:"ParameterKey"`
			ParameterValue   string `json:"ParameterValue"`
			UsePreviousValue bool   `json:"UsePreviousValue"`
		} `json:"Parameters"`
		Capabilities     []string `json:"Capabilities"`
		NotificationARNs []string `json:"NotificationARNs"`
		ResourceTypes    []string `json:"ResourceTypes"`
		Tags             []struct {
			Key   string `json:"Key"`
			Value string `json:"Value"`
		} `json:"Tags"`
	}

	if err := json.Unmarshal(cloudFormationFormToJSON(form), &got); err != nil {
		t.Fatalf("decode CloudFormation form: %v", err)
	}

	if got.StackName != "direct-index-stack" || len(got.Parameters) != 1 || got.Parameters[0].ParameterKey != queryEnvironmentValue || got.Parameters[0].ParameterValue != "123" || got.Parameters[0].UsePreviousValue {
		t.Fatalf("decoded parameters = %#v", got.Parameters)
	}
	if !reflect.DeepEqual(got.Capabilities, []string{"CAPABILITY_IAM"}) {
		t.Fatalf("decoded capabilities = %#v", got.Capabilities)
	}
	if !reflect.DeepEqual(got.NotificationARNs, []string{"arn:aws:sns:us-east-1:123456789012:topic"}) {
		t.Fatalf("decoded notification ARNs = %#v", got.NotificationARNs)
	}
	if !reflect.DeepEqual(got.ResourceTypes, []string{"AWS::S3::Bucket"}) {
		t.Fatalf("decoded resource types = %#v", got.ResourceTypes)
	}
	if len(got.Tags) != 1 || got.Tags[0].Key != queryEnvironmentValue || got.Tags[0].Value != queryProductionValue {
		t.Fatalf("decoded tags = %#v", got.Tags)
	}
}

//nolint:tagliatelle,wsl // The fixture mirrors CloudFormation's wire casing.
func TestCloudFormationQueryDecoderSortsNestedChangeSetParameters(t *testing.T) {
	t.Parallel()

	form := url.Values{
		queryActionField:                       {"CreateChangeSet"},
		queryStackNameField:                    {"query-change-set"},
		"ChangeSetName":                        {"review"},
		"Parameters.member.2.ParameterKey":     {"Zed"},
		"Parameters.member.2.ParameterValue":   {"z"},
		"Parameters.member.1.ParameterKey":     {"Alpha"},
		"Parameters.member.1.ParameterValue":   {"a"},
		"Parameters.member.1.UsePreviousValue": {"false"},
		"Parameters.member.2.UsePreviousValue": {"true"},
	}

	var decoded struct {
		Parameters []struct {
			ParameterKey     string `json:"ParameterKey"`
			ParameterValue   string `json:"ParameterValue"`
			UsePreviousValue bool   `json:"UsePreviousValue"`
		} `json:"Parameters"`
	}
	if err := json.Unmarshal(cloudFormationFormToJSON(form), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Parameters) != 2 || decoded.Parameters[0].ParameterKey != "Alpha" || decoded.Parameters[1].ParameterKey != "Zed" || !decoded.Parameters[1].UsePreviousValue {
		t.Fatalf("decoded change-set parameters = %#v, want indexed order and booleans", decoded.Parameters)
	}
}
