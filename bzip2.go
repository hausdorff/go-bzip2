// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package bzip2 implements bzip2 decompression.
package main

import "io"
import "fmt"

// There's no RFC for bzip2. I used the Wikipedia page for reference and a lot
// of guessing: http://en.wikipedia.org/wiki/Bzip2
// The source code to pyflate was useful for debugging:
// http://www.paul.sladen.org/projects/pyflate

// A StructuralError is returned when the bzip2 data is found to be
// syntactically invalid.
type StructuralError string

func (s StructuralError) Error() string {
	return "bzip2 data invalid: " + string(s)
}

// A reader decompresses bzip2 compressed data.
type reader struct {
	br           bitReader
	fileCRC      uint32
	blockCRC     uint32
	wantBlockCRC uint32
	setupDone    bool // true if we have parsed the bzip2 header.
	blockSize    int  // blockSize in bytes, i.e. 900 * 1024.
	eof          bool
	buf          []byte    // stores Burrows-Wheeler transformed data.
	c            [256]uint // the `C' array for the inverse BWT.
	tt           []uint32  // mirrors the `tt' array in the bzip2 source and contains the P array in the upper 24 bits.
	tPos         uint32    // Index of the next output byte in tt.

	preRLE      []uint32 // contains the RLE data still to be processed.
	preRLEUsed  int      // number of entries of preRLE used.
	lastByte    int      // the last byte value seen.
	byteRepeats uint     // the number of repeats of lastByte seen.
	repeats     uint     // the number of copies of lastByte to output.
}

type writer struct {
	bw        io.Writer
	fileCRC   uint32
	blockCRC  uint32
	blockSize int
}

func NewWriter(w io.Writer) io.Writer {
	bz2 := new(writer)
	bz2.bw = w
	return bz2
}

func (bz2 *writer) Write(buf []byte) (int, error) {
	return bz2.bw.Write(buf)
}

// NewReader returns an io.Reader which decompresses bzip2 data from r.
// If r does not also implement io.ByteReader,
// the decompressor may read more data than necessary from r.
func NewReader(r io.Reader) io.Reader {
	bz2 := new(reader)
	bz2.br = newBitReader(r)
	return bz2
}

const bzip2FileMagic = 0x425a // "BZ"
const bzip2BlockMagic = 0x314159265359
const bzip2FinalMagic = 0x177245385090

// Parses the header of the bz2 file and returns an error if it finds one.
// Also initializes the file CRC, the block size, and the `tt` array.
func (bz2 *reader) setup(needMagic bool) error {
	br := &bz2.br

	// if we haven't consumed the magic number at the start of the block yet
	if needMagic {
		magic := br.ReadBits(16)
		if magic != bzip2FileMagic {
			return StructuralError("bad magic value")
		}
		fmt.Printf("header\t\t0x%x\t\t0b%b\t%s\n", magic, magic, "BZ")
	}

	// look for the 'h' in the header
	t := br.ReadBits(8)
	if t != 'h' {
		return StructuralError("non-Huffman entropy encoding")
	}
	fmt.Printf("comp type\t0x%x\t\t0b%b\t\t%s\n", t, t, string(t))

	// look for the block size in block header
	level := br.ReadBits(8)
	if level < '1' || level > '9' {
		return StructuralError("invalid compression level")
	}
	fmt.Printf("level\t\t0x%x\t\t0b%b\t\t%s\n", level, level, string(level))

	// Initialize the file-wide CRC, block size, and the `tt` array
	// (whatever that is)
	bz2.fileCRC = 0
	bz2.blockSize = 100 * 1024 * (int(level) - '0')
	// This conditional is useful for when we call setup more than once and
	// the tt array is not big enough the second time.
	if bz2.blockSize > len(bz2.tt) {
		bz2.tt = make([]uint32, bz2.blockSize)
	}
	fmt.Printf("blockSz\t\t0x%x\t\t0b%b\n", bz2.blockSize, bz2.blockSize)
	return nil
}

