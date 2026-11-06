// Command example demonstrates github.com/hicode0101/go-sqlite usage with
// GORM: pure-Go, CGO-free SQLite.
//
// Run it with:
//
//	go run ./example
package main

import (
	"errors"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	sqlite "github.com/hicode0101/go-sqlite"
)

// User demonstrates a normal model.
type User struct {
	ID        uint   `gorm:"primaryKey;autoIncrement"`
	Name      string `gorm:"size:100;not null"`
	Email     string `gorm:"size:100;uniqueIndex"`
	Age       int
	Active    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName pins the table name instead of relying on pluralization.
func (User) TableName() string { return "users" }

// Post demonstrates a belongs-to relation; GORM renders the foreign key as
// a plain column (SQLite enforces it only when _pragma=foreign_keys(1)).
type Post struct {
	ID       uint   `gorm:"primaryKey;autoIncrement"`
	Title    string `gorm:"size:200"`
	AuthorID uint
	Author   *User `gorm:"foreignKey:AuthorID"`
}

func main() {
	// Enable foreign keys and a busy timeout through DSN PRAGMAs.
	dsn := "file:demo.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Warn),
		TranslateError: true, // map driver errors to gorm.Err* sentinels
	})
	if err != nil {
		log.Fatalf("open: %v", err)
	}

	// Schema migration.
	if err := db.AutoMigrate(&User{}, &Post{}); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	// --- Create ---
	alice := User{Name: "Alice", Email: "alice@example.com", Age: 28, Active: true}
	if err := db.Create(&alice).Error; err != nil {
		log.Fatalf("create user: %v", err)
	}
	fmt.Printf("created user id=%d\n", alice.ID)

	// Batch create: IDs are backfilled for every row.
	users := []User{
		{Name: "Bob", Email: "bob@example.com", Age: 35},
		{Name: "Carol", Email: "carol@example.com", Age: 41},
	}
	if err := db.Create(&users).Error; err != nil {
		log.Fatalf("batch create: %v", err)
	}
	fmt.Printf("batch ids: %d %d\n", users[0].ID, users[1].ID)

	// --- Query with joins ---
	var post Post
	if err := db.Where(Post{AuthorID: alice.ID}).FirstOrCreate(&post, Post{
		Title:    "Hello SQLite",
		AuthorID: alice.ID,
	}).Error; err != nil {
		log.Fatalf("create post: %v", err)
	}
	var loaded Post
	if err := db.Preload("Author").First(&loaded, post.ID).Error; err != nil {
		log.Fatalf("preload: %v", err)
	}
	fmt.Printf("post %q by %s\n", loaded.Title, loaded.Author.Name)

	// --- Update ---
	if err := db.Model(&alice).Update("age", 29).Error; err != nil {
		log.Fatalf("update: %v", err)
	}

	// --- Error translation ---
	dup := User{Name: "Dup", Email: "alice@example.com"}
	err = db.Create(&dup).Error
	fmt.Println("duplicate email ->", errors.Is(err, gorm.ErrDuplicatedKey)) // true

	// --- Transaction with rollback ---
	_ = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&User{Name: "Ghost", Email: "ghost@example.com"}).Error; err != nil {
			return err
		}
		return errors.New("force rollback")
	})
	var n int64
	db.Model(&User{}).Where("name = ?", "Ghost").Count(&n)
	fmt.Println("ghost rows after rollback:", n) // 0

	// --- Aggregations ---
	var avg float64
	db.Model(&User{}).Where("active = ?", true).Select("avg(age)").Scan(&avg)
	fmt.Printf("avg age of active users: %.1f\n", avg)

	// --- Table inventory via the migrator ---
	m := db.Migrator()
	tables, _ := m.GetTables()
	fmt.Println("tables:", tables)
	fmt.Println("has index idx_users_email:", m.HasIndex(&User{}, "idx_users_email"))
}
