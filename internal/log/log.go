// Package log defines custom slog levels for nanosec services.
package log

import "log/slog"

// LevelVerbose is a custom log level below DEBUG for high-frequency
// protocol messages (e.g. poll_pending_containers send/receive).
// Enable with LOG_LEVEL=VERBOSE.
const LevelVerbose = slog.Level(-8)
