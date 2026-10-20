package providerkit

import (
	"context"
	"fmt"
	"sync"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
)

// Finish withdraws the pending requests while others may resolve at the
// same moment; resolving must not change the list Finish walks.
func TestFinishWhileRequestsResolve(t *testing.T) {
	for range 500 {
		rt := NewRuntime("test", agent.Config{})
		var reqs []*agent.PermissionRequest
		for i := range 4 {
			req := NewPermissionRequest(agent.PermissionRequest{ID: fmt.Sprint(i)}, func(context.Context, agent.PermissionResponse) error { return nil })
			rt.Request(req)
			reqs = append(reqs, req)
		}
		var wg sync.WaitGroup
		for _, req := range reqs[:2] {
			wg.Go(func() { _ = req.Deny(context.Background(), "no") })
		}
		rt.Finish(nil)
		wg.Wait()
	}
}
