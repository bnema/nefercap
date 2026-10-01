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
	// AllInOne is the command-less invocation: it stops an active recording,
	// otherwise opens the selector where Tab switches between shot and rec.
	// It is never named on the command line and takes no capture flags.
	AllInOne Command = "all-in-one"
	// Shot and Rec pick a target with the layer-shell selector; Rec toggles:
	// while a recording is active it stops that recording instead. Their mode
	// is fixed: no Tab toggle.
	Shot   Command = "shot"
	Rec    Command = "rec"
	Stop   Command = "stop"
	Status Command = "status"
	// Outputs, Screenshot and Record never open the selector. Outputs needs
	// no -file; Screenshot requires it unless clipboard-only, Record always.
	Outputs    Command = "outputs"
	Screenshot Command = "screenshot"
	Record     Command = "record"
)

// DefaultFPS is the recording frame rate when -fps is not given.
const DefaultFPS = 30

// ErrUsage wraps every command-line usage error.
var ErrUsage = errors.New("usage error")

// Options is the parsed command line. Video is set only for Record and Rec.
// Path is optional for Shot and Rec, where an empty Path lets the caller
// choose one. Clipboard with an empty Path means clipboard only.
type Options struct {
	Command   Command
	Output    string // output name; empty lets the caller choose
	Region    ports.Region
	Path      string
	Video     ports.VideoSettings
	Duration  time.Duration
	Debug     bool
	Clipboard bool
}

// Usage describes the command line.
func Usage() string {
	return fmt.Sprintf(`Usage: nefercap [command] [flags]

  (no command) all-in-one: stop an active recording, else open the selector
               in shot mode; Tab switches shot <-> rec. Takes no flags but
               -debug; settings come from the config file.

Interactive commands (layer-shell selector over the connected outputs):
  shot        select what to capture and save a PNG; fixed mode
  rec         select what to record; run again while recording to stop it
  stop        stop the active recording
  status      print the recording state

Selector keys and mouse:
  drag        select a region, accepted when the left button is released
  click       capture the whole monitor; after w, the current workspace
  r           back to region selection
  m           choose the whole monitor, Enter confirms
  w           choose the current workspace when known, click or Enter confirms
  1..9        pick that whole monitor from any overlay
  g           toggle full-monitor center and thirds guides (on by default)
  Tab         switch shot <-> rec (all-in-one only)
  Escape      cancel

Config: $XDG_CONFIG_HOME/nefercap/config (default ~/.config/nefercap/config)
  selector.grid = off                 hide guides on startup; G still toggles them
  screenshot.dir = ~/Pictures/Shots   absolute or ~/ path; default XDG Pictures/Screenshots
  screenshot.output = file+clipboard  file (default), file+clipboard or clipboard
  video.dir = ~/Videos                absolute or ~/ path; default XDG Videos

Script commands (never open the selector):
  outputs     list outputs
  screenshot  save a PNG screenshot
  record      record silent video until interrupted or target video length is reached

Other:
  version     print the nefercap version

Flags:
  -clipboard        copy PNG with wl-copy; no file unless -file is given (shot, screenshot)
  -debug            enable debug logging (all commands)
  -output NAME      output name (all except stop, status, outputs)
  -file PATH        new file to write; optional for shot, rec; screenshot requires it unless -clipboard; record requires it
  -region X,Y,WxH   output-local logical region; default is the full output (screenshot, record)
  -fps N            frames per second, 1..%d (rec, record, default %d)
  -size WxH         scaled video size, both even, at most %d each (rec, record, default source size)
  -duration D       encoded video length such as 30s; 0 records until stopped (rec, record)
`, ports.MaxFPS, DefaultFPS, ports.MaxDimension)
}

// Parse parses args (without the program name). Help requests print Usage to
// out and return flag.ErrHelp; other failures wrap ErrUsage.
func Parse(args []string, out io.Writer) (Options, error) {
	if out == nil {
		out = io.Discard
	}
	opts := Options{Command: AllInOne}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch cmd := Command(args[0]); cmd {
		case Shot, Rec, Stop, Status, Outputs, Screenshot, Record:
			opts.Command = cmd
			args = args[1:]
		default:
			return Options{}, usageErr("unknown command %q", args[0])
		}
	}

	scripted := opts.Command == Screenshot || opts.Command == Record
	interactive := opts.Command == Shot || opts.Command == Rec
	video := opts.Command == Record || opts.Command == Rec

	fs := flag.NewFlagSet("nefercap "+string(opts.Command), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.Debug, "debug", false, "")
	var region, size string
	fps := DefaultFPS
	if scripted || interactive {
		fs.StringVar(&opts.Output, "output", "", "")
		fs.StringVar(&opts.Path, "file", "", "")
	}
	if scripted {
		fs.StringVar(&region, "region", "", "")
	}
	if opts.Command == Shot || opts.Command == Screenshot {
		fs.BoolVar(&opts.Clipboard, "clipboard", false, "")
	}
	if video {
		fs.IntVar(&fps, "fps", DefaultFPS, "")
		fs.StringVar(&size, "size", "", "")
		fs.DurationVar(&opts.Duration, "duration", 0, "")
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(out, Usage())
			return Options{}, flag.ErrHelp
		}
		if opts.Command == AllInOne {
			return Options{}, errAllInOneFlags
		}
		return Options{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if fs.NArg() > 0 {
		return Options{}, usageErr("unexpected argument %q", fs.Arg(0))
	}
	if !scripted && !interactive {
		return opts, nil
	}

	if scripted && opts.Path == "" && !opts.Clipboard {
		return Options{}, usageErr("-file is required")
	}
	if interactive || opts.Clipboard {
		// Set explicitly so "-file ''" is rejected rather than treated as auto.
		explicit := false
		fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "file" })
		if explicit && opts.Path == "" {
			return Options{}, usageErr("-file must not be empty")
		}
	}
	if region != "" {
		r, err := parseRegion(region)
		if err != nil {
			return Options{}, usageErr("invalid -region %q: %v", region, err)
		}
		opts.Region = r
	}
	if video {
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

// usageError is a usage error that prints without the generic prefix.
type usageError string

func (e usageError) Error() string        { return string(e) }
func (e usageError) Is(target error) bool { return target == ErrUsage }

const errAllInOneFlags = usageError("nefercap: all-in-one mode takes no flags except -debug; it reads $XDG_CONFIG_HOME/nefercap/config (default ~/.config/nefercap/config); use shot, rec, screenshot or record for per-run overrides")

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
