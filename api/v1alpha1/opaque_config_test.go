package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestHarnessOpaqueConfigSurvivesReadModifyWrite(t *testing.T) {
	input := []byte(`{
		"apiVersion":"t3code.janpuc.com/v1alpha1",
		"kind":"Harness",
		"metadata":{"name":"future-driver"},
		"spec":{
			"instanceId":"future_driver",
			"driver":"futureDriver",
			"config":{"future":{"nested":{"enabled":true,"weights":[1,2,3]}}},
			"workstationRefs":[{"name":"primary"}]
		}
	}`)

	var harness Harness
	if err := json.Unmarshal(input, &harness); err != nil {
		t.Fatal(err)
	}
	harness.Spec.DisplayName = "Future driver"

	output, err := json.Marshal(harness)
	if err != nil {
		t.Fatal(err)
	}

	assertNestedJSONEqual(t, input, output, "spec", "config")
}

func TestMCPServerOpaqueConfigSurvivesReadModifyWrite(t *testing.T) {
	input := []byte(`{
		"apiVersion":"t3code.janpuc.com/v1alpha1",
		"kind":"MCPServer",
		"metadata":{"name":"future-transport"},
		"spec":{
			"transport":"futureTransport",
			"config":{"future":{"nested":{"enabled":true,"weights":[1,2,3]}}},
			"harnessRefs":[{"name":"codex"}]
		}
	}`)

	var server MCPServer
	if err := json.Unmarshal(input, &server); err != nil {
		t.Fatal(err)
	}
	server.Spec.Headers = []HTTPHeader{}

	output, err := json.Marshal(server)
	if err != nil {
		t.Fatal(err)
	}

	assertNestedJSONEqual(t, input, output, "spec", "config")
}

func assertNestedJSONEqual(t *testing.T, before, after []byte, path ...string) {
	t.Helper()

	want := nestedJSONValue(t, before, path...)
	got := nestedJSONValue(t, after, path...)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("nested JSON changed: want %#v, got %#v", want, got)
	}
}

func nestedJSONValue(t *testing.T, document []byte, path ...string) any {
	t.Helper()

	var current any
	if err := json.Unmarshal(document, &current); err != nil {
		t.Fatal(err)
	}
	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%q is not an object", segment)
		}
		current, ok = object[segment]
		if !ok {
			t.Fatalf("%q is missing", segment)
		}
	}
	return current
}
