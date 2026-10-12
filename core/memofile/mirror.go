package memofile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/pkg/errors"

	"github.com/usememos/memos/core/memopayload"
	"github.com/usememos/memos/markdown"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/store"
)

// Mirror keeps the memo table and the markdown files in step. It is attached to
// the store, so every memo the store writes is written to disk as well, and
// every file the store has not seen is imported on Sync.
type Mirror struct {
	Store           *store.Store
	MarkdownService markdown.Service
	DataDir         string
	// ReadOnly mirrors store writes to disk but never imports files.
	ReadOnly bool

	// mu guards owner. holder is the goroutine currently inside the mirror and
	// depth counts its nesting: Sync writes to the store, the store calls back
	// into MirrorMemo/RemoveMemo on the same goroutine, and those nested calls
	// must run without waiting for the pass that invoked them.
	mu     sync.Mutex
	cond   *sync.Cond
	holder uint64
	depth  int
	dir    string
	index  *fileIndex
	// unowned maps file uid to path for notes that could not be imported yet
	// because the instance has no user to own them. The first account created
	// clears the list through StartSync.
	unowned map[string]string
}

// Report summarises one reconciliation pass.
type Report struct {
	Written  int
	Imported int
	Updated  int
	Removed  int
	Skipped  int
	Errors   []error
}

type indexEntry struct {
	Path     string `json:"path"`
	BodyHash string `json:"bodyHash"`
	ModTime  int64  `json:"modTime"`
	Size     int64  `json:"size"`
}

type fileIndex struct {
	Version int                    `json:"version"`
	Entries map[string]*indexEntry `json:"entries"`
}

func newFileIndex() *fileIndex {
	return &fileIndex{Version: 1, Entries: map[string]*indexEntry{}}
}

// NewMirror prepares the notes directory and loads the sidecar index.
func NewMirror(ctx context.Context, st *store.Store, markdownService markdown.Service, profileDataDir string) (*Mirror, error) {
	mirror := &Mirror{
		Store:           st,
		MarkdownService: markdownService,
		DataDir:         profileDataDir,
		index:           newFileIndex(),
		unowned:         map[string]string{},
	}
	mirror.cond = sync.NewCond(&mirror.mu)
	if err := mirror.init(); err != nil {
		return nil, err
	}
	_ = ctx
	return mirror, nil
}

// enter acquires the mirror for the calling goroutine. It reports false when
// this goroutine already holds it, in which case the caller must not release it
// afterwards.
func (m *Mirror) enter() bool {
	self := goroutineID()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holder == self && m.depth > 0 {
		// Re-entrant call from the pass that holds the mirror.
		return false
	}
	for m.depth > 0 {
		m.cond.Wait()
	}
	m.holder = self
	m.depth = 1
	return true
}

func (m *Mirror) leave(held bool) {
	if !held {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.depth > 0 {
		m.depth = 0
		m.holder = 0
	}
	m.cond.Broadcast()
}

func (m *Mirror) init() error {
	m.dir = filepath.Join(m.DataDir, DirName)
	if err := os.MkdirAll(filepath.Join(m.dir, AttachmentsDirName), 0o755); err != nil {
		return errors.Wrap(err, "failed to create memos directory")
	}
	indexPath := filepath.Join(m.dir, IndexFileName)
	content, err := os.ReadFile(indexPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return errors.Wrap(err, "failed to read memos file index")
		}
		return nil
	}
	index := newFileIndex()
	if err := json.Unmarshal(content, index); err != nil {
		// A damaged index is derived state: rebuild it from the files.
		slog.Warn("memos file index is unreadable, rebuilding", slog.Any("err", err))
		return nil
	}
	if index.Entries == nil {
		index.Entries = map[string]*indexEntry{}
	}
	m.index = index
	return nil
}

func (m *Mirror) indexPath() string {
	return filepath.Join(m.dir, IndexFileName)
}

func (m *Mirror) saveIndexLocked() {
	content, err := json.MarshalIndent(m.index, "", "  ")
	if err != nil {
		slog.Warn("failed to encode memos file index", slog.Any("err", err))
		return
	}
	if err := writeFileAtomic(m.indexPath(), content, 0o644); err != nil {
		slog.Warn("failed to persist memos file index", slog.Any("err", err))
	}
}

