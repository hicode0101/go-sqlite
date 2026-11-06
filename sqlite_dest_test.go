package sqlite

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

type destUser struct {
	ID    uint   `gorm:"primaryKey;autoIncrement"`
	Email string `gorm:"size:100;uniqueIndex"`
}

func (destUser) TableName() string { return "dest_users" }

// TestConcurrentFirstDestMustBeZeroed documents a GORM semantic that
// concurrent code must respect: when the destination struct already holds
// a non-zero primary key, gorm adds it to the query's WHERE clause. A
// worker goroutine that reuses one dest variable across reads therefore
// turns every non-consecutive read into an impossible
// "id = new AND id = previous" condition and gets ErrRecordNotFound.
//
// The test pins both the failure mode and the correct pattern (zero the
// dest before every read).
func TestConcurrentFirstDestMustBeZeroed(t *testing.T) {
	db := openWALDB(t, "dest.db")
	if err := db.AutoMigrate(&destUser{}); err != nil {
		t.Fatal(err)
	}
	rows := make([]destUser, 100)
	for i := range rows {
		rows[i] = destUser{Email: fmt.Sprintf("u%d@example.com", i)}
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	// Wrong pattern: one dest reused across reads.
	var u destUser
	if err := db.First(&u, 1).Error; err != nil {
		t.Fatalf("First(1) failed: %v", err)
	}
	err := db.First(&u, 2).Error // u.ID == 1 -> "id = 2 AND id = 1"
	if err == nil {
		t.Fatal("expected the reused-dest read to fail (GORM adds the scanned PK to WHERE)")
	}

	// Correct pattern: zero the dest before every read.
	var seq atomic.Int64
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				id := seq.Add(1)%100 + 1
				var v destUser // zeroed dest
				if err := db.First(&v, id).Error; err != nil {
					errCh <- fmt.Errorf("First(%d): %w", id, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
