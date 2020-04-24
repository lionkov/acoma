package l1

import (
	"math/bits"
	"errors"
_	"fmt"
	"acoma/oligo"
	"acoma/oligo/long"
	"acoma/criteria"
	"acoma/l0"
	"github.com/klauspost/reedsolomon"
)

const (
	PrimerErrors = 3	// how many errors still match the primer
)

// Level 1 codec
type Codec struct {
	blknum	int	// number of data blocks
	rsnum	int	// number of Reed-Solomon metadata blocks
	mdsz	int	// length of the metadata block (in nts)
	crit	criteria.Criteria

	olen	int	// oligo length, not including the primers
	ec	reedsolomon.Encoder
}

var Eprimer = errors.New("primer mistmatch")
var Emetadata = errors.New("can't recover metadata")

var maxvals = []int {
	3: 47,
	4: 186,
	5: 733,
}

func NewCodec(blknum, mdsz, rsnum int, crit criteria.Criteria) *Codec {
	var err error

	c := new(Codec)
	c.blknum = blknum
	c.rsnum = rsnum
	c.mdsz = mdsz
	c.crit = crit

	// TODO: make it work with longer metadata blocks
	if mdsz < 3 || mdsz > 5 {
		return nil
	}

	mdnum := c.blknum  - c.rsnum
	c.ec, err = reedsolomon.New(mdnum, c.rsnum)
	if err != nil {
		panic("reedsolomon error")
	}

	c.olen = blknum * 17 +		// data blocks
		mdsz*(blknum - rsnum) +	// metadata blocks
		5*rsnum		  	// metadata erasure blocks (they have to be able to store a byte)

	return c
}

// number of blocks per oligo
func (c *Codec) BlockNum() int {
	return c.blknum
}

// number of bytes per data block
func (c *Codec) BlockSize() int {
	return 4
}

// length of the data saved per oligo (in bytes)
func (c *Codec) DataLen() int {
	return c.blknum * 4
}

func (c *Codec) OligoLen() int {
	return c.olen
}

// maximum address that the codec can encode
func (c *Codec) MaxAddr() uint64 {
	mdnum := c.blknum - c.rsnum

	ma := uint64(1)
	maxval :=uint64( maxvals[c.mdsz])
	for i := 0; i < mdnum; i++ {
		ma *= maxval
	}

	return uint64(ma / 4)
}

// Encode data into a an oligo
// The p5 and p3 oligos specify the 5'-end and the 3'-end primers that start and end the oligo. At the
// moment p5 needs to be at least 4 nts long.
// The ef parameter specifies whether the oligo is an erasure oligo (i.e. provides some erasure data 
// instead of data data).
func (c *Codec) Encode(p5, p3 oligo.Oligo, address uint64, ef bool, data [][]byte) (o oligo.Oligo, err error) {
	o, err = c.encode(p5, p3, address, ef, false, data)
	if err == nil && oligo.GCcontent(o) > 0.6 {
		var o1 oligo.Oligo

		o1, err = c.encode(p5, p3, address, ef, true, data)
		if err == nil {
			if oligo.GCcontent(o1) > 0.6 {
				// FIXME: should we just pick the one that has lower content?
				panic("both high GC content")
			}

			o = o1
		}
	}

	return
}

// The actual implementation of the encoding. 
// The sf paramter defines if the payload needs to be negated so 
// the GC content is kept low.
func (c *Codec) encode(p5, p3 oligo.Oligo, address uint64, ef, sf bool, data [][]byte) (o oligo.Oligo, err error) {
	var mdb []uint64
	var b oligo.Oligo

	if len(data) != c.BlockNum() {
		return nil, errors.New("invalid block number")
	}

	for _, blk := range data {
		if len(blk) != c.BlockSize() {
			return nil, errors.New("invalid data size")
		}
	}

	// TODO: should we make it work without primers?
	if p5.Len() < 4 {
		return nil, errors.New("5'-end primer must be at least four nt long")
	}

	mdb, err = c.calculateMdBlocks(address, ef, sf)
	if err != nil {
		return nil, err
	}

	// negate the values if sf is true
	if sf {
		d := make([][]byte, len(data))
		for i := 0; i < len(data); i++ {
			d[i] = make([]byte, len(data[i]))
			for j := 0; j < len(data[i]); j++ {
				d[i][j] = ^data[i][j]
			}
		}
		data = d

		// TODO: do something similar for metadata
	}

	// Construct the oligo
	// start with the 5'-end primer
	o, _ = long.Copy(p5)

	for i := 0; i < c.blknum; i++ {
		buf := data[i]

		// combine the data bytes into uint64
		v := uint64(buf[0]) | (uint64(buf[1]) << 8) | (uint64(buf[2]) << 16) |
                        (uint64(buf[3]) << 24)

		// calculate parity
		v <<= 1
		if bits.OnesCount64(v)%2 != 0 {
			v += 1
		}

		// append the data block
		prefix := o.Slice(o.Len() - 4, o.Len())
		b, err = l0.Encode(prefix, v, 17, c.crit)
		if err != nil {
			return nil, err
		}
		o.Append(b)

		// append the metadata block
		prefix = o.Slice(o.Len() - 4, 0)

		// FIXME: the RS implementation that we are using works on bytes
		// So the erasure metadata blocks need to be 8 bits long, no matter
		// what the size of the metadata blocks is. 
		// We should find a variable-bit-length RS implementation for the 
		// metadata
		sz := c.mdsz
		if i >= c.blknum - c.rsnum {
			sz = 5
		}

		b, err = l0.Encode(prefix, mdb[i], sz, c.crit)
		if err != nil {
			return nil, err
		}

		o.Append(b)
	}

	// append the 3'-end primer
	// FIXME: we don't apply the criteria when appending p3,
	// so theoretically we can have homopolymers etc.
	o.Append(p3)

	return o, nil
}