// Close persists the sidecar index.
func (m *Mirror) Close() error {
	held := m.enter()
	defer m.leave(held)
	m.saveIndexLocked()
	return nil
}

// StoreChanged imports files that were waiting for an owner and otherwise does
// nothing. It backs the store's notification for writes the mirror does not
// model, which today means the first account being created: a memo cannot be
// owned before that, so files dropped into a fresh data directory are imported
// the moment setup finishes instead of being reported as failures.
func (m *Mirror) StoreChanged(ctx context.Context) {
	if m == nil {
		return
	}
	held := m.enter()
	defer m.leave(held)
	if len(m.unowned) == 0 {
		return
	}
	report := &Report{}
	m.importUnownedLocked(ctx, report)
	for _, err := range report.Errors {
		slog.Warn("failed to import memo file", slog.Any("err", err))
	}
}

// MirrorMemo writes one memo to disk. It is called by the store after every
// successful memo mutation.
func (m *Mirror) MirrorMemo(ctx context.Context, memo *store.Memo) {
	if m == nil || memo == nil {
		return
	}
	held := m.enter()
	defer m.leave(held)
	if err := m.mirrorMemoLocked(ctx, memo); err != nil {
		slog.Warn("failed to mirror memo to file", slog.String("uid", memo.UID), slog.Any("err", err))
	}
}

// RemoveMemo deletes the file of a memo that no longer exists in the store.
func (m *Mirror) RemoveMemo(_ context.Context, uid string) {
	if m == nil || uid == "" {
		return
	}
	held := m.enter()
	defer m.leave(held)
	entry, ok := m.index.Entries[uid]
	if !ok {
		return
	}
	if err := os.Remove(filepath.Join(m.dir, filepath.FromSlash(entry.Path))); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove memo file", slog.String("uid", uid), slog.Any("err", err))
	}
	delete(m.index.Entries, uid)
	m.saveIndexLocked()
}

func (m *Mirror) mirrorMemoLocked(ctx context.Context, memo *store.Memo) error {
	document := m.renderDocument(ctx, memo)
	path := filepath.Join(m.dir, FileName(memo.UID))
	existing, err := os.ReadFile(path)
	if err == nil && string(existing) == string(document) {
		// Nothing changed; keep the index fresh without touching the file. The
		// index must be persisted here too, otherwise a later pass forgets that
		// this memo is mirrored and removes its file.
		if info, statErr := os.Stat(path); statErr == nil {
			m.index.Entries[memo.UID] = &indexEntry{
				Path:     FileName(memo.UID),
				BodyHash: hashString(memo.Content),
				ModTime:  info.ModTime().UnixNano(),
				Size:     info.Size(),
			}
			m.saveIndexLocked()
		}
		return nil
	}
	if err := writeFileAtomic(path, document, 0o644); err != nil {
		return errors.Wrap(err, "failed to write memo file")
	}
	m.mirrorAttachmentsLocked(ctx, memo)
	info, err := os.Stat(path)
	if err != nil {
		return errors.Wrap(err, "failed to stat memo file")
	}
	m.index.Entries[memo.UID] = &indexEntry{
		Path:     FileName(memo.UID),
		BodyHash: hashString(memo.Content),
		ModTime:  info.ModTime().UnixNano(),
		Size:     info.Size(),
	}
	m.saveIndexLocked()
	return nil
}

