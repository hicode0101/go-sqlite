package sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	gosqlite "modernc.org/sqlite"
)

// --- Dialector edge cases ---

func TestInitializeUnknownDriverFails(t *testing.T) {
	// sql.Open is lazy, but the version probe must surface the unknown
	// driver as an Initialize error.
	_, err := gorm.Open(&Dialector{DriverName: "no-such-driver", DSN: ":memory:"}, &gorm.Config{Logger: logger.Discard})
	if err == nil {
		t.Fatal("expected Initialize to fail for unknown driver name")
	}
}

func TestInitializeWithStructConnField(t *testing.T) {
	dir := t.TempDir()
	sqlDB, err := sql.Open("sqlite", dir+"/struct.db")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	db, err := gorm.Open(&Dialector{Conn: sqlDB}, &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("gorm.Open(struct Conn) failed: %v", err)
	}
	if err := db.Exec("CREATE TABLE t1 (id INTEGER)").Error; err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
}

func TestInsertModifierORIgnore(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)

	if err := db.Create(&User{Name: "a", Email: "mod@example.com"}).Error; err != nil {
		t.Fatal(err)
	}
	// INSERT OR IGNORE on a conflicting row: no error, no new row.
	err := db.Clauses(clause.Insert{Modifier: "OR IGNORE"}).
		Create(&User{Name: "b", Email: "mod@example.com"}).Error
	if err != nil {
		t.Fatalf("INSERT OR IGNORE failed: %v", err)
	}
	var n int64
	db.Model(&User{}).Where("email = ?", "mod@example.com").Count(&n)
	if n != 1 {
		t.Errorf("rows with email = %d, want 1", n)
	}
}

func TestDefaultValueOf(t *testing.T) {
	d := Dialector{}
	if got := d.DefaultValueOf(&schema.Field{AutoIncrement: true}).(clause.Expr).SQL; got != "NULL" {
		t.Errorf("DefaultValueOf(autoincrement) = %q, want NULL", got)
	}
	if got := d.DefaultValueOf(&schema.Field{}).(clause.Expr).SQL; got != "DEFAULT" {
		t.Errorf("DefaultValueOf(plain) = %q, want DEFAULT", got)
	}
}

func TestBindVarTo(t *testing.T) {
	var b strings.Builder
	Dialector{}.BindVarTo(&b, nil, nil)
	if b.String() != "?" {
		t.Errorf("BindVarTo wrote %q, want ?", b.String())
	}
}

