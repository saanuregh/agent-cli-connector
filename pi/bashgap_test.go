package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// run feeds chunk groups; each group is one throttle window (one snapshot).
func runGroups(t *testing.T, groups [][]string) (got, want string, gapTotal int64) {
	acc := newPiAccumulator()
	var dec piDecoder
	var w, g strings.Builder
	var prev partialOutput
	for _, grp := range groups {
		for _, c := range grp {
			w.WriteString(dec.decode([]byte(c)))
			acc.append([]byte(c))
		}
		cur := acc.snapshot(t)
		if cur.total > 0 && prev.emitted < cur.total-int64(len(cur.text))-1 {
			gapTotal = cur.total
		}
		g.WriteString(outputDelta(prev, &cur))
		prev = cur
	}
	if prev.open && prev.guess == 1 {
		g.WriteString("\n")
	}
	return g.String(), w.String(), gapTotal
}

func checkAfterGap(t *testing.T, name string, groups [][]string) {
	got, want, gt := runGroups(t, groups)
	tail := want[gt:]
	if !strings.HasSuffix(got, tail) {
		i := 0
		for i < len(tail) && i < len(got) && got[len(got)-1-i] == tail[len(tail)-1-i] {
			i++
		}
		t.Errorf("%s: post-gap output diverges; got len %d want-tail len %d; matching suffix %d; got around: %q want around: %q", name, len(got), len(tail), i,
			got[max(len(got)-i-20, 0):min(len(got)-i+20, len(got))], tail[max(len(tail)-i-20, 0):min(len(tail)-i+20, len(tail))])
	}
}

func TestOutputDeltaPostGapAfterGap(t *testing.T) {
	var big []string
	for i := range 30000 {
		big = append(big, strconv.Itoa(i)+"\n")
	}
	// A: fast dump ending at newline, then slow lines.
	a := [][]string{big}
	for k := range 20 {
		a = append(a, []string{"step " + strconv.Itoa(k) + "\n"})
	}
	checkAfterGap(t, "dump then lines", a)
	// B: fast dump plus a progress line with no newline, then \r updates.
	b := [][]string{append(append([]string(nil), big...), "progress 1%")}
	for k := 2; k < 30; k++ {
		b = append(b, []string{"\rprogress " + strconv.Itoa(k) + "%"})
	}
	b = append(b, []string{"\ndone\n"})
	checkAfterGap(t, "dump then progress", b)
	// C: pytest-style dots.
	c := [][]string{append(append([]string(nil), big...), "tests/test_a.py ")}
	for range 40 {
		c = append(c, []string{"."})
	}
	c = append(c, []string{"\n", "== 40 passed ==\n"})
	checkAfterGap(t, "dump then dots", c)
	// D: dump then prompt-like partial line, then newline-terminated lines.
	d := [][]string{append(append([]string(nil), big...), "Compiling foo")}
	for k := range 10 {
		d = append(d, []string{" v" + strconv.Itoa(k) + "\nCompiling foo"})
	}
	d = append(d, []string{"\n"})
	checkAfterGap(t, "dump then compiling", d)
}

// After a gap, test-runner dots and multi-byte progress keep their bytes,
// and every delta stays valid UTF-8.
func TestOutputDeltaPostGapProgress(t *testing.T) {
	var big []string
	for i := range 30000 {
		big = append(big, strconv.Itoa(i)+"\n")
	}
	dots := [][]string{append(append([]string(nil), big...), "tests/test_a.py ")}
	for range 5 {
		dots = append(dots, []string{"."})
	}
	checkAfterGap(t, "dots", append(dots, []string{"\n", "== 5 passed ==\n"}))

	accents := [][]string{append(append([]string(nil), big...), "état ")}
	for range 5 {
		accents = append(accents, []string{"é"})
	}
	accents = append(accents, []string{"\n", "fin\n"})
	checkAfterGap(t, "accents", accents)
	acc := newPiAccumulator()
	var prev partialOutput
	for _, grp := range accents {
		for _, c := range grp {
			acc.append([]byte(c))
		}
		cur := acc.snapshot(t)
		if d := outputDelta(prev, &cur); !utf8.ValidString(d) {
			t.Errorf("delta %q is not valid UTF-8", d)
		}
		prev = cur
	}
}

// When two placements fit after a gap and differ only in the newest
// snapshot's final newline, that newline waits instead of being guessed.
func TestOutputDeltaPostGapUndecidedNewline(t *testing.T) {
	var big strings.Builder
	for i := range 20000 {
		big.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	checkAfterGap(t, "newline after a burst", [][]string{{big.String() + "Downloading"}, {"\n", "ok"}, {" 1\nnext\n"}, {"tail"}})
	checkAfterGap(t, "after a cut line", [][]string{{"start\n" + pseudoRandom(120000, 5) + "\n"}, {"a\nb\n"}, {"c\nd\n"}, {"e"}})
}

func TestFinalNewlineSources(t *testing.T) {
	file := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(file, []byte("a\nend"), 0o600); err != nil {
		t.Fatal(err)
	}
	open := partialOutput{open: true, guess: 1, fullOutput: file}
	if !finalNewline(open, json.RawMessage(`{"structuredContent":{"output":"x\n"}}`)) {
		t.Error("the tool's output, which ends in a newline, was not used")
	}
	if finalNewline(open, json.RawMessage(`{"content":[]}`)) {
		t.Error("without output, the full output file (ending mid-line) was not used")
	}
	open.fullOutput = filepath.Join(t.TempDir(), "missing")
	if !finalNewline(open, nil) {
		t.Error("without output or file, the guess was not used")
	}
}
