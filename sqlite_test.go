package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// User is the shared fixture model for CRUD and migration tests.
type User struct {
	ID        uint   `gorm:"primaryKey;autoIncrement"`
	Name      string `gorm:"size:100"`
	Email     string `gorm:"size:100;uniqueIndex"`
	Age       int
	Active    bool
	Bio       string
	Salary    float64
	CreatedAt time.Time
	UpdatedAt time.Time
}

func openTestDB(t *testing.T, dsn string, cfg *gorm.Config) *gorm.DB {
	t.Helper()
	if cfg == nil {
		cfg = &gorm.Config{}
	}
	cfg.Logger = logger.Discard
	db, err := gorm.Open(Open(dsn), cfg)
	if err != nil {
		t.Fatalf("gorm.Open(%q) failed: %v", dsn, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func autoMigrateUsers(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&User{}); err != nil {
		t.Fatalf("AutoMigrate failed: %v", err)
	}
}

// --- Dialector basics ---

func TestDialectorName(t *testing.T) {
	if got := (Dialector{}).Name(); got != "sqlite" {
		t.Errorf("Name() = %q, want sqlite", got)
	}
}

func TestOpenDialector(t *testing.T) {
	d := Open("test.db")
	if got := d.(*Dialector).DSN; got != "test.db" {
		t.Errorf("Open().DSN = %q, want test.db", got)
	}
}

func TestQuoteTo(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	dialector := db.Dialector.(*Dialector)
	for _, tc := range []struct{ in, want string }{
		{"name", "`name`"},
		{"na`me", "`na``me`"},
		{"na``me", "`na``me`"},
		{"users.id", "`users`.`id`"},
		{"`users`.`id`", "`users`.`id`"},
		{"`users.id`", "`users.id`"},
	} {
		var builder strings.Builder
		dialector.QuoteTo(&builder, tc.in)
		if got := builder.String(); got != tc.want {
			t.Errorf("QuoteTo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExplain(t *testing.T) {
	dialector := Open(":memory:")
	got := dialector.Explain("select * from users where name=? and age=?", "jinzhu", 18)
	want := `select * from users where name="jinzhu" and age=18`
	if got != want {
		t.Errorf("Explain = %q, want %q", got, want)
	}
}

func TestDataTypeOf(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	dialector := db.Dialector.(*Dialector)
	for _, tc := range []struct {
		field schema.Field
		want  string
	}{
		{schema.Field{DataType: schema.Bool}, "numeric"},
		{schema.Field{DataType: schema.Int}, "integer"},
		{schema.Field{DataType: schema.Uint}, "integer"},
		{schema.Field{DataType: schema.Int, AutoIncrement: true}, "integer PRIMARY KEY AUTOINCREMENT"},
		{schema.Field{DataType: schema.Float}, "real"},
		{schema.Field{DataType: schema.String}, "text"},
		{schema.Field{DataType: schema.Time}, "datetime"},
		{schema.Field{DataType: schema.Bytes}, "blob"},
	} {
		if got := dialector.DataTypeOf(&tc.field); got != tc.want {
			t.Errorf("DataTypeOf(%v) = %q, want %q", tc.field.DataType, got, tc.want)
		}
	}
}

func TestCompareVersion(t *testing.T) {
	for _, tc := range []struct {
		v1, v2 string
		want   int
	}{
		{"3.35.0", "3.35.0", 0},
		{"3.50.4", "3.35.0", 1},
		{"3.34.9", "3.35.0", -1},
		{"3.35", "3.35.0", 0},
	} {
		if got := compareVersion(tc.v1, tc.v2); got != tc.want {
			t.Errorf("compareVersion(%q, %q) = %d, want %d", tc.v1, tc.v2, got, tc.want)
		}
	}
}

// --- CRUD ---

func TestCreateAndQuery(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	u := User{Name: "alice", Email: "alice@example.com", Age: 30, Active: true, Salary: 99.5}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if u.ID == 0 {
		t.Fatal("expected auto-increment ID to be backfilled")
	}

	var got User
	if err := db.First(&got, u.ID).Error; err != nil {
		t.Fatalf("First failed: %v", err)
	}
	if got.Name != "alice" || got.Age != 30 || !got.Active || got.Salary != 99.5 {
		t.Errorf("unexpected row: %+v", got)
	}
}

func TestBatchCreateBackfillsIDs(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	users := []User{
		{Name: "u1", Email: "u1@example.com"},
		{Name: "u2", Email: "u2@example.com"},
		{Name: "u3", Email: "u3@example.com"},
	}
	if err := db.Create(&users).Error; err != nil {
		t.Fatalf("batch Create failed: %v", err)
	}
	// LastInsertIDReversed: IDs must be sequential per row, not the same.
	for i, u := range users {
		if u.ID == 0 {
			t.Errorf("users[%d].ID not backfilled", i)
		}
		if i > 0 && users[i-1].ID == u.ID {
			t.Errorf("users[%d] and users[%d] share ID %d", i-1, i, u.ID)
		}
	}
	var count int64
	db.Model(&User{}).Count(&count)
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	u := User{Name: "bob", Email: "bob@example.com", Age: 40}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if err := db.Model(&u).Update("age", 41).Error; err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	var got User
	db.First(&got, u.ID)
	if got.Age != 41 {
		t.Errorf("Age = %d, want 41", got.Age)
	}

	if err := db.Delete(&u).Error; err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	var n int64
	db.Model(&User{}).Count(&n)
	if n != 0 {
		t.Errorf("count after delete = %d, want 0", n)
	}
}

func TestFindWithLimitOffset(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)
	var users []User
	for i := 0; i < 10; i++ {
		users = append(users, User{Name: fmt.Sprintf("n%d", i), Email: fmt.Sprintf("n%d@example.com", i)})
	}
	if err := db.Create(&users).Error; err != nil {
		t.Fatalf("batch Create failed: %v", err)
	}

	var page []User
	if err := db.Limit(3).Offset(2).Order("id").Find(&page).Error; err != nil {
		t.Fatalf("Find failed: %v", err)
	}
	if len(page) != 3 || page[0].Name != "n2" {
		t.Errorf("unexpected page: %+v", page)
	}

	// Offset without limit must still emit "LIMIT -1 OFFSET n".
	var rest []User
	if err := db.Offset(8).Order("id").Find(&rest).Error; err != nil {
		t.Fatalf("offset-only Find failed: %v", err)
	}
	if len(rest) != 2 {
		t.Errorf("len(rest) = %d, want 2", len(rest))
	}
}

func TestClausesForUpdateIsDropped(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)
	var users []User
	// FOR UPDATE is unsupported by SQLite; the clause builder must drop it.
	if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).Find(&users).Error; err != nil {
		t.Fatalf("Find with locking clause failed: %v", err)
	}
}

func TestUpsertClause(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	u := User{Name: "up", Email: "dup@example.com", Age: 1}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "email"}},
		DoUpdates: clause.Assignments(map[string]interface{}{"age": 2}),
	}).Create(&User{Name: "up2", Email: "dup@example.com", Age: 2}).Error; err != nil {
		t.Fatalf("upsert failed: %v", err)
	}
	var got User
	db.Where("email = ?", "dup@example.com").First(&got)
	if got.Age != 2 || got.Name != "up" {
		t.Errorf("upsert result: %+v", got)
	}
}

