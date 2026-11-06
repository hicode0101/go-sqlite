package sqlite

import "errors"

// ErrConstraintsNotImplemented is returned when a migration requests a
// foreign-key constraint that SQLite migrator cannot attach via table
// recreation. Callers usually silence it by enabling
// gorm.Config.DisableForeignKeyConstraintWhenMigrating, which stores
// references as plain columns instead of SQL constraints.
var ErrConstraintsNotImplemented = errors.New("constraints not implemented on sqlite, consider using DisableForeignKeyConstraintWhenMigrating, more details https://github.com/go-gorm/gorm/wiki/GORM-V2-Release-Note-Draft#all-new-migrator")