func (bz2 *reader) Read(buf []byte) (n int, err error) {
	if bz2.eof {
		return 0, io.EOF
	}

	// ??? under what circumstances are we calling this and bz2.setupDone is
	// not complete???
	if !bz2.setupDone {
		// Here we parse the header and get an error `err` if something
		// failed. Then we check the bitReader ito see if something
		// failed and overwrite the local error if there is some
		// failure. If either of those things is true, then we return
		// the error. Else we mark setup as done and proceed.
		err = bz2.setup(true)
		brErr := bz2.br.Err()
		if brErr != nil {
			err = brErr
		}
		if err != nil {
			return 0, err
		}
		bz2.setupDone = true
	}

	// Attempt to read decoded data into buf. If there's an error, report
	// it; if the bitReader has an error, replace our error with that one.
	// Return the n and the err using the default return path.
	n, err = bz2.read(buf)
	brErr := bz2.br.Err()
	if brErr != nil {
		err = brErr
	}
	return
}

func (bz2 *reader) readFromBlock(buf []byte) int {
	// bzip2 is a block based compressor, except that it has a run-length
	// preprocessing step. The block based nature means that we can
	// preallocate fixed-size buffers and reuse them. However, the RLE
	// preprocessing would require allocating huge buffers to store the
	// maximum expansion. Thus we process blocks all at once, except for
	// the RLE which we decompress as required.
	n := 0
	for (bz2.repeats > 0 || bz2.preRLEUsed < len(bz2.preRLE)) && n < len(buf) {
		// We have RLE data pending.

		// The run-length encoding works like this:
		// Any sequence of four equal bytes is followed by a length
		// byte which contains the number of repeats of that byte to
		// include. (The number of repeats can be zero.) Because we are
		// decompressing on-demand our state is kept in the reader
		// object.

		if bz2.repeats > 0 {
			buf[n] = byte(bz2.lastByte)
			n++
			bz2.repeats--
			if bz2.repeats == 0 {
				bz2.lastByte = -1
			}
			continue
		}

		bz2.tPos = bz2.preRLE[bz2.tPos]
		b := byte(bz2.tPos)
		bz2.tPos >>= 8
		bz2.preRLEUsed++

		if bz2.byteRepeats == 3 {
			bz2.repeats = uint(b)
			bz2.byteRepeats = 0
			continue
		}

		if bz2.lastByte == int(b) {
			bz2.byteRepeats++
		} else {
			bz2.byteRepeats = 0
		}
		bz2.lastByte = int(b)

		buf[n] = b
		n++
	}

	return n
}

// Decompress bytes from bz2 to the byte array, returning the number of
// bytes read and an error if applicable.
func (bz2 *reader) read(buf []byte) (int, error) {
	for {
		// Un-RLE the data, put the unencoded data in buf, and update
		// the checksum.
		n := bz2.readFromBlock(buf)
		fmt.Println("\tn", n)
		if n > 0 {
			bz2.blockCRC = updateCRC(bz2.blockCRC, buf[:n])
			return n, nil
		}

		// Error if _last_ block's CRC doesn't match desired CRC.
		if bz2.blockCRC != bz2.wantBlockCRC {
			bz2.br.err = StructuralError("block checksum mismatch")
			return 0, bz2.br.err
		}

		// Attempt to get the block header. Is either the start-of-block
		// marker, or the final block marker.
		br := &bz2.br
		switch br.ReadBits64(48) {
		default:
			return 0, StructuralError("bad magic value found")

		// NEW BWT BLOCK
		case bzip2BlockMagic:
			fmt.Printf("blockStartMagic\t0x%x\t0b%b\n", bzip2BlockMagic, bzip2BlockMagic)
			err := bz2.readBlock()
			if err != nil {
				return 0, err
			}

		// FINAL BLOCK
		case bzip2FinalMagic:
			wantFileCRC := uint32(br.ReadBits64(32))
			fmt.Printf("blockFinalMagic\t0x%x\t0b%b\n", bzip2FinalMagic, bzip2BlockMagic)
			fmt.Printf("wantFileCRC\t0x%x\t0b%b\n", wantFileCRC, wantFileCRC)
			if br.err != nil {
				return 0, br.err
			}
			if bz2.fileCRC != wantFileCRC {
				br.err = StructuralError("file checksum mismatch")
				return 0, br.err
			}

			// Skip ahead to byte boundary.
			// Is there a file concatenated to this one?
			// It would start with BZ.
			if br.bits%8 != 0 {
				br.ReadBits(br.bits % 8)
			}
			b, err := br.r.ReadByte()
			if err == io.EOF {
				br.err = io.EOF
				bz2.eof = true
				return 0, io.EOF
			}
			if err != nil {
				br.err = err
				return 0, err
			}
			z, err := br.r.ReadByte()
			if err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				br.err = err
				return 0, err
			}
			if b != 'B' || z != 'Z' {
				return 0, StructuralError("bad magic value in continuation file")
			}
			if err := bz2.setup(false); err != nil {
				return 0, err
			}
		}
	}
}

