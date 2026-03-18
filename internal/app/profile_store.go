package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var profileBucket = []byte("profiles")

// UserProfile holds concise factual tags about a Telegram user.
// Only users with a TG username are stored (unique identifier).
type UserProfile struct {
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Facts       []string  `json:"facts"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ProfileStore persists user profiles in a bbolt (NoSQL) key-value database.
type ProfileStore struct {
	db *bolt.DB

	mu          sync.Mutex
	msgCount    map[string]int       // username → messages since last extraction
	lastExtract map[string]time.Time // username → last extraction timestamp
	extracting  sync.Map             // username → bool (prevent concurrent extractions)
}

// NewProfileStore opens (or creates) the bbolt database at the given path.
func NewProfileStore(path string) (*ProfileStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create profile db dir: %w", err)
	}

	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open profile db: %w", err)
	}

	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(profileBucket)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("init profile bucket: %w", err)
	}

	return &ProfileStore{
		db:          db,
		msgCount:    make(map[string]int),
		lastExtract: make(map[string]time.Time),
	}, nil
}

// Close shuts down the underlying bbolt database.
func (ps *ProfileStore) Close() error {
	return ps.db.Close()
}

// Get retrieves a user profile by username. Returns nil, nil if not found.
func (ps *ProfileStore) Get(username string) (*UserProfile, error) {
	var profile *UserProfile
	err := ps.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(profileBucket)
		data := b.Get([]byte(username))
		if data == nil {
			return nil
		}
		profile = &UserProfile{}
		return json.Unmarshal(data, profile)
	})
	return profile, err
}

// Save persists a user profile keyed by Username.
func (ps *ProfileStore) Save(profile *UserProfile) error {
	data, err := json.Marshal(profile)
	if err != nil {
		return fmt.Errorf("marshal profile: %w", err)
	}
	return ps.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(profileBucket).Put([]byte(profile.Username), data)
	})
}

// Delete removes a user profile by username.
func (ps *ProfileStore) Delete(username string) error {
	ps.mu.Lock()
	delete(ps.msgCount, username)
	delete(ps.lastExtract, username)
	ps.mu.Unlock()
	ps.extracting.Delete(username)

	return ps.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(profileBucket).Delete([]byte(username))
	})
}

// ShouldExtract increments the per-user message counter and decides whether
// to trigger profile extraction.
//
// Extraction is triggered when:
//  1. The user has no profile yet (first interaction), OR
//  2. msgCount >= threshold AND minInterval has elapsed since last extraction.
//
// It also prevents triggering if an extraction goroutine is already running.
func (ps *ProfileStore) ShouldExtract(username string, threshold int, minInterval time.Duration) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.msgCount[username]++

	// Don't trigger if already running.
	if _, running := ps.extracting.Load(username); running {
		return false
	}

	last, hasExtracted := ps.lastExtract[username]
	if !hasExtracted {
		// Never extracted before — check if a stored profile already exists.
		profile, err := ps.Get(username)
		if err != nil {
			log.Printf("[profile] db read in ShouldExtract: %v", err)
			return false
		}
		if profile == nil || len(profile.Facts) == 0 {
			return true // no profile yet → extract immediately
		}
		// Profile exists (e.g. from a previous run) but we haven't extracted
		// this session. Fall through to threshold check.
	}

	if ps.msgCount[username] >= threshold && time.Since(last) >= minInterval {
		return true
	}
	return false
}

// MarkExtracting acquires a per-user lock to prevent duplicate goroutines.
// Returns false if extraction is already running for this user.
func (ps *ProfileStore) MarkExtracting(username string) bool {
	_, loaded := ps.extracting.LoadOrStore(username, true)
	return !loaded
}

// DoneExtracting releases the per-user lock and resets counters.
func (ps *ProfileStore) DoneExtracting(username string) {
	ps.mu.Lock()
	ps.msgCount[username] = 0
	ps.lastExtract[username] = time.Now()
	ps.mu.Unlock()
	ps.extracting.Delete(username)
}
