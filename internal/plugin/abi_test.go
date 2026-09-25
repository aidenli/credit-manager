package plugin

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// The host HTML-escapes every string value of a management JSON response while a
// plugin registers an RPC schema below SchemaVersionRawManagementResponse. That
// turns a question-2 document into "&lt;!DOCTYPE html&gt;", which the console
// preview then renders as a page of source code instead of an animation, so the
// floor is pinned here: building against an older SDK silently reintroduces it.
func TestPluginABIKeepsManagementResponsesRaw(t *testing.T) {
	if pluginabi.SchemaVersion < pluginabi.SchemaVersionRawManagementResponse {
		t.Fatalf("pluginabi.SchemaVersion = %d, but the host escapes management JSON below %d",
			pluginabi.SchemaVersion, pluginabi.SchemaVersionRawManagementResponse)
	}
	// The host negotiates the smaller of the two schema versions, so a host older
	// than this SDK still escapes. The plugin must never be the reason.
	if got := negotiateRPCSchema(pluginabi.SchemaVersion + 10); got != pluginabi.SchemaVersion {
		t.Fatalf("negotiated schema = %d, want %d", got, pluginabi.SchemaVersion)
	}
}
