package sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// openWALDB opens a file-backed database in WAL mode with a generous busy
// timeout — the setup under which SQLite allows concurrent readers to run
// alongside a single writer. All concurrency tests share it. Error
// translation is on so unique-key races surface as gorm.ErrDuplicatedKey.
func openWALDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.ToSlash(filepath.Join(dir, name))
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(15000)&_pragma=synchronous(NORMAL)"
	db, err := gorm.Open(Open(dsn), &gorm.Config{Logger: logger.Discard, TranslateError: true})
	if err != nil {
		t.Fatalf("open WAL db failed: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestConcurrentWrites hammers Create from many goroutines at once and
// asserts every write lands exactly once.
func TestConcurrentWrites(t *testing.T) {
	db := openWALDB(t, "writes.db")
	autoMigrateUsers(t, db)

	const (
		goroutines = 8
		perGo      = 25
	)
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGo; i++ {
				u := User{
					Name:  fmt.Sprintf("w%d-%d", g, i),
					Email: fmt.Sprintf("w%d-%d@example.com", g, i),
					Age:   g*100 + i,
				}
				if err := db.Create(&u).Error; err != nil {
					errCh <- fmt.Errorf("goroutine %d insert %d: %w", g, i, err)
					return
				}
				if u.ID == 0 {
					errCh <- fmt.Errorf("goroutine %d insert %d: ID not backfilled", g, i)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	var n int64
	db.Model(&User{}).Count(&n)
	if n != goroutines*perGo {
		t.Errorf("rows = %d, want %d", n, goroutines*perGo)
	}
}

// TestConcurrentReadsAndWrites runs readers in a continuous loop while
// writers insert; WAL snapshot isolation must keep readers error-free and
// the final row count must match the writes.
func TestConcurrentReadsAndWrites(t *testing.T) {
	db := openWALDB(t, "mixed.db")
	autoMigrateUsers(t, db)

	const (
		writers = 4
		perGo   = 20
		readers = 6
	)

	writerWG := sync.WaitGroup{}
	for g := 0; g < writers; g++ {
		writerWG.Add(1)
		go func(g int) {
			defer writerWG.Done()
			for i := 0; i < perGo; i++ {
				u := User{
					Name:  fmt.Sprintf("m%d-%d", g, i),
					Email: fmt.Sprintf("m%d-%d@example.com", g, i),
				}
				if err := db.Create(&u).Error; err != nil {
					t.Errorf("writer %d insert %d: %v", g, i, err)
					return
				}
			}
		}(g)
	}

	stop := make(chan struct{})
	readerErrs := make(chan error, readers)
	readerWG := sync.WaitGroup{}
	for r := 0; r < readers; r++ {
		readerWG.Add(1)
		go func(r int) {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var users []User
				if err := db.Where("name LIKE ?", "m%").Order("id").Limit(10).Find(&users).Error; err != nil {
					readerErrs <- fmt.Errorf("reader %d find: %w", r, err)
					return
				}
				var n int64
				if err := db.Model(&User{}).Where("name LIKE ?", "m%").Count(&n).Error; err != nil {
					readerErrs <- fmt.Errorf("reader %d count: %w", r, err)
					return
				}
			}
		}(r)
	}

	// Wait for the writers; readers keep running the whole time (this is
	// exactly the WAL read-while-write scenario).
	writerWG.Wait()
	close(stop)
	readerWG.Wait()
	close(readerErrs)

	for err := range readerErrs {
		t.Fatal(err)
	}

	var n int64
	db.Model(&User{}).Where("name LIKE ?", "m%").Count(&n)
	if n != writers*perGo {
		t.Errorf("final rows = %d, want %d", n, writers*perGo)
	}
}

// TestConcurrentTransactions runs one transaction per goroutine; SQLite
// serializes the writes and every transaction must commit in full.
func TestConcurrentTransactions(t *testing.T) {
	db := openWALDB(t, "tx.db")
	autoMigrateUsers(t, db)

	const (
		goroutines = 8
		perGo      = 10
	)
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			err := db.Transaction(func(tx *gorm.DB) error {
				for i := 0; i < perGo; i++ {
					u := User{
						Name:  fmt.Sprintf("tx%d-%d", g, i),
						Email: fmt.Sprintf("tx%d-%d@example.com", g, i),
					}
					if err := tx.Create(&u).Error; err != nil {
						return err
					}
				}
				// Touch one inserted row inside the same transaction.
				return tx.Model(&User{}).
					Where("name = ?", fmt.Sprintf("tx%d-0", g)).
					Update("age", g).Error
			})
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d tx: %w", g, err)
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	var n int64
	db.Model(&User{}).Count(&n)
	if n != goroutines*perGo {
		t.Errorf("rows = %d, want %d", n, goroutines*perGo)
	}
}

// TestConcurrentUniqueInsertRace sends many goroutines after the same
// unique key: exactly one insert wins, the rest must surface
// gorm.ErrDuplicatedKey (with TranslateError enabled).
func TestConcurrentUniqueInsertRace(t *testing.T) {
	db := openWALDB(t, "race.db")
	if err := db.AutoMigrate(&User{}); err != nil {
		t.Fatal(err)
	}

	const goroutines = 10
	var (
		wg        sync.WaitGroup
		wins      atomic.Int64
		dupErrors atomic.Int64
		otherErrs atomic.Int64
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.Create(&User{Name: "racer", Email: "racer@example.com"}).Error
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, gorm.ErrDuplicatedKey):
				dupErrors.Add(1)
			default:
				otherErrs.Add(1)
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if wins.Load() != 1 {
		t.Errorf("wins = %d, want exactly 1", wins.Load())
	}
	if dupErrors.Load() != goroutines-1 {
		t.Errorf("duplicate errors = %d, want %d", dupErrors.Load(), goroutines-1)
	}
	var n int64
	db.Model(&User{}).Where("email = ?", "racer@example.com").Count(&n)
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

// TestConcurrentSharedSession exercises the pattern of many goroutines
// sharing one *gorm.DB handle for reads and writes simultaneously.
func TestConcurrentSharedSession(t *testing.T) {
	db := openWALDB(t, "shared.db")
	autoMigrateUsers(t, db)

	// Seed a known row.
	seed := User{Name: "seed", Email: "seed@example.com", Age: 1}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatal(err)
	}

	const goroutines = 12
	var (
		wg   sync.WaitGroup
		errs atomic.Int64
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				var got User
				if err := db.First(&got, seed.ID).Error; err != nil {
					errs.Add(1)
					t.Errorf("g%d First: %v", g, err)
					return
				}
				if got.Age != 1 {
					errs.Add(1)
					t.Errorf("g%d got age %d", g, got.Age)
					return
				}
				var n int64
				if err := db.Model(&User{}).Count(&n).Error; err != nil {
					errs.Add(1)
					t.Errorf("g%d Count: %v", g, err)
					return
				}
				if err := db.Create(&User{
					Name:  fmt.Sprintf("s%d-%d", g, i),
					Email: fmt.Sprintf("s%d-%d@example.com", g, i),
				}).Error; err != nil {
					errs.Add(1)
					t.Errorf("g%d Create: %v", g, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	if errs.Load() != 0 {
		t.Fatalf("%d concurrent session errors", errs.Load())
	}
	var n int64
	db.Model(&User{}).Count(&n)
	if n != 1+goroutines*15 {
		t.Errorf("rows = %d, want %d", n, 1+goroutines*15)
	}
}

// TestConcurrentUpsertRace runs upserts (ON CONFLICT DO UPDATE) against the
// same unique key from many goroutines; every invocation must succeed and
// leave exactly one row.
func TestConcurrentUpsertRace(t *testing.T) {
	db := openWALDB(t, "upsert.db")
	if err := db.AutoMigrate(&User{}); err != nil {
		t.Fatal(err)
	}

	const (
		goroutines = 8
		rounds     = 5
	)
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				err := db.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "email"}},
					DoUpdates: clause.Assignments(map[string]interface{}{"age": g*rounds + r}),
				}).Create(&User{Name: "up", Email: "up@example.com", Age: g*rounds + r}).Error
				if err != nil {
					errCh <- fmt.Errorf("g%d r%d: %w", g, r, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	var (
		n   int64
		age int
	)
	db.Model(&User{}).Where("email = ?", "up@example.com").Count(&n)
	db.Model(&User{}).Where("email = ?", "up@example.com").Select("age").Scan(&age)
	if n != 1 {
		t.Errorf("rows = %d, want exactly 1", n)
	}
	// Whatever writer ran last, the stored age must stay inside the
	// range any goroutine could have written.
	if age < 0 || age > goroutines*rounds-1 {
		t.Errorf("final age = %d, want within [0, %d]", age, goroutines*rounds-1)
	}
}
