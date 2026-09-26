package config

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// logLevel is shared by every handler SetupLogger builds, so SetLogLevel reaches the live logger.
var logLevel slog.LevelVar

// logLevels are the logLevel values validation accepts, in any case.
var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

func parseLogLevel(level string) slog.Level {
	if l, ok := logLevels[strings.ToLower(level)]; ok {
		return l
	}
	return slog.LevelInfo
}

func newLogHandler(level slog.Leveler, format string) slog.Handler {
	opts := &slog.HandlerOptions{Level: level, AddSource: true}
	if strings.EqualFold(format, "text") {
		return slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.NewJSONHandler(os.Stdout, opts)
}

func SetupLogger(level, format string) {
	logLevel.Set(parseLogLevel(level))
	slog.SetDefault(slog.New(newLogHandler(&logLevel, format)))
}

// builtinLogHandler is slog's own default, which writes plain text to stderr whatever logLevel and
// logFormat say.
var builtinLogHandler = slog.Default().Handler()

// warnLogger is where validation warns: the installed logger, or, while slog's built-in one is
// still in place (main calls Load before SetupLogger), a logger built from the config being loaded.
func warnLogger(cfg *Config) *slog.Logger {
	if l := slog.Default(); l.Handler() != builtinLogHandler {
		return l
	}
	return slog.New(newLogHandler(parseLogLevel(cfg.LogLevel), cfg.LogFormat))
}

// SetLogLevel changes the level of the logger SetupLogger installed, without replacing it: logLevel
// is hot on reload, logFormat needs a restart.
func SetLogLevel(level string) {
	next := parseLogLevel(level)
	prev := logLevel.Level()
	if prev == next {
		return
	}
	// Announced at the more verbose of the two levels, and logged at it too: an INFO line would be
	// dropped between warn and error.
	announce := min(prev, next)
	logLevel.Set(announce)
	slog.Log(context.Background(), announce, "log level changed", "from", prev.String(), "to", next.String())
	logLevel.Set(next)
}
