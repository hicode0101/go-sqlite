package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gorm.io/gorm/migrator"
)

// sqliteSeparator lists the characters that can quote or delimit tokens in a
// CREATE TABLE body: backticks, double quotes, single quotes and tabs.
const sqliteSeparator = "`|\"|'|\t"

var (
	// uniqueRegexp captures the column list of "CONSTRAINT <name> UNIQUE (...)".
	uniqueRegexp = regexp.MustCompile(fmt.Sprintf(`^CONSTRAINT [%v]?[\w-]+[%v]? UNIQUE (.*)$`, sqliteSeparator, sqliteSeparator))
	// indexRegexp captures the "ON <table> (...)" tail of CREATE [UNIQUE] INDEX statements.
	indexRegexp = regexp.MustCompile(fmt.Sprintf(`(?is)CREATE(?: UNIQUE)? INDEX [%v]?[\w\d-]+[%v]?(?s:.*?)ON (.*)$`, sqliteSeparator, sqliteSeparator))
	// tableRegexp splits a CREATE TABLE statement into its head
	// ("CREATE TABLE <name>") and body (the parenthesized column list).
	tableRegexp = regexp.MustCompile(fmt.Sprintf(`(?is)(CREATE TABLE [%v]?[\w\d-]+[%v]?)(?:\s*\((.*)\))?`, sqliteSeparator, sqliteSeparator))
	// separatorRegexp matches any single separator character.
	separatorRegexp = regexp.MustCompile(fmt.Sprintf("[%v]", sqliteSeparator))
	// columnsRegexp extracts column names following '(' or ','.
	columnsRegexp = regexp.MustCompile(fmt.Sprintf(`[(,][%v]?(\w+)[%v]?`, sqliteSeparator, sqliteSeparator))
	// columnRegexp splits one column definition into name, type and modifiers.
	columnRegexp = regexp.MustCompile(fmt.Sprintf(`^[%v]?([\w\d]+)[%v]?\s+([\w\(\)\d]+)(.*)$`, sqliteSeparator, sqliteSeparator))
	// defaultValueRegexp captures a DEFAULT literal; the lookahead stops at a
	// space, COLLATE/GENERATED keywords or end-of-string so later modifiers
	// are not swallowed into the value.
	defaultValueRegexp = regexp.MustCompile(`(?i) DEFAULT \(?(.+)?\)?( |COLLATE|GENERATED|$)`)
	// regRealDataType extracts the first size suffix, e.g. the 8 in varchar(8).
	regRealDataType = regexp.MustCompile(`[^\d](\d+)[^\d]?`)
)

// getAllColumns extracts the column names from a parenthesized list such as
// "(`a`,`b`)" or "(a, b)".
func getAllColumns(s string) []string {
	allMatches := columnsRegexp.FindAllStringSubmatch(s, -1)
	columns := make([]string, 0, len(allMatches))
	for _, matches := range allMatches {
		if len(matches) > 1 {
			columns = append(columns, matches[1])
		}
	}
	return columns
}

// ddl is a parsed CREATE TABLE statement. head is the "CREATE TABLE <name>"
// prefix, fields the raw column/table-constraint definitions between the
// outer parentheses, and columns the per-column metadata parsed out of
// fields (populated only for genuine column definitions).
type ddl struct {
	head    string
	fields  []string
	columns []migrator.ColumnType
}

