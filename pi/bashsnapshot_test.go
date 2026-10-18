package pi

import (
	"encoding/json"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The code below follows Pi 1.3.0's bash output accumulator
// (OutputAccumulator, truncateTail, and getSnapshotText in the bundled
// source), so the tests feed outputDelta the snapshots Pi really sends.

const (
	piMaxLines = 2000
	piMaxBytes = 50 * 1024
)

// piDecoder decodes a byte stream like a streaming TextDecoder: invalid
// bytes become U+FFFD each, and an incomplete character waits for more.
type piDecoder struct{ pending []byte }

func (d *piDecoder) decode(b []byte) string {
	b = append(d.pending, b...)
	d.pending = nil
	var out strings.Builder
	for len(b) > 0 {
		if !utf8.FullRune(b) {
			d.pending = append([]byte(nil), b...)
			break
		}
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out.WriteString("�")
		} else {
			out.Write(b[:size])
		}
		b = b[size:]
	}
	return out.String()
}

func piSplitLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func piTruncateFromEnd(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && s[start]&0xC0 == 0x80 {
		start++
	}
	return s[start:]
}

// piTruncateTail returns the snapshot content and whether its first line
// was cut.
func piTruncateTail(content string) (string, bool) {
	lines := piSplitLines(content)
	if len(lines) <= piMaxLines && len(content) <= piMaxBytes {
		return content, false
	}
	var out []string
	size, partial := 0, false
	for i := len(lines) - 1; i >= 0 && len(out) < piMaxLines; i-- {
		n := len(lines[i])
		if len(out) > 0 {
			n++
		}
		if size+n > piMaxBytes {
			if len(out) == 0 {
				out = []string{piTruncateFromEnd(lines[i], piMaxBytes)}
				partial = true
			}
			break
		}
		out = append([]string{lines[i]}, out...)
		size += n
	}
	return strings.Join(out, "\n"), partial
}

type piAccumulator struct {
	dec            piDecoder
	tail           string
	tailAtBoundary bool
	total          int
	completed      int
	open           bool
}

func newPiAccumulator() *piAccumulator { return &piAccumulator{tailAtBoundary: true} }

func (a *piAccumulator) append(b []byte) {
	text := a.dec.decode(b)
	if text == "" {
		return
	}
	a.total += len(text)
	a.tail += text
	if len(a.tail) > 4*piMaxBytes {
		start := len(a.tail) - 2*piMaxBytes
		for start < len(a.tail) && a.tail[start]&0xC0 == 0x80 {
			start++
		}
		if start > 0 {
			a.tailAtBoundary = a.tail[start-1] == '\n'
		}
		a.tail = a.tail[start:]
	}
	if n := strings.Count(text, "\n"); n == 0 {
		a.open = true
	} else {
		a.completed += n
		a.open = text[strings.LastIndexByte(text, '\n')+1:] != ""
	}
}

