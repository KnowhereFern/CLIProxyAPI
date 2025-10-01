package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ConvBoltPath returns the BoltDB file path used for both account metadata and conversation data.
// Different logical datasets are kept in separate buckets within this single DB file.
func ConvBoltPath(tokenFilePath string) string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		wd = "."
	}
	convDir := filepath.Join(wd, "conv")
	base := strings.TrimSuffix(filepath.Base(tokenFilePath), filepath.Ext(tokenFilePath))
	return filepath.Join(convDir, base+".bolt")
}

// LoadConvStore reads the account-level metadata store from disk.
func LoadConvStore(path string) (map[string][]string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	out := map[string][]string{}
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("account_meta"))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var arr []string
			if len(v) > 0 {
				if e := json.Unmarshal(v, &arr); e != nil {
					// Skip malformed entries instead of failing the whole load
					return nil
				}
			}
			out[string(k)] = arr
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SaveConvStore writes the account-level metadata store to disk atomically.
func SaveConvStore(path string, data map[string][]string) error {
	if data == nil {
		data = map[string][]string{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return db.Update(func(tx *bolt.Tx) error {
		// Recreate bucket to reflect the given snapshot exactly.
		if b := tx.Bucket([]byte("account_meta")); b != nil {
			if err = tx.DeleteBucket([]byte("account_meta")); err != nil {
				return err
			}
		}
		b, errCreateBucket := tx.CreateBucket([]byte("account_meta"))
		if errCreateBucket != nil {
			return errCreateBucket
		}
		for k, v := range data {
			enc, e := json.Marshal(v)
			if e != nil {
				return e
			}
			if e = b.Put([]byte(k), enc); e != nil {
				return e
			}
		}
		return nil
	})
}

// LoadConvData reads the full conversation data and index from disk.
func LoadConvData(path string) (map[string]ConversationRecord, map[string]string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = db.Close() }()
	items := map[string]ConversationRecord{}
	index := map[string]string{}
	err = db.View(func(tx *bolt.Tx) error {
		// Load conv_items
		if b := tx.Bucket([]byte("conv_items")); b != nil {
			if e := b.ForEach(func(k, v []byte) error {
				var rec ConversationRecord
				if len(v) > 0 {
					if e2 := json.Unmarshal(v, &rec); e2 != nil {
						// Skip malformed
						return nil
					}
					items[string(k)] = rec
				}
				return nil
			}); e != nil {
				return e
			}
		}
		// Load conv_index
		if b := tx.Bucket([]byte("conv_index")); b != nil {
			if e := b.ForEach(func(k, v []byte) error {
				index[string(k)] = string(v)
				return nil
			}); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return items, index, nil
}

// SaveConvData writes the full conversation data and index to disk atomically.
func SaveConvData(path string, items map[string]ConversationRecord, index map[string]string) error {
	if items == nil {
		items = map[string]ConversationRecord{}
	}
	if index == nil {
		index = map[string]string{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return db.Update(func(tx *bolt.Tx) error {
		// Recreate items bucket
		if b := tx.Bucket([]byte("conv_items")); b != nil {
			if err = tx.DeleteBucket([]byte("conv_items")); err != nil {
				return err
			}
		}
		bi, errCreateBucket := tx.CreateBucket([]byte("conv_items"))
		if errCreateBucket != nil {
			return errCreateBucket
		}
		for k, rec := range items {
			enc, e := json.Marshal(rec)
			if e != nil {
				return e
			}
			if e = bi.Put([]byte(k), enc); e != nil {
				return e
			}
		}

		// Recreate index bucket
		if b := tx.Bucket([]byte("conv_index")); b != nil {
			if err = tx.DeleteBucket([]byte("conv_index")); err != nil {
				return err
			}
		}
		bx, errCreateBucket := tx.CreateBucket([]byte("conv_index"))
		if errCreateBucket != nil {
			return errCreateBucket
		}
		for k, v := range index {
			if e := bx.Put([]byte(k), []byte(v)); e != nil {
				return e
			}
		}
		return nil
	})
}
