package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

	return &ProfileStore{db: db}, nil
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
	return ps.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(profileBucket).Delete(profileUserIDKey(userID))
	})
}