// --- Transactions & savepoints ---

func TestTransaction(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&User{Name: "tx1", Email: "tx1@example.com"}).Error; err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback error")
	}
	var n int64
	db.Model(&User{}).Count(&n)
	if n != 0 {
		t.Errorf("count after rollback = %d, want 0", n)
	}

	_ = db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&User{Name: "tx2", Email: "tx2@example.com"}).Error
	})
	db.Model(&User{}).Count(&n)
	if n != 1 {
		t.Errorf("count after commit = %d, want 1", n)
	}
}

func TestSavePoint(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&User{Name: "sp", Email: "sp@example.com"}).Error; err != nil {
			return err
		}
		if err := tx.SavePoint("sp1").Error; err != nil {
			return err
		}
		if err := tx.Create(&User{Name: "sp2", Email: "sp2@example.com"}).Error; err != nil {
			return err
		}
		if err := tx.RollbackTo("sp1").Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("savepoint transaction failed: %v", err)
	}
	var names []string
	db.Model(&User{}).Order("id").Pluck("name", &names)
	if len(names) != 1 || names[0] != "sp" {
		t.Errorf("rows after rollback-to-savepoint: %v", names)
	}
}

// --- Error translation ---

func TestTranslateDuplicatedKey(t *testing.T) {
	db := openTestDB(t, ":memory:", &gorm.Config{TranslateError: true})
	autoMigrateUsers(t, db)

	if err := db.Create(&User{Name: "a", Email: "same@example.com"}).Error; err != nil {
		t.Fatalf("first Create failed: %v", err)
	}
	err := db.Create(&User{Name: "b", Email: "same@example.com"}).Error
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("expected gorm.ErrDuplicatedKey, got %v", err)
	}
}

