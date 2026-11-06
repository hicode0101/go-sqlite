// Package sqlite is a GORM dialector that drives SQLite entirely in pure Go
// (no CGO, no native SQLite library required).
//
// pioneered, restructured so that it depends directly on modernc.org/sqlite
// — the canonical pure-Go SQLite implementation — instead of an intermediate
// fork. That package registers itself as a database/sql driver named "sqlite"
// on import, and this package implements gorm.Dialector, the savepoint
// dialect extension and the error-translation extension on top of it.
//
// Basic usage:
//
//	import (
//	    "gorm.io/gorm"
//	    sqlite "github.com/hicode0101/go-sqlite"
//	)
//
//	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
package sqlite

import (
	"context"
	"database/sql"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"

	gosqlite "modernc.org/sqlite"
	// gosqlite registers itself with database/sql under the name "sqlite"
	// in its init function; the import above also provides *sqlite.Error,
	// the concrete error type this dialector translates.
	sqlite3 "modernc.org/sqlite/lib"
)

// DriverName is the name modernc.org/sqlite registers its database/sql
// driver under. It is exported so callers can customize Dialector.DriverName
// when they registered a separate driver instance themselves (for example
// sql.Register("app", &gosqlite.Driver{})).
const DriverName = "sqlite"

// minReturningVersion is the first SQLite version supporting the RETURNING
// clause (https://www.sqlite.org/releaselog/3_35_0.html). Modernc builds ship
// far newer cores, but the check keeps the dialector correct even when the
// caller swaps in a custom connection pool backed by an older engine.
const minReturningVersion = "3.35.0"

// Dialector implements the GORM dialect for pure-Go SQLite.
//
// The zero value is not usable directly; build one with Open (a DSN) or by
// filling the struct with either a DSN or a ready-made connection pool.
type Dialector struct {
	// DriverName selects the database/sql driver to open DSN with.
	// Empty means DriverName ("sqlite", the modernc registration).
	DriverName string
	// DSN is the data source name passed to sql.Open. Examples:
	//   ":memory:", "app.db", "file:app.db?_pragma=foreign_keys(1)"
	DSN string
	// Conn, when set, replaces sql.Open entirely: the given pool becomes
	// the gorm.DB ConnPool and DSN is ignored.
	Conn gorm.ConnPool
}

// Open returns a Dialector for the given DSN. DSN syntax is whatever the
// underlying modernc.org/sqlite driver accepts; PRAGMAs can be preset via
// the _pragma query parameter, e.g.
//
//	sqlite.Open("file:app.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
func Open(dsn string) gorm.Dialector {
	return &Dialector{DSN: dsn}
}

// OpenWithConn returns a Dialector backed by an existing connection pool,
// bypassing sql.Open. Use it when the caller manages connection setup —
// sql.OpenDB with a custom connector, instrumentation wrappers, etc.
func OpenWithConn(conn gorm.ConnPool) gorm.Dialector {
	return &Dialector{Conn: conn}
}

// Name implements gorm.Dialector.
func (Dialector) Name() string {
	return "sqlite"
}

// Initialize implements gorm.Dialector. It wires the connection pool into
// db, registers GORM's default callbacks (with RETURNING clauses when the
// engine supports them) and installs the dialect-specific clause builders.
func (dialector Dialector) Initialize(db *gorm.DB) (err error) {
	if dialector.DriverName == "" {
		dialector.DriverName = DriverName
	}

	if dialector.Conn != nil {
		db.ConnPool = dialector.Conn
	} else {
		conn, err := sql.Open(dialector.DriverName, dialector.DSN)
		if err != nil {
			return err
		}
		db.ConnPool = conn
	}

	// Probe the engine version once; modernc.org/sqlite answers with the
	// bundled SQLite core version (e.g. "3.50.4").
	var version string
	if err := db.ConnPool.QueryRowContext(context.Background(), "select sqlite_version()").Scan(&version); err != nil {
		return err
	}

	config := &callbacks.Config{
		// SQLite's last_insert_rowid() reports the rowid of the most
		// recent insert, i.e. the LAST row of a batch insert; GORM must
		// therefore walk the destination backwards when backfilling IDs.
		LastInsertIDReversed: true,
	}
	if compareVersion(version, minReturningVersion) >= 0 {
		// RETURNING lets CREATE/UPDATE/DELETE stream back the affected
		// rows, which avoids the batch-ID arithmetic above entirely.
		config.CreateClauses = []string{"INSERT", "VALUES", "ON CONFLICT", "RETURNING"}
		config.UpdateClauses = []string{"UPDATE", "SET", "FROM", "WHERE", "RETURNING"}
		config.DeleteClauses = []string{"DELETE", "FROM", "WHERE", "RETURNING"}
	}
	callbacks.RegisterDefaultCallbacks(db, config)

	for k, v := range dialector.ClauseBuilders() {
		db.ClauseBuilders[k] = v
	}
	return nil
}

