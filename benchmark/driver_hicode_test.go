//go:build !bench_glebarez

package bench

import (
	"database/sql"
	"fmt"

	hicode "github.com/hicode0101/go-sqlite"
	"gorm.io/gorm"
)

// driverLabel identifies the driver under test; it is printed once so
// benchmark logs are self-describing.
const driverLabel = "hicode0101/go-sqlite (modernc.org/sqlite v1.61.1)"

// newDialector returns the dialector under test for the given DSN.
func newDialector(dsn string) gorm.Dialector {
	return hicode.Open(dsn)
}

// init prints the driver label once per test binary.
func init() {
	fmt.Printf("DRIVER: %s\n", driverLabel)
	_ = sql.Open // keep database/sql linked for raw benchmarks
}
