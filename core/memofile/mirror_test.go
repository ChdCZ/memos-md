package memofile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/core/memopayload"
	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/internal/random"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db"
)

// newTestMirror builds a migrated SQLite store in a temporary directory and
// attaches a mirror to it, exactly like `memos --memo-files` does at startup.
func newTestMirror(t *testing.T) (*Mirror, *store.Store, *store.User, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	instanceProfile := &profile.Profile{
		Driver:  "sqlite",
		DSN:     filepath.Join(dataDir, "memos.db"),
		Data:    dataDir,
		Version: "testing",
	}
	driver, err := db.NewDBDriver(instanceProfile)
	require.NoError(t, err)
	st := store.New(driver, instanceProfile)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.Migrate(ctx))

	user, err := st.CreateUser(ctx, &store.User{
		Username: "tester",
		Role:     store.RoleAdmin,
		Email:    "tester@example.com",
	})
	require.NoError(t, err)

	markdownService := NewMarkdownService()
	mirror, err := NewMirror(ctx, st, markdownService, dataDir)
	require.NoError(t, err)
	st.SetMemoMirror(mirror)
	t.Cleanup(func() { _ = mirror.Close() })
	return mirror, st, user, dataDir
}

func createMemo(t *testing.T, st *store.Store, user *store.User, content string, visibility store.Visibility) *store.Memo {
	t.Helper()
	memo := &store.Memo{
		UID:        random.UUID(),
		CreatorID:  user.ID,
		Content:    content,
		Visibility: visibility,
		RowStatus:  store.Normal,
	}
	require.NoError(t, memopayload.RebuildMemoPayload(context.Background(), memo, NewMarkdownService()))
	created, err := st.CreateMemo(context.Background(), memo)
	require.NoError(t, err)
	return created
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(content)
}

func TestMirrorWritesMemoFile(t *testing.T) {
	mirror, st, user, dataDir := newTestMirror(t)
	ctx := context.Background()

	memo := createMemo(t, st, user, "# Hello\n\nfirst note #tag", store.Public)
	path := filepath.Join(dataDir, DirName, FileName(memo.UID))
	content := readFile(t, path)

	require.Contains(t, content, "uid: "+memo.UID)
	require.Contains(t, content, "creator: tester")
	require.Contains(t, content, "visibility: PUBLIC")
	require.Contains(t, content, "# Hello")
	require.Contains(t, content, "#tag")

	document, err := Parse([]byte(content))
	require.NoError(t, err)
	require.Equal(t, memo.UID, document.FrontMatter.UID)
	require.Equal(t, "# Hello\n\nfirst note #tag", strings.TrimRight(document.Body, "\n"))
	require.Equal(t, []string{"tag"}, derefTags(document.FrontMatter.Tags))
	require.Equal(t, store.Public, NormalizeVisibility(document.FrontMatter.Visibility))

	// A second pass must not rewrite an unchanged memo.
	info, err := os.Stat(path)
	require.NoError(t, err)
	report, err := mirror.Sync(ctx)
	require.NoError(t, err)
	require.Empty(t, report.Errors)
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, info.ModTime(), after.ModTime())
}

// TestMirrorWritesBoundAttachment is a regression test for two bugs found by
// running the fork against a real server:
//
//  1. A managed attachment is uploaded unbound and bound in the same
//     transaction that creates the memo. Binding does not change the memo file
//     itself, so mirroring attachments only after deciding "the file is
//     unchanged" left the bytes out of the data directory entirely.
//  2. An uploaded attachment keeps its bytes outside the row 鈥?for the default
//     LOCAL storage, in a file named by `reference` 鈥?while the blob column
//     stays empty. Reading only the blob mirrored nothing on a real upload.
func TestMirrorWritesBoundAttachment(t *testing.T) {
	mirror, st, user, dataDir := newTestMirror(t)
	ctx := context.Background()

	// Stage the bytes the way an upload does: a file under the data directory,
	// referenced from the row, with no inline blob.
	blob := []byte("\x89PNG\r\n\x1a\nnot-really-a-png")
	storedPath := "assets/" + "1791897745_isolate.png"
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, filepath.FromSlash(storedPath)), blob, 0o644))

	attachment, err := st.CreateAttachment(ctx, &store.Attachment{
		UID:         "Attachment01",
		CreatorID:   user.ID,
		Filename:    "screenshot.png",
		Type:        "image/png",
		Size:        int64(len(blob)),
		StorageType: storepb.AttachmentStorageType_LOCAL,
		Reference:   storedPath,
	})
	require.NoError(t, err)
	require.Empty(t, attachment.Blob, "an uploaded attachment does not carry an inline blob")

	attachmentDir := filepath.Join(dataDir, DirName, AttachmentDir(attachment.UID))
	require.NoDirExists(t, attachmentDir, "an unbound attachment has no memo to mirror yet")

	memo := createMemo(t, st, user, "note with an image #shot", store.Public)
	require.NoError(t, st.ApplyMemoMutation(ctx, &store.MemoMutation{
		MemoID:                memo.ID,
		MemoCreatorID:         user.ID,
		ExpectedMemoContent:   memo.Content,
		Bindings:              []*store.MemoAttachmentBinding{{ID: attachment.ID, UID: attachment.UID}},
		RequiredAttachmentIDs: []int32{attachment.ID},
	}))

	require.DirExists(t, attachmentDir, "binding an attachment must mirror its bytes")
	require.Equal(t, blob, []byte(readFile(t, filepath.Join(attachmentDir, "screenshot.png"))))

	// The memo file names the attachment so a reader can find the bytes.
	require.Contains(t, readFile(t, filepath.Join(dataDir, DirName, FileName(memo.UID))), "Attachment01")

	// A later sync must not drop it either.
	report, err := mirror.Sync(ctx)
	require.NoError(t, err)
	require.Empty(t, report.Errors)
	require.DirExists(t, attachmentDir)
}