func TestTranslateForeignKeyViolated(t *testing.T) {
	db := openTestDB(t, ":memory:?_pragma=foreign_keys(1)", &gorm.Config{TranslateError: true})
	if err := db.Exec(`CREATE TABLE authors (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE books (id INTEGER PRIMARY KEY AUTOINCREMENT, author_id INTEGER REFERENCES authors(id))`).Error; err != nil {
		t.Fatal(err)
	}

	type Book struct {
		ID       uint `gorm:"primaryKey;autoIncrement"`
		AuthorID uint
	}
	err := db.Create(&Book{AuthorID: 12345}).Error
	if !errors.Is(err, gorm.ErrForeignKeyViolated) {
		t.Fatalf("expected gorm.ErrForeignKeyViolated, got %v", err)
	}
}

// --- Migrator ---

func TestMigratorTableOperations(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	m := db.Migrator()
	if !m.HasTable(&User{}) {
		t.Error("HasTable(User) = false, want true")
	}
	if m.HasTable("nope") {
		t.Error("HasTable(nope) = true, want false")
	}
	tables, err := m.GetTables()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tb := range tables {
		if tb == "users" {
			found = true
		}
	}
	if !found {
		t.Errorf("GetTables missing users: %v", tables)
	}

	// RenameTable / DropTable round-trip.
	if err := m.RenameTable(&User{}, "users_old"); err != nil {
		t.Fatalf("RenameTable failed: %v", err)
	}
	if !m.HasTable("users_old") {
		t.Error("renamed table missing")
	}
	if err := m.DropTable("users_old"); err != nil {
		t.Fatalf("DropTable failed: %v", err)
	}
	if m.HasTable("users_old") {
		t.Error("dropped table still present")
	}
}

func TestMigratorColumnOperations(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	m := db.Migrator()
	if !m.HasColumn(&User{}, "name") {
		t.Error("HasColumn(name) = false, want true")
	}

	// AddColumn: create the table by hand without `note`, then add it via
	// the migrator (ALTER TABLE ... ADD COLUMN).
	type Profile struct {
		ID       uint   `gorm:"primaryKey;autoIncrement"`
		Nickname string `gorm:"size:50"`
		Note     string
	}
	if err := db.Exec("CREATE TABLE `profiles` (`id` integer PRIMARY KEY AUTOINCREMENT, `nickname` text)").Error; err != nil {
		t.Fatal(err)
	}
	if err := m.AddColumn(&Profile{}, "Note"); err != nil {
		t.Fatalf("AddColumn failed: %v", err)
	}
	if !m.HasColumn(&Profile{}, "note") {
		t.Error("added column missing")
	}

	// Fill it so rename/drop run against non-empty data.
	if err := db.Create(&Profile{Nickname: "nick", Note: "hello"}).Error; err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// RenameColumn
	if err := m.RenameColumn(&Profile{}, "note", "note2"); err != nil {
		t.Fatalf("RenameColumn failed: %v", err)
	}
	if !m.HasColumn(&Profile{}, "note2") {
		t.Error("renamed column missing")
	}

	// ColumnTypes
	columnTypes, err := m.ColumnTypes(&Profile{})
	if err != nil {
		t.Fatalf("ColumnTypes failed: %v", err)
	}
	byName := map[string]gorm.ColumnType{}
	for _, ct := range columnTypes {
		byName[ct.Name()] = ct
	}
	if ct, ok := byName["nickname"]; !ok {
		t.Error("ColumnTypes missing 'nickname'")
	} else {
		if nullable, valid := ct.Nullable(); valid && !nullable {
			t.Error("column 'nickname' should be nullable (no NOT NULL)")
		}
		// Declared type parsed from the DDL in sqlite_master.
		if got := ct.DatabaseTypeName(); got != "text" {
			t.Errorf("nickname declared type = %q, want text", got)
		}
	}

	// DropColumn
	if err := m.DropColumn(&Profile{}, "note2"); err != nil {
		t.Fatalf("DropColumn failed: %v", err)
	}
	if m.HasColumn(&Profile{}, "note2") {
		t.Error("dropped column still present")
	}
	// Row must have survived drop.
	var n int64
	db.Table("profiles").Count(&n)
	if n != 1 {
		t.Errorf("rows after DropColumn = %d, want 1", n)
	}
}

