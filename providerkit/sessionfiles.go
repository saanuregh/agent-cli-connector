package providerkit

import (
	"context"
	"slices"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// SessionFile is a persisted session stored as one file.
type SessionFile struct {
	Path    string
	ModTime time.Time
}

// PageSessionFiles summarizes files newest first, for a harness that
// stores one session per file. It sorts files in place by ModTime.
// summarize returns false for a file to leave out, such as one that is
// not a session or belongs to another directory; only kept sessions count
// toward offset and limit. Limit zero means no limit. A zero UpdatedAt
// defaults to the file's ModTime. When ctx ends, PageSessionFiles returns
// the sessions summarized so far with ctx's error.
func PageSessionFiles(ctx context.Context, files []SessionFile, limit, offset int, summarize func(path string) (agent.SessionSummary, bool)) ([]agent.SessionSummary, error) {
	slices.SortFunc(files, func(a, b SessionFile) int { return b.ModTime.Compare(a.ModTime) })
	var out []agent.SessionSummary
	skipped := 0
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		sum, ok := summarize(f.Path)
		if !ok {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		if sum.UpdatedAt.IsZero() {
			sum.UpdatedAt = f.ModTime
		}
		out = append(out, sum)
	}
	return out, nil
}
