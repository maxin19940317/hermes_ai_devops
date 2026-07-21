package adb

import (
	"reflect"
	"strings"
	"testing"
)

const serial = "R5CT10XXXXX"

func TestBuildersAlwaysPinSerial(t *testing.T) {
	cases := [][]string{
		GetProp(serial, "ro.product.cpu.abi"),
		DiskFreeKB(serial, "/data/local/tmp"),
		Push(serial, "/tmp/a", "/data/local/tmp/a"),
		Pull(serial, "/data/local/tmp/r", "/tmp/r"),
		ShellMkdirAll(serial, "/data/local/tmp/x"),
		ShellRemoveAll(serial, "/data/local/tmp/x"),
		ShellChmod(serial, "0755", "/data/local/tmp/x/run.sh"),
		ShellPkill(serial, "run.sh"),
		LogcatClear(serial),
		LogcatDump(serial),
		LogcatTail(serial, 100),
		ShellListGlob(serial, "/data/local/tmp/x", "results/*.json"),
		ShellRunEntry(serial, "/d", nil, "./run.sh", nil),
	}
	for i, argv := range cases {
		if len(argv) < 3 || argv[0] != "-s" || argv[1] != serial {
			t.Errorf("case %d: argv %v 未强制 -s <serial>", i, argv)
		}
	}
}

func TestBuilderArgv(t *testing.T) {
	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{"getprop", GetProp(serial, "ro.product.cpu.abi"),
			[]string{"-s", serial, "shell", "getprop", "ro.product.cpu.abi"}},
		{"df", DiskFreeKB(serial, "/data/local/tmp"),
			[]string{"-s", serial, "shell", "df", "-k", "/data/local/tmp"}},
		{"push", Push(serial, "/tmp/a", "/data/local/tmp/a"),
			[]string{"-s", serial, "push", "/tmp/a", "/data/local/tmp/a"}},
		{"pull", Pull(serial, "/data/local/tmp/r", "/tmp/r"),
			[]string{"-s", serial, "pull", "/data/local/tmp/r", "/tmp/r"}},
		{"mkdir", ShellMkdirAll(serial, "/data/local/tmp/x"),
			[]string{"-s", serial, "shell", "mkdir -p '/data/local/tmp/x'"}},
		{"rm", ShellRemoveAll(serial, "/data/local/tmp/x"),
			[]string{"-s", serial, "shell", "rm -rf '/data/local/tmp/x'"}},
		{"chmod", ShellChmod(serial, "0755", "/data/local/tmp/x/run.sh"),
			[]string{"-s", serial, "shell", "chmod 0755 '/data/local/tmp/x/run.sh'"}},
		{"pkill", ShellPkill(serial, "run.sh"),
			[]string{"-s", serial, "shell", "pkill -f 'run.sh'"}},
		{"logcat-c", LogcatClear(serial), []string{"-s", serial, "logcat", "-c"}},
		{"logcat-d", LogcatDump(serial), []string{"-s", serial, "logcat", "-d"}},
		{"logcat-tail", LogcatTail(serial, 200), []string{"-s", serial, "logcat", "-d", "-t", "200"}},
		{"devices", Devices(), []string{"devices", "-l"}},
		{"ls-glob", ShellListGlob(serial, "/data/local/tmp/x", "results/*.json"),
			[]string{"-s", serial, "shell", "cd '/data/local/tmp/x' && ls -1d results/*.json"}},
	}
	for _, tt := range tests {
		if !reflect.DeepEqual(tt.got, tt.want) {
			t.Errorf("%s:\n got %q\nwant %q", tt.name, tt.got, tt.want)
		}
	}
}

func TestShellRunEntryDeterministicEnvAndQuoting(t *testing.T) {
	env := map[string]string{
		"LD_LIBRARY_PATH":   "/d/lib",
		"ADSP_LIBRARY_PATH": "/d/lib/dsp;/system/lib/rfsa/adsp",
	}
	got := ShellRunEntry(serial, "/d", env, "./run.sh", []string{"--suite", "snpe-smoke"})
	want := []string{"-s", serial, "shell",
		"cd '/d' && ADSP_LIBRARY_PATH='/d/lib/dsp;/system/lib/rfsa/adsp' LD_LIBRARY_PATH='/d/lib' './run.sh' '--suite' 'snpe-smoke'"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

func TestQuoteNeutralizesSingleQuotes(t *testing.T) {
	got := Quote(`a'; rm -rf / #`)
	want := `'a'\''; rm -rf / #'`
	if got != want {
		t.Errorf("Quote = %s, want %s", got, want)
	}
}

// lines 按契约(client-agent-api openapi)钳制到 1..1000。
func TestLogcatTailClampsLines(t *testing.T) {
	tests := []struct {
		lines int
		want  string
	}{
		{0, "1"},
		{-5, "1"},
		{1, "1"},
		{42, "42"},
		{1000, "1000"},
		{5000, "1000"},
	}
	for _, tt := range tests {
		got := LogcatTail(serial, tt.lines)
		want := []string{"-s", serial, "logcat", "-d", "-t", tt.want}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("LogcatTail(%d):\n got %q\nwant %q", tt.lines, got, want)
		}
	}
}

func TestParseDevices(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"空输出", "", []string{}},
		{"仅表头", "List of devices attached\n", []string{}},
		{"单个在线设备", "List of devices attached\nR5CT10XXXXX\tdevice usb:1-1 product:trinket model:QCM6125 device:trinket\n",
			[]string{"R5CT10XXXXX"}},
		{"多设备混合状态", `List of devices attached
R5CT10XXXXX	device usb:1-1 product:trinket model:QCM6125 device:trinket
emulator-5554	device product:sdk model:emu device:generic
ABCDEF	unauthorized usb:1-2
GHIJKL	offline
?	device usb:1-3

`, []string{"R5CT10XXXXX", "emulator-5554"}},
		{"全部不可用", "List of devices attached\nABC\toffline\nDEF\tunauthorized\n", []string{}},
	}
	for _, tt := range tests {
		got := ParseDevices(tt.out)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: ParseDevices = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestExecRunnerUsesPrivatePort(t *testing.T) {
	for _, r := range []*ExecRunner{{ADBPath: "adb"}, {ADBPath: "adb", ServerPort: 5137}} {
		env := r.commandEnv()
		found := false
		for _, kv := range env {
			if kv == "ANDROID_ADB_SERVER_PORT=5137" {
				found = true
			}
			if strings.HasPrefix(kv, "ANDROID_ADB_SERVER_PORT=") && kv != "ANDROID_ADB_SERVER_PORT=5137" {
				t.Fatalf("非法端口: %s", kv)
			}
		}
		if !found {
			t.Errorf("commandEnv 缺少 ANDROID_ADB_SERVER_PORT=5137: %v", env)
		}
	}
}
