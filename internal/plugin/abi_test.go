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
	// The RPC contract is extended locally without upgrading the SDK.
	if got := negotiateRPCSchema(10); got < pluginabi.SchemaVersionRawManagementResponse {
		t.Fatalf("negotiated schema = %d, want at least %d", got, pluginabi.SchemaVersionRawManagementResponse)
	}
}