// parseDDL parses one or more SQL statements. The first CREATE TABLE wins:
// its head, fields and columns are extracted. CREATE INDEX statements are
// tolerated (and ignored) because the migrator queries table and index DDL
// together. Anything else is invalid.
//
// Body splitting is quote- and bracket-aware: commas inside string literals
// or nested parentheses (e.g. CHECK (x IN ('a,b'))) do not split fields, and
// doubled quotes inside literals are treated as escapes.
func parseDDL(strs ...string) (*ddl, error) {
	var result ddl
	for _, str := range strs {
		if sections := tableRegexp.FindStringSubmatch(str); len(sections) > 0 {
			var (
				ddlBody      = sections[2]
				ddlBodyRunes = []rune(ddlBody)
				bracketLevel int
				quote        rune
				buf          string
			)
			ddlBodyRunesLen := len(ddlBodyRunes)

			result.head = sections[1]

			for idx := 0; idx < ddlBodyRunesLen; idx++ {
				var (
					next rune = 0
					c         = ddlBodyRunes[idx]
				)
				if idx+1 < ddlBodyRunesLen {
					next = ddlBodyRunes[idx+1]
				}

				if sc := string(c); separatorRegexp.MatchString(sc) {
					if c == next {
						buf += sc // Skip escaped quote
						idx++
					} else if quote > 0 {
						quote = 0
					} else {
						quote = c
					}
				} else if quote == 0 {
					if c == '(' {
						bracketLevel++
					} else if c == ')' {
						bracketLevel--
					} else if bracketLevel == 0 {
						if c == ',' {
							result.fields = append(result.fields, strings.TrimSpace(buf))
							buf = ""
							continue
						}
					}
				}

				if bracketLevel < 0 {
					return nil, errors.New("invalid DDL, unbalanced brackets")
				}

				buf += string(c)
			}

			if bracketLevel != 0 {
				return nil, errors.New("invalid DDL, unbalanced brackets")
			}

			if buf != "" {
				result.fields = append(result.fields, strings.TrimSpace(buf))
			}

			// Classify each field: table constraints (CHECK /
			// CONSTRAINT ... UNIQUE / PRIMARY KEY (...)) carry
			// per-column metadata onto existing columns; anything
			// else is a column definition of its own.
			for _, f := range result.fields {
				fUpper := strings.ToUpper(f)
				if strings.HasPrefix(fUpper, "CHECK") {
					continue
				}
				if strings.HasPrefix(fUpper, "CONSTRAINT") {
					matches := uniqueRegexp.FindStringSubmatch(f)
					if len(matches) > 0 {
						if columns := getAllColumns(matches[1]); len(columns) == 1 {
							for idx, column := range result.columns {
								if column.NameValue.String == columns[0] {
									column.UniqueValue = sql.NullBool{Bool: true, Valid: true}
									result.columns[idx] = column
									break
								}
							}
						}
					}
					continue
				}
				if strings.HasPrefix(fUpper, "PRIMARY KEY") {
					for _, name := range getAllColumns(f) {
						for idx, column := range result.columns {
							if column.NameValue.String == name {
								column.PrimaryKeyValue = sql.NullBool{Bool: true, Valid: true}
								result.columns[idx] = column
								break
							}
						}
					}
				} else if matches := columnRegexp.FindStringSubmatch(f); len(matches) > 0 {
					columnType := migrator.ColumnType{
						NameValue:         sql.NullString{String: matches[1], Valid: true},
						DataTypeValue:     sql.NullString{String: matches[2], Valid: true},
						ColumnTypeValue:   sql.NullString{String: matches[2], Valid: true},
						PrimaryKeyValue:   sql.NullBool{Valid: true},
						UniqueValue:       sql.NullBool{Valid: true},
						NullableValue:     sql.NullBool{Bool: true, Valid: true},
						DefaultValueValue: sql.NullString{Valid: false},
					}

					matchUpper := strings.ToUpper(matches[3])
					if strings.Contains(matchUpper, " NOT NULL") {
						columnType.NullableValue = sql.NullBool{Bool: false, Valid: true}
					} else if strings.Contains(matchUpper, " NULL") {
						columnType.NullableValue = sql.NullBool{Bool: true, Valid: true}
					}
					if strings.Contains(matchUpper, " UNIQUE") {
						columnType.UniqueValue = sql.NullBool{Bool: true, Valid: true}
					}
					if strings.Contains(matchUpper, " PRIMARY") {
						columnType.PrimaryKeyValue = sql.NullBool{Bool: true, Valid: true}
					}
					if defaultMatches := defaultValueRegexp.FindStringSubmatch(matches[3]); len(defaultMatches) > 1 {
						if strings.ToLower(defaultMatches[1]) != "null" {
							columnType.DefaultValueValue = sql.NullString{String: strings.Trim(defaultMatches[1], `"`), Valid: true}
						}
					}

					// Split a size suffix off the declared type, e.g.
					// "varchar(100)" -> type "varchar", length 100.
					matches := regRealDataType.FindAllStringSubmatch(columnType.DataTypeValue.String, -1)
					if len(matches) == 1 && len(matches[0]) == 2 {
						size, _ := strconv.Atoi(matches[0][1])
						columnType.LengthValue = sql.NullInt64{Valid: true, Int64: int64(size)}
						columnType.DataTypeValue.String = strings.TrimSuffix(columnType.DataTypeValue.String, matches[0][0])
					}

					result.columns = append(result.columns, columnType)
				}
			}
		} else if matches := indexRegexp.FindStringSubmatch(str); len(matches) > 0 {
			// CREATE INDEX: intentionally ignored, it carries no column
			// metadata beyond what the table DDL already provided.
		} else {
			return nil, errors.New("invalid DDL")
		}
	}

	return &result, nil
}