func TestQuoteToEdgeCases(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	dialector := db.Dialector.(*Dialector)
	for _, tc := range []struct{ in, want string }{
		{"", "`"},             // empty identifier -> lone quote
		{"`name`", "`name`"},  // already quoted segment passes through
		{"a.", "`a`.`"},       // trailing dot: closer, dot, no opener
		{".a", "`.`a`"},       // leading dot: closer for empty left part
		{"a..b", "`a`.`.`b`"}, // consecutive dots
		{"na'me", "`na'me`"},  // single quote is not special here
		{"users.name.email", "`users`.`name`.`email`"},
	} {
		var b strings.Builder
		dialector.QuoteTo(&b, tc.in)
		if got := b.String(); got != tc.want {
			t.Errorf("QuoteTo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDataTypeOfEdgeCases(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	dialector := db.Dialector.(*Dialector)

	// schema.Time with an explicit TYPE tag keeps the tag's type.
	if got := dialector.DataTypeOf(&schema.Field{DataType: schema.Time, TagSettings: map[string]string{"TYPE": "timestamp"}}); got != "timestamp" {
		t.Errorf("DataTypeOf(time+TYPE) = %q, want timestamp", got)
	}
	// Unknown custom data types pass through untouched.
	if got := dialector.DataTypeOf(&schema.Field{DataType: "vector"}); got != "vector" {
		t.Errorf("DataTypeOf(custom) = %q, want vector", got)
	}
}

func TestTranslatePassthrough(t *testing.T) {
	d := Dialector{}

	// Non-sqlite errors are returned unchanged.
	sentinel := errors.New("plain error")
	if got := d.Translate(sentinel); !errors.Is(got, sentinel) || got != sentinel {
		t.Errorf("Translate(plain) = %v, want the same error", got)
	}

	// sqlite errors with codes outside the translated set pass through.
	db := openTestDB(t, ":memory:", nil)
	sqlErr := db.Exec("this is not SQL").Error
	if sqlErr == nil {
		t.Fatal("expected a syntax error")
	}
	if _, ok := sqlErr.(*gosqlite.Error); !ok {
		t.Fatalf("expected *sqlite.Error, got %T", sqlErr)
	}
	if got := d.Translate(sqlErr); got != sqlErr {
		t.Errorf("Translate(syntax error) should be identity, got %v", got)
	}
}

func TestCompareVersionMore(t *testing.T) {
	for _, tc := range []struct {
		v1, v2 string
		want   int
	}{
		{"3.36", "3.35.0", 1},
		{"3.5", "3.50.4", -1}, // numeric, not lexicographic
		{"4", "3.99.99", 1},
		{"3.35.1", "3.35", 1},
	} {
		if got := compareVersion(tc.v1, tc.v2); got != tc.want {
			t.Errorf("compareVersion(%q, %q) = %d, want %d", tc.v1, tc.v2, got, tc.want)
		}
	}
}

// --- Migrator edge cases ---

func TestRunWithoutForeignKeyTogglesPragma(t *testing.T) {
	// FK enabled in the DSN: RunWithoutForeignKey must disable it for the
	// duration and restore it afterwards.
	db := openTestDB(t, ":memory:?_pragma=foreign_keys(1)", nil)
	m := db.Migrator().(Migrator)

	var inside int
	if err := m.RunWithoutForeignKey(func() error {
		db.Raw("PRAGMA foreign_keys").Scan(&inside)
		return nil
	}); err != nil {
		t.Fatalf("RunWithoutForeignKey failed: %v", err)
	}
	if inside != 0 {
		t.Errorf("foreign_keys inside = %d, want 0", inside)
	}
	var after int
	db.Raw("PRAGMA foreign_keys").Scan(&after)
	if after != 1 {
		t.Errorf("foreign_keys after = %d, want 1 (restored)", after)
	}

	// FK disabled: the helper must not touch the pragma at all, fc just runs.
	var ran int
	db2 := openTestDB(t, ":memory:", nil)
	m2 := db2.Migrator().(Migrator)
	if err := m2.RunWithoutForeignKey(func() error {
		ran++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Errorf("fc ran %d times, want 1", ran)
	}
}

func TestAlterColumnUnknownFieldFails(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)
	err := db.Migrator().AlterColumn(&User{}, "no_such_field")
	if err == nil || !strings.Contains(err.Error(), "failed to alter field") {
		t.Fatalf("expected alter-field failure, got %v", err)
	}
}

func TestAlterColumnOnMissingTableFails(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	err := db.Migrator().AlterColumn(&User{}, "name")
	if err == nil {
		t.Fatal("expected failure for missing table (no DDL)")
	}
}

// UniqT maps to the hand-built uniq_t table (gorm would otherwise
// pluralize the type name to "uniq_ts").
type UniqT struct {
	ID   uint
	Code string `gorm:"unique"`
}

func (UniqT) TableName() string { return "uniq_t" }

func TestAlterColumnKeepsInlineUnique(t *testing.T) {
	// Tables created with an inline column UNIQUE must keep uniqueness
	// after AlterColumn (the rebuild branch that re-adds the constraint).
	// The model must declare `unique` so GuessConstraintInterfaceAndTable
	// can resolve the constraint name uni_<table>_<column>.
	db := openTestDB(t, ":memory:", nil)
	if err := db.Exec("CREATE TABLE uniq_t (`id` integer PRIMARY KEY AUTOINCREMENT, `code` text UNIQUE)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO uniq_t (code) VALUES ('u1')").Error; err != nil {
		t.Fatal(err)
	}

	m := db.Migrator()
	if err := m.AlterColumn(&UniqT{}, "code"); err != nil {
		t.Fatalf("AlterColumn failed: %v", err)
	}

	// Duplicate code must still be rejected.
	err := db.Exec("INSERT INTO uniq_t (code) VALUES ('u1')").Error
	if err == nil {
		t.Fatal("unique constraint lost after AlterColumn")
	}
	if !errors.Is(err, gorm.ErrDuplicatedKey) && !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("unexpected error type: %v", err)
	}
}

func TestDropTableMultipleModelsReverseOrder(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	type A struct {
		ID uint `gorm:"primaryKey"`
	}
	type B struct {
		ID uint `gorm:"primaryKey"`
	}
	if err := db.AutoMigrate(&A{}, &B{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&A{}, &B{}); err != nil {
		t.Fatalf("DropTable(multi) failed: %v", err)
	}
	if db.Migrator().HasTable("as") || db.Migrator().HasTable("bs") {
		t.Error("tables survived DropTable")
	}
}

func TestHasColumnAndHasIndexNegative(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)
	m := db.Migrator()
	if m.HasColumn(&User{}, "ghost_col") {
		t.Error("HasColumn(ghost_col) = true, want false")
	}
	if m.HasIndex(&User{}, "ghost_idx") {
		t.Error("HasIndex(ghost_idx) = true, want false")
	}
}

func TestCreateIndexUnknownNameFails(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)
	if err := db.Migrator().CreateIndex(&User{}, "ghost_idx"); err == nil {
		t.Fatal("expected CreateIndex failure for unknown index")
	}
}

func TestRenameIndexMissingFails(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)
	if err := db.Migrator().RenameIndex(&User{}, "ghost", "ghost2"); err == nil {
		t.Fatal("expected RenameIndex failure for missing index")
	}
}

