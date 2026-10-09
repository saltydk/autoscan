package autoscan

import (
	"path"
	"strings"
)

// ValidScanPath accepts absolute paths without requiring them to exist locally.
// Validate input before Clean or Dir can turn a missing path into ".".
func ValidScanPath(value string) bool {
	return path.IsAbs(value) && !strings.ContainsRune(value, 0)
}

// ValidRelativeFilePath validates vendor file paths before joining a library path.
func ValidRelativeFilePath(value string) bool {
	if strings.TrimSpace(value) == "" || path.IsAbs(value) || strings.ContainsRune(value, 0) || strings.HasSuffix(value, "/") {
		return false
	}
	if leaf := path.Base(value); leaf == "." || leaf == ".." {
		return false
	}
	clean := path.Clean(value)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}
