package management

import (
	"context"
	"testing"
)

// Read endpoints must not be cacheable.
//
// The console renders these payloads (keys, usage, the overview breakouts)
// straight into the dashboard. While they were cacheable the browser served a
// stale response after an upgrade, so newly added fields looked absent and the
// account filter silently did nothing (2026-09-20).
//
// POST mutation endpoints intentionally keep the plain jsonOK helper; only the
// read paths are asserted here.
func TestReadEndpointsDisableCaching(t *testing.T) {
	ctx := context.Background()
	svc := bindingsTestService(t)

	cases := []struct {
		name        string
		cacheHeader func() string
	}{
		{"overview", func() string {
			resp, err := getOverview(ctx, svc, nil)
			if err != nil {
				t.Fatal(err)
			}
			return resp.Headers.Get("Cache-Control")
		}},
		{"usage", func() string {
			resp, err := listUsage(ctx, svc, nil)
			if err != nil {
				t.Fatal(err)
			}
			return resp.Headers.Get("Cache-Control")
		}},
		{"usage/summary", func() string {
			resp, err := usageSummary(ctx, svc, nil)
			if err != nil {
				t.Fatal(err)
			}
			return resp.Headers.Get("Cache-Control")
		}},
		{"audit", func() string {
			resp, err := listAudit(ctx, svc, nil)
			if err != nil {
				t.Fatal(err)
			}
			return resp.Headers.Get("Cache-Control")
		}},
		{"keys", func() string {
			resp, err := listKeys(ctx, svc, nil)
			if err != nil {
				t.Fatal(err)
			}
			return resp.Headers.Get("Cache-Control")
		}},
		{"callers", func() string {
			resp, err := listCallers(ctx, svc, nil)
			if err != nil {
				t.Fatal(err)
			}
			return resp.Headers.Get("Cache-Control")
		}},
		{"pricing", func() string {
			resp, err := listPricing(ctx, svc)
			if err != nil {
				t.Fatal(err)
			}
			return resp.Headers.Get("Cache-Control")
		}},
		{"auth-quotas", func() string {
			resp, err := getAuthQuotas(ctx, svc, nil)
			if err != nil {
				t.Fatal(err)
			}
			return resp.Headers.Get("Cache-Control")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cacheHeader(); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want \"no-store\"", got)
			}
		})
	}
}