func TestBuildIndexOptionsVariants(t *testing.T) {
	// Expression, collation and sort options must render into CREATE INDEX.
	type Opt struct {
		ID    uint   `gorm:"primaryKey"`
		Email string `gorm:"index:idx_expr,expression:lower(email)"`
		City  string `gorm:"index:idx_city,collate:nocase,sort:desc"`
	}
	db := openTestDB(t, ":memory:", nil)
	if err := db.AutoMigrate(&Opt{}); err != nil {
		t.Fatal(err)
	}
	m := db.Migrator()
	if !m.HasIndex(&Opt{}, "idx_expr") {
		t.Error("expression index missing")
	}
	if !m.HasIndex(&Opt{}, "idx_city") {
		t.Error("collate/sort index missing")
	}
	// Verify the rendered SQL really carries the options.
	var sqlText string
	db.Raw("SELECT sql FROM sqlite_master WHERE name = ?", "idx_city").Scan(&sqlText)
	if !strings.Contains(strings.ToUpper(sqlText), "COLLATE NOCASE") || !strings.Contains(strings.ToUpper(sqlText), "DESC") {
		t.Errorf("idx_city SQL = %q, want COLLATE NOCASE ... DESC", sqlText)
	}
	var exprSQL string
	db.Raw("SELECT sql FROM sqlite_master WHERE name = ?", "idx_expr").Scan(&exprSQL)
	if !strings.Contains(strings.ToUpper(exprSQL), "LOWER") {
		t.Errorf("idx_expr SQL = %q, want a lower() expression index", exprSQL)
	}
}

// UO maps to the hand-built uo table.
type UO struct {
	ID   uint
	Code string
}

func (UO) TableName() string { return "uo" }

func TestGetIndexesSkipsUniqueConstraintOrigin(t *testing.T) {
	// Indexes with origin "u" (backing an inline UNIQUE constraint) are
	// constraints, not declared indexes, and must be filtered out.
	db := openTestDB(t, ":memory:", nil)
	if err := db.Exec("CREATE TABLE uo (`id` integer PRIMARY KEY, `code` text UNIQUE)").Error; err != nil {
		t.Fatal(err)
	}
	indexes, err := db.Migrator().GetIndexes(&UO{})
	if err != nil {
		t.Fatalf("GetIndexes failed: %v", err)
	}
	for _, idx := range indexes {
		if idx.Name() == "sqlite_autoindex_uo_1" {
			t.Error("unique-constraint index should be filtered out")
		}
	}
}

// Plain maps to the hand-built plain table.
type Plain struct {
	ID uint
	V  string
}

func (Plain) TableName() string { return "plain" }

