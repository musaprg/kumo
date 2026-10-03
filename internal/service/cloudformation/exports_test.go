//nolint:wsl // Export lifecycle assertions keep setup and state checks adjacent.
package cloudformation

import (
	"context"
	"strings"
	"testing"

	"github.com/sivchari/kumo/internal/service/cloudformation/resource"
	"github.com/sivchari/kumo/internal/service/cloudformation/template"
)

const (
	testEmptyResourcesTemplate = "Resources: {}"
	testApplicationURLExport   = "ApplicationURL"
	testExampleURL             = "https://example.test"
	testMutatedValue           = "mutated"
	testSortedAExport          = "AExport"
)

//nolint:wsl // The assertions intentionally inspect pre- and post-commit snapshots.
func TestExportsStageAndCommitAtomically(t *testing.T) {
	storage := NewMemoryStorage()
	exporter := createExportTestStack(t, storage, "exporter")

	stage, err := storage.StageExports(t.Context(), exporter.StackID, []StackExport{{Name: testApplicationURLExport, Value: testExampleURL}})
	if err != nil {
		t.Fatalf("StageExports() error = %v", err)
	}
	if got := len(exporter.Exports); got != 0 {
		t.Fatalf("staging changed exports before commit: %#v", exporter.Exports)
	}
	if err := storage.CommitExports(t.Context(), stage); err != nil {
		t.Fatalf("CommitExports() error = %v", err)
	}

	exports, err := storage.GetExports(t.Context(), exporter.StackID)
	if err != nil {
		t.Fatalf("GetExports() error = %v", err)
	}
	if len(exports) != 1 || exports[0].Name != testApplicationURLExport || exports[0].Value != testExampleURL {
		t.Fatalf("committed exports = %#v", exports)
	}
}

//nolint:wsl // The test exercises both blocked graph mutations in one scenario.
func TestActiveImportersBlockExporterChangesAndDeletion(t *testing.T) {
	storage := NewMemoryStorage()
	exporter := createExportTestStack(t, storage, "exporter")
	importer := createExportTestStack(t, storage, "importer")
	commitExport(t, storage, exporter.StackID)

	stage, err := storage.StageImports(t.Context(), importer.StackID, []StackImport{{ExportName: testApplicationURLExport}})
	if err != nil {
		t.Fatalf("StageImports() error = %v", err)
	}
	if err := storage.CommitImports(t.Context(), stage); err != nil {
		t.Fatalf("CommitImports() error = %v", err)
	}

	changed, err := storage.StageExports(t.Context(), exporter.StackID, []StackExport{{Name: testApplicationURLExport, Value: "https://changed.test"}})
	if err != nil {
		t.Fatalf("StageExports(changed) error = %v", err)
	}
	if err := storage.CommitExports(t.Context(), changed); err == nil || !strings.Contains(err.Error(), "import") {
		t.Fatalf("CommitExports(changed) error = %v, want importer conflict", err)
	}
	if err := storage.DeleteStack(t.Context(), exporter.StackID); err == nil || !strings.Contains(err.Error(), "import") {
		t.Fatalf("DeleteStack(exporter) error = %v, want importer conflict", err)
	}
}

//nolint:wsl // The test verifies each atomic boundary before inspecting state.
func TestExportImportCommitRejectsStaleOrMissingStagesWithoutPartialMutation(t *testing.T) {
	storage := NewMemoryStorage()
	exporter := createExportTestStack(t, storage, "exporter")
	importer := createExportTestStack(t, storage, "importer")
	commitExport(t, storage, exporter.StackID)

	stage, err := storage.StageImports(t.Context(), importer.StackID, []StackImport{{ExportName: testApplicationURLExport}})
	if err != nil {
		t.Fatalf("StageImports() error = %v", err)
	}
	if err := storage.RunStackOperation(t.Context(), importer.StackID, func(_ context.Context, working *Stack) error {
		working.StackStatusReason = "revision bump"

		return nil
	}); err != nil {
		t.Fatalf("RunStackOperation() error = %v", err)
	}
	if err := storage.CommitImports(t.Context(), stage); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("CommitImports(stale) error = %v, want revision conflict", err)
	}
	imports, err := storage.GetImports(t.Context(), importer.StackID)
	if err != nil {
		t.Fatalf("GetImports() error = %v", err)
	}
	if len(imports) != 0 {
		t.Fatalf("stale import commit partially mutated graph: %#v", imports)
	}
}

