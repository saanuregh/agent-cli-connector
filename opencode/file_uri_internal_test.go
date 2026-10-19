package opencode

import "testing"

// File URIs follow RFC 8089 on both Unix and Windows paths, whatever the
// OS running the test.
func TestFileURI(t *testing.T) {
	for _, c := range []struct {
		path    string
		windows bool
		uri     string
	}{
		{"/work/notes.txt", false, "file:///work/notes.txt"},
		{"/work/a b#1.txt", false, "file:///work/a%20b%231.txt"},
		{"D:/work/notes.txt", true, "file:///D:/work/notes.txt"},
		{"//server/share/notes.txt", true, "file://server/share/notes.txt"},
	} {
		if got := fileURI(c.path, c.windows); got != c.uri {
			t.Errorf("fileURI(%q, windows=%v) = %q; want %q", c.path, c.windows, got, c.uri)
		}
		if got, ok := fileURIPath(c.uri, c.windows); !ok || got != c.path {
			t.Errorf("fileURIPath(%q, windows=%v) = %q, %v; want %q", c.uri, c.windows, got, ok, c.path)
		}
	}
	for _, c := range []struct {
		uri     string
		windows bool
		path    string
		ok      bool
	}{
		{"file://localhost/work/x", false, "/work/x", true},
		{"file://server/share/x", false, "", false},
		{"https://example.com/x.png", false, "", false},
		{"file:///D:/x", false, "/D:/x", true},
	} {
		if got, ok := fileURIPath(c.uri, c.windows); ok != c.ok || got != c.path {
			t.Errorf("fileURIPath(%q, windows=%v) = %q, %v; want %q, %v", c.uri, c.windows, got, ok, c.path, c.ok)
		}
	}
}
