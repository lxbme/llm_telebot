package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var profileBucket = []byte("profiles")

// UserProfile holds concise factual tags about a Telegram user.
type UserProfile struct {
	UserID      int64     `json:"user_id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Facts       []string  `json:"facts"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ProfileStore persists user profiles in a bbolt (NoSQL) key-value database.
type ProfileStore struct {
	db *bolt.DB

	mu          sync.Mutex
	msgCount    map[int64]int       // userID → messages since last extraction
	lastExtract map[int64]time.Time // userID → last extraction timestamp
	extracting  sync.Map            // userID → bool (prevent concurrent extractions)
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
		msgCount:    make(map[int64]int),
		lastExtract: make(map[int64]time.Time),
	}, nil
}

// Close shuts down the underlying bbolt database.
func (ps *ProfileStore) Close() error {
	return ps.db.Close()
}

func profileUserIDKey(userID int64) []byte {
	return []byte(strconv.FormatInt(userID, 10))
}

// Get retrieves a user profile by user ID. Returns nil, nil if not found.
func (ps *ProfileStore) Get(userID int64) (*UserProfile, error) {
	if userID == 0 {
		return nil, nil
	}
	var profile *UserProfile
	err := ps.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(profileBucket)
		data := b.Get(profileUserIDKey(userID))
		if data == nil {
			return nil
		}
		profile = &UserProfile{}
		return json.Unmarshal(data, profile)
	})
	return profile, err
}

// ListUserIDs returns all user IDs that have stored profiles.
func (ps *ProfileStore) ListUserIDs() ([]int64, error) {
	var ids []int64
	err := ps.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(profileBucket).ForEach(func(k, v []byte) error {
			userID, err := strconv.ParseInt(string(k), 10, 64)
			if err == nil {
				ids = append(ids, userID)
			}
			return nil
		})
	})
	return ids, err
}

// Save persists a user profile keyed by user ID.
func (ps *ProfileStore) Save(profile *UserProfile) error {
	if profile == nil || profile.UserID == 0 {
		return fmt.Errorf("profile user id is required")
	}
	data, err := json.Marshal(profile)
	if err != nil {
		return fmt.Errorf("marshal profile: %w", err)
	}
	return ps.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(profileBucket).Put(profileUserIDKey(profile.UserID), data)
	})
}

// Delete removes a user profile by user ID.
func (ps *ProfileStore) Delete(userID int64) error {
	if userID == 0 {
		return nil
	}
	ps.mu.Lock()
	delete(ps.msgCount, userID)
	delete(ps.lastExtract, userID)
	ps.mu.Unlock()
	ps.extracting.Delete(userID)

	return ps.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(profileBucket).Delete(profileUserIDKey(userID))
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
func (ps *ProfileStore) ShouldExtract(userID int64, threshold int, minInterval time.Duration) bool {
	if userID == 0 {
		return false
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.msgCount[userID]++

	// Don't trigger if already running.
	if _, running := ps.extracting.Load(userID); running {
		return false
	}

	last, hasExtracted := ps.lastExtract[userID]
	if !hasExtracted {
		// Never extracted before — check if a stored profile already exists.
		profile, err := ps.Get(userID)
		if err != nil {
			log.Printf("[profile] db read in ShouldExtract user=%d: %v", userID, err)
			return false
		}
		if profile == nil || len(profile.Facts) == 0 {
			return true // no profile yet → extract immediately
		}
		// Profile exists (e.g. from a previous run) but we haven't extracted
		// this session. Fall through to threshold check.
	}

	if ps.msgCount[userID] >= threshold && time.Since(last) >= minInterval {
		return true
	}
	return false
}

// MarkExtracting acquires a per-user lock to prevent duplicate goroutines.
// Returns false if extraction is already running for this user.
func (ps *ProfileStore) MarkExtracting(userID int64) bool {
	if userID == 0 {
		return false
	}
	_, loaded := ps.extracting.LoadOrStore(userID, true)
	return !loaded
}

// DoneExtracting releases the per-user lock and resets counters.
func (ps *ProfileStore) DoneExtracting(userID int64) {
	if userID == 0 {
		return
	}
	ps.mu.Lock()
	ps.msgCount[userID] = 0
	ps.lastExtract[userID] = time.Now()
	ps.mu.Unlock()
	ps.extracting.Delete(userID)
}