// ClauseBuilders implements gorm.Dialector. It adjusts three clauses whose
// generic rendering does not fit SQLite.
func (Dialector) ClauseBuilders() map[string]clause.ClauseBuilder {
	return map[string]clause.ClauseBuilder{
		// SQLite writes modifiers (e.g. "OR IGNORE") before INTO and has
		// no INSERT alias syntax, so the INSERT clause is rendered here
		// rather than by the generic builder.
		"INSERT": func(c clause.Clause, builder clause.Builder) {
			if insert, ok := c.Expression.(clause.Insert); ok {
				if stmt, ok := builder.(*gorm.Statement); ok {
					stmt.WriteString("INSERT ")
					if insert.Modifier != "" {
						stmt.WriteString(insert.Modifier)
						stmt.WriteByte(' ')
					}
					stmt.WriteString("INTO ")
					if insert.Table.Name == "" {
						stmt.WriteQuoted(stmt.Table)
					} else {
						stmt.WriteQuoted(insert.Table)
					}
					return
				}
			}
			c.Build(builder)
		},
		// SQLite has LIMIT but no standalone OFFSET keyword: both parts of
		// a LIMIT clause are emitted together, and LIMIT must still appear
		// (as -1) when only an offset is requested.
		"LIMIT": func(c clause.Clause, builder clause.Builder) {
			if limit, ok := c.Expression.(clause.Limit); ok {
				lmt := -1
				if limit.Limit != nil && *limit.Limit >= 0 {
					lmt = *limit.Limit
				}
				if lmt >= 0 || limit.Offset > 0 {
					builder.WriteString("LIMIT ")
					builder.WriteString(strconv.Itoa(lmt))
				}
				if limit.Offset > 0 {
					builder.WriteString(" OFFSET ")
					builder.WriteString(strconv.Itoa(limit.Offset))
				}
			}
		},
		// SQLite has no row-level locking (SELECT ... FOR UPDATE); the
		// clause is silently dropped instead of producing a syntax error.
		"FOR": func(c clause.Clause, builder clause.Builder) {
			if _, ok := c.Expression.(clause.Locking); ok {
				return
			}
			c.Build(builder)
		},
	}
}

// DefaultValueOf implements gorm.Dialector. Auto-increment columns fall back
// to NULL so SQLite assigns the next rowid; other columns render DEFAULT.
func (Dialector) DefaultValueOf(field *schema.Field) clause.Expression {
	if field.AutoIncrement {
		return clause.Expr{SQL: "NULL"}
	}
	return clause.Expr{SQL: "DEFAULT"}
}

// Migrator implements gorm.Dialector.
func (dialector Dialector) Migrator(db *gorm.DB) gorm.Migrator {
	return Migrator{migrator.Migrator{Config: migrator.Config{
		DB:                          db,
		Dialector:                   dialector,
		CreateIndexAfterCreateTable: true,
	}}}
}

// BindVarTo implements gorm.Dialector. SQLite takes positional '?' holders.
func (Dialector) BindVarTo(writer clause.Writer, stmt *gorm.Statement, v interface{}) {
	writer.WriteByte('?')
}

