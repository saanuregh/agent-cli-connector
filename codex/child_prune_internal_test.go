package codex

import (
	"context"
	"fmt"
	"testing"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/fakeproc"
)

// A long session forgets the oldest ended subagents, but never a running
// one, so its per-subagent state stays bounded.
func TestEndedSubagentsAreForgotten(t *testing.T) {
	spawner, _ := fakeproc.Spawner(t, func(h *fakeproc.Harness) {
		if !rStart(h) {
			return
		}
		h.WaitClosed()
	})
	s, err := New(Options{Spawner: spawner}).StartSession(context.Background(), agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go func() {
		for range s.Events() {
		}
	}()
	tr := s.tr
	tr.addChild("running", "spawn")
	tr.updateTask("running", agent.EventMeta{}, func(task *agent.Task) { task.Status = agent.TaskRunning })
	for i := range 3 * maxChildren {
		thread := fmt.Sprintf("ended-%d", i)
		tr.addChild(thread, "spawn")
		tr.updateTask(thread, agent.EventMeta{}, func(task *agent.Task) { task.Status = agent.TaskCompleted })
	}
	last := fmt.Sprintf("ended-%d", 3*maxChildren-1)
	s.mu.Lock()
	children, tasks, order := len(tr.children), len(tr.tasks), len(tr.childOrder)
	_, keptRunning := tr.tasks["running"]
	_, keptLast := tr.children[last]
	_, keptFirst := tr.children["ended-0"]
	s.mu.Unlock()
	if children > maxChildren || tasks > maxChildren || order > maxChildren {
		t.Errorf("children %d, tasks %d, order %d; want at most %d", children, tasks, order, maxChildren)
	}
	if !keptRunning || !keptLast || keptFirst {
		t.Errorf("kept running %v, newest %v, oldest %v", keptRunning, keptLast, keptFirst)
	}
	s.client.mu.Lock()
	routed := len(s.client.children)
	s.client.mu.Unlock()
	if routed > maxChildren {
		t.Errorf("client routes %d child threads", routed)
	}
}