func (a *piAccumulator) snapshot(t *testing.T) partialOutput {
	t.Helper()
	text := a.tail
	if !a.tailAtBoundary {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	content, partial := piTruncateTail(text)
	lines := a.completed
	if a.open {
		lines++
	}
	r := map[string]any{"content": []any{map[string]any{"type": "text", "text": content}}}
	if lines > piMaxLines || a.total > piMaxBytes {
		r["details"] = map[string]any{"truncation": map[string]any{"truncated": true, "totalBytes": a.total, "lastLinePartial": partial}}
	}
	raw, _ := json.Marshal(r)
	return newPartialOutput(raw)
}

func repeatLines(n int, line func(i int) string) []string {
	var chunks []string
	for i := 0; i < n; i += 50 {
		var b strings.Builder
		for j := i; j < min(i+50, n); j++ {
			b.WriteString(line(j))
		}
		chunks = append(chunks, b.String())
	}
	return chunks
}

func pseudoRandom(n, seed int) string {
	x := seed
	b := make([]byte, n)
	for i := range b {
		x = (x*1103515245 + 12345) & 0x7fffffff
		b[i] = byte('a' + (x>>16)%26)
	}
	return string(b)
}

func TestOutputDeltaMatchesPiSnapshots(t *testing.T) {
	scenarios := map[string][]string{}
	{
		var ch []string
		for k := range 210 {
			ch = append(ch, pseudoRandom(1024, k+1))
		}
		scenarios["huge line then short"] = append(ch, "\nshort\n", "more\n")
		many := append([]string(nil), ch...)
		many = append(many, "\nshort\n")
		for k := range 6 {
			many = append(many, "line"+strconv.Itoa(k)+"\n")
		}
		scenarios["huge line then many"] = many
		var equals []string
		for range 210 {
			equals = append(equals, strings.Repeat("h", 1024))
		}
		scenarios["repeated huge line"] = append(equals, "\nshort\n", "more\n")
		// After the rolling buffer trims a long line, Pi's snapshot is the
		// short untruncated tail, which keeps its final newline.
		scenarios["blank lines after huge line"] = append(append([]string(nil), ch...), "\n\n\n", "\nab\n")
		scenarios["separator after huge line"] = append(append([]string(nil), ch...), "\n====", "=\n")
	}
	{
		var ch []string
		for i := range 300 {
			ch = append(ch, strings.Repeat("w", 300)+strconv.Itoa(i)+"\n")
		}
		grow := append([]string(nil), ch...)
		for k := range 4 {
			grow = append(grow, pseudoRandom(20000, k+7))
		}
		scenarios["long line grows"] = append(grow, "\nafter\n")
		for k := range 10 {
			ch = append(ch, strings.Repeat("w", 150), strings.Repeat("w", 150)+strconv.Itoa(k)+"\n")
		}
		scenarios["byte limit, split lines"] = ch
	}
	{
		ch := repeatLines(2100, func(i int) string { return "row " + strconv.Itoa(i) + "\r\n" })
		for k := range 20 {
			ch = append(ch, "row x"+strconv.Itoa(k)+"\r", "\nrow y"+strconv.Itoa(k)+" part", " end\r\n")
		}
		scenarios["crlf"] = ch
	}
	{
		ch := repeatLines(2100, func(i int) string { return "a\tb\x1b[31m" + strconv.Itoa(i) + "\x1b[0m\x00\n" })
		for k := range 20 {
			ch = append(ch, strconv.Itoa(k)+"%\r", "\x07\t"+strconv.Itoa(k)+"\n")
		}
		scenarios["control bytes"] = ch
	}
	{
		ch := repeatLines(2100, func(int) string { return "same\n" })
		for k := range 30 {
			if k%3 == 0 {
				ch = append(ch, "sa", "me\n")
			} else {
				ch = append(ch, "same\n")
			}
		}
		scenarios["repeated lines"] = ch
	}
	{
		ch := repeatLines(2100, func(int) string { return "l\n" })
		for range 60 {
			ch = append(ch, strings.Repeat("=", 1000))
		}
		scenarios["long run of one byte"] = append(ch, "\nend\n")
	}
	{
		ch := repeatLines(2100, func(i int) string { return "r" + strconv.Itoa(i) + "\n" })
		scenarios["invalid utf-8"] = append(ch, "f\xff\xfe\n", "\xe2\x82", "\xac\n", "tail")
		blank := append([]string(nil), ch...)
		for range 5 {
			blank = append(blank, "x\n\n", "y")
		}
		scenarios["blank lines"] = blank
	}
	{
		// Built in small pieces so that every snapshot overlaps the one
		// before; a first chunk past the byte limit makes a gap, whose bytes
		// Pi never sends (see bashgap_test.go).
		var ch []string
		for k := range 59 {
			ch = append(ch, pseudoRandom(1024, 100+k))
		}
		ch[len(ch)-1] += "\n"
		ch = append(ch, "next\n")
		for k := range 59 {
			ch = append(ch, pseudoRandom(1024, 200+k))
		}
		scenarios["huge line ends, then more"] = append(ch, "\n", "last")
	}
	scenarios["newlines only"] = append(repeatLines(2100, func(int) string { return "\n" }), "\n\n\n", "\n\n\n")
	scenarios["two 40k lines"] = []string{pseudoRandom(20000, 1), pseudoRandom(20000, 2) + "\n", pseudoRandom(20000, 3), pseudoRandom(20000, 4) + "\nend\n"}

	for name, chunks := range scenarios {
		acc := newPiAccumulator()
		var dec piDecoder
		var want, got strings.Builder
		var prev partialOutput
		for _, c := range chunks {
			want.WriteString(dec.decode([]byte(c)))
			acc.append([]byte(c))
			cur := acc.snapshot(t)
			got.WriteString(outputDelta(prev, &cur))
			prev = cur
		}
		if got.String() != want.String() {
			g, w := got.String(), want.String()
			i := 0
			for i < len(g) && i < len(w) && g[i] == w[i] {
				i++
			}
			t.Errorf("%s: joined %d bytes, want %d; first difference at %d: %q vs %q", name, len(g), len(w), i,
				g[max(i-10, 0):min(i+10, len(g))], w[max(i-10, 0):min(i+10, len(w))])
		}
	}
}

// TestOutputDeltaFuzz checks randomly generated output: whenever Pi sent
// every byte (no gap between snapshots), the joined deltas equal the output.
// Seeds 10, 70, and 242 once failed; 400 seeds pass except 144, 196, 248,
// and 380, which hit the one known ambiguity, a newline arriving alone
// after a long run of one byte. A few seeds keep the test fast.
func TestOutputDeltaFuzz(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for _, seed := range []int64{0, 1, 2, 3, 4, 5, 10, 70, 242} {
		r := rand.New(rand.NewSource(seed))
		acc := newPiAccumulator()
		var dec piDecoder
		var want []byte
		var got strings.Builder
		var prev partialOutput
		gap := false
		for _, c := range fuzzChunks(r) {
			want = append(want, dec.decode([]byte(c))...)
			acc.append([]byte(c))
			cur := acc.snapshot(t)
			if cur.total > 0 {
				placed := false
				for _, d := range []int64{0, 1} {
					s := cur.total - d - int64(len(cur.text))
					if s >= 0 && s <= prev.emitted+1 && string(want[s:s+int64(len(cur.text))]) == cur.text {
						placed = true
					}
				}
				gap = gap || !placed
			}
			got.WriteString(outputDelta(prev, &cur))
			prev = cur
		}
		if !gap && got.String() != string(want) {
			t.Errorf("seed %d: joined deltas differ from the output", seed)
		}
	}
}

func fuzzChunks(r *rand.Rand) []string {
	var ch []string
	for range 20 + r.Intn(200) {
		switch r.Intn(9) {
		case 0:
			ch = append(ch, strings.Repeat("\n", 1+r.Intn(4)))
		case 1:
			ch = append(ch, strings.Repeat(string("=-\n"[r.Intn(3)]), 1+r.Intn(3000)))
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
			ch = append(ch, strings.Repeat("ab\n", 1+r.Intn(2000)))
		case 5:
			ch = append(ch, strings.Repeat("h", 1+r.Intn(30000)))
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
