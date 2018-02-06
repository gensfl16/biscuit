package main

import "fmt"
import "sync"
import "unsafe"
import "strconv"

// A block has a lock, since, it may store an inode block, which has 4 inodes,
// and we need to ensure that hat writes aren't lost due to concurrent inode
// updates.  The inode code is careful about releasing lock.  Other types of
// blocks not shared and the caller releases the lock immediately.
//
// The data of a block is a page allocated from the page allocator.  The disk
// DMAs to and from the physical address for the page.  data is the virtual
// address for the page.
//

// If you change this, you must change corresponding constants in mkbdisk.py,
// fs.go, litc.c (fopendir, BSIZE), usertests.c (BSIZE).
const BSIZE=4096

const bdev_debug = false
	
type bdev_block_t struct {
	sync.Mutex
	disk	int
	block	int
	pa      pa_t
	data	*bytepg_t
	s       string
}

func (blk *bdev_block_t) Key() int {
	return blk.block
}

func (blk *bdev_block_t) Evict() {
	if bdev_debug {
		fmt.Printf("evict: block %v %#x %v\n", blk.block, blk.pa, refcnt(blk.pa))
	}
	if memfs {
		panic("Running with memory FS")
	}
	blk.free_page()
}

func (blk *bdev_block_t) Evictnow() bool {
	return false
}

func mkBlock_newpage(block int, s string) *bdev_block_t {
	b := mkblock(block, pa_t(0), s)
	b.New_page()
	return b
}

func (b *bdev_block_t) Write() {
	if bdev_debug {
		fmt.Printf("bdev_write %v %v\n", b.block, b.s)
	}
	if b.data[0] == 0xc && b.data[1] == 0xc {  // XXX check
		panic("write\n")
	}
	req := bdev_req_new([]*bdev_block_t{b}, BDEV_WRITE, true)
	if ahci_start(req) {
		<- req.ackCh
	}
} 

func (b *bdev_block_t) Write_async() {
	if bdev_debug {
		fmt.Printf("bdev_write_async %v %s\n", b.block, b.s)
	}
	// if b.data[0] == 0xc && b.data[1] == 0xc {  // XXX check
	//	panic("write_async\n")
	//}
	ider := bdev_req_new([]*bdev_block_t{b}, BDEV_WRITE, false)
	ahci_start(ider)
}

func (b *bdev_block_t) Read() {
	ider := bdev_req_new([]*bdev_block_t{b}, BDEV_READ, true)
	if ahci_start(ider) {
		<- ider.ackCh
	}
	if bdev_debug {
		fmt.Printf("bdev_read %v %v %#x %#x\n", b.block, b.s, b.data[0], b.data[1])
	}
	
	// XXX sanity check, but ignore it during recovery
	if b.data[0] == 0xc && b.data[1] == 0xc {
		fmt.Printf("WARNING: %v %v\n", b.s, b.block)
	}
	
}

func (blk *bdev_block_t) New_page() {
	_, pa, ok := refpg_new()
	if !ok {
		panic("oom during bdev.new_page")
	}
	blk.pa = pa
	blk.data = (*bytepg_t)(unsafe.Pointer(dmap(pa)))
	refup(blk.pa)
}

//
// Implementation of blocks
//

func mkblock(block int, pa pa_t, s string) *bdev_block_t {
	b := &bdev_block_t{};
	b.block = block
	b.pa = pa
	b.data = (*bytepg_t)(unsafe.Pointer(dmap(pa)))
	b.s = s
	return b
}


func (blk *bdev_block_t) free_page() {
	refdown(blk.pa)
}

// block cache, all device interactions run through block cache.
//
// The cache returns a pointer to a block_dev_t.  There is *one* bdev_block_t
// for a block number (and physical page associated with that blockno).  Callers
// share same block_dev_t (and physical page) for a block. The callers must
// coordinate using the lock of the block.
//
// When a reference to a bdev block in the cache is sent to the log daemon, or
// handed to the disk driver, we increment the refcount on the physical page
// using bcache_refup(). When code (including driver) is done with a bdev_block,
// it decrements the reference count with bdev_relse (e.g., in the driver
// interrupt handler).
//

var bcache = bcache_t{}

type bcache_t struct {
	refcache  *refcache_t
}

func mkBcache() {
	bcache.refcache = mkRefcache(syslimit.blocks, false)
}

// returns locked buf with refcnt on page bumped up by 1. caller must call
// bdev_relse when done with buf.
func (bcache *bcache_t) Get_fill(blkn int, s string, lock bool) (*bdev_block_t, err_t) {
	b, created, err := bcache.bref(blkn, s)

	if bdev_debug {
		fmt.Printf("bcache_get_fill: %v %v created? %v\n", blkn, s, created)
	}

	if err != 0 {
		return nil, err
	}
	
	if created {
		b.New_page()
		b.Read() // fill in new bdev_cache entry
	}
	if !lock {
		b.Unlock()
	}
	return b, 0
}

// returns locked buf with refcnt on page bumped up by 1. caller must call
// bcache_relse when done with buf
func (bcache *bcache_t) Get_zero(blkn int, s string, lock bool) (*bdev_block_t, err_t) {
	b, created, err := bcache.bref(blkn, s)
	if bdev_debug {
		fmt.Printf("bcache_get_zero: %v %v %v\n", blkn, s, created)
	}
	if err != 0 {
		return nil, err
	}
	if created {
		b.New_page()   // zero
	} 
	if !lock {
		b.Unlock()
	}
	return b, 0
}

