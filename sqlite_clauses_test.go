package sqlite

import (
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// testClauseWriter is a minimal clause.Builder capturing written SQL.
type testClauseWriter struct {
	b   strings.Builder
	err error
}

func (w *testClauseWriter) WriteString(s string) (int, error) { return w.b.WriteString(s) }
func (w *testClauseWriter) WriteByte(c byte) error            { return w.b.WriteByte(c) }
func (w *testClauseWriter) AddError(err error) error {
	if w.err == nil {
		w.err = err
	}
	return nil
}

func (w *testClauseWriter) AddVar(_ clause.Writer, _ ...interface{}) {
	_ = w.b.WriteByte('?')
}

func (w *testClauseWriter) WriteQuoted(_ interface{}) {
	_, _ = w.b.WriteString(`"q"`)
}

// The clause builders are installed into db.ClauseBuilders; these tests
// call them directly to cover the defensive branches that normal query
// building never reaches.

func TestClauseBuildersDirectInsertWithoutStatement(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	dialector := db.Dialector.(*Dialector)

	// A non-*gorm.Statement builder falls back to the generic clause build;
	// the INSERT keyword comes from the clause name in that path.
	builders := dialector.ClauseBuilders()
	w := &testClauseWriter{}
	builders["INSERT"](clause.Clause{Name: "INSERT", Expression: clause.Insert{Table: clause.Table{Name: "t1"}}}, w)
	if got := w.b.String(); got != `INSERT INTO "q"` {
		t.Errorf("INSERT fallback build = %q, want INSERT INTO \"q\"", got)
	}
}

func TestClauseBuildersDirectInsertWithModifierAndQuotedTable(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	dialector := db.Dialector.(*Dialector)
	builders := dialector.ClauseBuilders()

	stmt := &gorm.Statement{DB: db}
	stmt.Table = "t2"
	builders["INSERT"](
		clause.Clause{Expression: clause.Insert{Modifier: "OR REPLACE", Table: clause.Table{Name: ""}}},
		stmt,
	)
	got := stmt.SQL.String()
	if !strings.Contains(got, "OR REPLACE") || !strings.Contains(got, "`t2`") {
		t.Errorf("INSERT with modifier = %q", got)
	}
}

func TestClauseBuildersDirectLimitNilAndZero(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	dialector := db.Dialector.(*Dialector)
	builders := dialector.ClauseBuilders()

	// nil Limit and zero offset: nothing may be emitted.
	w := &testClauseWriter{}
	builders["LIMIT"](clause.Clause{Expression: clause.Limit{}}, w)
	if got := w.b.String(); got != "" {
		t.Errorf("LIMIT(nil,0) = %q, want empty", got)
	}

	// explicit limit only
	w2 := &testClauseWriter{}
	three := 3
	builders["LIMIT"](clause.Clause{Expression: clause.Limit{Limit: &three}}, w2)
	if got := w2.b.String(); got != "LIMIT 3" {
		t.Errorf("LIMIT(3) = %q, want LIMIT 3", got)
	}

	// explicit zero limit still renders (LIMIT 0 is meaningful SQL)
	w3 := &testClauseWriter{}
	zero := 0
	builders["LIMIT"](clause.Clause{Expression: clause.Limit{Limit: &zero}}, w3)
	if got := w3.b.String(); got != "LIMIT 0" {
		t.Errorf("LIMIT(0) = %q, want LIMIT 0", got)
	}
}

func TestClauseBuildersDirectForPassthrough(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	dialector := db.Dialector.(*Dialector)
	builders := dialector.ClauseBuilders()

	// A non-locking expression in the FOR clause builds generically.
	w := &testClauseWriter{}
	builders["FOR"](clause.Clause{Expression: clause.Expr{SQL: "SOMETHING"}}, w)
	if got := w.b.String(); got != "SOMETHING" {
		t.Errorf("FOR passthrough = %q, want SOMETHING", got)
	}
}

func TestDropConstraintUnknownNameIsNoop(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	autoMigrateUsers(t, db)
	if err := db.Create(&User{Name: "dc", Email: "dc@example.com"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropConstraint(&User{}, "ghost_c"); err != nil {
		t.Fatalf("DropConstraint(unknown) = %v, want nil", err)
	}
	var n int64
	db.Model(&User{}).Count(&n)
	if n != 1 {
		t.Errorf("rows = %d, want 1 (table untouched)", n)
	}
}

func TestCreateConstraintDirectWithForeignKey(t *testing.T) {
	db := openTestDB(t, ":memory:", nil)
	if err := db.AutoMigrate(&Author{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Post{}); err != nil {
		t.Fatal(err)
	}

	// Find the foreign-key constraint name from the DDL, then call
	// CreateConstraint directly (constraint != nil branch).
	var ddl string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name='posts'").Scan(&ddl).Error; err != nil {
		t.Fatal(err)
	}
	name := foreignKeyConstraintName(ddl)
	if name == "" {
		t.Fatalf("no foreign key constraint in DDL: %q", ddl)
	}

	if err := db.Migrator().CreateConstraint(&Post{}, name); err != nil {
		t.Fatalf("CreateConstraint(%q) failed: %v", name, err)
	}
	// The named constraint must still be present after recreation.
	var ddl2 string
	db.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name='posts'").Scan(&ddl2)
	if !strings.Contains(strings.ToUpper(ddl2), "FOREIGN KEY") {
		t.Errorf("constraint lost after CreateConstraint: %q", ddl2)
	}
}

// foreignKeyConstraintName extracts the constraint name from a
// "CONSTRAINT `x` FOREIGN KEY" declaration, empty when absent.
func foreignKeyConstraintName(ddl string) string {
	const marker = "CONSTRAINT `"
	i := strings.Index(strings.ToUpper(ddl), strings.ToUpper(marker))
	if i < 0 {
		return ""
	}
	rest := ddl[i+len(marker):]
	if j := strings.Index(rest, "`"); j >= 0 {
		return rest[:j]
	}
	return ""
}
