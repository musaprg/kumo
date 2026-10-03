package cligen

import (
	"reflect"
	"strings"
	"testing"
)

//nolint:wsl // The test keeps generator input, rendering, and assertions together.
func TestBuildFlagViewPointerBoolPreservesExplicitFalse(t *testing.T) {
	type holder struct {
		DisableRollback *bool
	}
	flag, requiresOverride := classifyField(fieldByName(reflect.TypeOf(holder{}), "DisableRollback"))
	if requiresOverride {
		t.Fatal("pointer bool unexpectedly requires override")
	}

	view, err := buildFlagView(&flag, "update-stack", &importSet{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Assign, `cmd.Flags().Changed("disable-rollback")`) || !strings.Contains(view.Assign, "input.DisableRollback = aws.Bool(disableRollback)") {
		t.Fatalf("pointer bool assignment = %q, want flag presence check and false-preserving assignment", view.Assign)
	}
}
