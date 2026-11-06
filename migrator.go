package sqlite

import (
	"database/sql"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

// Migrator implements gorm.Migrator for SQLite.
//
// SQLite can ALTER only a small subset of table shapes: RENAME and ADD
// COLUMN work directly, but changing a column's type, dropping a column or
// editing constraints requires the classic 12-step copy recipe
// (https://www.sqlite.org/lang_altertable.html#otheralter). This migrator
// implements that recipe once in recreateTable and routes every structural
// change through it.
type Migrator struct {
	migrator.Migrator
}

// RunWithoutForeignKey executes fc with foreign-key enforcement turned off,
// restoring the previous setting afterwards. Needed because DROP/RENAME
// inside table recreation would otherwise trip deferred references.
func (m Migrator) RunWithoutForeignKey(fc func() error) error {
	var enabled int
	m.DB.Raw("PRAGMA foreign_keys").Scan(&enabled)
	if enabled == 1 {
		m.DB.Exec("PRAGMA foreign_keys = OFF")
		defer m.DB.Exec("PRAGMA foreign_keys = ON")
	}
	return fc()
}

// HasTable reports whether a table named after value exists in sqlite_master.
func (m Migrator) HasTable(value interface{}) bool {
	var count int
	m.Migrator.RunWithValue(value, func(stmt *gorm.Statement) error {
		return m.DB.Raw(
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", stmt.Table,
		).Row().Scan(&count)
	})
	return count > 0
}

// DropTable drops the given tables in reverse dependency order, with
// foreign keys disabled for the duration.
func (m Migrator) DropTable(values ...interface{}) error {
	return m.RunWithoutForeignKey(func() error {
		values = m.ReorderModels(values, false)
		tx := m.DB.Session(&gorm.Session{})
		for i := len(values) - 1; i >= 0; i-- {
			if err := m.RunWithValue(values[i], func(stmt *gorm.Statement) error {
				return tx.Exec("DROP TABLE IF EXISTS ?", clause.Table{Name: stmt.Table}).Error
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetTables lists every user table in the current database.
func (m Migrator) GetTables() (tableList []string, err error) {
	return tableList, m.DB.Raw("SELECT name FROM sqlite_master where type=?", "table").Scan(&tableList).Error
}

// HasColumn reports whether the named column appears in the table's DDL.
// Column names cannot be queried from a catalog, so the CREATE TABLE text
// stored in sqlite_master is pattern-matched with the field's quoting
// variants (plain, `"`, “ ` “, `[`, tab-separated).
func (m Migrator) HasColumn(value interface{}, name string) bool {
	var count int
	m.Migrator.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema != nil {
			if field := stmt.Schema.LookUpField(name); field != nil {
				name = field.DBName
			}
		}
		if name != "" {
			m.DB.Raw(
				"SELECT count(*) FROM sqlite_master WHERE type = ? AND tbl_name = ? AND (sql LIKE ? OR sql LIKE ? OR sql LIKE ? OR sql LIKE ? OR sql LIKE ?)",
				"table", stmt.Table,
				`%"`+name+`" %`, `%`+name+` %`, "%`"+name+"`%", "%["+name+"]%", "%\t"+name+"\t%",
			).Row().Scan(&count)
		}
		return nil
	})
	return count > 0
}

// AlterColumn changes a column's definition. SQLite cannot do this in place,
// so the table is recreated and the column definition is substituted from
// the current schema. A UNIQUE constraint on the old definition is rebuilt
// too, because the regenerated column type omits it.
func (m Migrator) AlterColumn(value interface{}, name string) error {
	return m.RunWithoutForeignKey(func() error {
		return m.recreateTable(value, nil, func(ddl *ddl, stmt *gorm.Statement) (*ddl, []interface{}, error) {
			if field := stmt.Schema.LookUpField(name); field != nil {
				var sqlArgs []interface{}
				for i, f := range ddl.fields {
					if matches := columnRegexp.FindStringSubmatch(f); len(matches) > 1 && matches[1] == field.DBName {
						ddl.fields[i] = fmt.Sprintf("`%v` ?", field.DBName)
						sqlArgs = []interface{}{m.FullDataTypeOf(field)}
						// Tables created by older versions store the
						// UNIQUE flag inline in the column definition;
						// FullDataTypeOf does not carry it, so rebuild
						// the standalone constraint.
						if strings.Contains(strings.ToUpper(matches[3]), " UNIQUE") {
							uniName := m.DB.NamingStrategy.UniqueName(stmt.Table, field.DBName)
							uni, _ := m.GuessConstraintInterfaceAndTable(stmt, uniName)
							if uni != nil {
								uniSQL, uniArgs := uni.Build()
								ddl.addConstraint(uniName, uniSQL)
								sqlArgs = append(sqlArgs, uniArgs...)
							}
						}
						break
					}
				}
				return ddl, sqlArgs, nil
			}
			return nil, nil, fmt.Errorf("failed to alter field with name %v", name)
		})
	})
}

// ColumnTypes describes the table's columns by combining the DDL stored in
// sqlite_master (declared types, defaults, constraints) with the runtime
// column metadata of a sampled row.
func (m Migrator) ColumnTypes(value interface{}) ([]gorm.ColumnType, error) {
	columnTypes := make([]gorm.ColumnType, 0)
	execErr := m.RunWithValue(value, func(stmt *gorm.Statement) (err error) {
		var (
			sqls   []string
			sqlDDL *ddl
		)
		if err := m.DB.Raw(
			"SELECT sql FROM sqlite_master WHERE type IN ? AND tbl_name = ? AND sql IS NOT NULL order by type = ? desc",
			[]string{"table", "index"}, stmt.Table, "table",
		).Scan(&sqls).Error; err != nil {
			return err
		}
		if sqlDDL, err = parseDDL(sqls...); err != nil {
			return err
		}

		rows, err := m.DB.Session(&gorm.Session{}).Table(stmt.Table).Limit(1).Rows()
		if err != nil {
			return err
		}
		defer func() {
			err = rows.Close()
		}()

		rawColumnTypes, err := rows.ColumnTypes()
		if err != nil {
			return err
		}

		for _, c := range rawColumnTypes {
			columnType := migrator.ColumnType{SQLColumnType: c}
			for _, column := range sqlDDL.columns {
				if column.NameValue.String == c.Name() {
					column.SQLColumnType = c
					columnType = column
					break
				}
			}
			columnTypes = append(columnTypes, columnType)
		}
		return err
	})
	return columnTypes, execErr
}

// DropColumn removes a column from the table definition. SQLite >= 3.35 has
// native DROP COLUMN, but it refuses several shapes (indexed or referenced
// columns), so the recreate path is used uniformly.
func (m Migrator) DropColumn(value interface{}, name string) error {
	return m.recreateTable(value, nil, func(ddl *ddl, stmt *gorm.Statement) (*ddl, []interface{}, error) {
		if field := stmt.Schema.LookUpField(name); field != nil {
			name = field.DBName
		}
		ddl.removeColumn(name)
		return ddl, nil, nil
	})
}

// CreateConstraint (re)adds a named table constraint.
func (m Migrator) CreateConstraint(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraint, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		return m.recreateTable(value, &table, func(ddl *ddl, stmt *gorm.Statement) (*ddl, []interface{}, error) {
			if constraint == nil {
				return nil, nil, nil
			}
			constraintSQL, constraintValues := constraint.Build()
			ddl.addConstraint(constraint.GetName(), constraintSQL)
			return ddl, constraintValues, nil
		})
	})
}

// DropConstraint removes a named table constraint.
func (m Migrator) DropConstraint(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraint, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		if constraint != nil {
			name = constraint.GetName()
		}
		return m.recreateTable(value, &table, func(ddl *ddl, stmt *gorm.Statement) (*ddl, []interface{}, error) {
			ddl.removeConstraint(name)
			return ddl, nil, nil
		})
	})
}

// HasConstraint reports whether a CONSTRAINT declaration with the given name
// appears in the table's DDL.
func (m Migrator) HasConstraint(value interface{}, name string) bool {
	var count int64
	m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraint, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		if constraint != nil {
			name = constraint.GetName()
		}
		m.DB.Raw(
			"SELECT count(*) FROM sqlite_master WHERE type = ? AND tbl_name = ? AND (sql LIKE ? OR sql LIKE ? OR sql LIKE ? OR sql LIKE ? OR sql LIKE ?)",
			"table", table,
			`%CONSTRAINT "`+name+`" %`, `%CONSTRAINT `+name+` %`,
			"%CONSTRAINT `"+name+"`%", "%CONSTRAINT ["+name+"]%", "%CONSTRAINT \t"+name+"\t%",
		).Row().Scan(&count)
		return nil
	})
	return count > 0
}