//nolint:wsl // The test mutates every public stage field before committing.
func TestExportStageUsesImmutableServerSideRecord(t *testing.T) {
	storage := NewMemoryStorage()
	exporter := createExportTestStack(t, storage, "exporter")

	stage, err := storage.StageExports(t.Context(), exporter.StackID, []StackExport{{Name: testApplicationURLExport, Value: testExampleURL}})
	if err != nil {
		t.Fatalf("StageExports() error = %v", err)
	}
	stage.BaseRevision++
	stage.StackARN = "arn:aws:cloudformation:us-east-1:123456789012:stack/other/id"
	stage.Exports[0].Value = "https://mutated.test"
	stage.Exports = append(stage.Exports, StackExport{Name: "Mutated"})

	if err := storage.CommitExports(t.Context(), stage); err != nil {
		t.Fatalf("CommitExports() error = %v", err)
	}
	exports, err := storage.GetExports(t.Context(), exporter.StackID)
	if err != nil {
		t.Fatalf("GetExports() error = %v", err)
	}
	if len(exports) != 1 || exports[0].Value != testExampleURL || exports[0].ExporterStackARN != exporter.StackID {
		t.Fatalf("committed exports = %#v, want immutable staged values", exports)
	}
	if err := storage.CommitExports(t.Context(), stage); err == nil {
		t.Fatal("CommitExports() reused a consumed stage")
	}
}

//nolint:wsl // The test mutates every public import-stage field before committing.
func TestImportStageUsesImmutableServerSideRecord(t *testing.T) {
	storage := NewMemoryStorage()
	exporter := createExportTestStack(t, storage, "exporter")
	importer := createExportTestStack(t, storage, "importer")
	commitExport(t, storage, exporter.StackID)

	stage, err := storage.StageImports(t.Context(), importer.StackID, []StackImport{{ExportName: testApplicationURLExport}})
	if err != nil {
		t.Fatalf("StageImports() error = %v", err)
	}
	stage.BaseRevision++
	stage.StackARN = "arn:aws:cloudformation:us-east-1:123456789012:stack/other/id"
	stage.Imports[0].ExporterStackARN = testMutatedValue
	stage.Imports = append(stage.Imports, StackImport{ExportName: "Mutated"})

	if err := storage.CommitImports(t.Context(), stage); err != nil {
		t.Fatalf("CommitImports() error = %v", err)
	}
	imports, err := storage.GetImports(t.Context(), importer.StackID)
	if err != nil {
		t.Fatalf("GetImports() error = %v", err)
	}
	if len(imports) != 1 || imports[0].ExportName != testApplicationURLExport || imports[0].ExporterStackARN != exporter.StackID || imports[0].ImporterStackARN != importer.StackID {
		t.Fatalf("committed imports = %#v, want immutable staged values", imports)
	}
	if err := storage.CommitImports(t.Context(), stage); err == nil {
		t.Fatal("CommitImports() reused a consumed stage")
	}
}

//nolint:wsl // The test verifies stale export commits leave the graph and revision unchanged.
func TestExportCommitRejectsStaleStageAtomically(t *testing.T) {
	storage := NewMemoryStorage()
	exporter := createExportTestStack(t, storage, "exporter")
	stage, err := storage.StageExports(t.Context(), exporter.StackID, []StackExport{{Name: testApplicationURLExport, Value: testExampleURL}})
	if err != nil {
		t.Fatalf("StageExports() error = %v", err)
	}
	if err := storage.RunStackOperation(t.Context(), exporter.StackID, func(_ context.Context, working *Stack) error {
		working.StackStatusReason = "revision bump"

		return nil
	}); err != nil {
		t.Fatalf("RunStackOperation() error = %v", err)
	}

	if err := storage.CommitExports(t.Context(), stage); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("CommitExports(stale) error = %v, want revision conflict", err)
	}
	exports, err := storage.GetExports(t.Context(), exporter.StackID)
	if err != nil {
		t.Fatalf("GetExports() error = %v", err)
	}
	if len(exports) != 0 {
		t.Fatalf("stale export commit partially mutated graph: %#v", exports)
	}
}

