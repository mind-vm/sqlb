// Package logs provides the process logger and routes fx's own boot events
// into it.
//
// The second half is the part worth having. Without fx.WithLogger, fx prints
// its provide/invoke/start events to stderr in its own format, so a service
// whose logs are structured has one component that is not — and the boot
// sequence, which is exactly what you read when a start-up fails, is the part
// that goes missing from the log aggregator.
package logs

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/mind-vm/sqlb/example/fxapp/config"
)

var Module = fx.Module("logs",
	fx.Provide(
		NewConfig,
		NewLogger,
	),
	// fx's own events, through the same logger. The constructor is a plain
	// dependency like any other, so this line is also the shortest possible
	// demonstration of the container being used on itself.
	fx.WithLogger(func(log *slog.Logger) fxevent.Logger {
		return &fxevent.SlogLogger{Logger: log.With("component", "fx")}
	}),
)

// Config is what the logger needs.
type Config struct {
	// Format is "text" (the default, for a terminal) or "json" (for a log
	// aggregator).
	Format string

	// Level is debug, info, warn or error.
	Level slog.Level
}

// NewConfig reads FXAPP_LOG_FORMAT and FXAPP_LOG_LEVEL.
func NewConfig() (Config, error) {
	cfg := Config{Format: strings.ToLower(config.Get("LOG_FORMAT", "text"))}
	if cfg.Format != "text" && cfg.Format != "json" {
		return Config{}, fmt.Errorf("logs: FXAPP_LOG_FORMAT is %q; it must be text or json", cfg.Format)
	}
	if err := cfg.Level.UnmarshalText([]byte(config.Get("LOG_LEVEL", "info"))); err != nil {
		return Config{}, fmt.Errorf("logs: FXAPP_LOG_LEVEL: %w", err)
	}
	return cfg, nil
}

// NewLogger builds the process logger and installs it as slog's default.
//
// The default is set because the code that logs is not all reachable from
// here: sqlb takes no logger, and a hook or a driver that decides to log does
// it through slog.Default(). Setting it once, from the module that owns the
// decision, is better than every such call site inventing its own.
func NewLogger(cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Level}

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}

	log := slog.New(handler)
	slog.SetDefault(log)
	return log
}
