# nefercap

Fast Wayland screenshots and silent video recording with a NeferGUI layer-shell selector.

## Quick capture

```sh
make bin
./bin/nefercap
./bin/nefercap shot
./bin/nefercap shot -clipboard
./bin/nefercap shot -clipboard -file capture.png
./bin/nefercap rec
./bin/nefercap stop
./bin/nefercap status
```

`nefercap` without a command is all-in-one: it stops an active recording, otherwise opens the selector in shot mode. **Tab** switches shot ⇄ rec and keeps the chosen region, monitor or workspace; Enter or a click confirms in the current mode. It takes no flags except `-debug` and reads its settings from the config file. `shot` and `rec` keep a fixed mode (no Tab) and accept `-file`/`-clipboard` (shot) to override the config per run. A badge at the top left of every overlay always shows the mode: `SHOT`, or a red `● REC`.

The selector covers the connected outputs, up to nine:

- **Drag and release:** capture a rectangle, clamped to its monitor.
- **Click:** capture the monitor; small pointer jitter counts as a click.
- **M, then Enter:** select the monitor.
- **W, then Enter:** select the current workspace when native workspace metadata is available.
- **R:** return to rectangle selection. **G:** toggle full-monitor center and thirds guides.
- **1–9:** select that monitor. **Esc:** cancel without creating a capture.

The selector shows mode keys and the current confirmation action in a two-line legend. Workspace hints appear only when a current workspace is available. Text uses a 1rem base (16 logical pixels), with proportional spacing and controls; the output's fractional scaling determines physical size.

The guides are visible by default: three vertical and three horizontal medium-gray lines, one physical pixel wide, aligned to each monitor's scaling. They stay fixed while dragging. Settings live in `$XDG_CONFIG_HOME/nefercap/config` (default `~/.config/nefercap/config`):

```ini
selector.grid = on
screenshot.dir = ~/Pictures/Screenshots
screenshot.output = file+clipboard
video.dir = ~/Videos
```

- `selector.grid`: `on` (default) or `off` to hide the guides on startup.
- `screenshot.dir`, `video.dir`: an absolute path or `~/…`; relative paths are errors. Absent, the XDG defaults below apply. Before the selector opens, nefercap creates (private) and checks each directory the command can use: both for all-in-one, one for `shot` or `rec`.
- `screenshot.output`: `file` (default), `file+clipboard` (save, then copy the same PNG) or `clipboard` (copy only; no file, no path printed).

Each key may appear once; blank lines and `#` comment lines are allowed; unknown keys, duplicates and invalid values are errors reported before the selector opens. `screenshot.output` and the directories apply to all-in-one, `shot` and `rec`; the scripted `screenshot` and `record` never read them. On `shot`, `-clipboard` alone copies only, `-clipboard -file PATH` saves and copies, and `-file PATH` alone saves only, whatever `screenshot.output` says. **G** changes visibility for the current monitor's overlay without rewriting the config. Selection dimensions are logical pixels; the captured file's dimensions depend on the monitor scaling.

A screenshot saves immediately. Recording displays a red border around the visible target and a small **REC / Stop** HUD. Both are excluded from nefercap's video. The HUD takes no keyboard focus; clicks outside Stop pass through. Press the recording shortcut again, click Stop, or run `nefercap stop` to finish the file.

Monitor recording follows workspace changes. A workspace recording follows its stable identity and continues when another workspace is displayed. A fixed region stays attached to its monitor. A hidden workspace has no border on the unrelated visible workspace; the recording HUD remains available.

Without `screenshot.dir`/`video.dir`, screenshots go to the configured XDG Pictures directory's `Screenshots` subdirectory, and videos to the XDG Videos directory. Names include a nanosecond timestamp. `-file` chooses an explicit destination. Files are private to their owner and are never overwritten. The saved path is printed to stdout, including for scripted commands. Cancelling before any frame is written creates no file and prints no path.

`shot -clipboard` copies an `image/png` screenshot using `wl-copy` without creating a capture file or printing a path. Add `-file PATH` to save and copy the same PNG. A successfully saved file is retained and its path printed even if clipboard copying later fails. The clipboard owner intentionally survives nefercap's exit. This CLI-only option also works with scripted `screenshot`; other commands reject it. Install `wl-clipboard` to use it.

`shot` while a recording is active is refused: its overlay is not part of the existing session's authorized controls. A scripted `screenshot` still uses standard capture and includes the active HUD and border.

## Requirements

- Go 1.27 for development; normal builds use `CGO_ENABLED=0`.
- For selection: layer-shell v4, Vulkan, libxkbcommon, linux-dmabuf and linux-drm-syncobj.
- For capture: `zwlr_screencopy_manager_v1`.
- For recording indicators and workspace targets: NeferWL's native capture-session protocol.
- FFmpeg with `libx264` for silent H.264 video in fragmented MP4.
- `wl-copy` (wl-clipboard) for clipboard screenshots.

A standard compositor can support monitor/region screenshots. Recording refuses to start without native session support rather than capture its own controls. Standard capture clients keep their normal behavior and see the HUD and border.

## Scripted commands

```sh
./bin/nefercap outputs
./bin/nefercap screenshot -output DP-1 -file capture.png
./bin/nefercap screenshot -output DP-1 -region 0,0,1920x1080 -file region.png
./bin/nefercap record -output DP-1 -fps 30 -duration 10s -file capture.mp4
./bin/nefercap rec -size 1920x1080 -file presentation.mp4
```

Use an output name from `outputs`. Scripted `screenshot` requires `-file` unless `-clipboard` is used; `record` always requires `-file`; omitting `-output` is allowed with exactly one output. Regions use output-local logical coordinates, not desktop-global coordinates. Capture dimensions are physical pixels negotiated with the compositor.

`-fps` accepts 1–120, default 30. `-size` requires two even dimensions and changes encoded resolution, not application scaling. Odd source dimensions require an explicit even size; pixels are not silently cropped. `-debug` enables diagnostic logging.

Untimed recording stops with the HUD, the control command, Ctrl+C or SIGTERM and finalizes the file. The `stop` command acknowledges the request; finalization finishes in the recorder process, not before the control reply. Timed recording encodes `ceil(duration × fps)` frames. Slow capture can repeat frames; encoder backpressure can extend wall time without adding an unbounded queue.

Screenshots and video are opaque SDR. Audio, pause and cursor guarantees are not supported. NeferWL ignores the capture protocol's cursor option.

## Architecture and footprint

`internal/core` owns capture and selection state and imports only stdlib and `internal/ports`. `internal/adapters` contains Wayland, selection, indicator, control, PNG and FFmpeg integrations. `internal/app` wires them together.

One capture is in flight. Writers synchronously consume borrowed, reusable shared-memory storage. Video strips stride padding and handles vertical inversion without a full-frame heap copy. The selector opens one transient layer surface per output; the HUD is one small surface, not a fullscreen GPU buffer. Idle UI does not redraw continuously.

The encoder uses `veryfast`, `zerolatency`, two encoding threads and one filter thread. Its separate-process footprint is not the Go heap; lower buffering can produce larger files than slower presets.

```sh
make check
make race
make mocks-check
staticcheck ./...
go test ./... -run '^$' -bench . -benchmem
```

Mockery v3 generates test doubles. Allocation guards cover frame views, selection state, unchanged overlay geometry, HUD refresh, video rows, encoder writes and output lookup. Protocol capture has a measured allocation budget; the application does not claim zero total allocations.

## License

GPL-3.0. See [LICENSE](LICENSE).
