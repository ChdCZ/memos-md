package memofile

import (
	"mime"
	"path/filepath"
	"strings"

	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/markdown"
)

// NewMarkdownService builds the markdown service the API uses, so tags and
// mentions written in a file are indexed exactly like tags typed in the UI.
func NewMarkdownService() markdown.Service {
	return markdown.NewService(
		markdown.WithTagExtension(),
		markdown.WithMentionExtension(),
	)
}

// randomMemoUID mints a uid in the same shape the API uses for new memos.
func randomMemoUID() string {
	return random.UUID()
}

// contentTypeOf guesses an attachment content type from its file name so an
// imported attachment renders like an uploaded one.
func contentTypeOf(filename string) string {
	extension := strings.ToLower(filepath.Ext(filename))
	if extension == "" {
		return "application/octet-stream"
	}
	if contentType := mime.TypeByExtension(extension); contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}