func (m *Mirror) renderDocument(ctx context.Context, memo *store.Memo) []byte {
	pinned := memo.Pinned
	tags := tagNames(memo)
	frontMatter := FrontMatter{
		UID:        memo.UID,
		Creator:    m.usernameOf(ctx, memo.CreatorID),
		Created:    FormatTime(memo.CreatedTs),
		Updated:    FormatTime(memo.UpdatedTs),
		Visibility: string(memo.Visibility),
		Pinned:     &pinned,
		Tags:       &tags,
		Space:      m.spaceUIDOf(ctx, memo.SpaceID),
	}
	attachments, err := m.Store.ListAttachments(ctx, &store.FindAttachment{MemoID: &memo.ID})
	if err != nil {
		slog.Warn("failed to list memo attachments for mirroring", slog.String("uid", memo.UID), slog.Any("err", err))
	}
	attachmentUIDs := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		attachmentUIDs = append(attachmentUIDs, attachment.UID)
	}
	frontMatter.Attachments = attachmentUIDs

	relations, err := m.Store.ListMemoRelations(ctx, &store.FindMemoRelation{MemoID: &memo.ID})
	if err != nil {
		slog.Warn("failed to list memo relations for mirroring", slog.String("uid", memo.UID), slog.Any("err", err))
	}
	relationUIDs := make([]string, 0, len(relations))
	for _, relation := range relations {
		if related, err := m.Store.GetMemo(ctx, &store.FindMemo{ID: &relation.RelatedMemoID, ExcludeContent: true}); err == nil && related != nil {
			relationUIDs = append(relationUIDs, related.UID)
		}
	}
	frontMatter.Relations = relationUIDs

	document, err := Render(frontMatter, memo.Content)
	if err != nil {
		slog.Warn("failed to render memo file", slog.String("uid", memo.UID), slog.Any("err", err))
		return []byte(memo.Content)
	}
	return document
}

func (m *Mirror) mirrorAttachmentsLocked(ctx context.Context, memo *store.Memo) {
	attachments, err := m.Store.ListAttachments(ctx, &store.FindAttachment{MemoID: &memo.ID, GetBlob: true})
	if err != nil {
		slog.Warn("failed to load attachment blobs", slog.String("uid", memo.UID), slog.Any("err", err))
		return
	}
	for _, attachment := range attachments {
		if len(attachment.Blob) == 0 {
			continue
		}
		path := filepath.Join(m.dir, filepath.FromSlash(AttachmentDir(attachment.UID)), safeFileName(attachment.Filename))
		if existing, err := os.ReadFile(path); err == nil && len(existing) == len(attachment.Blob) {
			continue
		}
		if err := writeFileAtomic(path, attachment.Blob, 0o644); err != nil {
			slog.Warn("failed to write attachment file", slog.String("uid", attachment.UID), slog.Any("err", err))
		}
	}
}

func (m *Mirror) usernameOf(ctx context.Context, userID int32) string {
	if userID <= 0 {
		return ""
	}
	user, err := m.Store.GetUser(ctx, &store.FindUser{ID: &userID})
	if err != nil || user == nil {
		return ""
	}
	return user.Username
}

func (m *Mirror) spaceUIDOf(ctx context.Context, spaceID *int32) string {
	if spaceID == nil {
		return ""
	}
	space, err := m.Store.GetSpace(ctx, &store.FindSpace{ID: spaceID})
	if err != nil || space == nil {
		return ""
	}
	return space.UID
}

// Sync reconciles the store with the files on disk. Store rows win for memos
// that are already mirrored: the file is rewritten from the store. Files the
// store has never seen are imported, and a file that changed since the last
// pass (same content hash, newer modification time) updates its memo.
func (m *Mirror) Sync(ctx context.Context) (*Report, error) {
	held := m.enter()
	defer m.leave(held)

	report := &Report{}
	byUID, err := m.loadMemosLocked(ctx)
	if err != nil {
		return nil, err
	}
	files, err := m.scanFilesLocked()
	if err != nil {
		return nil, err
	}

	// 1. Import files the store has never seen. A file whose memo is missing is
	// imported even when the sidecar index still remembers it, which is what
	// makes a deleted database recoverable from the notes on disk. Freshly
	// imported memos join byUID so pass 2 reconciles them like any other memo.
	if !m.ReadOnly {
		m.importUnownedLocked(ctx, report)
		for uid, file := range files {
			if _, known := byUID[uid]; known {
				continue
			}
			if _, pending := m.unowned[uid]; pending {
				continue
			}
			imported, err := m.importFileLocked(ctx, uid, file, report)
			if err != nil {
				report.Errors = append(report.Errors, err)
				continue
			}
			if imported != nil {
				byUID[imported.UID] = imported
			}
		}
	}

	// 2. Import local edits and mirror every memo the store holds.
	knownUIDs := make(map[string]struct{}, len(byUID))
	for uid, memo := range byUID {
		knownUIDs[uid] = struct{}{}
		if !m.ReadOnly {
			if file, ok := files[uid]; ok {
				if err := m.importLocalEditLocked(ctx, memo, file, report); err != nil {
					report.Errors = append(report.Errors, err)
				}
			}
		}
		if err := m.mirrorMemoLocked(ctx, memo); err != nil {
			report.Errors = append(report.Errors, err)
		} else {
			report.Written++
		}
	}

	// 3. Drop index rows for memos that no longer exist, and remove their files.
	for uid, entry := range m.index.Entries {
		if _, inStore := knownUIDs[uid]; inStore {
			continue
		}
		if entry != nil {
			if err := os.Remove(filepath.Join(m.dir, filepath.FromSlash(entry.Path))); err != nil && !os.IsNotExist(err) {
				report.Errors = append(report.Errors, err)
			}
		}
		delete(m.index.Entries, uid)
		report.Removed++
	}

	m.saveIndexLocked()
	return report, nil
}

