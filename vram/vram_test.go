package vram

import "testing"

func TestParseMiB(t *testing.T) {
	cases := map[string]int{
		"18GiB":    18432,
		"18Gi":     18432,
		"18000MiB": 18000,
		"512Mi":    512,
		"18000":    18000, // bare = MiB
	}
	for in, want := range cases {
		got, err := ParseMiB(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("%s: got %d want %d", in, got, want)
		}
	}
	if _, err := ParseMiB("garbage"); err == nil {
		t.Error("want error on garbage")
	}
}

func TestNeededMiB(t *testing.T) {
	// 10GiB = 10240 MiB; *1.2 = 12288 exactly.
	if got, err := NeededMiB("10GiB"); err != nil || got != 12288 {
		t.Fatalf("10GiB: got %d err %v, want 12288", got, err)
	}
	// 1GiB = 1024; *1.2 = 1228.8 -> rounds to 1229 (not truncated to 1228).
	if got, err := NeededMiB("1GiB"); err != nil || got != 1229 {
		t.Fatalf("1GiB: got %d err %v, want 1229 (rounded)", got, err)
	}
	if _, err := NeededMiB("garbage"); err == nil {
		t.Error("want error on garbage")
	}
}