func TestMirrorFollowsStoreUpdatesAndDeletes(t *testing.T) {
	_, st, user, dataDir := newTestMirror(t)
	ctx := context.Background()

	memo := createMemo(t, st, user, "original body", store.Private)
	path := filepath.Join(dataDir, DirName, FileName(memo.UID))

	updated := "edited body #later"
	visibility := store.Protected
	require.NoError(t, st.UpdateMemo(ctx, &store.UpdateMemo{ID: memo.ID, Content: &updated, Visibility: &visibility}))
	content := readFile(t, path)
	require.Contains(t, content, "edited body #later")
	require.Contains(t, content, "visibility: PROTECTED")

	require.NoError(t, st.DeleteMemo(ctx, &store.DeleteMemo{ID: memo.ID}))
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err), "deleted memo should lose its file")
}

// newOwnerlessMirror builds a migrated store with no user, like a fresh
// instance before setup finishes.
func newOwnerlessMirror(t *testing.T) (*Mirror, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	instanceProfile := &profile.Profile{
		Driver:  "sqlite",
		DSN:     filepath.Join(dataDir, "memos.db"),
		Data:    dataDir,
		Version: "testing",
	}
	driver, err := db.NewDBDriver(instanceProfile)
	require.NoError(t, err)
	st := store.New(driver, instanceProfile)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.Migrate(ctx))

	mirror, err := NewMirror(ctx, st, NewMarkdownService(), dataDir)
	require.NoError(t, err)
	st.SetMemoMirror(mirror)
	t.Cleanup(func() { _ = mirror.Close() })
	return mirror, st, dataDir
}

func TestNotesDroppedInBeforeSetupAreImportedAfterTheFirstAccount(t *testing.T) {
	mirror, st, dataDir := newOwnerlessMirror(t)
	ctx := context.Background()

	// A fresh data directory with notes in it: no account exists to own them.
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, DirName, "first-note.md"),
		[]byte("---\nuid: FirstNote\nvisibility: PUBLIC\n---\n\nDropped in before setup. #early\n"),
		0o644,
	))

	report, err := mirror.Sync(ctx)
	require.NoError(t, err)
	require.Empty(t, report.Errors, "a missing owner is a wait, not a failure")
	require.Equal(t, 0, report.Imported)
	require.Equal(t, 0, report.Removed, "the note must not be deleted while it waits")

	memos, err := st.ListMemos(ctx, &store.FindMemo{})
	require.NoError(t, err)
	require.Empty(t, memos)

	// Creating the first account clears the queue, without a restart.
	_, err = st.CreateUser(ctx, &store.User{Username: "owner", Role: store.RoleAdmin, Email: "owner@example.com"})
	require.NoError(t, err)

	imported, err := st.GetMemo(ctx, &store.FindMemo{UID: strPtr("FirstNote")})
	require.NoError(t, err)
	require.NotNil(t, imported, "the waiting note should be imported once an owner exists")
	require.Contains(t, imported.Content, "Dropped in before setup")
	require.Equal(t, []string{"early"}, imported.Payload.Tags)

	// A second sync is a no-op rather than a duplicate import.
	report, err = mirror.Sync(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, report.Imported)
}

