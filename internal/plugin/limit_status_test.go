package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/yuluo688/credit-manager/internal/store"
)

func TestLimitRejectEnvelopeUses429ForConcurrency(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("reserve: %w", store.ErrConcurrentLimit), http.StatusTooManyRequests},
		{errors.New("quota exceeded"), 0},
	}
	for _, tc := range cases {
		var env envelope
		if err := json.Unmarshal(limitRejectEnvelope(tc.err), &env); err != nil {
			t.Fatal(err)
		}
		if env.OK || env.Error == nil || env.Error.Code != "limit_rejected" || env.Error.HTTPStatus != tc.want {
			t.Fatalf("%v: envelope = %+v, want status %d", tc.err, env.Error, tc.want)
		}
	}
}
