package workspace

import "testing"

// TestAServiceIsToldToListenOnlyWhereTheProxyReachesIt: on the local driver
// every interface includes the internet's, and an app reached around the edge
// believes any identity it is handed.
func TestAServiceIsToldToListenOnlyWhereTheProxyReachesIt(t *testing.T) {
	t.Parallel()
	shared := map[string]string{"A": "b"}
	for address, want := range map[string]string{"127.0.0.1": "127.0.0.1", "::1": "::1", "10.0.0.5": ""} {
		if got := withListen(shared, address, 41000); got["HOST"] != want || got["PORT"] != "41000" {
			t.Errorf("%s: HOST=%q PORT=%q, want HOST=%q", address, got["HOST"], got["PORT"], want)
		}
	}
	if len(shared) != 1 {
		t.Error("the shared environment was changed")
	}
}
