// Copyright 2014 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package btree implements in-memory B-Trees of arbitrary degree.
//
// btree implements an in-memory B-Tree for use as an ordered data structure.
// It is not meant for persistent storage solutions.
//
// It has a flatter structure than an equivalent red-black or other binary tree,
// which in some cases yields better memory usage and/or performance.
// See some discussion on the matter here:
//   http://google-opensource.blogspot.com/2013/01/c-containers-that-save-memory-and-time.html
// Note, though, that this project is in no way related to the C++ B-Tree
// implementation written about there.
//
// Within this tree, each node contains a slice of items and a (possibly nil)
// slice of children.  For basic numeric values or raw structs, this can cause
// efficiency differences when compared to equivalent C++ template code that
// stores values in arrays within the node:
//   * Due to the overhead of storing values as interfaces (each
//     value needs to be stored as the value itself, then 2 words for the
//     interface pointing to that value and its type), resulting in higher
//     memory use.
//   * Since interfaces can point to values anywhere in memory, values are
//     most likely not stored in contiguous blocks, resulting in a higher
//     number of cache misses.
// These issues don't tend to matter, though, when working with strings or other
// heap-allocated structures, since C++-equivalent structures also must store
// pointers and also distribute their values across the heap.
//
// This implementation is designed to be a drop-in replacement to gollrb.LLRB
// trees, (http://github.com/petar/gollrb), an excellent and probably the most
// widely used ordered tree implementation in the Go ecosystem currently.
// Its functions, therefore, exactly mirror those of
// llrb.LLRB where possible.  Unlike gollrb, though, we currently don't
// support storing multiple equivalent values.
package fz

import (
	"hash/fnv"
	"io"
	"os"
	"sort"
	"time"
	"unsafe"
)

const maxItems = 63
const maxChildren = maxItems + 1

type uint128 [2]uint64

func (l uint128) less(r uint128) bool {
	if l[0] == r[0] {
		return l[1] < r[1]
	}
	return l[0] < r[0]
}

func (s *nodeBlock) markDirty() {
	s._super.addDirtyNode(s)
}

// insertAt inserts a value into the given index, pushing all subsequent values
// forward.
func (s *nodeBlock) insertItemAt(index int, item pair) {
	if s.itemsSize >= maxItems {
		panic("already full")
	}

	copy(s.items[index+1:], s.items[index:])
	s.items[index] = item
	s.itemsSize++
	s.markDirty()
}

func (s *nodeBlock) appendItems(pairs ...pair) {
	copy(s.items[s.itemsSize:], pairs)
	s.itemsSize += uint16(len(pairs))
	if s.itemsSize > maxItems {
		panic("wtf")
	}
	s.markDirty()
}

// find returns the index where the given item should be inserted into this
// list.  'found' is true if the item already exists in the list at the given
// index.
func (s *nodeBlock) find(key uint128) (index int, found bool) {
	i := sort.Search(int(s.itemsSize), func(i int) bool {
		return key.less(s.items[i].key)
	})
	if i > 0 && !(s.items[i-1].key.less(key)) {
		return i - 1, true
	}
	return i, false
}

// insertAt inserts a value into the given index, pushing all subsequent values
// forward.
func (s *nodeBlock) insertChildAt(index int, n *nodeBlock) {
	if s.childrenSize >= maxChildren {
		panic("already full")
	}

	copy(s._children[index+1:], s._children[index:])
	copy(s.childrenOffset[index+1:], s.childrenOffset[index:])
	s._children[index] = n
	s.childrenSize++
	s.markDirty()
}

func (s *nodeBlock) appendChildren(nodes ...*nodeBlock) {
	copy(s._children[s.childrenSize:], nodes)
	for i := uint16(0); i < uint16(len(nodes)); i++ {
		if nodes[i] != nil {
			s.childrenOffset[s.childrenSize+i] = nodes[i].offset
		}
	}

	s.childrenSize += uint16(len(nodes))
	if s.childrenSize > maxChildren {
		panic("wtf")
	}
	s.markDirty()
}

func (s *nodeBlock) appendChildrenAndOffsets(nodes []*nodeBlock, offsets []int64) {
	copy(s._children[s.childrenSize:], nodes)
	for i := uint16(0); i < uint16(len(nodes)); i++ {
		s.childrenOffset[s.childrenSize+i] = offsets[i]
	}

	s.childrenSize += uint16(len(nodes))
	if s.childrenSize > maxChildren {
		panic("wtf")
	}
	s.markDirty()
}