func TestGetIndexesOnIndexlessTable(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	if err := db.Exec("CREATE TABLE plain (`id` integer PRIMARY KEY, `v` text)").Error; err != nil {
		t.Fatal(err)
	}
	indexes, err := db.Migrator().GetIndexes(&Plain{})
	if err != nil {
		t.Fatalf("GetIndexes failed: %v", err)
	}
	if len(indexes) != 0 {
		t.Errorf("indexes = %v, want none (pk only)", indexes)
	}
}

func TestHasConstraintNegative(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)
	if db.Migrator().HasConstraint(&User{}, "ghost_c") {
		t.Error("HasConstraint(ghost_c) = true, want false")
	}
}

func TestCreateConstraintUnknownNameIsNoop(t *testing.T) {
	// No schema constraint matches the name: GuessConstraintInterfaceAndTable
	// returns nil and the migrator must not recreate the table.
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)
	if err := db.Create(&User{Name: "cc", Email: "cc@example.com"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().CreateConstraint(&User{}, "ghost_c"); err != nil {
		t.Fatalf("CreateConstraint(unknown) = %v, want nil", err)
	}
	var n int64
	db.Model(&User{}).Count(&n)
	if n != 1 {
		t.Errorf("rows = %d, want 1 (table must be untouched)", n)
	}
}

// Author/Post exercise the relation path that makes AutoMigrate call
// CreateConstraint (foreign-key constraints on an existing table).
type Author struct {
	ID   uint   `gorm:"primaryKey;autoIncrement"`
	Name string `gorm:"size:50"`
}

type Post struct {
	ID       uint   `gorm:"primaryKey;autoIncrement"`
	Title    string `gorm:"size:100"`
	AuthorID uint
	Author   *Author `gorm:"foreignKey:AuthorID"`
}

func TestAutoMigrateCreatesForeignKeyConstraint(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	if err := db.AutoMigrate(&Author{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Post{}); err != nil {
		t.Fatal(err)
	}
	// Migrate again on an existing table: this is where CreateConstraint
	// runs (recreateTable path).
	if err := db.Create(&Author{Name: "a"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Post{Title: "p", AuthorID: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Post{}); err != nil {
		t.Fatalf("re-migrate failed: %v", err)
	}
	var sqlText string
	db.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name='posts'").Scan(&sqlText)
	if !strings.Contains(strings.ToUpper(sqlText), "FOREIGN KEY") {
		t.Errorf("posts DDL lost the foreign key: %q", sqlText)
	}
	// Data must survive the constraint recreation.
	var n int64
	db.Model(&Post{}).Count(&n)
	if n != 1 {
		t.Errorf("rows after re-migrate = %d, want 1", n)
	}
}

func TestColumnTypesOnEmptyTable(t *testing.T) {
	// ColumnTypes samples one row; an empty table must still describe columns.
	type Empty struct {
		ID  uint `gorm:"primaryKey"`
		Val string
	}
	db := openTestDB(t, ":memory:", nil)
	if err := db.AutoMigrate(&Empty{}); err != nil {
		t.Fatal(err)
	}
	cts, err := db.Migrator().ColumnTypes(&Empty{})
	if err != nil {
		t.Fatalf("ColumnTypes failed: %v", err)
	}
	if len(cts) != 2 {
		t.Errorf("column count = %d, want 2", len(cts))
	}
}

func TestGetRawDDLMissingTable(t *testing.T) {
	// A missing table yields no rows: the ErrNoRows from Scan is not a
	// stored db error, so getRawDDL returns an empty DDL without error.
	// recreateTable catches this downstream when parseDDL rejects "".
	db := openTestDB(t, ":memory:", nil)
	m := db.Migrator().(Migrator)
	ddlText, err := m.getRawDDL("ghost_table")
	if err != nil {
		t.Fatalf("getRawDDL(missing) = %v, want nil", err)
	}
	if ddlText != "" {
		t.Errorf("getRawDDL(missing) = %q, want empty", ddlText)
	}
}

// --- ddlmod edge cases ---

func TestParseDDLDefaultWithCollate(t *testing.T) {
	// The DEFAULT capture stops at the space before COLLATE (the value
	// itself keeps its quotes; the trailing keywords are not swallowed
	// into the value beyond this separator boundary).
	d, err := parseDDL("CREATE TABLE `t` (`a` text DEFAULT 'x' COLLATE nocase)")
	if err != nil {
		t.Fatalf("parseDDL failed: %v", err)
	}
	if got := d.columns[0].DefaultValueValue.String; got != "'x' COLLATE nocase" {
		t.Errorf("default = %q, want \"'x' COLLATE nocase\" (greedy capture to end-of-modifiers)", got)
	}
}

func TestParseDDLDefaultNullIsNotRecorded(t *testing.T) {
	d, err := parseDDL("CREATE TABLE `t` (`a` text DEFAULT NULL)")
	if err != nil {
		t.Fatal(err)
	}
	if d.columns[0].DefaultValueValue.Valid {
		t.Errorf("DEFAULT NULL must not be recorded, got %q", d.columns[0].DefaultValueValue.String)
	}
}

func TestParseDDLUniqueIndexStatement(t *testing.T) {
	_, err := parseDDL("CREATE UNIQUE INDEX `idx_u` ON `t`(`a`)")
	if err != nil {
		t.Fatalf("UNIQUE INDEX should parse cleanly: %v", err)
	}
}

func TestParseDDLTabSeparatedColumn(t *testing.T) {
	d, err := parseDDL("CREATE TABLE `t` (\n\t`a`\tinteger,\n\t`b`\ttext\n)")
	if err != nil {
		t.Fatalf("parseDDL failed: %v", err)
	}
	if len(d.columns) != 2 || d.columns[0].NameValue.String != "a" || d.columns[1].NameValue.String != "b" {
		t.Errorf("columns = %+v", d.columns)
	}
}

func TestParseDDLRenameUnquotedHead(t *testing.T) {
	d, _ := parseDDL("CREATE TABLE users (`id` integer)")
	if err := d.renameTable("users__temp", "users"); err != nil {
		t.Fatalf("renameTable failed: %v", err)
	}
	if !strings.Contains(d.head, "users__temp") {
		t.Errorf("head = %q", d.head)
	}
}

func TestDDLCompileHeadOnly(t *testing.T) {
	d := &ddl{head: "CREATE TABLE `t`"}
	if got := d.compile(); got != "CREATE TABLE `t`" {
		t.Errorf("compile() = %q", got)
	}
}

func TestDDLRemoveColumnMissing(t *testing.T) {
	d, _ := parseDDL("CREATE TABLE `t` (`a` text)")
	if d.removeColumn("nope") {
		t.Error("removeColumn(nope) = true, want false")
	}
}

func TestDDLRemoveConstraintMissing(t *testing.T) {
	d, _ := parseDDL("CREATE TABLE `t` (`a` text)")
	if d.removeConstraint("nope") {
		t.Error("removeConstraint(nope) = true, want false")
	}
}

// --- misc ---

func TestErrConstraintsNotImplementedMessage(t *testing.T) {
	if ErrConstraintsNotImplemented == nil || !strings.Contains(ErrConstraintsNotImplemented.Error(), "DisableForeignKeyConstraintWhenMigrating") {
		t.Error("ErrConstraintsNotImplemented message drifted")
	}
}

func TestFileDSNWithQueryParams(t *testing.T) {
	// Plain path + query parameters must open a real file database.
	dir := t.TempDir()
	dsn := filepath.ToSlash(filepath.Join(dir, "q.db")) + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(3000)"
	db, err := gorm.Open(Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		defer sqlDB.Close()
	}
	autoMigrateUsers(t, db)
	if err := db.Create(&User{Name: "q", Email: "q@example.com"}).Error; err != nil {
		t.Fatalf("create failed: %v", err)
	}
	// Data must be readable from a fresh handle on the same file.
	db2, err := gorm.Open(Open(filepath.ToSlash(filepath.Join(dir, "q.db"))), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB2, err := db2.DB(); err == nil {
		defer sqlDB2.Close()
	}
	var n int64
	db2.Model(&User{}).Count(&n)
	if n != 1 {
		t.Errorf("rows from second handle = %d, want 1", n)
	}
}