// CurrentDatabase returns the name of the current database. In SQLite the
// main (default) database is always named "main", per PRAGMA database_list.
func (m Migrator) CurrentDatabase() (name string) {
	var null interface{}
	m.DB.Raw("PRAGMA database_list").Row().Scan(&null, &name, &null)
	return
}

// BuildIndexOptions renders index field options (expressions, collations,
// sort directions) as bindable fragments.
func (m Migrator) BuildIndexOptions(opts []schema.IndexOption, stmt *gorm.Statement) (results []interface{}) {
	for _, opt := range opts {
		str := stmt.Quote(opt.DBName)
		if opt.Expression != "" {
			str = opt.Expression
		}
		if opt.Collate != "" {
			str += " COLLATE " + opt.Collate
		}
		if opt.Sort != "" {
			str += " " + opt.Sort
		}
		results = append(results, clause.Expr{SQL: str})
	}
	return
}

// CreateIndex issues CREATE INDEX for a schema-declared index.
func (m Migrator) CreateIndex(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema != nil {
			if idx := stmt.Schema.LookIndex(name); idx != nil {
				opts := m.BuildIndexOptions(idx.Fields, stmt)
				values := []interface{}{clause.Column{Name: idx.Name}, clause.Table{Name: stmt.Table}, opts}

				createIndexSQL := "CREATE "
				if idx.Class != "" {
					createIndexSQL += idx.Class + " "
				}
				createIndexSQL += "INDEX ?"
				if idx.Type != "" {
					createIndexSQL += " USING " + idx.Type
				}
				createIndexSQL += " ON ??"
				if idx.Where != "" {
					createIndexSQL += " WHERE " + idx.Where
				}
				return m.DB.Exec(createIndexSQL, values...).Error
			}
		}
		return fmt.Errorf("failed to create index with name %v", name)
	})
}