func TestMigratorAlterColumnWithData(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	if err := db.Create(&User{Name: "keep", Email: "keep@example.com", Age: 7}).Error; err != nil {
		t.Fatal(err)
	}

	m := db.Migrator()
	// Alter a column type: recreates the table and must preserve rows.
	if err := m.AlterColumn(&User{}, "age"); err != nil {
		t.Fatalf("AlterColumn failed: %v", err)
	}
	var got User
	if err := db.Where("name = ?", "keep").First(&got).Error; err != nil {
		t.Fatalf("row lost after AlterColumn: %v", err)
	}
	if got.Age != 7 {
		t.Errorf("Age = %d, want 7", got.Age)
	}
}

func TestMigratorIndexOperations(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	m := db.Migrator()

	// uniqueIndex on Email already created by AutoMigrate.
	indexes, err := m.GetIndexes(&User{})
	if err != nil {
		t.Fatalf("GetIndexes failed: %v", err)
	}
	var emailIndexed bool
	for _, idx := range indexes {
		if len(idx.Columns()) == 1 && idx.Columns()[0] == "email" {
			emailIndexed = true
		}
	}
	if !emailIndexed {
		t.Errorf("expected an index on email, got %+v", indexes)
	}

	// CreateIndex via schema tag
	type Tagged struct {
		ID   uint   `gorm:"primaryKey"`
		Code string `gorm:"index:idx_code"`
	}
	if err := db.AutoMigrate(&Tagged{}); err != nil {
		t.Fatal(err)
	}
	if !m.HasIndex(&Tagged{}, "idx_code") {
		t.Error("HasIndex(idx_code) = false, want true")
	}
	if err := m.RenameIndex(&Tagged{}, "idx_code", "idx_code_new"); err != nil {
		t.Fatalf("RenameIndex failed: %v", err)
	}
	if !m.HasIndex(&Tagged{}, "idx_code_new") {
		t.Error("renamed index missing")
	}
	if m.HasIndex(&Tagged{}, "idx_code") {
		t.Error("old index name still present")
	}
	if err := m.DropIndex(&Tagged{}, "idx_code_new"); err != nil {
		t.Fatalf("DropIndex failed: %v", err)
	}
	if m.HasIndex(&Tagged{}, "idx_code_new") {
		t.Error("dropped index still present")
	}
}

func TestMigratorCurrentDatabase(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	// In-memory databases report an empty file path; just require no error.
	_ = db.Migrator().CurrentDatabase()
}

func TestCurrentDatabaseFileDSN(t *testing.T) {
	dir := t.TempDir()
	db := openTestDB(t, dir+"/current.db", nil)
	// SQLite's default database is always named "main".
	if got := db.Migrator().CurrentDatabase(); got != "main" {
		t.Errorf("CurrentDatabase = %q, want main", got)
	}
}

func TestOpenWithConn(t *testing.T) {
	// A dialector built around an existing pool must not sql.Open itself.
	dir := t.TempDir()
	sqlDB, err := sql.Open(DriverName, dir+"/conn.db")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	db, err := gorm.Open(OpenWithConn(sqlDB), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("gorm.Open(OpenWithConn) failed: %v", err)
	}
	if err := db.AutoMigrate(&User{}); err != nil {
		t.Fatalf("AutoMigrate failed: %v", err)
	}
	if err := db.Create(&User{Name: "conn", Email: "conn@example.com"}).Error; err != nil {
		t.Fatalf("Create failed: %v", err)
	}
}

// --- Constraint migration ---

// Constrained maps to a table created by hand with a named UNIQUE table
// constraint (GORM's own tags render UNIQUE as an index instead).
type Constrained struct {
	ID   uint
	Code string
}

func (Constrained) TableName() string { return "constrained" }

func TestMigratorConstraintOperations(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	if err := db.Exec(`CREATE TABLE constrained (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT,
		CONSTRAINT uq_code UNIQUE (code)
	)`).Error; err != nil {
		t.Fatal(err)
	}

	m := db.Migrator()
	if !m.HasConstraint(&Constrained{}, "uq_code") {
		t.Error("HasConstraint(uq_code) = false, want true")
	}
	if err := m.DropConstraint(&Constrained{}, "uq_code"); err != nil {
		t.Fatalf("DropConstraint failed: %v", err)
	}
	if m.HasConstraint(&Constrained{}, "uq_code") {
		t.Error("constraint survived DropConstraint")
	}
	// With the constraint gone, duplicate codes must be accepted.
	if err := db.Create(&Constrained{Code: "same"}).Error; err != nil {
		t.Fatalf("first insert failed: %v", err)
	}
	if err := db.Create(&Constrained{Code: "same"}).Error; err != nil {
		t.Fatalf("duplicate insert should succeed after DropConstraint: %v", err)
	}
}
