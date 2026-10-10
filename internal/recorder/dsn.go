package recorder

import (
	"net/url"
	"path/filepath"
	"strings"
)

// fileURI preserves filename characters that would otherwise become URI
// fragments or connection options. Both SQLite drivers use the same path form.
func fileURI(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	path = filepath.ToSlash(path)
	if filepath.IsAbs(path) && !strings.HasPrefix(path, "/") {
		path = "/" + path // Windows drive letters: file:///D:/directory/file.
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}
