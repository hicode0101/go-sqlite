package sqlite

import (
	"strings"
	"testing"
)

func TestParseDDLSimple(t *testing.T) {
	d, err := parseDDL("CREATE TABLE `users` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text)")
	if err != nil {
		t.Fatalf("parseDDL failed: %v", err)
	}
	if d.head != "CREATE TABLE `users`" {
		t.Errorf("head = %q", d.head)
	}
	if len(d.fields) != 2 {
		t.Fatalf("fields = %v, want 2 entries", d.fields)
	}
	if len(d.columns) != 2 {
		t.Fatalf("columns = %v, want 2", d.columns)
	}
	if d.columns[0].NameValue.String != "id" {
		t.Errorf("column[0].Name = %q, want id", d.columns[0].NameValue.String)
	}
	if !d.columns[0].PrimaryKeyValue.Bool {
		t.Error("id should be primary key")
	}
}

func TestParseDDLQuotedCommasAndBrackets(t *testing.T) {
	// Commas inside string literals and nested parens must not split fields.
	d, err := parseDDL("CREATE TABLE `t` (`a` text, `b` text DEFAULT 'x,y(z)', `c` integer CHECK (c IN (1,2,3)), `d` text)")
	if err != nil {
		t.Fatalf("parseDDL failed: %v", err)
	}
	if len(d.fields) != 4 {
		t.Fatalf("fields = %v, want 4", d.fields)
	}
	names := make([]string, 0, len(d.columns))
	for _, c := range d.columns {
		names = append(names, c.NameValue.String)
	}
	if len(names) != 4 || names[0] != "a" || names[3] != "d" {
		t.Errorf("columns = %v", names)
	}
	// default value parsed
	if got := d.columns[1].DefaultValueValue.String; got != "'x,y(z)'" {
		t.Errorf("b default = %q, want 'x,y(z)'", got)
	}
}

func TestParseDDLEscapedQuote(t *testing.T) {
	// Doubled single quote inside a literal must not terminate the string.
	d, err := parseDDL("CREATE TABLE `t` (`a` text DEFAULT 'it''s, fine', `b` integer)")
	if err != nil {
		t.Fatalf("parseDDL failed: %v", err)
	}
	if len(d.fields) != 2 {
		t.Fatalf("fields = %v, want 2", d.fields)
	}
}

func TestParseDDLConstraintUnique(t *testing.T) {
	d, err := parseDDL("CREATE TABLE `t` (`a` text, `b` integer, CONSTRAINT `uq_ab` UNIQUE (`a`))")
	if err != nil {
		t.Fatalf("parseDDL failed: %v", err)
	}
	if len(d.columns) != 2 {
		t.Fatalf("columns = %d, want 2", len(d.columns))
	}
	if !d.columns[0].UniqueValue.Bool {
		t.Error("column a should be flagged unique via table constraint")
	}
	if d.columns[1].UniqueValue.Bool {
		t.Error("column b should not be flagged unique")
	}
}

func TestParseDDLTablePrimaryKey(t *testing.T) {
	d, err := parseDDL("CREATE TABLE `t` (`a` integer, `b` integer, PRIMARY KEY (`a`,`b`))")
	if err != nil {
		t.Fatalf("parseDDL failed: %v", err)
	}
	for i, want := range []bool{true, true} {
		if d.columns[i].PrimaryKeyValue.Bool != want {
			t.Errorf("columns[%d] primary key = %v, want %v", i, d.columns[i].PrimaryKeyValue.Bool, want)
		}
	}
}

func TestParseDDLNotNullAndUnique(t *testing.T) {
	d, err := parseDDL("CREATE TABLE `t` (`a` text NOT NULL UNIQUE, `b` integer NULL)")
	if err != nil {
		t.Fatalf("parseDDL failed: %v", err)
	}
	if d.columns[0].NullableValue.Bool {
		t.Error("a should be NOT NULL")
	}
	if !d.columns[0].UniqueValue.Bool {
		t.Error("a should be UNIQUE")
	}
	if !d.columns[1].NullableValue.Bool {
		t.Error("b should be nullable")
	}
}

func TestParseDDLTypeLength(t *testing.T) {
	d, err := parseDDL("CREATE TABLE `t` (`a` varchar(100), `b` text)")
	if err != nil {
		t.Fatalf("parseDDL failed: %v", err)
	}
	if got := d.columns[0].DataTypeValue.String; got != "varchar" {
		t.Errorf("a type = %q, want varchar", got)
	}
	if got := d.columns[0].LengthValue.Int64; got != 100 || !d.columns[0].LengthValue.Valid {
		t.Errorf("a length = %d (valid=%v), want 100", got, d.columns[0].LengthValue.Valid)
	}
	if got := d.columns[1].DataTypeValue.String; got != "text" {
		t.Errorf("b type = %q, want text", got)
	}
}

