package placement

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
		got, err := parseMiB(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("%s: got %d want %d", in, got, want)
		}
	}
	if _, err := parseMiB("garbage"); err == nil {
		t.Error("want error on garbage")
	}
}
