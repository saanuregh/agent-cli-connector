//go:build live

package pi_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/internal/livetest"
	"github.com/saanuregh/agent-cli-connector/pi"
)

// runBashScript has the model run script with bash and returns the joined
// ToolOutputDelta text, the script's real output, and the deltas.
func runBashScript(t *testing.T, script string) (got, want string, deltas []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cfg := liveConfig(t)
	cfg.PermissionMode = agent.PermissionBypass
	path := filepath.Join(cfg.Cwd, "gen.sh")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	sess, err := pi.New(pi.Options{}).StartSession(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	rec := &livetest.Recorder{}
	go rec.Drain(sess)
	res, err := sess.Run(ctx, agent.Text("Run exactly this bash command once and nothing else: `bash gen.sh`. Then reply with only the word done."))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, ev := range rec.All() {
		if d, ok := ev.(*agent.ToolOutputDelta); ok {
			b.WriteString(d.Delta)
			deltas = append(deltas, d.Delta)
		}
	}
	if len(deltas) == 0 {
		t.Skipf("the model did not run the command (%s)", res.Status)
	}
	return b.String(), string(out), deltas
}

// TestLiveBigBashOutputDeltas checks that the streamed output of a command
// long enough for Pi to truncate its snapshots joins up to the real output.
func TestLiveBigBashOutputDeltas(t *testing.T) {
	got, want, deltas := runBashScript(t, `for i in $(seq 1 6000); do
  if [ $((i % 400)) = 0 ]; then printf "part-a %d " "$i"; sleep 0.3; printf "part-b\n"; else echo "line $i"; fi
  if [ $((i % 250)) = 0 ]; then sleep 0.2; fi
done
`)
	// Pi sends a final snapshot when output is pending, so the deltas
	// cover all of it.
	if got != want {
		t.Errorf("joined %d deltas (%d bytes) differ from the %d-byte output", len(deltas), len(got), len(want))
	}
}

// TestLiveBashOutputAfterGap checks the output that follows a burst Pi
// cannot send in full (more than its limit within one 100 ms update): the
// burst's bytes are lost, but everything after it streams exactly.
func TestLiveBashOutputAfterGap(t *testing.T) {
	got, _, deltas := runBashScript(t, `seq 1 40000; printf "waiting é"
for i in 1 2 3 4 5; do sleep 0.3; printf "é"; done
echo; echo done
`)
	if tail := "\nwaiting éééééé\ndone\n"; !strings.HasSuffix(got, tail) {
		t.Errorf("streamed output ends %q, want %q", got[max(len(got)-len(tail)-10, 0):], tail)
	}
	for _, d := range deltas {
		if !utf8.ValidString(d) {
			t.Errorf("delta %q is not valid UTF-8", d)
		}
	}
}

// TestLiveBashBurstEndsMidLine checks that a command whose last output is a
// burst ending without a newline streams no newline it did not print.
func TestLiveBashBurstEndsMidLine(t *testing.T) {
	got, _, _ := runBashScript(t, "seq 1 40000; printf end\n")
	if !strings.HasSuffix(got, "\n40000\nend") {
		t.Errorf("streamed output ends %q, want it to end with %q", got[max(len(got)-20, 0):], "\n40000\nend")
	}
}