//nolint:wsl // The test intentionally persists, closes, and reloads in sequence.
func TestCommittedExportImportEdgesSurviveReload(t *testing.T) {
	dir := t.TempDir()
	first := NewMemoryStorage(WithDataDir(dir))
	exporter := createExportTestStack(t, first, "exporter")
	importer := createExportTestStack(t, first, "importer")
	commitExport(t, first, exporter.StackID)
	stage, err := first.StageImports(t.Context(), importer.StackID, []StackImport{{ExportName: testApplicationURLExport}})
	if err != nil {
		t.Fatalf("StageImports() error = %v", err)
	}
	if err := first.CommitImports(t.Context(), stage); err != nil {
		t.Fatalf("CommitImports() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	second := NewMemoryStorage(WithDataDir(dir))
	imports, err := second.GetImports(t.Context(), importer.StackID)
	if err != nil {
		t.Fatalf("GetImports(reloaded) error = %v", err)
	}
	if len(imports) != 1 || imports[0].ExporterStackARN != exporter.StackID {
		t.Fatalf("reloaded imports = %#v", imports)
	}
}

func TestLifecycleCompletionCommitsAndReplacesImportEdges(t *testing.T) {
	storage := newExecutorStorage(t)
	registry := resource.NewRegistry()
	registry.Register(&executorAdapter{typeName: executorResourceType})
	executor := NewExecutor(storage, registry)
	exporter := executorCreateStack(t, storage)
	importer, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: "importer-lifecycle", TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatal(err)
	}
	commitExport(t, storage.MemoryStorage, exporter.StackID)

	if err := executor.Create(t.Context(), LifecycleRequest{
		StackRef:   importer.StackID,
		Evaluation: &template.Evaluation{Resources: map[string]template.Resource{}},
		Imports:    []StackImport{{ExportName: testApplicationURLExport}},
		Options:    ExecutionOptions{ClientRequestToken: "import-create"},
	}); err != nil {
		t.Fatalf("Create(importer) error = %v", err)
	}
	imports, err := storage.GetImports(t.Context(), importer.StackID)
	if err != nil || len(imports) != 1 {
		t.Fatalf("committed imports = %#v, error = %v", imports, err)
	}

	if err := executor.Update(t.Context(), LifecycleRequest{
		StackRef:   importer.StackID,
		Evaluation: &template.Evaluation{Resources: map[string]template.Resource{}},
		Options:    ExecutionOptions{ClientRequestToken: "import-update"},
	}); err != nil {
		t.Fatalf("Update(importer) error = %v", err)
	}
	imports, err = storage.GetImports(t.Context(), importer.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if len(imports) != 0 {
		t.Fatalf("removed imports = %#v, want none", imports)
	}
}

//nolint:wsl // The test keeps export staging, deletion, and ordering assertions together.
func TestListExportsReturnsLiveExportsSortedByName(t *testing.T) {
	storage := NewMemoryStorage()
	first := createExportTestStack(t, storage, "first-exporter")
	second := createExportTestStack(t, storage, "second-exporter")
	for _, test := range []struct {
		stack *Stack
		name  string
		value string
	}{
		{stack: first, name: "ZExport", value: "z"},
		{stack: second, name: testSortedAExport, value: "a"},
	} {
		stage, err := storage.StageExports(t.Context(), test.stack.StackID, []StackExport{{Name: test.name, Value: test.value}})
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.CommitExports(t.Context(), stage); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.DeleteStack(t.Context(), first.StackID); err != nil {
		t.Fatal(err)
	}

	exports, err := storage.ListExports(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(exports) != 1 || exports[0].Name != testSortedAExport || exports[0].Value != "a" {
		t.Fatalf("ListExports() = %#v, want only sorted live export", exports)
	}
}

//nolint:wsl // The helper keeps common stack setup concise.
func createExportTestStack(t *testing.T, storage *MemoryStorage, name string) *Stack {
	t.Helper()
	stack, err := storage.CreateStack(t.Context(), &CreateStackRequest{StackName: name, TemplateBody: testEmptyResourcesTemplate})
	if err != nil {
		t.Fatalf("CreateStack(%q) error = %v", name, err)
	}

	return stack
}

//nolint:wsl // The helper keeps stage creation and commit paired for test setup.
func commitExport(t *testing.T, storage *MemoryStorage, stackARN string) {
	t.Helper()
	stage, err := storage.StageExports(t.Context(), stackARN, []StackExport{{Name: testApplicationURLExport, Value: testExampleURL}})
	if err != nil {
		t.Fatalf("StageExports() error = %v", err)
	}
	if err := storage.CommitExports(t.Context(), stage); err != nil {
		t.Fatalf("CommitExports() error = %v", err)
	}
}