// readBlock reads a bzip2 block. The magic number should already have been consumed.
func (bz2 *reader) readBlock() (err error) {
	// Read the CRC data, init the block CRC data, update the file's CRC
	br := &bz2.br
	bz2.wantBlockCRC = uint32(br.ReadBits64(32)) // skip checksum. TODO: check it if we can figure out what it is.
	fmt.Printf("wantBlockCRC\t0x%x\t0b%b\n", bz2.wantBlockCRC, bz2.wantBlockCRC)
	bz2.blockCRC = 0
	bz2.fileCRC = (bz2.fileCRC<<1 | bz2.fileCRC>>31) ^ bz2.wantBlockCRC

	// Error if we use the (deprecated) "randomized" feature
	randomized := br.ReadBits(1)
	if randomized != 0 {
		return StructuralError("deprecated randomized files")
	}

	// ??? the original pointer into the BTW for after the untransform ???
	origPtr := uint(br.ReadBits(24))
	fmt.Printf("origPtr\t\t0x%x\t\t0b%b\n", origPtr, origPtr)

	// If not every byte value is used in the block (i.e., it's text) then
	// the symbol set is reduced. The symbols used are stored as a
	// two-level, 16x16 bitmap.
	symbolRangeUsedBitmap := br.ReadBits(16)
	fmt.Printf("symRangeBitmap\t0x%x\t\t0b%b\n", symbolRangeUsedBitmap, symbolRangeUsedBitmap)
	symbolPresent := make([]bool, 256)
	numSymbols := 0
	// Loops from [0,16). There are 16 "symbol ranges" available for
	// use, and this loops through all of them
	for symRange := uint(0); symRange < 16; symRange++ {
		// Check in the bitmap whether this particular "symbol
		// range" is used. If it is, read 16 more bits from the stream.
		if symbolRangeUsedBitmap&(1<<(15-symRange)) != 0 {
			bits := br.ReadBits(16)
			fmt.Printf("bits\t\t0x%x\t\t0b%b\n", bits, bits)
			// Loop through each of the 16 symbols. If they are
			// present, then mark them in the symbols array and
			// increment the count.
			for symbol := uint(0); symbol < 16; symbol++ {
				if bits&(1<<(15-symbol)) != 0 {
					symbolPresent[16*symRange+symbol] = true
					numSymbols++
				}
			}
		}
	}

	if numSymbols == 0 {
		// There must be an EOF symbol.
		return StructuralError("no symbols in input")
	}

	// The number of huffman tables to use. This in range [2,6] or else error.
	// A block uses between two and six different Huffman trees.
	numHuffmanTrees := br.ReadBits(3)
	fmt.Printf("numHuffmanTrees\t0x%x\t\t0b%b\n", numHuffmanTrees, numHuffmanTrees)
	if numHuffmanTrees < 2 || numHuffmanTrees > 6 {
		return StructuralError("invalid number of Huffman trees")
	}

	// The Huffman tree can switch every 50 symbols so there's a list of
	// tree indexes telling us which tree to use for each 50 symbol block.
	numSelectors := br.ReadBits(15)
	fmt.Printf("numSelectors\t0x%x\t\t0b%b\n", numSelectors, numSelectors)
	treeIndexes := make([]uint8, numSelectors)

	// The tree indexes are move-to-front transformed and stored as unary
	// numbers.
	mtfTreeDecoder := newMTFDecoderWithRange(numHuffmanTrees)
	// populate the treeIndexes arr with the MTF'd index of the swapped
	// huffman table
	for i := range treeIndexes {
		c := 0
		for {
			// The bits here are aggregated as a unary count; the
			// final count is the MTF'd index of the swapped huffman
			// table
			inc := br.ReadBits(1)
			if inc == 0 {
				break
			}
			c++
		}
		fmt.Printf("treeIndex\t0x%x\t\t0b%b\n", c, c)
		if c >= numHuffmanTrees {
			return StructuralError("tree index too large")
		}

		// place the MTF'd tree in the tree indexes slot
		treeIndexes[i] = uint8(mtfTreeDecoder.Decode(c))
	}

	// The list of symbols for the move-to-front transform is taken from
	// the previously decoded symbol bitmap.
	symbols := make([]byte, numSymbols)
	nextSymbol := 0
	for i := 0; i < 256; i++ {
		// Loop through all the symbols, and if it's present in this
		// block then record it in the symbols array.
		if symbolPresent[i] {
			symbols[nextSymbol] = byte(i)
			nextSymbol++
		}
	}
	mtf := newMTFDecoder(symbols)

	numSymbols += 2 // to account for RUNA and RUNB symbols
	huffmanTrees := make([]huffmanTree, numHuffmanTrees)

	// Now we decode the arrays of code-lengths for each tree.
	lengths := make([]uint8, numSymbols)
	for i := range huffmanTrees {
		// The code lengths are delta encoded starting with an initial
		// 5-bit "seed" number.
		length := br.ReadBits(5)
		fmt.Printf("treeLength\t0x%x\t\t0b%b\n", length, length)
		fmt.Printf("\t\t\t\t")
		for j := range lengths {
			for {
				if !br.ReadBit() {
					fmt.Printf("0")
					break
				} else {
					fmt.Printf("1")
				}
				if br.ReadBit() {
					fmt.Printf("1")
					length--
				} else {
					fmt.Printf("0")
					length++
				}
			}
			if length < 0 || length > 20 {
				return StructuralError("Huffman length out of range")
			}
			lengths[j] = uint8(length)
		}
		fmt.Println()
		huffmanTrees[i], err = newHuffmanTree(lengths)
		if err != nil {
			return err
		}
	}

	selectorIndex := 1 // the next tree index to use
	if len(treeIndexes) == 0 {
		return StructuralError("no tree selectors given")
	}
	if int(treeIndexes[0]) >= len(huffmanTrees) {
		return StructuralError("tree selector out of range")
	}
	currentHuffmanTree := huffmanTrees[treeIndexes[0]]
	bufIndex := 0 // indexes bz2.buf, the output buffer.
	// The output of the move-to-front transform is run-length encoded and
	// we merge the decoding into the Huffman parsing loop. These two
	// variables accumulate the repeat count. See the Wikipedia page for
	// details.
	repeat := 0
	repeatPower := 0

	// The `C' array (used by the inverse BWT) needs to be zero initialized.
	for i := range bz2.c {
		bz2.c[i] = 0
	}

	decoded := 0 // counts the number of symbols decoded by the current tree.
	for {
		if decoded == 50 {
			if selectorIndex >= numSelectors {
				return StructuralError("insufficient selector indices for number of symbols")
			}
			if int(treeIndexes[selectorIndex]) >= len(huffmanTrees) {
				return StructuralError("tree selector out of range")
			}
			currentHuffmanTree = huffmanTrees[treeIndexes[selectorIndex]]
			selectorIndex++
			decoded = 0
		}

		v := currentHuffmanTree.Decode(br)
		decoded++

		if v < 2 {
			// This is either the RUNA or RUNB symbol.
			if repeat == 0 {
				repeatPower = 1
			}
			repeat += repeatPower << v
			repeatPower <<= 1

			// This limit of 2 million comes from the bzip2 source
			// code. It prevents repeat from overflowing.
			if repeat > 2*1024*1024 {
				return StructuralError("repeat count too large")
			}
			continue
		}

		if repeat > 0 {
			// We have decoded a complete run-length so we need to
			// replicate the last output symbol.
			if repeat > bz2.blockSize-bufIndex {
				return StructuralError("repeats past end of block")
			}
			for i := 0; i < repeat; i++ {
				b := byte(mtf.First())
				bz2.tt[bufIndex] = uint32(b)
				bz2.c[b]++
				bufIndex++
			}
			repeat = 0
		}

		if int(v) == numSymbols-1 {
			// This is the EOF symbol. Because it's always at the
			// end of the move-to-front list, and never gets moved
			// to the front, it has this unique value.
			break
		}

		// Since two metasymbols (RUNA and RUNB) have values 0 and 1,
		// one would expect |v-2| to be passed to the MTF decoder.
		// However, the front of the MTF list is never referenced as 0,
		// it's always referenced with a run-length of 1. Thus 0
		// doesn't need to be encoded and we have |v-1| in the next
		// line.
		b := byte(mtf.Decode(int(v - 1)))
		if bufIndex >= bz2.blockSize {
			return StructuralError("data exceeds block size")
		}
		bz2.tt[bufIndex] = uint32(b)
		bz2.c[b]++
		bufIndex++
	}

	if origPtr >= uint(bufIndex) {
		return StructuralError("origPtr out of bounds")
	}

	// We have completed the entropy decoding. Now we can perform the
	// inverse BWT and setup the RLE buffer.
	bz2.preRLE = bz2.tt[:bufIndex]
	bz2.preRLEUsed = 0
	bz2.tPos = inverseBWT(bz2.preRLE, origPtr, bz2.c[:])
	bz2.lastByte = -1
	bz2.byteRepeats = 0
	bz2.repeats = 0

	return nil
}

