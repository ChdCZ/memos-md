package memofile

import (
	"runtime"
	"strconv"
	"strings"
)

// goroutineID returns the id of the calling goroutine.
//
// It exists for one reason: Sync writes to the store, the store calls back into
// this package after every memo write, and a naive lock around Sync would
// deadlock on its own import pass. The store cannot tell a mirror-internal
// write from an API write, so the mirror recognises its own goroutine instead.
// The value is only compared for equality and never exposed.
func goroutineID() uint64 {
	buffer := make([]byte, 64)
	length := runtime.Stack(buffer, false)
	// The header is "goroutine <id> [<state>]: ".
	text := string(buffer[:length])
	rest, found := strings.CutPrefix(text, "goroutine ")
	if !found {
		return 0
	}
	id, _, found := strings.Cut(rest, " ")
	if !found {
		return 0
	}
	parsed, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}