func TestParseDDLWithIndexStatement(t *testing.T) {
	// Index DDL is ignored; table DDL still parses.
	d, err := parseDDL(
		"CREATE TABLE `t` (`a` text)",
		"CREATE INDEX `idx_a` ON `t`(`a`)",
	)
	if err != nil {
		t.Fatalf("parseDDL failed: %v", err)
	}
	if len(d.columns) != 1 || d.columns[0].NameValue.String != "a" {
		t.Errorf("columns = %+v", d.columns)
	}
}

func TestParseDDLInvalid(t *testing.T) {
	// Non-DDL input.
	if _, err := parseDDL("SELECT 1"); err == nil {
		t.Error("expected error for non-DDL input")
	}
	// Bare CREATE TABLE with no table name.
	if _, err := parseDDL("CREATE TABLE"); err == nil {
		t.Error("expected error for bare CREATE TABLE")
	}
	// Missing closing parenthesis: body ends inside a nested bracket.
	if _, err := parseDDL("CREATE TABLE test (ID int NOT NULL,Name varchar(255)"); err == nil {
		t.Error("expected error for unbalanced brackets (missing close)")
	}
	// Extra closing parenthesis drives the bracket level negative.
	if _, err := parseDDL("CREATE TABLE test (ID int NOT NULL,Name varchar(255)))"); err == nil {
		t.Error("expected error for unbalanced brackets (extra close)")
	}
}

func TestDDLRenameTable(t *testing.T) {
	d, _ := parseDDL("CREATE TABLE `users` (`id` integer)")
	if err := d.renameTable("users__temp", "users"); err != nil {
		t.Fatalf("renameTable failed: %v", err)
	}
	if !strings.Contains(d.head, "users__temp") || strings.Contains(d.head, "`users`") {
		t.Errorf("head after rename = %q", d.head)
	}
	// Renaming a table that is not present in the head must fail.
	d2, _ := parseDDL("CREATE TABLE `other` (`id` integer)")
	if err := d2.renameTable("x__temp", "users"); err == nil {
		t.Error("renameTable with absent src should fail")
	}
}

func TestDDLAddRemoveConstraint(t *testing.T) {
	d, _ := parseDDL("CREATE TABLE `t` (`a` text, CONSTRAINT `uq_a` UNIQUE (`a`))")
	if !d.hasConstraint("uq_a") {
		t.Error("uq_a should exist")
	}
	if !d.removeConstraint("uq_a") {
		t.Error("removeConstraint(uq_a) = false, want true")
	}
	if d.hasConstraint("uq_a") {
		t.Error("uq_a should be gone")
	}
	d.addConstraint("uq_a", "CONSTRAINT `uq_a` UNIQUE (`a`)")
	if !d.hasConstraint("uq_a") {
		t.Error("uq_a should exist after addConstraint")
	}
	// addConstraint over an existing name replaces it.
	d.addConstraint("uq_a", "CONSTRAINT `uq_a2` UNIQUE (`a`)")
	if d.hasConstraint("uq_a") || !d.hasConstraint("uq_a2") {
		t.Error("addConstraint should have replaced uq_a with uq_a2")
	}
}

func TestDDLGetColumnsAndRemoveColumn(t *testing.T) {
	d, _ := parseDDL("CREATE TABLE `t` (`a` integer PRIMARY KEY, `b` text, `c` integer GENERATED ALWAYS AS (a*2), PRIMARY KEY (`a`))")
	cols := d.getColumns()
	if len(cols) != 2 || cols[0] != "`a`" || cols[1] != "`b`" {
		t.Errorf("getColumns = %v, want [`a` `b`]", cols)
	}
	if !d.removeColumn("b") {
		t.Error("removeColumn(b) = false, want true")
	}
	cols = d.getColumns()
	if len(cols) != 1 || cols[0] != "`a`" {
		t.Errorf("getColumns after removal = %v", cols)
	}
}

func TestDDLCloneIsIndependent(t *testing.T) {
	d, _ := parseDDL("CREATE TABLE `t` (`a` text, `b` integer)")
	c := d.clone()
	c.removeColumn("a")
	if len(d.fields) != 2 {
		t.Error("clone must not share the fields slice with the original")
	}
}
