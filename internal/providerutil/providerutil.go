// Package providerutil holds small helpers the built-in providers share.
// They are internal so they do not become part of the library's API.
package providerutil

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// NewUUID returns a random version 4 UUID.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:])
}

// MediaType returns the MIME type for a file path from its extension, or
// "application/octet-stream".
func MediaType(path string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); t != "" {
		if i := strings.IndexByte(t, ';'); i >= 0 {
			t = t[:i]
		}
		return t
	}
	return "application/octet-stream"
}

// Raw marshals v, returning nil on failure. Use it for values known to
// encode.
func Raw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}

// ObjectSchema returns schema, or an open object schema when schema is
// empty.
func ObjectSchema(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return schema
}

// AbsDir returns dir as an absolute path; an empty dir means the current
// directory.
func AbsDir(dir string) string {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// ParseTime parses an RFC 3339 time, or returns the zero time when s is
// empty or malformed.
func ParseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// FromUnixMilli returns the time of Unix milliseconds ms, or the zero time
// when ms is not positive.
func FromUnixMilli(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// LastPage returns the page of items that ends offset items before the
// end and holds at most limit items, in their original order. A zero limit
// means every item before that point.
func LastPage[T any](items []T, limit, offset int) []T {
	end := max(len(items)-max(offset, 0), 0)
	start := 0
	if limit > 0 {
		start = max(end-limit, 0)
	}
	return items[start:end]
}

// Deref returns *p, or the zero value when p is nil.
func Deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

var versionPattern = regexp.MustCompile(`\d+\.\d+\.\d+[0-9A-Za-z.+-]*`)

// ParseVersion returns the first semantic version in s, such as "2.1.301"
// in "2.1.301 (Claude Code)", or "" when there is none.
func ParseVersion(s string) string { return versionPattern.FindString(s) }