// loadMemosLocked indexes the store's memos by uid.
func (m *Mirror) loadMemosLocked(ctx context.Context) (map[string]*store.Memo, error) {
	memos, err := m.Store.ListMemos(ctx, &store.FindMemo{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list memos")
	}
	byUID := make(map[string]*store.Memo, len(memos))
	for _, memo := range memos {
		byUID[memo.UID] = memo
	}
	return byUID, nil
}

// scanFilesLocked lists memo files by uid.
func (m *Mirror) scanFilesLocked() (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(m.dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == AttachmentsDirName {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(strings.ToLower(entry.Name()), FileExtension) {
			return nil
		}
		files[strings.TrimSuffix(entry.Name(), FileExtension)] = path
		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to scan memos directory")
	}
	return files, nil
}

func (m *Mirror) importFileLocked(ctx context.Context, uid string, path string, report *Report) (*store.Memo, error) {
	document, err := readDocument(path)
	if err != nil {
		return nil, err
	}
	creatorID := m.resolveCreatorID(ctx, document.FrontMatter.Creator)
	if creatorID == 0 {
		// No account exists yet: a memo needs an owner, so keep the file as is
		// and import it once the instance has a user.
		if _, known := m.unowned[uid]; !known {
			slog.Info("memo file has no owner yet, will import it after the first account is created", slog.String("path", path))
		}
		m.unowned[uid] = path
		return nil, nil
	}
	visibility := NormalizeVisibility(document.FrontMatter.Visibility)
	createdTs, err := ParseTime(document.FrontMatter.Created)
	if err != nil {
		if info, statErr := os.Stat(path); statErr == nil {
			createdTs = info.ModTime().Unix()
		}
	}
	updatedTs := createdTs
	if parsed, err := ParseTime(document.FrontMatter.Updated); err == nil {
		updatedTs = parsed
	}

	// The file's front matter names the memo; the file name only stands in when
	// the front matter does not. A file called 01-welcome.md that declares
	// `uid: ExampleWelcome` imports as memos/ExampleWelcome and is renamed to
	// match on the way out.
	memoUID := strings.TrimSpace(document.FrontMatter.UID)
	if memoUID == "" {
		memoUID = uid
	}
	if !ValidUID(memoUID) {
		memoUID = randomMemoUID()
	}
	if existing, err := m.Store.GetMemo(ctx, &store.FindMemo{UID: &memoUID, ExcludeContent: true}); err == nil && existing != nil {
		// A different file already owns this uid: import under a fresh one and
		// heal the file so the two never fight over the name.
		slog.Warn("memo file declares a uid that already exists, assigning a new one", slog.String("uid", memoUID), slog.String("path", path))
		memoUID = randomMemoUID()
	}

	create := &store.Memo{
		UID:        memoUID,
		CreatorID:  creatorID,
		CreatedTs:  createdTs,
		UpdatedTs:  updatedTs,
		Content:    document.Body,
		Visibility: visibility,
		RowStatus:  store.Normal,
	}
	if err := memopayload.RebuildMemoPayload(ctx, create, m.MarkdownService); err != nil {
		return nil, errors.Wrapf(err, "failed to index imported memo %s", memoUID)
	}
	// The body's hashtags were indexed above; front matter tags declared
	// alongside them are part of the memo too, so the payload carries both.
	create.Payload.Tags = mergeTags(derefTags(document.FrontMatter.Tags), create.Payload.GetTags())
	created, err := m.Store.CreateMemo(ctx, create)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to import memo %s", memoUID)
	}
	// Creation never pins a memo, so a file that asks for it needs one explicit
	// update after the row exists.
	if document.FrontMatter.Pinned != nil && *document.FrontMatter.Pinned && !created.Pinned {
		pinned := true
		if err := m.Store.UpdateMemo(ctx, &store.UpdateMemo{ID: created.ID, Pinned: &pinned}); err != nil {
			return nil, errors.Wrapf(err, "failed to pin imported memo %s", memoUID)
		}
		created.Pinned = true
	}
	m.importAttachmentsLocked(ctx, created, document.FrontMatter.Attachments)
	// The store write mirror normally handles this; do it here too so a rename
	// caused by a uid rewrite is reflected immediately.
	if err := m.mirrorMemoLocked(ctx, created); err != nil {
		return nil, err
	}
	report.Imported++
	if created.UID != uid {
		// The file name no longer matches the memo: the mirror wrote the
		// canonical file, so drop the original.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			report.Errors = append(report.Errors, err)
		}
	}
	delete(m.unowned, uid)
	delete(m.unowned, created.UID)
	return created, nil
}

