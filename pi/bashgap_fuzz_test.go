package pi

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fuzz below groups chunks into Pi's 100 ms update windows, so gaps
// occur, and checks the joined deltas against where each snapshot really
// sits in the output. Only a newline at a gap boundary may differ: the gap
// lost the information that would decide it.

// snapD is snapshot plus the true count of final newlines after its text.
// fullOutput, when set, names the file Pi writes the whole output to.
func (a *piAccumulator) snapD(t *testing.T, fullOutput string) (partialOutput, int64) {
	text := a.tail
	if !a.tailAtBoundary {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	content, partial := piTruncateTail(text)
	trunc := len(piSplitLines(text)) > piMaxLines || len(text) > piMaxBytes
	d := int64(0)
	if trunc && strings.HasSuffix(text, "\n") {
		d = 1
	}
	lines := a.completed
	if a.open {
		lines++
	}
	r := map[string]any{"content": []any{map[string]any{"type": "text", "text": content}}}
	if lines > piMaxLines || a.total > piMaxBytes {
		r["details"] = map[string]any{"truncation": map[string]any{"truncated": true, "totalBytes": a.total, "lastLinePartial": partial},
			"fullOutputPath": fullOutput}
	}
	raw, _ := json.Marshal(r)
	return newPartialOutput(raw), d
}

type throttledResult struct {
	got, oracle string
	gaps        int
	segs        []string
}

func throttledRun(t *testing.T, groups [][]string) throttledResult {
	acc := newPiAccumulator()
	var dec piDecoder
	var w strings.Builder
	var g strings.Builder
	var o strings.Builder
	var prev partialOutput
	oEmitted := 0
	gaps := 0
	var segs []string
	// Pi writes the whole output to a file once it truncates; the tool's
	// end settles a final newline left open from it.
	full := filepath.Join(t.TempDir(), "output")
	for _, grp := range groups {
		for _, c := range grp {
			w.WriteString(dec.decode([]byte(c)))
			acc.append([]byte(c))
		}
		if err := os.WriteFile(full, []byte(w.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		cur, d := acc.snapD(t, full)
		ws := w.String()
		if cur.total == 0 {
			o.WriteString(ws[oEmitted:])
			oEmitted = len(ws)
		} else {
			s := int(cur.total-d) - len(cur.text)
			if ws[s:s+len(cur.text)] != cur.text {
				t.Fatalf("bad oracle placement")
			}
			if s > oEmitted {
				gaps++
				oEmitted = s
				segs = append(segs, o.String())
				o.Reset()
			}
			o.WriteString(ws[oEmitted:cur.total])
			oEmitted = int(cur.total)
		}
		g.WriteString(outputDelta(prev, &cur))
		prev = cur
	}
	if prev.open && finalNewline(prev, nil) {
		g.WriteString("\n")
	}
	segs = append(segs, o.String())
	return throttledResult{g.String(), strings.Join(segs, ""), gaps, segs}
}

func matchesAcrossGaps(got string, segs []string) bool {
	pos := map[int]bool{0: true}
	for i, sg := range segs {
		next := map[int]bool{}
		for p := range pos {
			var vars []string
			if i == len(segs)-1 {
				vars = []string{sg}
			} else {
				vars = []string{sg, sg + "\n"}
				if strings.HasSuffix(sg, "\n") {
					vars = append(vars, sg[:len(sg)-1])
				}
			}
			for _, v := range vars {
				if strings.HasPrefix(got[p:], v) {
					next[p+len(v)] = true
				}
			}
		}
		pos = next
	}
	return pos[len(got)]
}

func fuzzChunksNoRuns(r *rand.Rand) []string {
	var ch []string
	for range 20 + r.Intn(200) {
		switch r.Intn(9) {
		case 0, 1, 5:
			b := make([]byte, 1+r.Intn(30000))
			for i := range b {
				b[i] = byte('a' + r.Intn(26))
				if r.Intn(30) == 0 {
					b[i] = '\n'
				}
			}
			ch = append(ch, string(b))
		case 2:
			b := make([]byte, 1+r.Intn(20000))
			for i := range b {
				b[i] = byte('a' + r.Intn(3))
				if r.Intn(40) == 0 {
					b[i] = '\n'
				}
			}
			ch = append(ch, string(b))
		case 3:
			ch = append(ch, "é€"[:1+r.Intn(4)], "€é"[r.Intn(3):])
		case 4:
			var b strings.Builder
			for i := range 1 + r.Intn(2000) {
				b.WriteString("ln" + strconv.Itoa(i) + "\n")
			}
			ch = append(ch, b.String())
		case 6:
			ch = append(ch, "\n")
		case 7:
			ch = append(ch, "x\r", "y\n")
		default:
			ch = append(ch, strings.Repeat("q", r.Intn(5))+"\n"+strings.Repeat("z", r.Intn(5)))
		}
	}
	return ch
}

func TestOutputDeltaThrottledFuzz(t *testing.T) {
	seeds := 40
	if testing.Short() {
		seeds = 5
	}
	for seed := int64(0); seed < int64(seeds); seed++ {
		r := rand.New(rand.NewSource(seed))
		ch := fuzzChunksNoRuns(r)
		var groups [][]string
		for i := 0; i < len(ch); {
			k := 1 + r.Intn(6)
			groups = append(groups, ch[i:min(i+k, len(ch))])
			i += k
		}
		if res := throttledRun(t, groups); !matchesAcrossGaps(res.got, res.segs) {
			t.Errorf("seed %d (%d gaps): joined deltas differ beyond gap boundaries", seed, res.gaps)
		}
	}
}
