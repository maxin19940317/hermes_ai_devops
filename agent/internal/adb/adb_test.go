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
