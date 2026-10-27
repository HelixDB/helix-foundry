package connector

import "testing"

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 bytes", 1023: "1023 bytes", 1024: "1 KB", 1536: "1.5 KB", 2 << 30: "2 GB",
		3_328_599_654: "3.1 GB", 20 << 30: "20 GB", 6 << 40: "6 TB", 1 << 50: "1024 TB",
	} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
