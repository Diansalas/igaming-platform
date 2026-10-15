// Package backup holds static guards for the A12 backup / restore / point-in-time-recovery scripts
// (backup_static_test.go). The scripts themselves are shell, SQL and two small Go programs
// (restorecheck, and the seeder in internal/httpserver/drillseed_integration_test.go).
package backup
