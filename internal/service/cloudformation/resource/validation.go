package resource

import (
	"fmt"
	"slices"
)

func validateProperties(schema Schema, properties map[string]any) error {
	for name := range properties {
		if _, ok := schema.PropertyUpdate[name]; !ok {
			return fmt.Errorf("%s does not declare property %q", schema.TypeName, name)
		}
	}

	return nil
}

func validateNestedProperties(object string, properties map[string]any, allowed ...string) error {
	for name := range properties {
		if !slices.Contains(allowed, name) {
			return fmt.Errorf("%s does not declare property %q", object, name)
		}
	}

	return nil
}
