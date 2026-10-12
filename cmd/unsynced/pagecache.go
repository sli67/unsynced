package main

import (
	"bytes"
	"sync"
)

// pageCache plays the role of the kernel's page cache. Writes land here
// first and reach backingDir only when the program calls fsync. A simulated
// power loss throws away whatever is still here.
type pageCache struct {
	mu      sync.Mutex
	pending map[uint64][]write // inode number -> unsynced writes, oldest first
}

type write struct {
	off  int64
	data []byte
}

func newPageCache() *pageCache {
	return &pageCache{pending: make(map[uint64][]write)}
}

// add buffers a write. data is copied because go-fuse reuses its buffer.
func (c *pageCache) add(ino uint64, off int64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[ino] = append(c.pending[ino], write{off, bytes.Clone(data)})
}

// take removes and returns a file's unsynced writes so fsync can put them
// on disk.
func (c *pageCache) take(ino uint64) []write {
	c.mu.Lock()
	defer c.mu.Unlock()
	ws := c.pending[ino]
	delete(c.pending, ino)
	return ws
}

// end is the file size implied by the unsynced writes, or 0 if there are none.
func (c *pageCache) end(ino uint64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var end int64
	for _, w := range c.pending[ino] {
		end = max(end, w.off+int64(len(w.data)))
	}
	return end
}

// overlay lays the unsynced writes over buf, which holds the bytes read from
// disk starting at off. size is how many bytes the reader asked for.
func (c *pageCache) overlay(ino uint64, buf []byte, off int64, size int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.pending[ino] {
		start := w.off - off // where w begins, relative to buf
		stop := min(start+int64(len(w.data)), int64(size))
		if stop <= 0 || start >= stop {
			continue // w is outside the range being read
		}
		if int64(len(buf)) < stop {
			buf = append(buf, make([]byte, stop-int64(len(buf)))...) // w extends the file
		}
		src := w.data
		if start < 0 {
			src, start = src[-start:], 0
		}
		copy(buf[start:stop], src)
	}
	return buf
}

// dropAll is the power loss: every unsynced write is discarded.
func (c *pageCache) dropAll() (writes, lostBytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ws := range c.pending {
		for _, w := range ws {
			writes++
			lostBytes += len(w.data)
		}
	}
	clear(c.pending)
	return writes, lostBytes
}
