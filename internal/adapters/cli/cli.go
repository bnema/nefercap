// Package cli parses the command line into capture options.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/bnema/nefercap/internal/ports"
)

// Command is the selected subcommand.
type Command string

const (
	Outputs    Command = "outputs"
	Screenshot Command = "screenshot"
	Record     Command = "record"
	GUI        Command = "gui"
)

// DefaultFPS is the recording frame rate when -fps is not given.
const DefaultFPS = 30

// ErrUsage wraps every command-line usage error.
var ErrUsage = errors.New("usage error")

// Options is the parsed command line. Video is set only for Record.
type Options struct {
	Command  Command
	Output   string // output name; empty lets the caller choose
	Region   ports.Region
	Path     string
	Video    ports.VideoSettings
	Duration time.Duration
	Debug    bool
}

// Usage describes the command line.
func Usage() string {
	return fmt.Sprintf(`Usage: nefercap [command] [flags]

Commands:
  gui         open the capture control panel (default)
  outputs     list outputs
  screenshot  save a PNG screenshot
  record      record silent video until interrupted or -duration elapses

Flags:
  -debug            enable debug logging (all commands)
  -output NAME      output name (screenshot, record)
  -region X,Y,WxH   output-local logical region; default is the full output (screenshot, record)
  -file PATH        new file to write, required (screenshot, record)
  -fps N            frames per second, 1..%d (record, default %d)
  -size WxH         scaled video size, both even, at most %d each (record, default source size)
  -duration D       recording length such as 30s; 0 records until interrupted (record)
`, ports.MaxFPS, DefaultFPS, ports.MaxDimension)
}

// Parse parses args (without the program name). Help requests print Usage to
// out and return flag.ErrHelp; other failures wrap ErrUsage.
func Parse(args []string, out io.Writer) (Options, error) {
	if out == nil {
		out = io.Discard
	}
	opts := Options{Command: GUI}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch cmd := Command(args[0]); cmd {
		case Outputs, Screenshot, Record, GUI:
			opts.Command = cmd
			args = args[1:]
		default:
			return Options{}, usageErr("unknown command %q", args[0])
		}
	}

	fs := flag.NewFlagSet("nefercap "+string(opts.Command), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.Debug, "debug", false, "")
	var region, size string
	fps := DefaultFPS
	capture := opts.Command == Screenshot || opts.Command == Record
	if capture {
		fs.StringVar(&opts.Output, "output", "", "")
		fs.StringVar(&region, "region", "", "")
		fs.StringVar(&opts.Path, "file", "", "")
	}
	if opts.Command == Record {
		fs.IntVar(&fps, "fps", DefaultFPS, "")
		fs.StringVar(&size, "size", "", "")
		fs.DurationVar(&opts.Duration, "duration", 0, "")
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(out, Usage())
			return Options{}, flag.ErrHelp
		}
		return Options{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if fs.NArg() > 0 {
		return Options{}, usageErr("unexpected argument %q", fs.Arg(0))
	}
	if !capture {
		return opts, nil
	}

	if opts.Path == "" {
		return Options{}, usageErr("-file is required")
	}
	if region != "" {
		r, err := parseRegion(region)
		if err != nil {
			return Options{}, usageErr("invalid -region %q: %v", region, err)
		}
		opts.Region = r
	}
	if opts.Command == Record {
		if fps < 1 || fps > ports.MaxFPS {
			return Options{}, usageErr("invalid -fps %d: must be 1..%d", fps, ports.MaxFPS)
		}
		opts.Video.FPS = fps
		if size != "" {
			w, h, err := parseSize(size)
			if err != nil {
				return Options{}, usageErr("invalid -size %q: %v", size, err)
			}
			if w%2 != 0 || h%2 != 0 {
				return Options{}, usageErr("invalid -size %q: width and height must be even", size)
			}
			if w > ports.MaxDimension || h > ports.MaxDimension || w*ports.BytesPerPixel > ports.MaxFrameBytes/h {
				return Options{}, usageErr("invalid -size %q: exceeds frame limits", size)
			}
			opts.Video.Width, opts.Video.Height = w, h
		}
		if opts.Duration < 0 {
			return Options{}, usageErr("invalid -duration %s: must not be negative", opts.Duration)
		}
	}
	return opts, nil
}

func usageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUsage, fmt.Sprintf(format, a...))
}

// parseRegion parses "X,Y,WxH" with nonnegative X,Y and positive W,H, all
// within int32.
func parseRegion(s string) (ports.Region, error) {
	xy, wh, ok := strings.Cut(s, "x")
	parts := strings.Split(xy, ",")
	if !ok || len(parts) != 3 {
		return ports.Region{}, errors.New("want X,Y,WxH")
	}
	h, err := parseInt(wh, 1)
	if err != nil {
		return ports.Region{}, err
	}
	x, err := parseInt(parts[0], 0)
	if err != nil {
		return ports.Region{}, err
	}
	y, err := parseInt(parts[1], 0)
	if err != nil {
		return ports.Region{}, err
	}
	w, err := parseInt(parts[2], 1)
	if err != nil {
		return ports.Region{}, err
	}
	if int64(x)+int64(w) > math.MaxInt32 || int64(y)+int64(h) > math.MaxInt32 {
		return ports.Region{}, errors.New("region exceeds int32 bounds")
	}
	return ports.Region{X: x, Y: y, Width: w, Height: h}, nil
}

func parseSize(s string) (w, h int, err error) {
	ws, hs, ok := strings.Cut(s, "x")
	if !ok {
		return 0, 0, errors.New("want WxH")
	}
	if w, err = parseInt(ws, 1); err != nil {
		return 0, 0, err
	}
	if h, err = parseInt(hs, 1); err != nil {
		return 0, 0, err
	}
	return w, h, nil
}

// parseInt accepts plain decimal digits only, at least min, at most MaxInt32.
func parseInt(s string, min int) (int, error) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not a decimal number", s)
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is out of range", s)
	}
	if int(v) < min {
		return 0, fmt.Errorf("%q must be at least %d", s, min)
	}
	return int(v), nil
}