// returns locked buf with refcnt on page bumped up by 1. caller must call
// bcache_relse when done with buf
func (bcache *bcache_t) Get_nofill(blkn int, s string, lock bool) (*bdev_block_t, err_t) {
	b, created, err := bcache.bref(blkn, s)
	if bdev_debug {
		fmt.Printf("bcache_get_nofill1: %v %v %v\n", blkn, s, created)
	}
	if err != 0 {
		return nil, err
	}
	if created {
		b.New_page()   // XXX a non-zero page would be fine
	}
	if !lock {
		b.Unlock()
	}
	return b, 0
}

func (bcache *bcache_t) Write(b *bdev_block_t) {
	bcache.refcache.Refup(b, "bcache_write")
	b.Write()
}

func (bcache *bcache_t) Write_async(b *bdev_block_t) {
	bcache.refcache.Refup(b, "bcache_write_async")
	b.Write_async()
}

// blks must be contiguous on disk
func (bcache *bcache_t) Write_async_blks(blks []*bdev_block_t) {
	if bdev_debug {
		fmt.Printf("bcache_write_async_blks %v\n", len(blks))
	}
	if len(blks) == 0  {
		panic("bcache_write_async_blks\n")
	}
	n := blks[0].block-1
	for _, b := range blks {
		// sanity check
		if b.block != n + 1 {
			panic("not contiguous\n")
		}
		n++
		bcache.refcache.Refup(b, "bcache_write_async_blks")
	}
	// one request for all blks
	ider := bdev_req_new(blks, BDEV_WRITE, false)
	ahci_start(ider)
}

func (bcache *bcache_t) Refup(b *bdev_block_t, s string) {
	bcache.refcache.Refup(b, s)
}

func (bcache *bcache_t) Relse(b *bdev_block_t, s string) {
	if bdev_debug {
		fmt.Printf("bcache_relse: %v %v\n", b.block, s)
	}
	bcache.refcache.Refdown(b, s)
}

func (bcache *bcache_t) Stats() string {
	s := "bcache: size "
	s += strconv.Itoa(len(bcache.refcache.refs))
	s += " #evictions "
	s += strconv.Itoa(bcache.refcache.nevict)
	s += " #live "
	s += strconv.Itoa(bcache.refcache.nlive())
	s += "\n"
	return s
}

//
// Implementation
//

// returns the reference to a locked buffer
func (bcache *bcache_t) bref(blk int, s string) (*bdev_block_t, bool, err_t) {
	ref, err := bcache.refcache.Lookup(blk, s)
	if err != 0 {
		// fmt.Printf("bref error %v\n", err)
		return nil, false, err
	}
	defer ref.Unlock()

	created := false
	if !ref.valid {
		if bdev_debug {
			fmt.Printf("bref fill %v %v\n", blk, s)
		}
		buf := mkblock(blk, pa_t(0), s)
		ref.obj = buf
		ref.valid = true
		created = true
	}
	b := ref.obj.(*bdev_block_t)
	b.Lock()
	b.s = s
	return b, created, err
}



func bdev_test() {
	return
	
	fmt.Printf("disk test\n")

	const N = 3

	wbuf := new([N]*bdev_block_t)

	for b := 0; b < N; b++ {
		wbuf[b] = mkBlock_newpage(b, "disktest")
	}
	for j := 0; j < 100; j++ {

		for b := 0; b < N; b++ {
			fmt.Printf("req %v,%v\n", j, b)

			for i,_ := range wbuf[b].data {
				wbuf[b].data[i] = uint8(b)
			}
			wbuf[b].Write_async()
		}
		flush()
		for b := 0; b < N; b++ {
			rbuf, err := bcache.Get_fill(b, "read test", false)
			if err != 0 {
				panic("bdev_test\n")
			}
			for i, v := range rbuf.data {
				if v != uint8(b) {
					fmt.Printf("buf %v i %v v %v\n", j, i, v)
					panic("bdev_test\n")
				}
			}
		}
	}
	panic("disk test passed\n")
}


// Block allocator

type ballocater_t struct {
	alloc *allocater_t
	first int
}
var ballocater *ballocater_t

func balloc_init(start,len, first int) {
	ballocater = &ballocater_t{}
	ballocater.alloc = make_allocater(start, len)
	fmt.Printf("first datablock %v\n", first)
	ballocater.first = first
}

// allocates a block, marking it used in the free block bitmap. free blocks and
// log blocks are not accounted for in the free bitmap; all others are. balloc
// should only ever acquire fblock.
func balloc1() (int, err_t) {
	blkn, err := ballocater.alloc.alloc()
	if err != 0 {
		return 0, err
	}
	return blkn+ballocater.first, err
}

func balloc() (int, err_t) {
	ret, err := balloc1()
	if err != 0 {
		return 0, err
	}
	if ret < 0 {
		panic("balloc: bad blkn")
	}
	if ret >= superb.lastblock() {
		fmt.Printf("blkn %v last %v\n", ret, superb.lastblock())
		return 0, -ENOMEM
	}
	blk, err := bcache.Get_zero(ret, "balloc", true)
	if err != 0 {
		return 0, err
	}
	if bdev_debug {
		fmt.Printf("balloc: %v\n", ret)
	}

	var zdata [BSIZE]uint8
	copy(blk.data[:], zdata[:])
	blk.Unlock()
	fslog.Write(blk)
	bcache.Relse(blk, "balloc")
	return ret, 0
}

func bfree(blkno int) err_t {
	if bdev_debug {
		fmt.Printf("bfree: %v\n", blkno)
	}
	blkno -= ballocater.first
	if blkno < 0 {
		panic("bfree")
	}
	return ballocater.alloc.free(blkno)
}

func balloc_stat() string {
	return "balloc " + ballocater.alloc.stat()
}
