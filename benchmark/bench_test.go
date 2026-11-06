package bench

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"sync"

	"sync/atomic"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// BenchUser is the shared fixture: a plain table with a unique index,
// sized like a realistic application row.
type BenchUser struct {
	ID     uint   `gorm:"primaryKey;autoIncrement"`
	Name   string `gorm:"size:100"`
	Email  string `gorm:"size:100;uniqueIndex"`
	Age    int
	Active bool
}

func (BenchUser) TableName() string { return "bench_users" }

// --- setup helpers ---

// newMemDB opens a fresh in-memory database. ":memory:" databases are
// per-connection, so the pool is capped at one connection — this keeps the
// sequential benchmarks purely CPU-bound and comparable.
func newMemDB(b *testing.B) *gorm.DB {
	b.Helper()
	db, err := gorm.Open(newDialector(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		b.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&BenchUser{}); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	return db
}

// walDSNForDebug stashes the last WAL DSN so failure diagnostics can open
// a fresh, independent connection pool on the same file.
var walDSNForDebug atomic.Value // string

// newSharedDB builds a fresh file database in the default (DELETE) journal
// mode and returns a fully pre-opened, verified pool for the benchmark
// phase. DELETE mode is used instead of WAL on purpose: the parallel
// benchmarks were flaky under WAL on Windows (a pooled connection could
// keep serving a stale snapshot of the seeded rows), while DELETE mode has
// no snapshot semantics at all — every reader takes a shared lock and sees
// the latest committed state. Writes serialize identically under both
// modes, so the comparison stays fair across drivers.
func newSharedDB(b *testing.B, seed int) *gorm.DB {
	b.Helper()
	path := filepath.ToSlash(filepath.Join(b.TempDir(), "bench.db"))
	dsn := path + "?_pragma=busy_timeout(30000)"
	walDSNForDebug.Store(dsn)

	// Phase 1: build schema and data through one dedicated connection.
	setup, err := gorm.Open(newDialector(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		b.Fatalf("open setup db: %v", err)
	}
	setupSQL, err := setup.DB()
	if err != nil {
		b.Fatal(err)
	}
	setupSQL.SetMaxOpenConns(1)
	if err := setup.AutoMigrate(&BenchUser{}); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	if seed > 0 {
		seedUsers(b, setup, seed)
	}
	if err := setupSQL.Close(); err != nil {
		b.Fatalf("close setup db: %v", err)
	}

	// Phase 2: benchmark pool.
	return openBenchPool(b, dsn, int64(seed))
}

// openBenchPool opens the shared read/write pool on an already-built
// database and verifies every pooled connection sees the seeded rows.
func openBenchPool(b *testing.B, dsn string, seededRows int64) *gorm.DB {
	b.Helper()
	db, err := gorm.Open(newDialector(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		b.Fatalf("open pool: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		b.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(16)
	sqlDB.SetMaxIdleConns(16)
	// Registered after TempDir, so cleanup runs the pool close before the
	// directory removal — on Windows an open database file cannot be
	// unlinked.
	b.Cleanup(func() { _ = sqlDB.Close() })

	// Pre-open and verify every pooled connection: each must see the full
	// seeded data set before the benchmark starts.
	const conns = 16
	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := sqlDB.Conn(context.Background())
			if err != nil {
				b.Errorf("warm conn: %v", err)
				return
			}
			defer conn.Close()
			var n int64
			if err := conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM bench_users").Scan(&n); err != nil {
				b.Errorf("warm count: %v", err)
				return
			}
			if n != seededRows {
				b.Errorf("warm conn sees %d rows, want %d", n, seededRows)
			}
		}()
	}
	wg.Wait()
	return db
}

// seedUsers batch-inserts n rows and checkpoints the WAL, so every pooled
// connection sees the full data set no matter when it joined.
func seedUsers(b *testing.B, db *gorm.DB, n int) {
	b.Helper()
	const chunk = 500
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		rows := make([]BenchUser, 0, end-start)
		for i := start; i < end; i++ {
			rows = append(rows, BenchUser{
				Name: fmt.Sprintf("user%d", i), Email: fmt.Sprintf("user%d@example.com", i),
				Age: i % 80, Active: i%2 == 0,
			})
		}
		if err := db.Create(&rows).Error; err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	if err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		b.Fatalf("checkpoint: %v", err)
	}
}

var rawRowID atomic.Int64 // unique-email counter shared by raw benchmarks

// --- sequential write benchmarks (in-memory) ---

func BenchmarkInsert(b *testing.B) {
	db := newMemDB(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		u := BenchUser{Name: fmt.Sprintf("user%d", i), Email: fmt.Sprintf("i%d@example.com", i), Age: i % 80}
		if err := db.Create(&u).Error; err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInsertBatch100(b *testing.B) {
	db := newMemDB(b)
	var base atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := base.Add(100) - 100
		rows := make([]BenchUser, 100)
		for j := range rows {
			rows[j] = BenchUser{
				Name:  fmt.Sprintf("user%d", start+int64(j)),
				Email: fmt.Sprintf("b%d_%d@example.com", i, j),
				Age:   j % 80,
			}
		}
		if err := db.Create(&rows).Error; err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTxInsert100(b *testing.B) {
	db := newMemDB(b)
	var base atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := db.Transaction(func(tx *gorm.DB) error {
			for j := 0; j < 100; j++ {
				u := BenchUser{
					Name:  fmt.Sprintf("user%d", base.Load()),
					Email: fmt.Sprintf("t%d_%d@example.com", i, j),
					Age:   j % 80,
				}
				base.Add(1)
				if err := tx.Create(&u).Error; err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUpdate(b *testing.B) {
	db := newMemDB(b)
	seedUsers(b, db, 100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Model(&BenchUser{}).Where("id = ?", i%100+1).Update("age", i).Error; err != nil {
			b.Fatal(err)
		}
	}
}

// --- sequential read benchmarks (in-memory) ---

func BenchmarkSelectByPK(b *testing.B) {
	db := newMemDB(b)
	seedUsers(b, db, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var u BenchUser
		if err := db.First(&u, i%1000+1).Error; err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSelect100(b *testing.B) {
	db := newMemDB(b)
	seedUsers(b, db, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var rows []BenchUser
		if err := db.Where("id > ?", i%900).Order("id").Limit(100).Find(&rows).Error; err != nil {
			b.Fatal(err)
		}
	}
}

// --- raw database/sql benchmarks (driver-level floor, in-memory) ---

func BenchmarkRawInsert(b *testing.B) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec("CREATE TABLE bench_users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, email TEXT UNIQUE, age INTEGER, active NUMERIC)"); err != nil {
		b.Fatal(err)
	}
	stmt, err := sqlDB.Prepare("INSERT INTO bench_users (name, email, age, active) VALUES (?, ?, ?, ?)")
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := rawRowID.Add(1)
		if _, err := stmt.Exec(fmt.Sprintf("user%d", id), fmt.Sprintf("raw%d@example.com", id), i%80, i%2 == 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRawSelectByPK(b *testing.B) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec("CREATE TABLE bench_users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, email TEXT UNIQUE, age INTEGER, active NUMERIC)"); err != nil {
		b.Fatal(err)
	}
	tx, err := sqlDB.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 1000; i++ {
		if _, err := tx.Exec("INSERT INTO bench_users (name, email, age, active) VALUES (?, ?, ?, ?)", fmt.Sprintf("user%d", i), fmt.Sprintf("user%d@example.com", i), i%80, i%2 == 0); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	stmt, err := sqlDB.Prepare("SELECT id, name, email, age, active FROM bench_users WHERE id = ?")
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	var (
		id     int64
		name   string
		email  string
		age    int64
		active bool
	)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := stmt.QueryRow(i%1000+1).Scan(&id, &name, &email, &age, &active); err != nil {
			b.Fatal(err)
		}
	}
}

// --- parallel benchmarks (shared file database, per-worker sessions) ---

func BenchmarkParallelReads(b *testing.B) {
	db := newSharedDB(b, 1000)
	var seq atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// One goroutine-local session per worker: the documented safe
		// pattern for sharing a *gorm.DB across goroutines.
		tx := db.Session(&gorm.Session{NewDB: true})
		for pb.Next() {
			id := seq.Add(1)%1000 + 1
			// Fresh dest per iteration: a reused dest carries its scanned
			// primary key into the next query's WHERE clause, which turns
			// every non-consecutive read into a contradiction.
			var u BenchUser
			if err := tx.First(&u, id).Error; err != nil {
				b.Errorf("First(%d): %v", id, err)
				return
			}
		}
	})
}

func BenchmarkParallelWrites(b *testing.B) {
	db := newSharedDB(b, 0)
	var seq atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		tx := db.Session(&gorm.Session{NewDB: true})
		for pb.Next() {
			id := seq.Add(1)
			u := BenchUser{
				Name:  fmt.Sprintf("user%d", id),
				Email: fmt.Sprintf("p%d@example.com", id),
				Age:   int(id) % 80,
			}
			if err := tx.Create(&u).Error; err != nil {
				b.Errorf("Create(%d): %v", id, err)
				return
			}
		}
	})
}

func BenchmarkParallelMixed(b *testing.B) {
	db := newSharedDB(b, 1000)
	var seq atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		tx := db.Session(&gorm.Session{NewDB: true})
		for pb.Next() {
			id := seq.Add(1)
			if id%4 == 0 {
				// every 4th op writes
				wu := BenchUser{
					Name:  fmt.Sprintf("user%d", id),
					Email: fmt.Sprintf("m%d@example.com", id),
				}
				if err := tx.Create(&wu).Error; err != nil {
					b.Errorf("mixed Create(%d): %v", id, err)
					return
				}
			} else {
				var u BenchUser // fresh dest, see BenchmarkParallelReads
				if err := tx.First(&u, id%1000+1).Error; err != nil {
					b.Errorf("mixed First(%d): %v", id, err)
					return
				}
			}
		}
	})
}