// calculate the metadata blocks based on the metadata
func (c *Codec) calculateMdBlocks(address uint64, ef, sf bool) ([]uint64, error) {
	maxaddr := c.MaxAddr()
	if address > maxaddr {
		return nil, errors.New("address too big")
	}

	// calculate the metadata value
	if sf {
		address += maxaddr * 2
	}

	if ef {
		address += maxaddr
	}

	// split the metadata into md blocks
	mdnum := uint64(c.blknum - c.rsnum)
	mdlen := uint64(maxvals[c.mdsz])
	mdb := make([]uint64, mdnum + uint64(c.rsnum))
	for i := int(mdnum - 1); i >= 0; i-- {
		mdb[i] = address % mdlen
		address /= mdlen
	}

	if address != 0 {
		panic("Internal error: address not zero at the end")
	}

	if c.mdsz * 2 > 8 {
		panic("metadata block too big (FIXME)")
	}

	// calculate metadata erasure blocks
	// first we need to convert the metadata blocks to arrays of bytes
	mdshard := make([][]byte, len(mdb))
	for i := 0; i < len(mdshard); i++ {
		mdshard[i] = make([]byte, 1)
		mdshard[i][0] = byte(mdb[i])
	}

	err := c.ec.Encode(mdshard)
	if err != nil {
		return nil, err
	}

	for i := 0; i < len(mdshard); i++ {
		mdb[i] = uint64(mdshard[i][0])
	}
	
	return mdb, nil
}

// Decodes an oligo into the metadata and data it contains
// If the recover parameter is true, try harder to correct the metadata
// Returns a byte array for each data block that was recovered
// (i.e. the parity for the block was correct)
func (c *Codec) Decode(p5, p3, ol oligo.Oligo, recover bool) (address uint64, ef bool, data [][]byte, err error) {
	var sf bool

	address, ef, sf, data, err = c.decode(p5, p3, ol, false, recover)
	if err != nil || !sf {
		return
	}

	// HighGC oligo, "flip" it
	address, ef, sf, data, err = c.decode(p5, p3, ol, true, recover)
	return
}

// minimal decode, assumes no errors. Needs to be fixed
func (c *Codec) decode(p5, p3, ol oligo.Oligo, flip bool, recover bool) (address uint64, ef, sf bool, data [][]byte, err error) {
	var mdblk []uint64
	var mdok bool

	// first cut the primers
	pos5, len5 := oligo.Find(ol, p5, PrimerErrors)
	if pos5 != 0 {
		err = Eprimer
		return
	}

	pos3, len3 := oligo.Find(ol, p3, PrimerErrors)
	if pos3 < 0 || pos3+len3 != ol.Len() {
		err = Eprimer
		return
	}

	sol := ol.Slice(pos5+len5, pos3)
	mdblk = make([]uint64, c.blknum)
	prefix := p5.Slice(p5.Len() - 4, p5.Len())
	ol = sol
	mdok = true
	for i := 0; i < c.blknum; i++ {
		var v uint64
		var d []byte
		var pbit int

		if ol.Len() < 17 {
			goto savedblk
		}

		v, err = l0.Decode(prefix, ol.Slice(0, 17), c.crit)
		if err != nil {
			goto savedblk
		}

		pbit = int(v & 1)
		v >>= 1
		if (bits.OnesCount64(v) + pbit) % 2 == 0 {
			if flip {
				v = ^v
			}

			d = make([]byte, 0, 4)
			d = append(d, byte(v))
			d = append(d, byte(v >> 8))
			d = append(d, byte(v >> 16))
			d = append(d, byte(v >> 24))
		}

savedblk:
		data = append(data, d)

		mdsz := c.mdsz
		if i >= c.blknum - c.rsnum {
			mdsz = 5
		}

		mdol := ol.Slice(17, 17 + mdsz)
		if mdol.Len() != mdsz {
			// short oligo
			mdok = false
		} else {
			mdblk[i], err = l0.Decode(ol.Slice(13, 17), mdol, c.crit)
			if err != nil {
				mdok = false
			}
		}

		prefix = ol.Slice(13 + mdsz, 17 + mdsz)
		ol = ol.Slice(17 + mdsz, 0)
	}

	// Handle the data


	// Handle the metadata
	if mdok {
		mdshards := make([][]byte, len(mdblk))
		for i := 0; i < len(mdshards); i++ {
			mdshards[i] = append(mdshards[i], byte(mdblk[i]))
		}

		mdok, err = c.ec.Verify(mdshards)
		if err != nil {
			mdok = false
		}
	}

	if !mdok {
		if !recover {
			err = Emetadata
			return
		}

		// Try to recover the metadata, and eventually get better at the data too
		data, mdblk, err = c.tryRecover(p5, p3, sol, flip)
		if err != nil {
			return
		}
	}

	// FIXME: md can be more than 64 bits
	md := uint64(0)
	maxval := uint64(maxvals[c.mdsz])
	for i := 0; i < c.blknum - c.rsnum; i++ {
		md = md * maxval + mdblk[i]
	}

	maxaddr := c.MaxAddr()
	if md >= 2*maxaddr {
		sf = true
		md -= 2*maxaddr
	}

	if md >= maxaddr {
		ef = true
		md -= maxaddr
	}

	address = md

	return	
}

