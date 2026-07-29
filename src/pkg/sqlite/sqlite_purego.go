//go:build purego

package sqlite

import (
	"strings"

	_ "modernc.org/sqlite"
)

const DriverName = "sqlite"

// FormatChatStorageURI formats the URI for chat storage using modernc pragmas.
// Any existing query string (including go-sqlite3-style _foreign_keys / _journal_mode)
// is stripped so cross-compiled Windows binaries are not fed incompatible params.
func FormatChatStorageURI(baseURI string, enableWAL bool, enableFK bool) string {
	clean := baseURI
	if idx := strings.Index(baseURI, "?"); idx >= 0 {
		clean = baseURI[:idx]
	}

	var pragmaParams []string
	if enableWAL {
		pragmaParams = append(pragmaParams, "_pragma=journal_mode(WAL)", "_pragma=busy_timeout(30000)", "_pragma=synchronous(1)")
	}
	if enableFK {
		pragmaParams = append(pragmaParams, "_pragma=foreign_keys(1)")
	}

	if len(pragmaParams) == 0 {
		return clean
	}
	return clean + "?" + strings.Join(pragmaParams, "&")
}
