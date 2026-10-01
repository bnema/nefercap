<h1 align="center">nefercap</h1>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-GPLv3-blue?style=flat-square" alt="License: GPLv3"></a>
  <a href="https://github.com/bnema/nefercap"><img src="https://img.shields.io/badge/platform-Linux-blue?style=flat-square" alt="Platform: Linux"></a>
  <a href="https://github.com/bnema/nefercap/commits/main"><img src="https://badgen.net/github/last-commit/bnema/nefercap/main?icon=github" alt="Last commit"></a>
  <a href="https://github.com/bnema/nefercap/stargazers"><img src="https://badgen.net/github/stars/bnema/nefercap?icon=github" alt="GitHub stars"></a>
</p>

<p align="center">Screenshots and silent screen recording for Wayland, from one shortcut.</p>

> [!WARNING]
> **Early alpha.** Built for [NeferWL](https://github.com/bnema/neferwl), and works on other compositors with the standard capture protocols (see [Compatibility](#compatibility)). Expect bugs and breaking config changes.

---

## Usage

Bind `nefercap` to a key in your compositor. For NeferWL:

```text
bind.cmd+p = spawn nefercap
```

Press the key: a selector covers every monitor. Pick what to capture, press **Tab** to switch between screenshot and recording, then confirm. A badge at the top left shows the mode: `SHOT` or `REC`.

While recording on NeferWL, a border marks the target and a small **REC / Stop** HUD shows the time. Neither appears in the video. Press the key again, or click Stop, to finish the file. Other compositors show no indicator: the selector says so, and the same key (or `nefercap stop`) finishes the file.

| Input | Action |
| --- | --- |
| Drag | Capture a rectangle |
| Click | Capture the monitor |
| `W`, then Enter | Capture the current workspace |
| `M`, then Enter | Capture the monitor |
| `1`–`9` | Pick that monitor |
| `R` | Back to rectangle selection |
| `G` | Show or hide the thirds guides |
| `Tab` | Switch shot ⇄ rec |
| `Esc` | Cancel |

On NeferWL a workspace recording follows that workspace, even when you switch to another one. Elsewhere `W` offers only the workspace currently shown, and records the monitor. A monitor recording follows whatever the monitor shows.

## Features

- **One shortcut.** Screenshot and recording share one selector; the same key stops a recording.
- **Clean video.** With NeferWL the compositor keeps the border and HUD out of the recording.
- **Clipboard.** Screenshots can be saved, copied, or both.
- **Small footprint.** One capture in flight, reused buffers, no redraw while idle. Allocation tests guard the hot paths.
- **Scriptable.** `nefercap screenshot` and `nefercap record` run without the selector.

## Configuration

`~/.config/nefercap/config` (or `$XDG_CONFIG_HOME/nefercap/config`):

```ini
selector.grid = on
screenshot.dir = ~/Pictures/Screenshots
screenshot.output = file+clipboard
video.dir = ~/Videos
```

| Key | Values | Default |
| --- | --- | --- |
| `selector.grid` | `on`, `off` | `on` |
| `screenshot.dir` | absolute or `~/` path | `$XDG_PICTURES_DIR/Screenshots` |
| `screenshot.output` | `file`, `file+clipboard`, `clipboard` | `file` |
| `video.dir` | absolute or `~/` path | `$XDG_VIDEOS_DIR` |

Unknown keys, duplicates and invalid values are reported before the selector opens. Files are private, named with a timestamp, never overwritten, and their path is printed on stdout.

## Commands

```sh
nefercap              # all-in-one: stop the recording, else open the selector
nefercap shot         # selector, screenshot only
nefercap rec          # selector, recording only
nefercap stop         # stop the active recording
nefercap status       # print the recording state
```

Scripted commands never open the selector:

```sh
nefercap outputs
nefercap screenshot -output DP-1 -file shot.png
nefercap screenshot -output DP-1 -region 0,0,1920x1080 -clipboard
nefercap record -output DP-1 -fps 30 -duration 10s -file clip.mp4
```

`nefercap -h` lists every flag. Regions use output-local logical coordinates. `-size WxH` sets an even output resolution for odd-sized sources.

## Requirements

- Go 1.27 to build.
- A Wayland compositor with `ext_image_copy_capture_manager_v1`, `ext_output_image_capture_source_manager_v1` and `zwlr_layer_shell_v1` v4.
- Vulkan, libxkbcommon, linux-dmabuf and linux-drm-syncobj for the selector.
- FFmpeg with `libx264` to record.
- `wl-copy` (wl-clipboard) for clipboard screenshots.

NeferWL's optional capture extension adds workspace and region sources and keeps the HUD out of the video. Without it nefercap uses only the standard protocols.

## Compatibility

| Compositor | Screenshots, recording (outputs, regions) | Workspaces | Hidden workspace | Recording indicator |
| --- | --- | --- | --- | --- |
| NeferWL | yes | yes | yes | yes, imposed by the compositor |
| sway, river (wlroots ≥ 0.19), Hyprland, niri, COSMIC | assumed | assumed, where `ext-workspace-v1` exists: active only | no | no |
| KDE Plasma | assumed | no | no | no |
| GNOME | unsupported | no | no | no |

Workspaces need `ext-workspace-v1`; with NeferWL's extension the selector outlines the workspace frame (a workspace smaller than its monitor), and the HUD sits in it (placed when the recording starts: if the workspace frame changes during the recording, the video follows it and the HUD stays in place). Without the extension `W` outlines the whole monitor and captures the workspace currently shown, as its monitor. Without the extension a region is cropped from the monitor frame. *Assumed* means it follows from the protocols the compositor advertises; only NeferWL is tested.

A compositor may refuse a capture client; the capture then ends with an error.

Output is SDR, and video is silent H.264 in fragmented MP4. Audio, pause and cursor capture are not supported. Rotated outputs (any non-normal output transform) are not supported: the capture fails with an error naming the transform.

## Install

```sh
make bin    # builds bin/nefercap
```

Install it as `/usr/bin/nefercap`: NeferWL allows that path to capture by default. Elsewhere (for example `~/.local/bin`, or `go run`), NeferWL refuses capture until root lists the path in `/etc/neferwl/capture-allow`.

## Development

`internal/core` owns capture and selection state and imports only the standard library and `internal/ports`. `internal/adapters` holds Wayland, selector, HUD, PNG and FFmpeg code; `internal/app` wires them.

```sh
make check
make race
make mocks-check
staticcheck ./...
```

Test doubles are generated by Mockery v3.

## License

GPL-3.0. See [LICENSE](LICENSE).
