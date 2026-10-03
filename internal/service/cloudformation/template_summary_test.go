package cloudformation

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

//nolint:wsl // The test keeps capability and deterministic XML assertions together.
func TestGetTemplateSummaryDetectsCapabilitiesAndSortsXML(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	templateBody := `{"Description":"summary","Parameters":{"Zed":{"Type":"String"},"Alpha":{"Type":"String","Default":"a"}},"Resources":{"Bucket":{"Type":"AWS::S3::Bucket"},"Role":{"Type":"AWS::IAM::Role","Properties":{"RoleName":"named-role"}}}}`
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("GetTemplateSummary", `{"TemplateBody":`+strconv.Quote(templateBody)+`}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetTemplateSummary() status/body = %d/%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "CAPABILITY_NAMED_IAM") || !strings.Contains(body, "AWS::S3::Bucket") || !strings.Contains(body, "AWS::IAM::Role") {
		t.Fatalf("template summary XML = %s, missing capabilities/resource types", body)
	}
	if strings.Index(body, "ParameterKey>Alpha") > strings.Index(body, "ParameterKey>Zed") {
		t.Fatalf("template summary parameters are not sorted: %s", body)
	}
}

//nolint:wsl // The assertion table keeps all required summary fields together.
func TestGetTemplateSummaryReportsTransformAndParameterConstraints(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	templateBody := `{"Transform":"AWS::Serverless-2016-10-31","Parameters":{"Zed":{"Type":"Number","Description":"number description","ConstraintDescription":"must be bounded","MinValue":1,"MaxValue":10},"Alpha":{"Type":"String","Description":"string description","AllowedValues":["a","b"],"AllowedPattern":"^[a-z]$","MinLength":1,"MaxLength":2}},"Resources":{}}`
	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("GetTemplateSummary", `{"TemplateBody":`+strconv.Quote(templateBody)+`}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetTemplateSummary() status/body = %d/%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, field := range []string{"CAPABILITY_AUTO_EXPAND", "string description", "number description", "ConstraintDescription", "AllowedValues", "AllowedPattern", "MinLength", "MaxLength", "MinValue", "MaxValue"} {
		if !strings.Contains(body, field) {
			t.Fatalf("template summary XML = %s, missing %q", body, field)
		}
	}
	if strings.Index(body, "ParameterKey>Alpha") > strings.Index(body, "ParameterKey>Zed") {
		t.Fatalf("template summary parameters are not sorted: %s", body)
	}
}

//nolint:wsl // The test keeps stack lookup and unsupported-input assertions together.
func TestGetTemplateSummaryReadsStackAndRejectsUnsupportedInputs(t *testing.T) {
	storage := NewMemoryStorage()
	svc := New(storage)
	createStackForChangeSet(t, svc, "summary-stack", `{"Resources":{}}`)

	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, jsonRequest("GetTemplateSummary", `{"StackName":"summary-stack"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetTemplateSummary(StackName) status/body = %d/%s", rec.Code, rec.Body.String())
	}

	for _, field := range []string{"TemplateURL", "StackSetName"} {
		rec = httptest.NewRecorder()
		svc.DispatchAction(rec, jsonRequest("GetTemplateSummary", `{"`+field+`":"unsupported"}`))
		if rec.Code == http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), "unsupported") {
			t.Fatalf("GetTemplateSummary(%s) status/body = %d/%s, want explicit rejection", field, rec.Code, rec.Body.String())
		}
	}
}