// importUnownedLocked retries the files that had no owner on an earlier pass.
// It is what makes "copy notes into data/memos, then finish setup" work.
func (m *Mirror) importUnownedLocked(ctx context.Context, report *Report) {
	if len(m.unowned) == 0 || m.ReadOnly {
		return
	}
	if m.resolveCreatorID(ctx, "") == 0 {
		return
	}
	pending := make([]string, 0, len(m.unowned))
	for uid := range m.unowned {
		pending = append(pending, uid)
	}
	sort.Strings(pending)
	for _, uid := range pending {
		path := m.unowned[uid]
		if _, err := os.Stat(path); err != nil {
			delete(m.unowned, uid)
			continue
		}
		imported, err := m.importFileLocked(ctx, uid, path, report)
		if err != nil {
			report.Errors = append(report.Errors, err)
			continue
		}
		if imported == nil {
			// Still no owner: leave it queued for the next attempt.
			continue
		}
		if err := m.mirrorMemoLocked(ctx, imported); err != nil {
			report.Errors = append(report.Errors, err)
		}
	}
}

func (m *Mirror) importLocalEditLocked(ctx context.Context, memo *store.Memo, path string, report *Report) error {
	info, err := os.Stat(path)
	if err != nil {
		return errors.Wrap(err, "failed to stat memo file")
	}
	entry := m.index.Entries[memo.UID]
	if entry != nil && info.ModTime().UnixNano() <= entry.ModTime && info.Size() == entry.Size {
		report.Skipped++
		return nil
	}
	document, err := readDocument(path)
	if err != nil {
		return err
	}
	if hashString(document.Body) == hashString(memo.Content) {
		report.Skipped++
		return nil
	}

	update := &store.UpdateMemo{ID: memo.ID}
	tags := mergeTags(derefTags(document.FrontMatter.Tags), nil)
	if document.Body != memo.Content {
		update.Content = &document.Body
		payload := &store.Memo{}
		*payload = *memo
		payload.Content = document.Body
		if err := memopayload.RebuildMemoPayload(ctx, payload, m.MarkdownService); err != nil {
			return errors.Wrapf(err, "failed to index edited memo %s", memo.UID)
		}
		update.Payload = payload.Payload
		tags = mergeTags(derefTags(document.FrontMatter.Tags), payload.Payload.GetTags())
	}
	// Tags only change when the file says something about them, so a file
	// without a tags key keeps the tags the store already indexed.
	if hasTagSignal(document.FrontMatter.Tags, memo.Payload) && !equalTags(tags, memo.Payload.GetTags()) {
		payload := &storepb.MemoPayload{Property: memo.Payload.GetProperty(), Location: memo.Payload.GetLocation(), Tags: tags}
		update.Payload = payload
	}
	if parsed, err := ParseTime(document.FrontMatter.Updated); err == nil && parsed != memo.UpdatedTs {
		update.UpdatedTs = &parsed
	}
	if created, err := ParseTime(document.FrontMatter.Created); err == nil && created != memo.CreatedTs {
		update.CreatedTs = &created
	}
	if visibility := NormalizeVisibility(document.FrontMatter.Visibility); document.FrontMatter.Visibility != "" && visibility != memo.Visibility {
		update.Visibility = &visibility
	}
	if document.FrontMatter.Pinned != nil && *document.FrontMatter.Pinned != memo.Pinned {
		update.Pinned = document.FrontMatter.Pinned
	}
	if err := m.Store.UpdateMemo(ctx, update); err != nil {
		return errors.Wrapf(err, "failed to apply file edit for memo %s", memo.UID)
	}
	report.Updated++
	return nil
}

