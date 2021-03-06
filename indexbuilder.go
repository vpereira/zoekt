// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package zoekt

import (
	"encoding/binary"
	"fmt"
	"html/template"
	"log"
	"path/filepath"
	"sort"
	"unicode/utf8"
)

var _ = log.Println

const ngramSize = 3

type searchableString struct {
	// lower cased data.
	data []byte
}

// Store character (unicode codepoint) offset (in bytes) this often.
const runeOffsetFrequency = 100

type postingsBuilder struct {
	postings    map[ngram][]byte
	lastOffsets map[ngram]uint32

	// To support UTF-8 searching, we must map back runes to byte
	// offsets. As a first attempt, we sample regularly. The
	// precise offset can be found by walking from the recorded
	// offset to the desired rune.  TODO(hanwen): disable the
	// mapping if the index shard is 100% lo-bit ascii.
	runeOffsets []uint32
	runeCount   uint32

	endRunes []uint32
	endByte  uint32
}

func newPostingsBuilder() *postingsBuilder {
	return &postingsBuilder{
		postings:    map[ngram][]byte{},
		lastOffsets: map[ngram]uint32{},
	}
}

func (s *postingsBuilder) newSearchableString(data []byte) *searchableString {
	dest := searchableString{
		data: data,
	}
	var buf [8]byte
	var runeGram [3]rune

	runeIndex := -1

	dataSz := uint32(len(data))
	i := 0

	endRune := s.runeCount
	for len(data) > 0 {
		c, sz := utf8.DecodeRune(data)
		data = data[sz:]

		runeGram[0], runeGram[1], runeGram[2] = runeGram[1], runeGram[2], c
		runeIndex++

		s.runeCount++
		if idx := s.runeCount - 1; idx%runeOffsetFrequency == 0 {
			s.runeOffsets = append(s.runeOffsets, s.endByte+uint32(i))
		}
		i += sz

		if runeIndex < 2 {
			continue
		}

		ng := runesToNGram(runeGram)
		lastOff := s.lastOffsets[ng]
		newOff := endRune + uint32(runeIndex) - 2

		m := binary.PutUvarint(buf[:], uint64(newOff-lastOff))
		s.postings[ng] = append(s.postings[ng], buf[:m]...)
		s.lastOffsets[ng] = newOff
	}

	s.endRunes = append(s.endRunes, s.runeCount)
	s.endByte += dataSz
	return &dest
}

// IndexBuilder builds a single index shard.
type IndexBuilder struct {
	contentEnd uint32
	nameEnd    uint32

	files       []*searchableString
	fileNames   []*searchableString
	docSections [][]DocumentSection

	branchMasks []uint32
	subRepos    []uint32

	contents *postingsBuilder
	names    *postingsBuilder

	// root repository
	repo Repository

	// name to index.
	subRepoIndices map[string]uint32
}

func (d *Repository) verify() error {
	for _, t := range []string{d.FileURLTemplate, d.LineFragmentTemplate, d.CommitURLTemplate} {
		if _, err := template.New("").Parse(t); err != nil {
			return err
		}
	}
	return nil
}

// ContentSize returns the number of content bytes so far ingested.
func (b *IndexBuilder) ContentSize() uint32 {
	// Add the name too so we don't skip building index if we have
	// lots of empty files.
	return b.contentEnd + b.nameEnd
}

// NewIndexBuilder creates a fresh IndexBuilder. The passed in
// Repository contains repo metadata, and may be set to nil.
func NewIndexBuilder(r *Repository) (*IndexBuilder, error) {
	b := &IndexBuilder{
		contents: newPostingsBuilder(),
		names:    newPostingsBuilder(),
	}

	if r == nil {
		r = &Repository{}
	}
	if err := b.setRepository(r); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *IndexBuilder) setRepository(desc *Repository) error {
	if len(b.files) > 0 {
		return fmt.Errorf("AddSubRepository called after adding files.")
	}
	if err := desc.verify(); err != nil {
		return err
	}

	for _, subrepo := range desc.SubRepoMap {
		branchEqual := len(subrepo.Branches) == len(desc.Branches)
		if branchEqual {
			for i, b := range subrepo.Branches {
				branchEqual = branchEqual && (b.Name == desc.Branches[i].Name)
			}
		}
	}

	if len(desc.Branches) > 32 {
		return fmt.Errorf("too many branches.")
	}

	b.repo = *desc
	repoCopy := *desc
	repoCopy.SubRepoMap = nil

	if b.repo.SubRepoMap == nil {
		b.repo.SubRepoMap = map[string]*Repository{}
	}
	b.repo.SubRepoMap[""] = &repoCopy

	b.populateSubRepoIndices()
	return nil
}

