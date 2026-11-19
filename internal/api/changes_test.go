package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dabbers/devex/internal/vm"
)

// TestAFullHostIsNotTheCallersMistake: opening one change too many on a host
// with no room left is refused with 503, so the panel can say "finish or
// abandon one" rather than reporting a malformed request.
func TestAFullHostIsNotTheCallersMistake(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeError(recorder, fmt.Errorf("install: no room for another change to forms: %w", vm.ErrNoCapacity))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("a full host answered %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}