func (m *Mirror) importAttachmentsLocked(ctx context.Context, memo *store.Memo, attachmentUIDs []string) {
	for _, attachmentUID := range attachmentUIDs {
		dir := filepath.Join(m.dir, filepath.FromSlash(AttachmentDir(attachmentUID)))
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			content, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if existing, err := m.Store.GetAttachment(ctx, &store.FindAttachment{UID: &attachmentUID}); err == nil && existing != nil {
				continue
			}
			attachment := &store.Attachment{
				UID:       attachmentUID,
				CreatorID: memo.CreatorID,
				CreatedTs: memo.CreatedTs,
				UpdatedTs: memo.UpdatedTs,
				Filename:  entry.Name(),
				Type:      contentTypeOf(entry.Name()),
				Size:      int64(len(content)),
				Blob:      content,
				MemoID:    &memo.ID,
			}
			if _, err := m.Store.CreateAttachment(ctx, attachment); err != nil {
				slog.Warn("failed to import attachment file", slog.String("uid", attachmentUID), slog.Any("err", err))
			}
		}
	}
}

func (m *Mirror) resolveCreatorID(ctx context.Context, username string) int32 {
	if username != "" {
		if user, err := m.Store.GetUser(ctx, &store.FindUser{Username: &username}); err == nil && user != nil {
			return user.ID
		}
	}
	users, err := m.Store.ListUsers(ctx, &store.FindUser{})
	if err != nil || len(users) == 0 {
		return 0
	}
	// Prefer an administrator so an imported memo is owned by the instance
	// owner rather than whichever account happens to sort first.
	sorted := make([]*store.User, len(users))
	copy(sorted, users)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Role == store.RoleAdmin && sorted[j].Role != store.RoleAdmin })
	return sorted[0].ID
}

func readDocument(path string) (*Document, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read memo file")
	}
	document, err := Parse(content)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse %s", path)
	}
	return document, nil
}

func tagNames(memo *store.Memo) []string {
	if memo.Payload == nil {
		return []string{}
	}
	normalized := NormalizeTags(memo.Payload.Tags)
	if normalized == nil {
		return []string{}
	}
	return normalized
}

// hasTagSignal reports whether a file says anything about tags: either it
// declares them in front matter, or its body contains hashtags that the store
// already indexed.
func hasTagSignal(frontMatterTags *[]string, payload *storepb.MemoPayload) bool {
	if frontMatterTags != nil {
		return true
	}
	return len(payload.GetTags()) > 0
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func writeFileAtomic(path string, content []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errors.Wrap(err, "failed to create directory")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return errors.Wrap(err, "failed to create temporary file")
	}
	tempName := temp.Name()
	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return errors.Wrap(err, "failed to write temporary file")
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return errors.Wrap(err, "failed to close temporary file")
	}
	if err := os.Chmod(tempName, perm); err != nil {
		_ = os.Remove(tempName)
		return errors.Wrap(err, "failed to set file mode")
	}
	if err := os.Rename(tempName, path); err != nil {
		_ = os.Remove(tempName)
		return errors.Wrap(err, "failed to replace file")
	}
	return nil
}

func safeFileName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "file"
	}
	replaced := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		}
		if r < 0x20 {
			return '-'
		}
		return r
	}, base)
	if replaced == "" {
		return "file"
	}
	return replaced
}
