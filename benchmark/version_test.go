package bench

import (
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestSQLiteVersion reports the SQLite core version and driver identity
// under whichever build tag is active. Run with -v for both tag variants to
// collect the environment rows of the report.
func TestSQLiteVersion(t *testing.T) {
	db, err := gorm.Open(newDialector(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	var v string
	if err := db.Raw("select sqlite_version()").Scan(&v).Error; err != nil {
		t.Fatal(err)
	}
	t.Logf("driver: %s | sqlite core: %s", driverLabel, v)
}
