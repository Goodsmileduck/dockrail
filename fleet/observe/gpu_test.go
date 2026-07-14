package observe

import (
	"reflect"
	"testing"
)

func TestParseGPUs_FiltersAndSorts(t *testing.T) {
	out := "1, 24576, 10000, 14576\n0, 24576, 0, 24576\n2, 24576, 24576, 0\n"
	got, err := parseGPUs(out, []int{0, 1}) // 2 not in inventory → dropped
	if err != nil {
		t.Fatalf("parseGPUs: %v", err)
	}
	want := []GPUState{
		{Index: 0, TotalMiB: 24576, UsedMiB: 0, FreeMiB: 24576},
		{Index: 1, TotalMiB: 24576, UsedMiB: 10000, FreeMiB: 14576},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestParseGPUs_BadLine(t *testing.T) {
	if _, err := parseGPUs("0, 24576, oops\n", []int{0}); err == nil {
		t.Fatal("expected error on malformed line")
	}
}

func TestParseGPUs_Empty(t *testing.T) {
	got, err := parseGPUs("\n", []int{0})
	if err != nil {
		t.Fatalf("parseGPUs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %+v", got)
	}
}
