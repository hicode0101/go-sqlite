//go:build bench_glebarez

package bench

import (
	"fmt"

	gleba "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// driverLabel identifies the driver under test; it is printed once so
// benchmark logs are self-describing.
const driverLabel = "glebarez/sqlite v1.11.0 (glebarez/go-sqlite v1.23.0)"

// newDialector returns the dialector under test for the given DSN.
func newDialector(dsn string) gorm.Dialector {
	return gleba.Open(dsn)
}

// init prints the driver label once per test binary.
func init() {
	fmt.Printf("DRIVER: %s\n", driverLabel)
}
