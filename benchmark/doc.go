// Package bench runs the performance comparison suite between the
// hicode0101/go-sqlite GORM driver and the glebarez/sqlite GORM driver.
//
// Both drivers register a database/sql driver named "sqlite", so they can
// never be loaded into the same process. The suite therefore lives behind
// a build tag: the benchmark cases in bench_test.go are shared verbatim,
// while the two driver_*.go files provide the dialector wiring.
//
// Run against this repository's driver (default):
//
//	go test -bench . -benchmem -count 5 ./benchmark/
//
// Run against glebarez/sqlite:
//
//	go test -tags bench_glebarez -bench . -benchmem -count 5 ./benchmark/
//
// Results are aggregated in REPORT.md next to this file.
package bench
