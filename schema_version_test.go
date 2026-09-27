package main

import "testing"

// The host HTML-escapes every string in a plugin's management JSON unless the
// plugin registers at schema_version >= 6 (SchemaVersionRawManagementResponse).
// Version 1 was the cause of the "&#34;" wall the user kept reporting.
func TestRegistersRawManagementSchemaVersion(t *testing.T) {
	got := registerResponse().SchemaVersion
	if got < 6 {
		t.Fatalf("schema_version = %d, want >= 6 so the host preserves raw JSON; "+
			"below 6 CPA runs html.EscapeString over every string in our responses", got)
	}
	t.Logf("schema_version = %d (raw management responses)", got)
}
