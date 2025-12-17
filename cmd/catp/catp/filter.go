package catp

import (
	"bytes"
	"errors"
	"io"

	"github.com/cloudflare/ahocorasick"
)

type (
	filterAnd   [][]byte
	filterGroup struct {
		pass bool
		ors  []filterAnd

		// Prefilter checks for match of the first element of any ors item.
		// This first element is removed from and.
		pre *ahocorasick.Matcher

		save io.Writer
	}
	filters struct {
		g []*filterGroup
	}
)

func (f *filters) buildIndex() {
	for _, g := range f.g {
		g.buildIndex()
	}
}

func (f *filters) isSet() bool {
	return len(f.g) > 0
}

func (f *filters) addFilterString(pass bool, and ...string) {
	andb := make([][]byte, 0, len(and))

	for _, item := range and {
		andb = append(andb, []byte(item))
	}

	f.addFilter(pass, andb...)
}

func (f *filters) addPassAny() {
	f.g = append(f.g, &filterGroup{pass: true})
}

func (f *filters) saveTo(writer io.Writer) error {
	if len(f.g) == 0 {
		return errors.New("no filters set")
	}

	g := f.g[len(f.g)-1]
	if g.save != nil {
		return errors.New("save already set")
	}

	g.save = writer

	return nil
}

func (f *filters) addFilter(pass bool, and ...[]byte) {
	if len(and) == 0 {
		return
	}

	var g *filterGroup

	// Get current group if exists and has same pass, append new current group with new pass otherwise.
	if len(f.g) != 0 {
		g = f.g[len(f.g)-1]

		if g.pass != pass || g.save != nil {
			g = &filterGroup{pass: pass}
			f.g = append(f.g, g)
		}
	} else {
		// Create and append the very first group.
		g = &filterGroup{pass: pass}
		f.g = append(f.g, g)
	}

	g.ors = append(g.ors, and)
}

func (f *filters) shouldWrite(line []byte) (io.Writer, bool) {
	shouldWrite := true

	for _, g := range f.g {
		if g.pass {
			shouldWrite = false
		}

		matched := g.match(line)

		if matched {
			return g.save, g.pass
		}
	}

	return nil, shouldWrite
}

func (g *filterGroup) match(line []byte) bool {
	if g.pre != nil {
		ids := g.pre.Match(line)
		if len(ids) == 0 {
			return false
		}

		for _, id := range ids {
			or := g.ors[id]

			andMatched := true

			for _, and := range or {
				if !bytes.Contains(line, and) {
					andMatched = false

					break
				}
			}

			if andMatched {
				return true
			}
		}

		return false
	}

	for _, or := range g.ors {
		andMatched := true

		for _, and := range or {
			if !bytes.Contains(line, and) {
				andMatched = false

				break
			}
		}

		if andMatched {
			return true
		}
	}

	return false
}

func (g *filterGroup) buildIndex() {
	if g.pre != nil {
		return
	}

	if len(g.ors) < 5 {
		return
	}

	indexItems := make([][]byte, 0, len(g.ors))
	for i, or := range g.ors {
		indexItems = append(indexItems, or[0])
		g.ors[i] = or[1:]
	}

	g.pre = ahocorasick.NewMatcher(indexItems)
}
