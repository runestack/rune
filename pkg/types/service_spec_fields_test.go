package types

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// yamlTagNames returns the yaml key for every field of a struct type,
// skipping fields explicitly excluded with `yaml:"-"`.
func yamlTagNames(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// TestValidServiceFieldsMatchesSpec guards a drift class that shipped a real
// bug: cast validates a service mapping against the hand-maintained
// validServiceFields allowlist, which is separate from the ServiceSpec struct.
// Adding a field to the struct alone is not enough — the field unmarshals fine
// but cast rejects the file with "unknown field '<name>' in service
// specification". That is exactly how imagePullAnonymous shipped in
// v0.0.1-dev.139: every cast file using it failed validation.
//
// Keeping the two in sync is now a test failure rather than a user-facing one.
func TestValidServiceFieldsMatchesSpec(t *testing.T) {
	specFields := yamlTagNames(t, reflect.TypeOf(ServiceSpec{}))
	if len(specFields) == 0 {
		t.Fatal("reflection found no yaml-tagged fields on ServiceSpec")
	}

	// Every struct field must be accepted by the validator.
	for _, f := range specFields {
		if !validServiceFields[f] {
			t.Errorf("ServiceSpec has yaml field %q but validServiceFields does not accept it — "+
				"cast will reject any file using it; add it to validServiceFields", f)
		}
	}

	// And the validator must not advertise fields the struct cannot hold,
	// which would silently accept a key that then does nothing.
	inSpec := make(map[string]bool, len(specFields))
	for _, f := range specFields {
		inSpec[f] = true
	}
	for f := range validServiceFields {
		if !inSpec[f] {
			t.Errorf("validServiceFields accepts %q but ServiceSpec has no such yaml field — "+
				"the key would be silently ignored", f)
		}
	}
}

// TestServiceSpecImagePullAnonymousAccepted is the direct regression for the
// reported failure: a cast file setting imagePullAnonymous must validate.
func TestServiceSpecImagePullAnonymousAccepted(t *testing.T) {
	yamlData := `
name: flo
image: ghcr.io/floruntime/flo:0.1.0-dev.9
imagePull: always
imagePullAnonymous: true
scale: 1
`
	var spec ServiceSpec
	if err := yaml.Unmarshal([]byte(yamlData), &spec); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	// Validate() is what cast runs, and what rejected the field before this fix.
	if err := spec.Validate(); err != nil {
		t.Fatalf("a spec using imagePullAnonymous must validate, got: %v", err)
	}
	if !spec.ImagePullAnonymous {
		t.Error("imagePullAnonymous parsed as false; the value did not reach the spec")
	}
	// And it must survive conversion into the runtime Service.
	svc, err := spec.ToService()
	if err != nil {
		t.Fatalf("ToService error: %v", err)
	}
	if !svc.ImagePullAnonymous {
		t.Error("imagePullAnonymous did not carry through ToService")
	}
}