// split splits the given node at the given index.  The current node shrinks,
// and this function returns the item that existed at that index and a new node
// containing all items/children after it.
func (n *nodeBlock) split(i int) (pair, *nodeBlock) {
	item := n.items[i]
	next := n._super.newNode()
	next.appendItems(n.items[i+1:]...)
	n.itemsSize = uint16(i)
	if n.childrenSize > 0 {
		next.appendChildrenAndOffsets(n._children[i+1:], n.childrenOffset[i+1:])
		n.childrenSize = uint16(i + 1)
	}
	next.markDirty()
	n.markDirty()
	return item, next
}

// maybeSplitChild checks if a child should be split, and if so splits it.
// Returns whether or not a split occurred.
func (n *nodeBlock) maybeSplitChild(i int) bool {
	ci, _ := n.child(i)
	if ci == nil || ci.itemsSize < maxItems {
		return false
	}
	first := ci
	item, second := first.split(maxItems / 2)
	n.insertItemAt(i, item)
	n.insertChildAt(i+1, second)
	n.markDirty()
	return true
}

// insert inserts an item into the subtree rooted at this node, making sure
// no nodes in the subtree exceed maxpairs items.  Should an equivalent item be
// be found/replaced by insert, it will be returned.
func (n *nodeBlock) insert(key uint128) error {
	i, found := n.find(key)
	//log.Println(n.children, n.items, item, i, found)
	if found {
		return ErrKeyExisted
	}

	if n.childrenSize == 0 {
		p, err := n._super.writePair(key)
		if err != nil {
			return err
		}
		n.insertItemAt(i, p)
		return nil
	}

	if n.maybeSplitChild(i) {
		inTree := n.items[i]
		switch {
		case key.less(inTree.key):
			// no change, we want first split node
		case inTree.key.less(key):
			i++ // we want second split node
		default:
			return ErrKeyExisted
		}
	}

	//log.Println(n._children, n.childrenOffset, n.childrenSize)
	ch, err := n.child(i)
	if err != nil {
		return err
	}
	return ch.insert(key)
}

func (n *nodeBlock) child(i int) (*nodeBlock, error) {
	var err error
	if n._children[i] == nil {
		if n.childrenOffset[i] == 0 {
			return nil, nil
		}

		//log.Println(n.childrenOffset[i])
		n._children[i], err = n._super.loadNodeBlock(n.childrenOffset[i])
	}
	return n._children[i], err
}

func (n *nodeBlock) areChildrenSynced() bool {
	for i := uint16(0); i < n.childrenSize; i++ {
		if n.childrenOffset[i] == 0 {
			if n._children[i] != nil && n._children[i].offset > 0 {
				n.childrenOffset[i] = n._children[i].offset
				continue
			}
			return false
		}
	}
	return true
}

func (n *nodeBlock) sync() (err error) {
	if n.offset == 0 {
		n.offset, err = n._super._fd.Seek(0, os.SEEK_END)
		if err != nil {
			return
		}
	}

	_, err = n._super._fd.Seek(n.offset, 0)
	if err != nil {
		return
	}

	x := *(*[nodeBlockSize]byte)(unsafe.Pointer(n))
	_, err = n._super._fd.Write(x[:])
	if err == nil {
		n._snapshotPending = x
	}
	return
}

// get finds the given key in the subtree and returns it.
func (n *nodeBlock) get(key uint128) (pair, error) {
	i, found := n.find(key)
	if found {
		return n.items[i], nil
	} else if n.childrenSize > 0 {
		ch, err := n.child(i)
		if err != nil {
			return pair{}, err
		}
		return ch.get(key)
	}
	return pair{}, ErrKeyNotFound
}

