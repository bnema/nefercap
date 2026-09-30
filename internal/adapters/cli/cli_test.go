package cli_test

import (
	"bytes"
	"errors"
	"flag"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bnema/nefercap/internal/adapters/cli"
	"github.com/bnema/nefercap/internal/ports"
)

func TestParseValid(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want cli.Options
	}{
		{"default shot", nil, cli.Options{Command: cli.Shot}},
		{"default shot debug", []string{"-debug"}, cli.Options{Command: cli.Shot, Debug: true}},
		{"default shot flags", []string{"-file", "a.png", "-output", "DP-1"},
			cli.Options{Command: cli.Shot, Path: "a.png", Output: "DP-1"}},
		{"shot", []string{"shot"}, cli.Options{Command: cli.Shot}},
		{"shot all", []string{"shot", "-file", "a.png", "-output", "DP-1", "-debug"},
			cli.Options{Command: cli.Shot, Path: "a.png", Output: "DP-1", Debug: true}},
		{"rec defaults", []string{"rec"}, cli.Options{Command: cli.Rec, Video: ports.VideoSettings{FPS: 30}}},
		{"rec all", []string{"rec", "-file", "a.mkv", "-output", "eDP-1", "-fps", "60", "-size", "1280x720", "-duration", "10s", "-debug"},
			cli.Options{Command: cli.Rec, Path: "a.mkv", Output: "eDP-1", Duration: 10 * time.Second, Debug: true,
				Video: ports.VideoSettings{FPS: 60, Width: 1280, Height: 720}}},
		{"stop", []string{"stop"}, cli.Options{Command: cli.Stop}},
		{"stop debug", []string{"stop", "-debug"}, cli.Options{Command: cli.Stop, Debug: true}},
		{"status", []string{"status"}, cli.Options{Command: cli.Status}},
		{"status debug", []string{"status", "-debug"}, cli.Options{Command: cli.Status, Debug: true}},
		{"outputs", []string{"outputs"}, cli.Options{Command: cli.Outputs}},
		{"outputs debug", []string{"outputs", "-debug"}, cli.Options{Command: cli.Outputs, Debug: true}},
		{"screenshot", []string{"screenshot", "-file", "a.png"},
			cli.Options{Command: cli.Screenshot, Path: "a.png"}},
		{"screenshot all", []string{"screenshot", "-output", "DP-1", "-region", "10,20,300x400", "-file", "a.png", "-debug"},
			cli.Options{Command: cli.Screenshot, Output: "DP-1", Path: "a.png", Debug: true,
				Region: ports.Region{X: 10, Y: 20, Width: 300, Height: 400}}},
		{"record defaults", []string{"record", "-file", "a.mkv"},
			cli.Options{Command: cli.Record, Path: "a.mkv", Video: ports.VideoSettings{FPS: 30}}},
		{"record all", []string{"record", "-file", "a.mkv", "-fps", "60", "-size", "1280x720", "-duration", "1m30s", "-output", "eDP-1", "-region", "0,0,640x480"},
			cli.Options{Command: cli.Record, Path: "a.mkv", Output: "eDP-1", Duration: 90 * time.Second,
				Region: ports.Region{Width: 640, Height: 480},
				Video:  ports.VideoSettings{FPS: 60, Width: 1280, Height: 720}}},
		{"record zero duration", []string{"record", "-file", "a.mkv", "-duration", "0"},
			cli.Options{Command: cli.Record, Path: "a.mkv", Video: ports.VideoSettings{FPS: 30}}},
		{"record fps bounds", []string{"record", "-file", "a.mkv", "-fps", "120"},
			cli.Options{Command: cli.Record, Path: "a.mkv", Video: ports.VideoSettings{FPS: 120}}},
		{"double dash flag", []string{"record", "--file", "a.mkv", "--fps=1"},
			cli.Options{Command: cli.Record, Path: "a.mkv", Video: ports.VideoSettings{FPS: 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := cli.Parse(tc.args, &out)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Empty(t, out.String())
		})
	}
}

func TestParseUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"unknown command":          {"snap"},
		"positional after command": {"outputs", "extra"},
		"positional default":       {"extra"},
		"positional shot":          {"shot", "extra"},
		"positional stop":          {"stop", "extra"},
		"gui removed":              {"gui"},
		"gui removed with flag":    {"gui", "-debug"},
		"empty shot file":          {"shot", "-file", ""},
		"empty rec file":           {"rec", "-file", ""},
		"empty default file":       {"-file", ""},
		"region on shot":           {"shot", "-region", "0,0,10x10"},
		"region on rec":            {"rec", "-region", "0,0,10x10"},
		"region on default":        {"-region", "0,0,10x10"},
		"fps on shot":              {"shot", "-fps", "30"},
		"size on shot":             {"shot", "-size", "2x2"},
		"duration on shot":         {"shot", "-duration", "1s"},
		"fps zero on rec":          {"rec", "-fps", "0"},
		"fps high on rec":          {"rec", "-fps", "121"},
		"size odd on rec":          {"rec", "-size", "641x480"},
		"size huge on rec":         {"rec", "-size", "16384x16384"},
		"duration negative on rec": {"rec", "-duration", "-1s"},
		"file on stop":             {"stop", "-file", "a"},
		"output on stop":           {"stop", "-output", "DP-1"},
		"file on status":           {"status", "-file", "a"},
		"output on status":         {"status", "-output", "DP-1"},
		"fps on status":            {"status", "-fps", "30"},
		"file on outputs":          {"outputs", "-file", "a"},
		"positional after flags":   {"record", "-file", "a.mkv", "extra"},
		"unknown flag":             {"-nope"},
		"unknown flag in command":  {"outputs", "-nope"},
		"screenshot missing file":  {"screenshot"},
		"record missing file":      {"record", "-fps", "30"},
		"empty file":               {"screenshot", "-file", ""},
		"capture flag on outputs":  {"outputs", "-output", "DP-1"},
		"fps on screenshot":        {"screenshot", "-file", "a.png", "-fps", "30"},
		"duration on screenshot":   {"screenshot", "-file", "a.png", "-duration", "1s"},
		"size on screenshot":       {"screenshot", "-file", "a.png", "-size", "2x2"},
		"fps zero":                 {"record", "-file", "a", "-fps", "0"},
		"fps high":                 {"record", "-file", "a", "-fps", "121"},
		"fps text":                 {"record", "-file", "a", "-fps", "fast"},
		"size odd width":           {"record", "-file", "a", "-size", "641x480"},
		"size odd height":          {"record", "-file", "a", "-size", "640x481"},
		"size zero":                {"record", "-file", "a", "-size", "0x0"},
		"size too large":           {"record", "-file", "a", "-size", "16386x2"},
		"size frame too large":     {"record", "-file", "a", "-size", "16384x16384"},
		"size missing height":      {"record", "-file", "a", "-size", "640"},
		"size negative":            {"record", "-file", "a", "-size", "-2x-2"},
		"size junk":                {"record", "-file", "a", "-size", "axb"},
		"size signed":              {"record", "-file", "a", "-size", "+2x2"},
		"duration negative":        {"record", "-file", "a", "-duration", "-1s"},
		"duration bare number":     {"record", "-file", "a", "-duration", "5"},
		"region empty parts":       {"screenshot", "-file", "a", "-region", ",,x"},
		"region missing size":      {"screenshot", "-file", "a", "-region", "1,2"},
		"region missing y":         {"screenshot", "-file", "a", "-region", "1,10x10"},
		"region extra part":        {"screenshot", "-file", "a", "-region", "1,2,3,10x10"},
		"region negative x":        {"screenshot", "-file", "a", "-region", "-1,0,10x10"},
		"region negative y":        {"screenshot", "-file", "a", "-region", "0,-1,10x10"},
		"region zero width":        {"screenshot", "-file", "a", "-region", "0,0,0x10"},
		"region zero height":       {"screenshot", "-file", "a", "-region", "0,0,10x0"},
		"region overflow":          {"screenshot", "-file", "a", "-region", "0,0,99999999999x1"},
		"region sum overflow":      {"screenshot", "-file", "a", "-region", "2147483647,0,1x1"},
		"region signed":            {"screenshot", "-file", "a", "-region", "+1,0,10x10"},
		"region uppercase X":       {"screenshot", "-file", "a", "-region", "0,0,10X10"},
		"region spaces":            {"screenshot", "-file", "a", "-region", "0, 0, 10x10"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := cli.Parse(args, &out)
			require.Error(t, err)
			assert.ErrorIs(t, err, cli.ErrUsage)
			assert.NotErrorIs(t, err, flag.ErrHelp)
			assert.Equal(t, cli.Options{}, got)
			assert.Empty(t, out.String(), "errors are reported by the caller")
		})
	}
}

func TestParseErrorNamesProblem(t *testing.T) {
	_, err := cli.Parse([]string{"record"}, nil)
	assert.ErrorContains(t, err, "-file is required")
	_, err = cli.Parse([]string{"snap"}, nil)
	assert.ErrorContains(t, err, `unknown command "snap"`)
	_, err = cli.Parse([]string{"screenshot", "-file", "a", "-region", "1,2"}, nil)
	assert.ErrorContains(t, err, "X,Y,WxH")
}

func TestParseHelp(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"-help"}, {"record", "--help"}, {"outputs", "-h"}, {"shot", "-h"}, {"rec", "-h"}, {"stop", "-h"}, {"status", "--help"}} {
		var out bytes.Buffer
		got, err := cli.Parse(args, &out)
		assert.True(t, errors.Is(err, flag.ErrHelp), args)
		assert.Equal(t, cli.Options{}, got)
		assert.Equal(t, cli.Usage(), out.String())
	}
}

func TestUsageMentionsEverything(t *testing.T) {
	u := cli.Usage()
	for _, s := range []string{"shot", "rec", "stop", "status", "outputs", "screenshot", "record",
		"layer-shell", "drag", "click", "Escape", "Enter", "workspace", "1..9", "  r ", "  m ", "  w ", "  g ", "-output", "-region", "-file", "-fps", "-size", "-duration", "-debug"} {
		assert.Contains(t, u, s)
	}
	assert.NotContains(t, u, "  gui ")
}
