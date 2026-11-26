package emulator

import "testing"

func TestDescriptorNamed(t *testing.T) {
	for path, want := range map[string]int64{
		"/dev/stdin": 0, "/dev/stdout": 1, "/dev/stderr": 2, "/dev/fd/0": 0, "/dev/fd/63": 63, "/dev/fd//7": 7, "/dev/./fd/7": 7,
		"/dev/fd": -1, "/dev/fd/": -1, "/dev/fd/x": -1, "/dev/fd/-1": -1, "/dev/fd/3/x": -1, "/dev/fd/99999999999": -1, "/dev/null": -1, "dev/fd/3": -1,
	} {
		got := int64(-1)
		if n, ok := descriptorNamed(path); ok {
			got = int64(n)
		}
		if got != want {
			t.Errorf("%s: %d, want %d", path, got, want)
		}
	}
}
