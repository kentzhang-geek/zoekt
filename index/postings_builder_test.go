package index

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

// Compare compressed postings with an independent, uncompressed rune-window
// oracle, including one-byte documents, invalid UTF-8 and multi-byte deltas.
func TestPostingsBuilderRuneOracle(t *testing.T) {
	inputs := [][]byte{[]byte("abcabc"), nil, []byte("x"), []byte("你好😀你好😀"),
		{0xff, 0xfe, 'a', 0xff, 'b'}, bytes.Repeat([]byte("abcdefg"), 500), []byte("abc")}
	want := map[ngram][]uint32{}
	var base uint32
	b := newPostingsBuilder()
	for _, input := range inputs {
		runes := []rune(string(input))
		for i := 0; i+2 < len(runes); i++ {
			key := runesToNGram([3]rune{runes[i], runes[i+1], runes[i+2]})
			want[key] = append(want[key], base+uint32(i))
		}
		base += uint32(len(runes))
		if _, _, err := b.newSearchableString(input, nil); err != nil {
			t.Fatal(err)
		}
	}
	got := map[ngram][]uint32{}
	for key, id := range b.postingIDs {
		var offset uint32
		data := b.lists[id-1].data
		for len(data) > 0 {
			delta, n := binary.Uvarint(data)
			if n <= 0 {
				t.Fatal("invalid posting varint")
			}
			offset += uint32(delta)
			got[key] = append(got[key], offset)
			data = data[n:]
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("compressed postings differ from rune oracle")
	}
}