func TestSyncImportsHandWrittenFiles(t *testing.T) {
	mirror, st, _, dataDir := newTestMirror(t)
	ctx := context.Background()

	handwritten := strings.Join([]string{
		"---",
		"uid: HANDWRITTEN01",
		"visibility: PUBLIC",
		"pinned: true",
		"tags: [fromfrontmatter]",
		"---",
		"",
		"# Written by hand",
		"",
		"Body typed outside the app. #frombody",
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, DirName, FileName("HANDWRITTEN01")), []byte(handwritten), 0o644))

	// A bare markdown file without front matter is imported too.
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, DirName, "bare.md"), []byte("# Bare\n\nno front matter here\n"), 0o644))

	report, err := mirror.Sync(ctx)
	require.NoError(t, err)
	require.Empty(t, report.Errors)
	require.Equal(t, 2, report.Imported)

	imported, err := st.GetMemo(ctx, &store.FindMemo{UID: strPtr("HANDWRITTEN01")})
	require.NoError(t, err)
	require.NotNil(t, imported)
	require.Contains(t, imported.Content, "Written by hand")
	require.Equal(t, store.Public, imported.Visibility)
	require.True(t, imported.Pinned)
	require.Equal(t, []string{"fromfrontmatter", "frombody"}, imported.Payload.Tags)

	bare, err := st.GetMemo(ctx, &store.FindMemo{UID: strPtr("bare")})
	require.NoError(t, err)
	require.NotNil(t, bare)
	require.Contains(t, bare.Content, "no front matter here")
	require.Equal(t, store.Private, bare.Visibility)
	// The uid-less file is normalized in place: it becomes the canonical
	// <uid>.md file the mirror owns, now with front matter.
	require.Contains(t, readFile(t, filepath.Join(dataDir, DirName, FileName("bare"))), "uid: bare")
}

func TestSyncAppliesLocalEdits(t *testing.T) {
	mirror, st, user, dataDir := newTestMirror(t)
	ctx := context.Background()

	memo := createMemo(t, st, user, "before the edit", store.Private)
	path := filepath.Join(dataDir, DirName, FileName(memo.UID))

	edited := strings.Replace(readFile(t, path), "before the edit", "after the edit #local", 1)
	require.NoError(t, os.WriteFile(path, []byte(edited), 0o644))

	report, err := mirror.Sync(ctx)
	require.NoError(t, err)
	require.Empty(t, report.Errors)
	require.Equal(t, 1, report.Updated)

	reloaded, err := st.GetMemo(ctx, &store.FindMemo{UID: &memo.UID})
	require.NoError(t, err)
	require.Equal(t, "after the edit #local", strings.TrimRight(reloaded.Content, "\n"))
	require.Equal(t, []string{"local"}, reloaded.Payload.Tags)
}

// TestSyncAppliesLocalEditWithTagChanges guards the case where an edit also
// changes the tags: the payload carries both the indexed tags and the memo
// properties, so rebuilding it has to include the new body. Writing a payload
// that lacks the new body left the store on the old content and the next mirror
// pass wrote that stale body back over the file the user had just edited.
func TestSyncAppliesLocalEditWithTagChanges(t *testing.T) {
	mirror, st, user, dataDir := newTestMirror(t)
	ctx := context.Background()

	memo := createMemo(t, st, user, "first version #original", store.Public)
	path := filepath.Join(dataDir, DirName, FileName(memo.UID))

	// Rewrite the body the way an external editor would: new text, new hashtag,
	// and the front matter tag mirror left as it was.
	edited := strings.Replace(readFile(t, path), "first version #original", "second version #changed", 1)
	require.NoError(t, os.WriteFile(path, []byte(edited), 0o644))

	report, err := mirror.Sync(ctx)
	require.NoError(t, err)
	require.Empty(t, report.Errors)
	require.Equal(t, 1, report.Updated)

	reloaded, err := st.GetMemo(ctx, &store.FindMemo{UID: &memo.UID})
	require.NoError(t, err)
	require.Contains(t, reloaded.Content, "second version", "the edited body must reach the store")
	require.Contains(t, reloaded.Payload.GetTags(), "changed")
	require.Contains(t, reloaded.Payload.GetTags(), "original")

	// The file must still hold the edit, not a reverted copy of the old body.
	after := readFile(t, path)
	require.Contains(t, after, "second version")
	require.NotContains(t, after, "first version")
}

