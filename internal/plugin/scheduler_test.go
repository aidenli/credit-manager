package plugin

import (
	"testing"

	"github.com/yuluo688/credit-manager/internal/service"
	"github.com/yuluo688/credit-manager/internal/store"
)

func TestAuthPickErrorCodeSeparatesBoundFailure(t *testing.T) {
	if got := authPickErrorCode(service.ErrNoBoundAuthAvailable); got != "bound_auth_unavailable" {
		t.Fatalf("bound error code = %q", got)
	}
	if got := authPickErrorCode(store.ErrConcurrentLimit); got != "limit_rejected" {
		t.Fatalf("limit error code = %q", got)
	}
}