// HasIndex reports whether the given index exists on the table.
func (m Migrator) HasIndex(value interface{}, name string) bool {
	var count int
	m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema != nil {
			if idx := stmt.Schema.LookIndex(name); idx != nil {
				name = idx.Name
			}
		}
		if name != "" {
			m.DB.Raw(
				"SELECT count(*) FROM sqlite_master WHERE type = ? AND tbl_name = ? AND name = ?",
				"index", stmt.Table, name,
			).Row().Scan(&count)
		}
		return nil
	})
	return count > 0
}

// RenameIndex renames an index by dropping it and recreating it under the
// new name with the original SQL (SQLite has no ALTER INDEX).
func (m Migrator) RenameIndex(value interface{}, oldName, newName string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		var sql string
		m.DB.Raw(
			"SELECT sql FROM sqlite_master WHERE type = ? AND tbl_name = ? AND name = ?",
			"index", stmt.Table, oldName,
		).Row().Scan(&sql)
		if sql != "" {
			if err := m.DropIndex(value, oldName); err != nil {
				return err
			}
			return m.DB.Exec(strings.Replace(sql, oldName, newName, 1)).Error
		}
		return fmt.Errorf("failed to find index with name %v", oldName)
	})
}

// DropIndex drops an index by name.
func (m Migrator) DropIndex(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema != nil {
			if idx := stmt.Schema.LookIndex(name); idx != nil {
				name = idx.Name
			}
		}
		return m.DB.Exec("DROP INDEX ?", clause.Column{Name: name}).Error
	})
}

