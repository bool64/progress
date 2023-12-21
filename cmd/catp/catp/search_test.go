// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package catp

import (
	"testing"
)

func TestFinderNext(t *testing.T) {
	testCases := []struct {
		pat, text string
		index     int
	}{
		{"", "", 0},
		{"", "abc", 0},
		{"abc", "", -1},
		{"abc", "abc", 0},
		{"d", "abcdefg", 3},
		{"nan", "banana", 2},
		{"pan", "anpanman", 2},
		{"nnaaman", "anpanmanam", -1},
		{"abcd", "abc", -1},
		{"abcd", "bcd", -1},
		{"bcd", "abcd", 1},
		{"abc", "acca", -1},
		{"aa", "aaa", 0},
		{"baa", "aaaaa", -1},
		{"at that", "which finally halts.  at that point", 22},
	}

	for _, tc := range testCases {
		sf := makeStringFinder(tc.pat)
		got := sf.next([]byte(tc.text))
		want := tc.index
		if got != want {
			t.Errorf("stringFind(%q, %q) got %d, want %d\n", tc.pat, tc.text, got, want)
		}
	}
}