// inverseBWT implements the inverse Burrows-Wheeler transform as described in
// http://www.hpl.hp.com/techreports/Compaq-DEC/SRC-RR-124.pdf, section 4.2.
// In that document, origPtr is called `I' and c is the `C' array after the
// first pass over the data. It's an argument here because we merge the first
// pass with the Huffman decoding.
//
// This also implements the `single array' method from the bzip2 source code
// which leaves the output, still shuffled, in the bottom 8 bits of tt with the
// index of the next byte in the top 24-bits. The index of the first byte is
// returned.
func inverseBWT(tt []uint32, origPtr uint, c []uint) uint32 {
	sum := uint(0)
	for i := 0; i < 256; i++ {
		sum += c[i]
		c[i] = sum - c[i]
	}

	for i := range tt {
		b := tt[i] & 0xff
		tt[c[b]] |= uint32(i) << 8
		c[b]++
	}

	return tt[origPtr] >> 8
}

// This is a standard CRC32 like in hash/crc32 except that all the shifts are reversed,
// causing the bits in the input to be processed in the reverse of the usual order.

var crctab [256]uint32

func init() {
	const poly = 0x04C11DB7
	for i := range crctab {
		crc := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ poly
			} else {
				crc <<= 1
			}
		}
		crctab[i] = crc
	}
}

// updateCRC updates the crc value to incorporate the data in b.
// The initial value is 0.
func updateCRC(val uint32, b []byte) uint32 {
	crc := ^val
	for _, v := range b {
		crc = crctab[byte(crc>>24)^v] ^ (crc << 8)
	}
	return ^crc
}
