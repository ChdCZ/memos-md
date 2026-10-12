package store

import (
	"context"
	"sync"
	"time"

	"github.com/usememos/memos/internal/profile"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/provider/storage"
	"github.com/usememos/memos/store/cache"
)

// MemoMirror receives every memo the store successfully writes or deletes so an
// additional storage surface can be kept in step with the database. It is
// optional: a nil mirror keeps the store a pure database store, which is how
// every build except the markdown-file one runs.
type MemoMirror interface {
	// MirrorMemo persists the current state of one memo.
	MirrorMemo(ctx context.Context, memo *Memo)
	// RemoveMemo drops the persisted copy of a memo that no longer exists.
	RemoveMemo(ctx context.Context, uid string)
	// StoreChanged reports that a write the mirror does not model itself
	// happened, such as the first account being created. A mirror that has
	// files waiting for an owner imports them here.
	StoreChanged(ctx context.Context)
}

// Store provides database access to all raw objects.
type Store struct {
	profile *profile.Profile
	driver  Driver

	mirrorMu sync.RWMutex
	mirror   MemoMirror

	userCreateMu   sync.Mutex
	authConfigMu   sync.Mutex
	refreshTokenMu sync.Mutex
	patMu          sync.Mutex
	memoViewMu     sync.Mutex

	deploymentConfigMu sync.RWMutex
	deploymentConfig   *deploymentConfiguration

	// Cache settings
	cacheConfig cache.Config

	// Caches
	instanceSettingCache *cache.Cache // cache for instance settings
	userCache            *cache.Cache // cache for users
	userSettingCache     *cache.Cache // cache for user settings

	// storageDriverCache reuses object-storage clients across requests, keyed
	// by the resolved configuration. Reset whenever the STORAGE setting changes.
	storageDriverMu         sync.Mutex
	storageDriverGeneration uint64
	storageDriverCache      map[storageDriverCacheKey]storage.Driver
}

type deploymentConfiguration struct {
	identityProviders map[string]*storepb.IdentityProvider
	instanceSettings  map[storepb.InstanceSettingKey]*storepb.InstanceSetting
}

// New creates a new instance of Store.
func New(driver Driver, profile *profile.Profile) *Store {
	// Default cache settings
	cacheConfig := cache.Config{
		DefaultTTL:      10 * time.Minute,
		CleanupInterval: 5 * time.Minute,
		MaxItems:        1000,
		OnEviction:      nil,
	}

	store := &Store{
		driver:               driver,
		profile:              profile,
		cacheConfig:          cacheConfig,
		instanceSettingCache: cache.New(cacheConfig),
		userCache:            cache.New(cacheConfig),
		userSettingCache:     cache.New(cacheConfig),
		deploymentConfig: &deploymentConfiguration{
			identityProviders: map[string]*storepb.IdentityProvider{},
			instanceSettings:  map[storepb.InstanceSettingKey]*storepb.InstanceSetting{},
		},
	}

	return store
}

func (s *Store) GetDriver() Driver {
	return s.driver
}

// SetMemoMirror attaches a mirror that follows every memo write. Passing nil
// detaches the current mirror.
func (s *Store) SetMemoMirror(mirror MemoMirror) {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()
	s.mirror = mirror
}

// getMemoMirror returns the attached mirror, if any.
func (s *Store) getMemoMirror() MemoMirror {
	s.mirrorMu.RLock()
	defer s.mirrorMu.RUnlock()
	return s.mirror
}

// mirroredMemo refreshes a memo from the database and hands it to the mirror.
// Mirroring failures never fail the database write they follow: the file layer
// is a projection that the next sync repairs.
func (s *Store) mirroredMemo(ctx context.Context, memoID int32) {
	mirror := s.getMemoMirror()
	if mirror == nil || memoID <= 0 {
		return
	}
	memo, err := s.GetMemo(ctx, &FindMemo{ID: &memoID})
	if err != nil || memo == nil {
		return
	}
	mirror.MirrorMemo(ctx, memo)
}

// notifyMirror reports a write the mirror does not model as a memo mutation.
func (s *Store) notifyMirror(ctx context.Context) {
	if mirror := s.getMemoMirror(); mirror != nil {
		mirror.StoreChanged(ctx)
	}
}

// GetDataDir returns the store data directory.
func (s *Store) GetDataDir() string {
	return s.profile.Data
}

func (s *Store) Close() error {
	// Stop all cache cleanup goroutines
	s.instanceSettingCache.Close()
	s.userCache.Close()
	s.userSettingCache.Close()

	return s.driver.Close()
}
