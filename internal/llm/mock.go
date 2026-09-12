package llm

import (
	"context"
	"fmt"
	"sync"
)

// Mock is a scripted Client for tests and for running the control plane with
// no model configured.
type Mock struct {
	mu sync.Mutex
	// Responses are returned in order; the last one repeats once exhausted.
	Responses []Response
	// Err, when set, is returned instead of a response.
	Err error
	// Requests records what the caller asked for.
	Requests []Request
	// ModelName is reported by Model.
	ModelName string
	calls     int
}

// NewMock returns a mock that replays the given responses.
func NewMock(responses ...Response) *Mock {
	return &Mock{Responses: responses, ModelName: "mock"}
}

// Complete implements Client.
func (m *Mock) Complete(_ context.Context, req Request) (*Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Requests = append(m.Requests, req)
	if m.Err != nil {
		return nil, m.Err
	}
	if len(m.Responses) == 0 {
		return nil, fmt.Errorf("llm: mock has no scripted response for call %d", m.calls+1)
	}

	idx := min(m.calls, len(m.Responses)-1)
	m.calls++
	resp := m.Responses[idx]
	return &resp, nil
}

// Model implements Client.
func (m *Mock) Model() string {
	if m.ModelName == "" {
		return "mock"
	}
	return m.ModelName
}

// Calls reports how many completions were requested.
func (m *Mock) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// LastRequest returns the most recent request, or the zero value.
func (m *Mock) LastRequest() Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.Requests) == 0 {
		return Request{}
	}
	return m.Requests[len(m.Requests)-1]
}

var _ Client = (*Mock)(nil)