// clone returns a deep-enough copy (slices copied, values are value types).
func (d *ddl) clone() *ddl {
	copied := new(ddl)
	*copied = *d

	copied.fields = make([]string, len(d.fields))
	copy(copied.fields, d.fields)
	copied.columns = make([]migrator.ColumnType, len(d.columns))
	copy(copied.columns, d.columns)

	return copied
}

// compile renders the statement back to SQL: head plus parenthesized fields.
func (d *ddl) compile() string {
	if len(d.fields) == 0 {
		return d.head
	}
	return fmt.Sprintf("%s (%s)", d.head, strings.Join(d.fields, ","))
}

// renameTable rewrites the table name inside head. src may appear quoted or
// unquoted; it is replaced with the back-quoted dst.
func (d *ddl) renameTable(dst, src string) error {
	tableReg, err := regexp.Compile("\\s*('|`|\")?\\b" + regexp.QuoteMeta(src) + "\\b('|`|\")?\\s*")
	if err != nil {
		return err
	}

	replaced := tableReg.ReplaceAllString(d.head, fmt.Sprintf(" `%s` ", dst))
	if replaced == d.head {
		return fmt.Errorf("failed to look up tablename `%s` from DDL head '%s'", src, d.head)
	}

	d.head = replaced
	return nil
}

// addConstraint replaces the definition of an existing constraint with the
// same name, or appends it when absent.
func (d *ddl) addConstraint(name string, sql string) {
	reg := regexp.MustCompile("^CONSTRAINT [\"`]?" + regexp.QuoteMeta(name) + "[\"` ]")

	for i := 0; i < len(d.fields); i++ {
		if reg.MatchString(d.fields[i]) {
			d.fields[i] = sql
			return
		}
	}
	d.fields = append(d.fields, sql)
}

// removeConstraint deletes a named constraint, reporting whether it existed.
func (d *ddl) removeConstraint(name string) bool {
	reg := regexp.MustCompile("^CONSTRAINT [\"`]?" + regexp.QuoteMeta(name) + "[\"` ]")

	for i := 0; i < len(d.fields); i++ {
		if reg.MatchString(d.fields[i]) {
			d.fields = append(d.fields[:i], d.fields[i+1:]...)
			return true
		}
	}
	return false
}

// hasConstraint reports whether a named constraint is present.
func (d *ddl) hasConstraint(name string) bool {
	reg := regexp.MustCompile("^CONSTRAINT [\"`]?" + regexp.QuoteMeta(name) + "[\"` ]")

	for _, f := range d.fields {
		if reg.MatchString(f) {
			return true
		}
	}
	return false
}

// getColumns returns the back-quoted names of every real column definition,
// skipping table constraints and generated columns: those must not appear in
// the INSERT ... SELECT used by table recreation.
func (d *ddl) getColumns() []string {
	res := []string{}

	for _, f := range d.fields {
		fUpper := strings.ToUpper(f)
		if strings.HasPrefix(fUpper, "PRIMARY KEY") ||
			strings.HasPrefix(fUpper, "CHECK") ||
			strings.HasPrefix(fUpper, "CONSTRAINT") ||
			strings.Contains(fUpper, "GENERATED ALWAYS AS") {
			continue
		}

		reg := regexp.MustCompile("^[\"`']?([\\w\\d]+)[\"`']?")
		match := reg.FindStringSubmatch(f)
		if match != nil {
			res = append(res, "`"+match[1]+"`")
		}
	}
	return res
}

// removeColumn deletes the named column definition, reporting whether the
// column was found.
func (d *ddl) removeColumn(name string) bool {
	reg := regexp.MustCompile("^(`|'|\"| )" + regexp.QuoteMeta(name) + "(`|'|\"| ) .*?$")

	for i := 0; i < len(d.fields); i++ {
		if reg.MatchString(d.fields[i]) {
			d.fields = append(d.fields[:i], d.fields[i+1:]...)
			return true
		}
	}
	return false
}