type DocumentSection struct {
	Start, End uint32
}

// Document holds a document (file) to index.
type Document struct {
	Name              string
	Content           []byte
	Branches          []string
	SubRepositoryPath string

	Symbols []DocumentSection
}

type docSectionSlice []DocumentSection

func (m docSectionSlice) Len() int           { return len(m) }
func (m docSectionSlice) Swap(i, j int)      { m[i], m[j] = m[j], m[i] }
func (m docSectionSlice) Less(i, j int) bool { return m[i].Start < m[j].Start }

// AddFile is a convenience wrapper for Add
func (b *IndexBuilder) AddFile(name string, content []byte) error {
	return b.Add(Document{Name: name, Content: content})
}

const maxTrigramCount = 20000
const maxLineSize = 1000

// IsText returns false if the given contents are probably not source texts.
func IsText(content []byte) bool {
	if len(content) < ngramSize {
		return true
	}

	trigrams := map[ngram]struct{}{}

	lineSize := 0

	var cur [3]rune
	for len(content) > 0 {
		if content[0] == 0 {
			return false
		}
		if content[0] == '\n' {
			lineSize = 0
		} else {
			lineSize++
		}
		if lineSize > maxLineSize {
			return false
		}

		r, sz := utf8.DecodeRune(content)
		if r == utf8.RuneError {
			return false
		}
		content = content[sz:]

		cur[0], cur[1], cur[2] = cur[1], cur[2], r
		if cur[0] == 0 {
			// start of file.
			continue
		}

		trigrams[runesToNGram(cur)] = struct{}{}
		if len(trigrams) > maxTrigramCount {
			// probably not text.
			return false
		}
	}
	return true
}

func (b *IndexBuilder) populateSubRepoIndices() {
	if b.subRepoIndices != nil {
		return
	}
	var paths []string
	for k := range b.repo.SubRepoMap {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	b.subRepoIndices = make(map[string]uint32, len(paths))
	for i, p := range paths {
		b.subRepoIndices[p] = uint32(i)
	}
}

// Add a file which only occurs in certain branches. The document
// should be checked for sanity with IsText first.
func (b *IndexBuilder) Add(doc Document) error {
	sort.Sort(docSectionSlice(doc.Symbols))
	var last DocumentSection
	for i, s := range doc.Symbols {
		if i > 0 {
			if last.End > s.Start {
				return fmt.Errorf("sections overlap")
			}
		}
		last = s
	}

	if doc.SubRepositoryPath != "" {
		rel, err := filepath.Rel(doc.SubRepositoryPath, doc.Name)
		if err != nil || rel == doc.Name {
			return fmt.Errorf("path %q must start subrepo path %q", doc.Name, doc.SubRepositoryPath)
		}
	}

	subRepoIdx, ok := b.subRepoIndices[doc.SubRepositoryPath]
	if !ok {
		return fmt.Errorf("unknown subrepo path %q", doc.SubRepositoryPath)
	}

	var mask uint32
	for _, br := range doc.Branches {
		m := b.branchMask(br)
		if m == 0 {
			return fmt.Errorf("no branch found for %s", br)
		}
		mask |= m
	}

	b.subRepos = append(b.subRepos, subRepoIdx)

	docStr := b.contents.newSearchableString(doc.Content)
	b.files = append(b.files, docStr)

	nameStr := b.names.newSearchableString([]byte(doc.Name))
	b.fileNames = append(b.fileNames, nameStr)
	b.docSections = append(b.docSections, doc.Symbols)

	b.branchMasks = append(b.branchMasks, mask)
	return nil
}

func (b *IndexBuilder) branchMask(br string) uint32 {
	for i, b := range b.repo.Branches {
		if b.Name == br {
			return uint32(1) << uint(i)
		}
	}
	return 0
}
