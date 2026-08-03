package activity

import "testing"

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"1.0.0", "1.0.0", 0},
		{"v1.2.3", "1.2.3", 0}, // v-prefix stripped
		{"0.1.0", "0.2.0", -1},
		{"0.9.0", "1.0.0", -1},
		{"1.0.0", "2.0.0", -1},
		{"2.0.0", "1.0.0", 1},
		{"1.10.0", "1.2.0", 1},
		{"dev", "0.1.0", -1},
		{"dev", "1.0.0", -1},
		{"0.1.0", "dev", 1},
		{"0.0.1", "0.0.0", 1},
		{"1.2.3.4", "1.2.3", 1},
	}
	for _, tc := range cases {
		got := compareVersion(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("compareVersion(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