func (sb *SuperBlock) writePair(key uint128) (pair, error) {
	if sb._reader == nil {
		panic("wtf")
	}

	v, err := sb._fd.Seek(0, os.SEEK_END)
	if err != nil {
		return pair{}, err
	}

	buf := make([]byte, 32*1024)
	written := int64(0)
	h := fnv.New64()
	for {
		nr, er := sb._reader.Read(buf)
		if nr > 0 {
			nw, ew := sb._fd.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
				h.Write(buf[0:nr])
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	if err != nil {
		return pair{}, err
	}

	p := pair{key: key, tstamp: uint32(time.Now().Unix()), value: v, size: written, hash: h.Sum64()}
	return p, nil
}

func (sb *SuperBlock) Put(k uint128, r io.Reader) error {
	sb._lock.Lock()
	defer sb._lock.Unlock()

	var err error
	sb._reader = r

	if sb.rootNode == 0 && sb._root == nil {
		p, err := sb.writePair(k)
		if err != nil {
			return err
		}

		sb._root = sb.newNode()
		sb._root.itemsSize = 1
		sb._root.items[0] = p
		sb._root.markDirty()
		sb.count++
		return sb.syncDirties()
	}

	if sb.rootNode != 0 && sb._root == nil {
		sb._root, err = sb.loadNodeBlock(sb.rootNode)
		if err != nil {
			return err
		}
	}

	if sb._root.itemsSize >= maxItems {
		item2, second := sb._root.split(maxItems / 2)
		oldroot := sb._root
		sb._root = sb.newNode()
		sb._root.appendItems(item2)
		sb._root.appendChildren(oldroot, second)
		sb._root.markDirty()
	}

	if err := sb._root.insert(k); err != nil {
		return err
	}

	sb.count++
	if err := sb.syncDirties(); err != nil {
		return err
	}

	return nil
}

func (sb *SuperBlock) Get(key uint128) (*Data, error) {
	sb._lock.RLock()
	defer sb._lock.RUnlock()

	var err error
	if sb.rootNode == 0 && sb._root == nil {
		return nil, ErrKeyNotFound
	}

	if sb.rootNode != 0 && sb._root == nil {
		sb._root, err = sb.loadNodeBlock(sb.rootNode)
		if err != nil {
			return nil, err
		}
	}

	node, err := sb._root.get(key)
	if err != nil {
		return nil, err
	}

	d := &Data{}
	d._fd, err = os.OpenFile(sb._filename, os.O_RDONLY, 0666)
	if err != nil {
		return nil, err
	}

	if _, err := d._fd.Seek(node.value, 0); err != nil {
		return nil, err
	}

	d.h = fnv.New64()
	d.pair = node
	return d, nil
}

func (sb *SuperBlock) Commit() error {
	if sb._flag&LsAsyncCommit == 0 {
		panic("SuperBlock is not in async state")
	}

	sb._lock.Lock()
	defer sb._lock.Unlock()

	return sb._syncDirties()
}

func (n *nodeBlock) iterate(callback func(key uint128, data *Data) error) error {
	for i := uint16(0); i < n.itemsSize; i++ {
		if n.childrenSize > 0 {
			ch, err := n.child(int(i))
			if err != nil {
				return err
			}
			if err := ch.iterate(callback); err != nil {
				return err
			}
		}

		var err error
		d := &Data{}
		d._fd, err = os.OpenFile(n._super._filename, os.O_RDONLY, 0666)
		if err != nil {
			return err
		}

		if _, err := d._fd.Seek(n.items[i].value, 0); err != nil {
			return err
		}

		d.h = fnv.New64()
		d.pair = n.items[i]
		callback(n.items[i].key, d)
	}
	if n.childrenSize > 0 {
		ch, err := n.child(int(n.childrenSize - 1))
		if err != nil {
			return err
		}

		if err := ch.iterate(callback); err != nil {
			return err
		}
	}
	return nil
}

func (sb *SuperBlock) Walk(callback func(key uint128, data *Data) error) error {
	sb._lock.RLock()
	defer sb._lock.RUnlock()

	var err error
	if sb.rootNode == 0 && sb._root == nil {
		return nil
	}

	if sb._root == nil {
		sb._root, err = sb.loadNodeBlock(sb.rootNode)
		if err != nil {
			return err
		}
	}

	return sb._root.iterate(callback)
}

func (sb *SuperBlock) syncDirties() error {
	if sb._flag&LsAsyncCommit > 0 {
		return nil
	}

	return sb._syncDirties()
}

func (sb *SuperBlock) _syncDirties() error {
	sb._masterSnapshot.Reset()
	sb._masterSnapshot.Write(sb._snapshot[:])

	for node := range sb._dirtyNodes {
		if node.offset == 0 {
			continue
		}
		sb._masterSnapshot.Write(node._snapshot[:])
	}

	for len(sb._dirtyNodes) > 0 {
		for node := range sb._dirtyNodes {
			if !node.areChildrenSynced() {
				continue
			}

			if err := node.sync(); err != nil {
				return err
			}
			delete(sb._dirtyNodes, node)

			if testCase1 {
				return testError
			}
		}
	}

	sb.rootNode = sb._root.offset
	return sb.Sync()
}