func TestSyncRestoresMemosFromFilesAfterDatabaseLoss(t *testing.T) {
	ctx := context.Background()
	mirror, st, user, dataDir := newTestMirror(t)
	memo := createMemo(t, st, user, "survives a database reset #keep", store.Public)
	require.NoError(t, mirror.Close())
	require.NoError(t, st.Close())

	// Reopen the same data directory with an empty database: the markdown file
	// is now the only copy of the memo.
	instanceProfile := &profile.Profile{Driver: "sqlite", DSN: filepath.Join(dataDir, "fresh.db"), Data: dataDir, Version: "testing"}
	driver, err := db.NewDBDriver(instanceProfile)
	require.NoError(t, err)
	fresh := store.New(driver, instanceProfile)
	t.Cleanup(func() { _ = fresh.Close() })
	require.NoError(t, fresh.Migrate(ctx))
	_, err = fresh.CreateUser(ctx, &store.User{Username: "tester", Role: store.RoleAdmin, Email: "tester@example.com"})
	require.NoError(t, err)

	freshMirror, err := NewMirror(ctx, fresh, NewMarkdownService(), dataDir)
	require.NoError(t, err)
	fresh.SetMemoMirror(freshMirror)
	t.Cleanup(func() { _ = freshMirror.Close() })
	report, err := freshMirror.Sync(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, report.Imported)

	restored, err := fresh.GetMemo(ctx, &store.FindMemo{UID: &memo.UID})
	require.NoError(t, err)
	require.NotNil(t, restored)
	require.Equal(t, "survives a database reset #keep", strings.TrimRight(restored.Content, "\n"))
	require.Equal(t, store.Public, restored.Visibility)
}

func TestRenderAndParseRoundTrip(t *testing.T) {
	frontMatter := FrontMatter{
		UID:         "ROUNDTRIP001",
		Creator:     "tester",
		Created:     "2026-02-14T09:31:02Z",
		Updated:     "2026-02-14T10:02:44Z",
		Visibility:  "PROTECTED",
		Pinned:      boolPtr(true),
		Tags:        tagsPtr("one", "two"),
		Space:       "space-uid",
		Attachments: []string{"ATTACHMENT01"},
	}
	body := "# Title\n\n- [ ] task\n\n`code_with_underscores`\n"
	rendered, err := Render(frontMatter, body)
	require.NoError(t, err)

	document, err := Parse(rendered)
	require.NoError(t, err)
	require.True(t, document.HadFrontMatter)
	require.Equal(t, "ROUNDTRIP001", document.FrontMatter.UID)
	require.Equal(t, "tester", document.FrontMatter.Creator)
	require.Equal(t, []string{"one", "two"}, derefTags(document.FrontMatter.Tags))
	require.Equal(t, []string{"ATTACHMENT01"}, document.FrontMatter.Attachments)
	require.True(t, derefBool(document.FrontMatter.Pinned))
	require.Equal(t, strings.TrimRight(body, "\n"), strings.TrimRight(document.Body, "\n"))

	created, err := ParseTime(document.FrontMatter.Created)
	require.NoError(t, err)
	require.Equal(t, int64(1771061462), created)
	updated, err := ParseTime(document.FrontMatter.Updated)
	require.NoError(t, err)
	require.Greater(t, updated, created)
}

func TestParseTreatsPlainMarkdownAsBody(t *testing.T) {
	document, err := Parse([]byte("just a note\n\nwith #tag\n"))
	require.NoError(t, err)
	require.False(t, document.HadFrontMatter)
	require.Equal(t, "just a note\n\nwith #tag\n", document.Body)
}

func TestParseRejectsUnterminatedFrontMatter(t *testing.T) {
	_, err := Parse([]byte("---\nuid: x\nbody without a closing marker\n"))
	require.Error(t, err)
}

func TestNormalizeVisibilityFailsClosed(t *testing.T) {
	require.Equal(t, store.Private, NormalizeVisibility(""))
	require.Equal(t, store.Private, NormalizeVisibility("nonsense"))
	require.Equal(t, store.Public, NormalizeVisibility("public"))
	require.Equal(t, store.Protected, NormalizeVisibility("Protected"))
	require.Equal(t, store.SpaceAudience, NormalizeVisibility("SPACE"))
}

func TestNormalizeTagsDropsDuplicatesAndHashes(t *testing.T) {
	require.Equal(t, []string{"One", "two"}, NormalizeTags([]string{"#One", "two", "one", " ", "#"}))
}

func TestSafeFileNameKeepsAttachmentReadable(t *testing.T) {
	require.Equal(t, "photo-1.png", safeFileName("photo-1.png"))
	require.Equal(t, "a-b.png", safeFileName("../a:b.png"))
	require.Equal(t, "file", safeFileName(""))
}

func strPtr(value string) *string {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}

func tagsPtr(values ...string) *[]string {
	return &values
}

func derefBool(value *bool) bool {
	return value != nil && *value
}
