package management

import (
	"context"
	"testing"

	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

// The store layer cannot know about deletions; the overview must hide accounts
// the host no longer holds.
func TestFilterUsedAuthsByLiveAccountsHidesDeleted(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	src := newQuotaSourceStub(map[string]struct{}{"codex-live@example.com-pro.json": {}})
	svc.SetAuthQuotaSource(src)

	used := []store.UsageAuthSummary{
		{AuthID: "codex-live@example.com-pro.json", AuthIndex: "idx-1", Provider: "codex"},
		{AuthID: "codex-deleted@example.com-pro.json", AuthIndex: "idx-2", Provider: "codex"},
	}
	got, known := filterUsedAuthsByLiveAccounts(ctx, svc, used)
	if !known {
		t.Fatal("live set should have been known")
	}
	if len(got) != 1 || got[0].AuthID != "codex-live@example.com-pro.json" {
		t.Fatalf("filtered = %#v", got)
	}
}

// An entry that only carries a runtime index must still be matched.
func TestFilterUsedAuthsByLiveAccountsMatchesIndexOnlyRows(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)
	svc.SetAuthQuotaSource(newQuotaSourceStub(map[string]struct{}{"runtime-idx": {}}))

	used := []store.UsageAuthSummary{{AuthID: "", AuthIndex: "runtime-idx", Provider: "codex"}}
	got, known := filterUsedAuthsByLiveAccounts(ctx, svc, used)
	if !known || len(got) != 1 {
		t.Fatalf("index-only row should survive, got %#v known=%t", got, known)
	}
}

// If the host is unreachable the list must pass through untouched: a hiccup must
// never hide accounts.
func TestFilterUsedAuthsByLiveAccountsIsBestEffort(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	used := []store.UsageAuthSummary{{AuthID: "codex-any@example.com-pro.json", Provider: "codex"}}

	// No source configured at all.
	got, known := filterUsedAuthsByLiveAccounts(ctx, svc, used)
	if known {
		t.Fatal("no source means the live set is unknown")
	}
	if len(got) != 1 {
		t.Fatalf("unknown live set must not filter, got %#v", got)
	}

	// A source that reports zero accounts is also treated as unknown.
	svc.SetAuthQuotaSource(newQuotaSourceStub(map[string]struct{}{}))
	got, known = filterUsedAuthsByLiveAccounts(ctx, svc, used)
	if known || len(got) != 1 {
		t.Fatalf("empty live set must not filter, got %#v known=%t", got, known)
	}

	// And a failing source too.
	svc.SetAuthQuotaSource(failingQuotaSource{})
	got, known = filterUsedAuthsByLiveAccounts(ctx, svc, used)
	if known || len(got) != 1 {
		t.Fatalf("failing source must not filter, got %#v known=%t", got, known)
	}
}

type quotaSourceStub struct {
	hostIDs map[string]struct{}
	err     error
}

func newQuotaSourceStub(hostIDs map[string]struct{}) *quotaSourceStub {
	return &quotaSourceStub{hostIDs: hostIDs}
}

func (s *quotaSourceStub) ListAuthQuotaFiles(context.Context) ([]service.AuthQuotaFile, error) {
	return nil, nil
}

func (s *quotaSourceStub) GetAuthQuotaJSON(context.Context, string) ([]byte, error) {
	return nil, s.err
}

func (s *quotaSourceStub) DoAuthQuotaHTTP(context.Context, string, service.AuthQuotaHTTPRequest) (service.AuthQuotaHTTPResponse, error) {
	return service.AuthQuotaHTTPResponse{}, s.err
}

func (s *quotaSourceStub) HostAuthIDs(context.Context) (map[string]struct{}, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]struct{}, len(s.hostIDs))
	for id := range s.hostIDs {
		out[id] = struct{}{}
	}
	return out, nil
}

type failingQuotaSource struct{}

func (failingQuotaSource) ListAuthQuotaFiles(context.Context) ([]service.AuthQuotaFile, error) {
	return nil, errQuotaStub
}

func (failingQuotaSource) GetAuthQuotaJSON(context.Context, string) ([]byte, error) {
	return nil, errQuotaStub
}

func (failingQuotaSource) DoAuthQuotaHTTP(context.Context, string, service.AuthQuotaHTTPRequest) (service.AuthQuotaHTTPResponse, error) {
	return service.AuthQuotaHTTPResponse{}, errQuotaStub
}

func (failingQuotaSource) HostAuthIDs(context.Context) (map[string]struct{}, error) {
	return nil, errQuotaStub
}

var errQuotaStub = errQuotaStubValue{}

type errQuotaStubValue struct{}

func (errQuotaStubValue) Error() string { return "host unavailable" }
