package catp //nolint:testpackage

import (
	"bytes"
	"os"
	"testing"
)

func TestFilter_Match(t *testing.T) {
	f := filters{}

	f.addFilterString(false, "dbg")
	f.addFilterString(true, "linux", "64")
	f.addFilterString(true, "windows")

	input, err := os.ReadFile("./testdata/release-assets.yml")
	if err != nil {
		t.Fatal(err)
	}

	for line := range bytes.SplitSeq(input, []byte("\n")) {
		if _, ok := f.shouldWrite(line); ok {
			println(string(line))
		}
	}
}
