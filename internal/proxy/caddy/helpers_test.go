package caddy

import (
	"io"
	"net/http"
)

// readAll drains a request body for assertions.
func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}
