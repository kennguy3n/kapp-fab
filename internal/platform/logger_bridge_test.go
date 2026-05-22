package platform

import "log"

// stdlibLogPrintf is a tiny indirection so logger_test.go can verify
// that stdlib log.Printf calls flow into the slog bridge installed by
// InstallDefault. Keeping the stdlib import in a separate file makes
// it obvious which test exercises the bridge.
func stdlibLogPrintf(format string, args ...any) {
	log.Printf(format, args...)
}