// QuoteTo implements gorm.Dialector. Identifiers are quoted with backticks;
// embedded backticks are doubled and dotted paths are quoted per segment so
// "users.id" becomes `users`.`id`. Segments that already arrive fully quoted
// (leading backtick) are passed through with escaping preserved.
func (Dialector) QuoteTo(writer clause.Writer, str string) {
	var (
		underQuoted, selfQuoted bool
		continuousBacktick      int8
		shiftDelimiter          int8
	)

	for _, v := range []byte(str) {
		switch v {
		case '`':
			continuousBacktick++
			if continuousBacktick == 2 {
				writer.WriteString("``")
				continuousBacktick = 0
			}
		case '.':
			if continuousBacktick > 0 || !selfQuoted {
				shiftDelimiter = 0
				underQuoted = false
				continuousBacktick = 0
				writer.WriteString("`")
			}
			writer.WriteByte(v)
			continue
		default:
			if shiftDelimiter-continuousBacktick <= 0 && !underQuoted {
				writer.WriteString("`")
				underQuoted = true
				if selfQuoted = continuousBacktick > 0; selfQuoted {
					continuousBacktick -= 1
				}
			}

			for ; continuousBacktick > 0; continuousBacktick -= 1 {
				writer.WriteString("``")
			}

			writer.WriteByte(v)
		}
		shiftDelimiter++
	}

	if continuousBacktick > 0 && !selfQuoted {
		writer.WriteString("``")
	}
	writer.WriteString("`")
}

// Explain implements gorm.Dialector. Values are rendered double-quoted.
func (Dialector) Explain(sql string, vars ...interface{}) string {
	return logger.ExplainSQL(sql, nil, `"`, vars...)
}

// DataTypeOf implements gorm.Dialector, mapping GORM's abstract types onto
// SQLite's flexible storage classes.
func (Dialector) DataTypeOf(field *schema.Field) string {
	switch field.DataType {
	case schema.Bool:
		return "numeric"
	case schema.Int, schema.Uint:
		if field.AutoIncrement {
			// PRIMARY KEY AUTOINCREMENT on INTEGER creates the implicit
			// rowid alias with a strict monotonic guarantee:
			// https://www.sqlite.org/autoinc.html
			return "integer PRIMARY KEY AUTOINCREMENT"
		}
		return "integer"
	case schema.Float:
		return "real"
	case schema.String:
		return "text"
	case schema.Time:
		if val, ok := field.TagSettings["TYPE"]; ok {
			return val
		}
		return "datetime"
	case schema.Bytes:
		return "blob"
	}
	return string(field.DataType)
}

// SavePoint implements gorm.SavePointerDialectorInterface.
func (Dialector) SavePoint(tx *gorm.DB, name string) error {
	tx.Exec("SAVEPOINT " + name)
	return nil
}

// RollbackTo implements gorm.SavePointerDialectorInterface.
func (Dialector) RollbackTo(tx *gorm.DB, name string) error {
	tx.Exec("ROLLBACK TO SAVEPOINT " + name)
	return nil
}

// Translate implements gorm.ErrorTranslator, mapping driver errors onto the
// GORM sentinels so applications can use errors.Is without parsing messages.
func (Dialector) Translate(err error) error {
	if terr, ok := err.(*gosqlite.Error); ok {
		switch terr.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE,
			sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return gorm.ErrDuplicatedKey
		case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			return gorm.ErrForeignKeyViolated
		}
	}
	return err
}

// compareVersion compares dotted version strings numerically: -1, 0 or 1 as
// version1 <, == or > version2. Shorter strings compare as if padded with 0.
func compareVersion(version1, version2 string) int {
	n, m := len(version1), len(version2)
	i, j := 0, 0
	for i < n || j < m {
		x := 0
		for ; i < n && version1[i] != '.'; i++ {
			x = x*10 + int(version1[i]-'0')
		}
		i++
		y := 0
		for ; j < m && version2[j] != '.'; j++ {
			y = y*10 + int(version2[j]-'0')
		}
		j++
		if x > y {
			return 1
		}
		if x < y {
			return -1
		}
	}
	return 0
}
