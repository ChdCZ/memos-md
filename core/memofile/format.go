package memofile

import (
	"strings"
	"time"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"

	"github.com/usememos/memos/internal/identifier"
	"github.com/usememos/memos/store"
)

const (
	// DirName is the directory under the data directory that holds the notes.
	DirName = "memos"
	// AttachmentsDirName is the directory under DirName that holds attachment
	// bytes, laid out as attachments/<attachment uid>/<filename>.
	AttachmentsDirName = "attachments"
	// IndexFileName is the sidecar index inside DirName. It is derived state:
	// deleting it only costs one full reconciliation pass.
	IndexFileName = ".memos-file-index.json"
	// FileExtension is the memo file extension.
	FileExtension = ".md"
	// frontMatterDelimiter opens and closes the YAML front matter block.
	frontMatterDelimiter = "---"
)

// FrontMatter is the YAML block at the top of a memo file.
//
// Tags and Pinned are pointers so an absent key is distinguishable from an
// explicit empty value: a file that says nothing about tags keeps whatever the
// store holds, while `tags: []` clears them.
type FrontMatter struct {
	UID         string    `yaml:"uid"`
	Creator     string    `yaml:"creator,omitempty"`
	Created     string    `yaml:"created,omitempty"`
	Updated     string    `yaml:"updated,omitempty"`
	Visibility  string    `yaml:"visibility,omitempty"`
	Pinned      *bool     `yaml:"pinned,omitempty"`
	Tags        *[]string `yaml:"tags,omitempty"`
	Space       string    `yaml:"space,omitempty"`
	Attachments []string  `yaml:"attachments,omitempty"`
	Relations   []string  `yaml:"relations,omitempty"`
	Location    string    `yaml:"location,omitempty"`
}

// Document is a parsed memo file.
type Document struct {
	FrontMatter FrontMatter
	// Body is the memo content, exactly as the store holds it.
	Body string
	// HadFrontMatter is false for files that only contain markdown.
	HadFrontMatter bool
}

// FileName returns the file name of a memo.
func FileName(uid string) string {
	return uid + FileExtension
}

// AttachmentDir returns the directory name of an attachment.
func AttachmentDir(uid string) string {
	return AttachmentsDirName + "/" + uid
}

// FormatTime renders a Unix timestamp the way front matter stores it.
func FormatTime(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// ParseTime reads a front matter timestamp. It accepts RFC 3339 with any
// offset and fractional seconds, plus the date-only form.
func ParseTime(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, errors.New("empty timestamp")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return parsed.Unix(), nil
	}
	parsed, err := time.Parse("2006-01-02", trimmed)
	if err != nil {
		return 0, errors.Wrapf(err, "invalid timestamp %q", value)
	}
	return parsed.Unix(), nil
}

// Render serializes a memo file. The body is written verbatim after the front
// matter block so hand edits stay byte-stable.
func Render(frontMatter FrontMatter, body string) ([]byte, error) {
	block, err := yaml.Marshal(&frontMatter)
	if err != nil {
		return nil, errors.Wrap(err, "failed to encode front matter")
	}
	var builder strings.Builder
	builder.WriteString(frontMatterDelimiter)
	builder.WriteString("\n")
	builder.Write(block)
	builder.WriteString(frontMatterDelimiter)
	builder.WriteString("\n")
	if body != "" {
		builder.WriteString("\n")
		builder.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			builder.WriteString("\n")
		}
	}
	return []byte(builder.String()), nil
}

// Parse reads a memo file. A file without front matter is a valid memo whose
// body is the whole file; Parse only fails when a front matter block is
// present but malformed.
func Parse(content []byte) (*Document, error) {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	document := &Document{Body: text}

	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != frontMatterDelimiter {
		return document, nil
	}
	end := -1
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == frontMatterDelimiter {
			end = index
			break
		}
	}
	if end == -1 {
		return nil, errors.New("front matter is not terminated")
	}

	block := strings.Join(lines[1:end], "\n")
	if err := yaml.Unmarshal([]byte(block), &document.FrontMatter); err != nil {
		return nil, errors.Wrap(err, "failed to decode front matter")
	}
	document.HadFrontMatter = true
	document.Body = strings.TrimLeft(strings.Join(lines[end+1:], "\n"), "\n")
	return document, nil
}

// NormalizeVisibility maps front matter text onto a store visibility. An empty
// value, and any value that is not a known visibility, means PRIVATE: an
// unrecognized audience must never widen access.
func NormalizeVisibility(value string) store.Visibility {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case string(store.Public):
		return store.Public
	case string(store.Protected):
		return store.Protected
	case string(store.SpaceAudience):
		return store.SpaceAudience
	default:
		return store.Private
	}
}

// ValidUID reports whether a file's uid can name a memo. A file whose uid is
// unusable is imported under a fresh one.
func ValidUID(uid string) bool {
	return uid != "" && len(uid) <= 64 && identifier.UIDMatcher.MatchString(uid)
}

// NormalizeTags trims, drops empties and duplicates, and strips a leading '#'.
func NormalizeTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tag), "#"))
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

// mergeTags unions two tag groups case-insensitively, preserving order with the
// first group's spelling winning.
func mergeTags(groups ...[]string) []string {
	result := []string{}
	seen := map[string]struct{}{}
	for _, group := range groups {
		for _, tag := range group {
			trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tag), "#"))
			if trimmed == "" {
				continue
			}
			key := strings.ToLower(trimmed)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, trimmed)
		}
	}
	return result
}

func derefTags(values *[]string) []string {
	if values == nil {
		return nil
	}
	return *values
}

func equalTags(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !strings.EqualFold(left[index], right[index]) {
			return false
		}
	}
	return true
}
