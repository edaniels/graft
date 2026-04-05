package slogor

import "log/slog"

type OptionFn func(*options)

// Options defines the options for configuring the Handler.
type options struct {
	// level is the minimum log level to handle.
	level slog.Leveler
	// addColorToBuf conditionally add color to the output buffer.
	addColorToBuf func([]byte, sgrCode) []byte
	// strLvl allow custom string to log level conversion.
	strLvl map[slog.Leveler]string
	// timeFormat specifies the time format for log records.
	// Empty string will remove the time in records.
	timeFormat string
	// showSource indicates whether to display the source of log records.
	showSource bool
}

type MapOfLevel = map[slog.Leveler]string

// Map for STD log level conversion.
var mapOfLevel = MapOfLevel{
	slog.LevelDebug: slog.LevelDebug.Level().String(),
	slog.LevelInfo:  slog.LevelInfo.Level().String() + " ",
	slog.LevelWarn:  slog.LevelWarn.Level().String() + " ",
	slog.LevelError: slog.LevelError.Level().String(),
}

// SetLevelStr set the handler "level to string" map.
// The default one is used if none specified.
func SetLevelStr(strLvl MapOfLevel) OptionFn {
	return func(options *options) { options.strLvl = strLvl }
}

// SetLevel set the minimum log level to handle.
// By default, level INFO is set.
func SetLevel(lvl slog.Leveler) OptionFn { return func(options *options) { options.level = lvl } }

// SetTimeFormat specifies the time format for the log records.
// By default, nothing is reported.
func SetTimeFormat(fmt string) OptionFn { return func(options *options) { options.timeFormat = fmt } }

// ShowSource indicates whether to display the source of the log records.
// By default, nothing is reported.
func ShowSource() OptionFn { return func(options *options) { options.showSource = true } }

// DisableColor.
func DisableColor() OptionFn {
	return func(options *options) {
		options.addColorToBuf = func(buf []byte, _ sgrCode) []byte { return buf }
	}
}

func getDefaultOptions() *options {
	return &options{
		strLvl:        mapOfLevel,
		level:         slog.LevelInfo,
		addColorToBuf: func(buf []byte, color sgrCode) []byte { return color.AppendTo(buf) },
	}
}

func (o *options) ConsumeFnOpt(fns ...OptionFn) *options {
	for _, fn := range fns {
		fn(o)
	}

	return o
}
