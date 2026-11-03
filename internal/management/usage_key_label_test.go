package management

import (
	"os"
	"strings"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

func TestUsageViewIncludesKeyName(t *testing.T) {
	view := usageView(store.UsageEntry{PluginKeyID: "key-id", KeyLabel: "Team API"})
	if view["key_label"] != "Team API" || view["plugin_key_id"] != "key-id" {
		t.Fatalf("key identity = %#v", view)
	}
}

func TestUsageRecentKeyColumnFollowsAccount(t *testing.T) {
	raw, err := os.ReadFile("web/console.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)
	if !strings.Contains(js, "<th>账号</th><th>API Key 名称</th><th>模型名称</th>") {
		t.Fatal("API key column must follow account")
	}
	if !strings.Contains(js, "usageAuthCell(u)+'<td>'+esc(u.key_label || u.plugin_key_id || '—')+'</td>'") {
		t.Fatal("API key cell must follow account, escape labels and handle missing names")
	}
}