// Index mirrors one row of PRAGMA index_list.
type Index struct {
	Seq     int
	Name    string
	Unique  bool
	Origin  string
	Partial bool
}

// GetIndexes lists the table's indexes from PRAGMA index_list, fetching each
// index's columns from PRAGMA index_info. Indexes with origin "u" (backing a
// UNIQUE column constraint) are skipped — they are constraints, not indexes
// the schema declared.
//
// See https://www.sqlite.org/pragma.html#pragma_index_list
func (m Migrator) GetIndexes(value interface{}) ([]gorm.Index, error) {
	indexes := make([]gorm.Index, 0)
	err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		rst := make([]*Index, 0)
		if err := m.DB.Debug().Raw("SELECT * FROM PRAGMA_index_list(?)", stmt.Table).Scan(&rst).Error; err != nil {
			return err
		}
		for _, index := range rst {
			if index.Origin == "u" {
				continue
			}
			var columns []string
			if err := m.DB.Raw("SELECT name FROM PRAGMA_index_info(?)", index.Name).Scan(&columns).Error; err != nil {
				return err
			}
			indexes = append(indexes, &migrator.Index{
				TableName:       stmt.Table,
				NameValue:       index.Name,
				ColumnList:      columns,
				PrimaryKeyValue: sql.NullBool{Bool: index.Origin == "pk", Valid: true},
				UniqueValue:     sql.NullBool{Bool: index.Unique, Valid: true},
			})
		}
		return nil
	})
	return indexes, err
}

// getRawDDL fetches the CREATE TABLE statement sqlite_master stores for the
// given table.
func (m Migrator) getRawDDL(table string) (string, error) {
	var createSQL string
	m.DB.Raw(
		"SELECT sql FROM sqlite_master WHERE type = ? AND tbl_name = ? AND name = ?",
		"table", table, table,
	).Row().Scan(&createSQL)
	if m.DB.Error != nil {
		return "", m.DB.Error
	}
	return createSQL, nil
}

// recreateTable rewrites a table's DDL and copies its data across, the
// canonical way to alter column definitions or constraints in SQLite:
//
//  1. read the original CREATE TABLE from sqlite_master;
//  2. let getCreateSQL mutate a parsed copy of it;
//  3. create the result under a temporary name, with any bind arguments;
//  4. INSERT INTO temp SELECT * FROM original (matching column lists only);
//  5. DROP the original and RENAME the temp table into place.
//
// All of steps 3-5 run inside one transaction.
func (m Migrator) recreateTable(
	value interface{}, tablePtr *string,
	getCreateSQL func(ddl *ddl, stmt *gorm.Statement) (*ddl, []interface{}, error),
) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		table := stmt.Table
		if tablePtr != nil {
			table = *tablePtr
		}

		rawDDL, err := m.getRawDDL(table)
		if err != nil {
			return err
		}
		originDDL, err := parseDDL(rawDDL)
		if err != nil {
			return err
		}

		createDDL, sqlArgs, err := getCreateSQL(originDDL.clone(), stmt)
		if err != nil {
			return err
		}
		if createDDL == nil {
			return nil
		}

		newTableName := table + "__temp"
		if err := createDDL.renameTable(newTableName, table); err != nil {
			return err
		}

		columns := createDDL.getColumns()
		createSQL := createDDL.compile()

		return m.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(createSQL, sqlArgs...).Error; err != nil {
				return err
			}
			queries := []string{
				fmt.Sprintf("INSERT INTO `%v`(%v) SELECT %v FROM `%v`",
					newTableName, strings.Join(columns, ","), strings.Join(columns, ","), table),
				fmt.Sprintf("DROP TABLE `%v`", table),
				fmt.Sprintf("ALTER TABLE `%v` RENAME TO `%v`", newTableName, table),
			}
			for _, query := range queries {
				if err := tx.Exec(query).Error; err != nil {
					return err
				}
			}
			return nil
		})
	})
}
