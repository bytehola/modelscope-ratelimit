package main

import (
	"encoding/json"
	"testing"
)

func TestNegotiatePluginSchemaVersion(t *testing.T) {
	cases := []struct {
		host uint32
		want uint32
	}{
		{0, 1},
		{1, 1},
		{3, 3},
		{5, 5},
		{6, 6},
		{10, 6},
	}
	for _, c := range cases {
		if got := negotiatePluginSchemaVersion(c.host); got != c.want {
			t.Fatalf("negotiatePluginSchemaVersion(%d) = %d, want %d", c.host, got, c.want)
		}
	}
}

func TestPluginRegistrationUsesNegotiatedSchemaVersion(t *testing.T) {
	original := activePluginSchemaVersion.Swap(6)
	t.Cleanup(func() { activePluginSchemaVersion.Store(original) })

	raw, errMarshal := json.Marshal(pluginRegistration())
	if errMarshal != nil {
		t.Fatalf("marshal registration: %v", errMarshal)
	}
	var registration struct {
		SchemaVersion uint32 `json:"schema_version"`
	}
	if errUnmarshal := json.Unmarshal(raw, &registration); errUnmarshal != nil {
		t.Fatalf("unmarshal registration: %v", errUnmarshal)
	}
	if registration.SchemaVersion != 6 {
		t.Fatalf("schema_version = %d, want 6", registration.SchemaVersion)
	}
}

func TestDecodePluginLifecycleRequestSchemaVersion(t *testing.T) {
	request, errDecode := decodePluginLifecycleRequest([]byte(`{
		"schema_version": 10,
		"config_yaml": "cHJvdmlkZXJzOgogIC0gbW9kZWxzY29wZQo="
	}`))
	if errDecode != nil {
		t.Fatalf("decode lifecycle request: %v", errDecode)
	}
	if request.SchemaVersion != 10 {
		t.Fatalf("SchemaVersion = %d, want 10", request.SchemaVersion)
	}
	if got := negotiatePluginSchemaVersion(request.SchemaVersion); got != 6 {
		t.Fatalf("negotiated schema = %d, want 6", got)
	}
}
