package avl

import (
	"bytes"
	"fmt"
	"io"
	"math/bits"
	"encoding/binary"
	"runtime"
	lru "github.com/hashicorp/golang-lru"
	"os"
)

// AVL+ organised into pages, with history

type Avl3 struct {
	currentVersion Version
	trace bool
	maxPageId PageID
	pageMap map[PageID][]byte // Pages by pageId
	maxValueId uint64
	valueMap map[uint64][]byte // Large values by valueId
	valueHashes map[uint64]Hash // Value hashes
	valueLens map[uint64]uint32 // Value lengths
	freelist []PageID // Free list of pages
	prevRoot Ref3 // Root of the previous version that is getting "peeled off" from the current version
	root Ref3 // Root of the current update buffer
	versions map[Version]PageID // root pageId for a version
	pageCache *lru.Cache
	pagesToRecycle map[PageID]struct{} // this is not strictly required, but allows to visualise prevRoot before commit
	commitedCounter uint64
	pageSpace uint64
	pageFile, valueFile, verFile *os.File
	hashLength uint32
	compare func([]byte, []byte) int
}

func NewAvl3() *Avl3 {
	t := &Avl3{
		pageMap: make(map[PageID][]byte),
		valueMap: make(map[uint64][]byte),
		valueHashes: make(map[uint64]Hash),
		valueLens: make(map[uint64]uint32),
		versions: make(map[Version]PageID),
		pagesToRecycle: make(map[PageID]struct{}),
	}
	pageCache, err := lru.New(128*1024)
	if err != nil {
		panic(err)
	}
	t.pageCache = pageCache
	t.hashLength = 32
	t.compare = bytes.Compare
	return t
}

func (t *Avl3) SetCompare(c func([]byte, []byte) int) {
	t.compare = c
}

func (t *Avl3) walkToArrowPoint(r Ref3, key []byte, height uint32) (point Ref3, parent *Fork3, parentC int, err error) {
	if t.trace {
		fmt.Printf("walkToArrowPoint arrow: %s %d, key: %s %d\n", r.getmax(), r.getheight(), key, height)
	}
	current := r
	for {
		switch n := current.(type) {
		case nil:
			return nil, nil, 0, nil
		case *Leaf3:
			if height != 1 || !bytes.Equal(key, n.key) {
				return nil, nil, 0, fmt.Errorf("Leaf3 with key %s, expected height %d key %s", n.key, height, key)
			}
			return n, nil, 0, nil
		case *Fork3:
			if t.trace {
				fmt.Printf("walkToArrowPoint(Fork3) %s %d, key %s %d\n", n.max, n.height, key, height)
			}
			if n.height < height {
				return nil, nil, 0, fmt.Errorf("Fork3 with height %d max %s, expected height %d key %s", n.height, n.max, height, key)
			} else if n.height > height {
				parent = n
				switch t.compare(key, n.left.getmax()) {
				case -1, 0:
					current = n.left
					parentC = -1
				case 1:
					current = n.right
					parentC = 1
				}
			} else {
				if !bytes.Equal(key, n.max) {
					return nil, nil, 0, fmt.Errorf("Fork3 with height %d max %s, expected height %d key %s", n.height, n.max, height, key)
				}
				return n, parent, parentC, nil
			}
		case *Arrow3:
			return nil, nil, 0, fmt.Errorf("Arrow3 P.%d with height %d max %s, expected height %d key %s", n.pageId, n.height, n.max, height, key)
		}
	}
}

func (t *Avl3) UseFiles(pageFileName, valueFileName, verFileName string, read bool) {
	var err error
	if read {
		t.pageFile, err = os.Open(pageFileName)
	} else {
		t.pageFile, err = os.OpenFile(pageFileName, os.O_RDWR|os.O_CREATE, 0600)
	}
	if err != nil {
		panic(err)
	}
	if read {
		t.valueFile, err = os.Open(valueFileName)
	} else {
		t.valueFile, err = os.OpenFile(valueFileName, os.O_RDWR|os.O_CREATE, 0600)
	}
	if err != nil {
		panic(err)
	}
	if read {
		t.verFile, err = os.Open(verFileName)
	} else {
		t.verFile, err = os.OpenFile(verFileName, os.O_RDWR|os.O_CREATE, 0600)
	}
	if err != nil {
		fmt.Printf("Could not open version file: %v\n", err)
		return
	}
	// Read versions
	var verdata [8]byte
	var lastPageID PageID
	for n, _ := t.verFile.ReadAt(verdata[:], int64(t.currentVersion)*int64(8)); n >0; n, _ = t.verFile.ReadAt(verdata[:], int64(t.currentVersion)*int64(8)) {
		lastPageID = PageID(binary.BigEndian.Uint64(verdata[:]))
		t.versions[t.currentVersion] = lastPageID
		t.currentVersion++
	}
	if t.currentVersion > 0 {
		t.maxPageId = lastPageID
		fmt.Printf("Deserialising page %d\n", lastPageID)
		root, _ := t.deserialisePage(lastPageID, nil, 0)
		prevRootArrow := &Arrow3{height: root.getheight(), max: root.getmax()}
		t.root = &Arrow3{pageId: lastPageID, height: root.getheight(), max: root.getmax(), arrow: prevRootArrow}
		t.prevRoot = prevRootArrow
	}
}

func (t *Avl3) Close() {
	if t.pageFile != nil {
		t.pageFile.Close()
	}
	if t.valueFile != nil {
		t.valueFile.Close()
	}
	if t.verFile != nil {
		t.verFile.Close()
	}
}

func (t *Avl3) CurrentVersion() Version {
	return t.currentVersion
}

func (t *Avl3) Scan() {
	if info, err := t.pageFile.Stat(); err != nil {
		panic(err)
	} else {
		t.maxPageId = PageID(info.Size()/int64(PageSize))
	}
	current, _ := t.deserialisePage(t.maxPageId-1, nil, 0)
	m := make(map[PageID]struct{})
	fmt.Printf("Max page depth: %d\n", t.scan(current, m))
	fmt.Printf("Pages in current state: %d\n", len(m))
}

func (t *Avl3) SpaceScan() {
	if info, err := t.pageFile.Stat(); err != nil {
		panic(err)
	} else {
		t.maxPageId = PageID(info.Size()/int64(PageSize))
	}
	var pCount, arrows, leaves, totalArrowBits, totalStructBits, totalPrefixLen, totalKeyBodies, totalValBodies uint64
	maxPageId := t.maxPageId
	for pageId := PageID(1); pageId < maxPageId; pageId++ {
		p, _ := t.deserialisePage(pageId, nil, 0)
		if p == nil {
			continue
		}
		pCount++
		var m PageID = maxPageId
		prefix, keyCount, pageCount, keyBodySize, valBodySize, structBits, _ := p.serialisePass1(t, &m)
		arrows += uint64(pageCount)
		leaves += uint64(keyCount)
		nodeCount := pageCount + keyCount
		totalArrowBits += uint64(4*((nodeCount+31)/32))
		totalStructBits += uint64(4*((structBits+31)/32))
		totalPrefixLen += uint64(len(prefix))
		totalKeyBodies += uint64(keyBodySize - nodeCount*uint32(len(prefix)))
		totalValBodies += uint64(valBodySize)
		if pageId % 10000 == 0 {
			fmt.Printf("Process %d pages\n", pageId)
		}
	}
	totalSize := uint64(pCount)*uint64(PageSize)
	totalPageFixed := uint64(pCount)*uint64(12)
	totalKeyHeader := (arrows+leaves)*uint64(4)
	totalArrowHeader := arrows*uint64(12+t.hashLength)
	totalValueHeader := leaves*uint64(4)
	totalSlack := totalSize - totalPageFixed - totalKeyHeader - totalArrowHeader - totalValueHeader - totalArrowBits - totalStructBits - totalPrefixLen - totalKeyBodies - totalValBodies
	fmt.Printf("Total size: %d\n", totalSize)
	fmt.Printf("Page fixed headers: %d, %.3f percent\n", totalPageFixed, 100.0*float64(totalPageFixed)/float64(totalSize))
	fmt.Printf("Key headers: %d, %.3f percent\n", totalKeyHeader, 100.0*float64(totalKeyHeader)/float64(totalSize))
	fmt.Printf("Arrow headers: %d, %.3f percent\n", totalArrowHeader, 100.0*float64(totalArrowHeader)/float64(totalSize))
	fmt.Printf("Value headers: %d, %.3f percent\n", totalValueHeader, 100.0*float64(totalValueHeader)/float64(totalSize))
	fmt.Printf("Arrow bits: %d, %.3f percent\n", totalArrowBits, 100.0*float64(totalArrowBits)/float64(totalSize))
	fmt.Printf("Struct bits: %d, %.3f percent\n", totalStructBits, 100.0*float64(totalStructBits)/float64(totalSize))
	fmt.Printf("Prefixes: %d, %.3f percent\n", totalPrefixLen, 100.0*float64(totalPrefixLen)/float64(totalSize))
	fmt.Printf("Key bodies: %d, %.3f percent\n", totalKeyBodies, 100.0*float64(totalKeyBodies)/float64(totalSize))
	fmt.Printf("Value bodies: %d, %3.f percent\n", totalValBodies, 100.0*float64(totalValBodies)/float64(totalSize))
	fmt.Printf("Slack: %d, %.3f percent\n", totalSlack, 100.0*float64(totalSlack)/float64(totalSize))
}

func (t *Avl3) scan(r Ref3, m map[PageID]struct{}) int {
	switch r := r.(type) {
	case *Leaf3:
		return 0
	case *Fork3:
		ld := t.scan(r.left, m)
		rd := t.scan(r.right, m)
		if ld > rd {
			return ld
		} else {
			return rd
		}
	case *Arrow3:
		point, _ := t.deserialisePage(r.pageId, r.max, r.height)
		if point == nil {
			panic("")
		}
		if _, ok := m[r.pageId]; !ok {
			m[r.pageId] = struct{}{}
			if len(m) % 10000 == 0 {
				fmt.Printf("Read %d pages\n", len(m))
			}
		}
		return 1 + t.scan(point, m)
	}
	return 0
}

func (t *Avl3) SetHashLength(hashLength uint32) {
	t.hashLength = hashLength
}

// Common fields for a tree node
type Node3 struct {
	arrow *Arrow3 // Pointer back to the arrow that points to this fork
	pinnedPageId PageID // 0 if node is not pinned to a page, otherwise the page Id where this node should stay
}

type Leaf3 struct {
	Node3
	key, value []byte
	valueId uint64
	valueLen uint32
}

type Fork3 struct {
	Node3
	height uint32 // Height of the entire subtree rooted at this node, including node itself
	left, right Ref3
	max []byte // Largest key in the subtree
}

type Arrow3 struct {
	arrow *Arrow3
	pageId PageID // Connection of this page to the page on the disk. Normally pageId corresponds to offset pageId*PageSize in the database file
	height uint32 // Height of the entire subtree rooted at this page
	max []byte
	parent *Fork3 // Fork that have this arrow as either left of right branch
	parentC int   // -1 if the parent has this arrow as left branch, 1 if the parent has this arrow as right branch
}

// Reference can be either a WbtNode3, or WbtArrow3. The latter is used when the leaves of one page reference another page
type Ref3 interface {
	getheight() uint32
	nkey() []byte
	getmax() []byte
	dot(*Avl3, *graphContext, string)
	heightsCorrect(path string) (uint32, bool)
	balanceCorrect() bool
	serialisePass1(t *Avl3, maxPageId *PageID) (prefix []byte, keyCount, pageCount, keyBodySize, valBodySize, structBits uint32, pinnedPageId PinnedPageID)
}

func (t *Avl3) nextPageId() PageID {
	var id PageID
	// Take tha max for determinism
	for pageId := range t.pagesToRecycle {
		if pageId > id {
			id = pageId
		}
	}
	if id != PageID(0) {
		delete(t.pagesToRecycle, id)
		return id
	}
	if len(t.freelist) > 0 {
		nextId := t.freelist[len(t.freelist)-1]
		t.freelist = t.freelist[:len(t.freelist)-1]
		return nextId
	}
	t.maxPageId++
	return t.maxPageId
}

func (t *Avl3) freePageId(pageId PageID) {
	//t.pageCache.Remove(pageId)
	if data, ok := t.pageMap[pageId]; ok {
		t.pageSpace -= uint64(len(data))
		delete(t.pageMap, pageId)
		t.freelist = append(t.freelist, pageId)
	}
}

func (t *Avl3) nextValueId() uint64 {
	return t.maxValueId + 1
}

func (t *Avl3) freeValueId(valueId uint64) {
	if valueId == 0 {
		return
	}
	delete(t.valueMap, valueId)
	delete(t.valueHashes, valueId)
	delete(t.valueLens, valueId)
}

func (t *Avl3) addValue(valueId uint64, value []byte) {
	if t.valueFile == nil {
		t.valueMap[valueId] = value
		t.valueLens[valueId] = uint32(len(value))
		t.maxValueId++
	} else {
		if _, err := t.valueFile.WriteAt(value, int64(valueId)-1); err != nil {
			panic(err)
		}
		t.maxValueId += uint64(len(value))
	}
}

func (t *Avl3) SetTracing(tracing bool) {
	t.trace = tracing
}

func (l *Leaf3) getheight() uint32 {
	return 1
}

func (f *Fork3) getheight() uint32 {
	return f.height
}

func (a *Arrow3) getheight() uint32 {
	return a.height
}

func (l *Leaf3) nkey() []byte {
	return l.key
}

func (f *Fork3) nkey() []byte {
	return f.max
}

func (a *Arrow3) nkey() []byte {
	return a.max
}

func (l *Leaf3) getmax() []byte {
	return l.key
}

func (f *Fork3) getmax() []byte {
	return f.max
}

func (a *Arrow3) getmax() []byte {
	return a.max
}

func (l *Leaf3) nvalue(t *Avl3) []byte {
	if l.valueId == 0 {
		return l.value
	} else if t.valueFile == nil {
		return t.valueMap[l.valueId]
	} else {
		val := make([]byte, l.valueLen)
		if _, err := t.valueFile.ReadAt(val, int64(l.valueId)-1); err != nil {
			panic(err)
		}
		return val
	}
}

func (t *Avl3) Get(key []byte) ([]byte, bool) {
	trace := t.trace
	var current Ref3 = t.root
	for {
		switch n := current.(type) {
		case nil:
			return nil, false
		case *Leaf3:
			if trace {
				fmt.Printf("Get %s on leaf %s\n", key, n.key)
			}
			if bytes.Equal(key, n.key) {
				return n.nvalue(t), true
			}
			return nil, false
		case *Fork3:
			if trace {
				fmt.Printf("Get %s on fork %s %d\n", key, n.max, n.height)
			}
			switch t.compare(key, n.left.getmax()) {
			case 0, -1:
				if trace {
					fmt.Printf("Go left\n")
				}	
				current = n.left
			case 1:
				if trace {
					fmt.Printf("Go right\n")
				}
				current = n.right
			}
		case *Arrow3:
			current, _ = t.deserialisePage(n.pageId, n.max, n.height)
			if current == nil {
				panic("")
			}
		}
	}
}

func (t *Avl3) IsLeaf(r Ref3) bool {
	current := r
	for {
		switch r := current.(type) {
		case nil:
			panic("nil")
		case *Arrow3:
			current, _ = t.deserialisePage(r.pageId, r.max, r.height)
			if current == nil {
				panic("")
			}
		case *Fork3:
			return false
		case *Leaf3:
			return true
		}
	}
	panic("")
}

func (t *Avl3) Peel(r Ref3, key []byte, ins int) Ref3 {
	current := r
	for {
		switch r := current.(type) {
		case nil:
			panic("nil")
		case *Arrow3:
			point, releaseId := t.deserialisePage(r.pageId, r.max, r.height)
			if point == nil {
				panic("")
			}
			if releaseId {
				if t.trace {
					fmt.Printf("Peel releases page %d\n", r.pageId)
				}
				t.pagesToRecycle[r.pageId] = struct{}{}
			}
			if r.arrow != nil {
				if t.trace {
					fmt.Printf("Moving arrow P.%d[%s %d] over the arrow P.%d[%s %d]\n", r.arrow.pageId, r.arrow.max, r.arrow.height, r.pageId, r.max, r.height)
				}
				r.arrow.pageId = r.pageId
				switch n := point.(type) {
				case *Leaf3:
					n.arrow = r.arrow
				case *Fork3:
					n.arrow = r.arrow
				case *Arrow3:
					n.arrow = r.arrow
				}
				r.arrow = nil
			}
			current = point
		case *Fork3:
			return r
		case *Leaf3:
			return r
		}
	}
	panic("")
}

func (t *Avl3) moveArrowOverFork(a *Arrow3, f *Fork3) {
	if t.trace {
		fmt.Printf("Moving arrow P.%d[%s %d] over the fork %s\n", a.pageId, a.max, a.height, f.nkey())
	}
	var lArrow *Arrow3 = &Arrow3{pageId: a.pageId, parentC: -1, height: f.left.getheight(), max: f.left.getmax()}
	var rArrow *Arrow3 = &Arrow3{pageId: a.pageId, parentC: 1, height: f.right.getheight(), max: f.right.getmax()}
	if t.trace {
		fmt.Printf("Left arrow P.%d[%s %d], right arrow P.%d[%s %d]\n", lArrow.pageId, lArrow.max, lArrow.height, rArrow.pageId, rArrow.max, rArrow.height)
	}
	fork := &Fork3{max: f.max, height: f.height, left: lArrow, right: rArrow}
	lArrow.parent = fork
	rArrow.parent = fork
	f.arrow = nil
	switch n := f.left.(type) {
	case *Fork3:
		n.arrow = lArrow
	case *Leaf3:
		n.arrow = lArrow
	case *Arrow3:
		n.arrow = lArrow
		lArrow.pageId = n.pageId
	}
	switch n := f.right.(type) {
	case *Fork3:
		n.arrow = rArrow
	case *Leaf3:
		n.arrow = rArrow
	case *Arrow3:
		n.arrow = rArrow
		rArrow.pageId = n.pageId
	}
	if a.parent == nil {
		t.prevRoot = fork
	} else {
		switch a.parentC {
		case -1:
			a.parent.left = fork
		case 1:
			a.parent.right = fork
		}
	}
	// When the pinned node gets modified, the arrow pointing to it moves over the pinned node.
	// That causes the pinned node to get unpinned in the current state, but pinned in the previous
	// state.
	if f.pinnedPageId != PageID(0) {
		fork.pinnedPageId = f.pinnedPageId
		f.pinnedPageId = PageID(0)
	}
}

func (t *Avl3) moveArrowOverLeaf(a *Arrow3, l *Leaf3) {
	if t.trace {
		fmt.Printf("Moving arrow P.%d[%s %d] over the leaf %s\n", a.pageId, a.max, a.height, l.nkey())
	}
	leaf := &Leaf3{key: l.key, value: l.nvalue(t)}
	leaf.valueLen = uint32(len(leaf.value))
	if a.parent == nil {
		t.prevRoot = leaf
	} else {
		switch a.parentC {
		case -1:
			a.parent.left = leaf
		case 1:
			a.parent.right = leaf
		}		
	}
	l.arrow = nil
}

func (t *Avl3) Insert(key, value []byte) bool {
	inserted := true
	var current Ref3 = t.root
	loop: for {
		switch n := current.(type) {
		case nil:
			break loop
		case *Leaf3:
			if bytes.Equal(key, n.key) {
				if bytes.Equal(value, n.nvalue(t)) {
					return false
				} else {
					inserted = false
				}
			}
			break loop
		case *Fork3:
			switch t.compare(key, n.left.getmax()) {
			case 0, -1:
				current = n.left
			case 1:
				current = n.right
			}
		case *Arrow3:
			current, _ = t.deserialisePage(n.pageId, n.max, n.height)
			if current == nil {
				panic("")
			}
		}
	}
	t.root = t.insert(t.root, key, value)
	return inserted
}

func (t *Avl3) insert(current Ref3, key, value []byte) Ref3 {
	trace := t.trace
	switch n := current.(type) {
	case nil:
		if trace {
			fmt.Printf("Inserting %s, on nil\n", key)
		}
		return &Leaf3{key: key, value: value, valueLen: uint32(len(value))}
	case *Arrow3:
		return t.insert(t.Peel(n, key, 1), key, value)
	case *Leaf3:
		if trace {
			fmt.Printf("Inserting %s, on Leaf %s\n", key, n.key)
		}
		var newnode *Fork3
		switch t.compare(key, n.key) {
		case 0:
			if n.arrow != nil {
				t.moveArrowOverLeaf(n.arrow, n)
			}
			n.value = value
			t.freeValueId(n.valueId)
			n.valueId = 0
			n.valueLen = uint32(len(value))
			return n
		case -1:
			newnode = &Fork3{max: n.key, height: 2, left: &Leaf3{key: key, value: value, valueLen: uint32(len(value))}, right: n}
		case 1:
			newnode = &Fork3{max: key, height: 2, left: n, right: &Leaf3{key: key, value: value, valueLen: uint32(len(value))}}
		}
		return newnode
	case *Fork3:
		c := t.compare(key, n.left.getmax())
		if trace {
			fmt.Printf("Inserting %s, on node %s, height %d\n", key, n.max, n.height)
		}
		if n.arrow != nil {
			t.moveArrowOverFork(n.arrow, n)
		}
		// Prepare it for the next iteraion
		switch c {
		case 0, -1:
			n.left = t.insert(n.left, key, value)
		case 1:
			n.right = t.insert(n.right, key, value)
			n.max = n.right.getmax()
		}
		lHeight := n.left.getheight()
		rHeight := n.right.getheight()
		n.height = 1 + maxu32(lHeight, rHeight)
		if rHeight > lHeight {
			if rHeight - lHeight > 1 {
				// nr is a Fork, because its height is at least 3
				nr := t.Peel(n.right, key, 2).(*Fork3)
				if nr.arrow != nil {
					t.moveArrowOverFork(nr.arrow, nr)
				}
				if nr.right.getheight() >= nr.left.getheight() {
					if trace {
						fmt.Printf("Single rotation from right to left, n %s %d, nr %s %d\n",
							n.nkey(), n.getheight(), nr.nkey(), nr.getheight())
					}
					n.right = nr.left
					n.height = 1 + maxu32(n.left.getheight(), n.right.getheight())
					n.max = nr.left.getmax()
					nr.left = n
					nr.height = 1 + maxu32(nr.left.getheight(), nr.right.getheight())
					if trace {
						fmt.Printf("n %s %d, nr %s %d, nrl %s %d\n",
							n.nkey(), n.getheight(), nr.nkey(), nr.getheight(), nr.left.nkey(), nr.left.getheight())
					}
					return nr
				} else {
					if trace {
						fmt.Printf("Double rotation from right to left, n %s %d, nr %s %d, nrl %s %d\n",
							n.nkey(), n.getheight(), nr.nkey(), nr.getheight(), nr.left.nkey(), nr.left.getheight())
					}
					// height of nrl is more than height of nrr. nr has height of at least 3, therefore nrl has a height of at least 2
					// nrl is a Fork
					nrl := t.Peel(nr.left, key, 3).(*Fork3)
					if nrl.arrow != nil {
						t.moveArrowOverFork(nrl.arrow, nrl)
					}
					n.right = nrl.left
					n.height = 1 + maxu32(n.left.getheight(), n.right.getheight())
					n.max = nrl.left.getmax()
					nrl.left = n
					nr.left = nrl.right
					nr.height = 1 + maxu32(nr.left.getheight(), nr.right.getheight())
					nrl.right = nr
					nrl.height = 1 + maxu32(nrl.left.getheight(), nrl.right.getheight())
					nrl.max = nr.max
					return nrl
				}
			} else {
				return n
			}
		} else if lHeight - rHeight > 1 {
			nl := t.Peel(n.left, key, 4).(*Fork3)
			if nl.arrow != nil {
				t.moveArrowOverFork(nl.arrow, nl)
			}
			if nl.left.getheight() >= nl.right.getheight() {
				if trace {
					fmt.Printf("Single rotation from left to right, n %s %d, nl %s %d\n",
						n.nkey(), n.getheight(), nl.nkey(), nl.getheight())
				}
				n.left = nl.right
				n.height = 1 + maxu32(n.left.getheight(), n.right.getheight())
				nl.right = n
				nl.height = 1 + maxu32(nl.left.getheight(), nl.right.getheight())
				nl.max = n.max
				if t.compare(key, nl.max) == 1 {
					nl.max = key
				}
				return nl
			} else {
				if trace {
					fmt.Printf("Double rotation from left to right, n %s %d, nl %s %d, nlr %s %d\n",
						n.nkey(), n.getheight(), nl.nkey(), nl.getheight(), nl.right.nkey(), nl.right.getheight())
				}
				nlr := t.Peel(nl.right, key, 5).(*Fork3)
				if nlr.arrow != nil {
					t.moveArrowOverFork(nlr.arrow, nlr)
				}
				n.left = nlr.right
				n.height = 1 + maxu32(n.left.getheight(), n.right.getheight())
				nlr.right = n
				nlr.max = n.max
				nl.right = nlr.left
				nl.height = 1 + maxu32(nl.left.getheight(), nl.right.getheight())
				nl.max = nlr.left.getmax()
				nlr.left = nl
				nlr.height = 1 + maxu32(nlr.left.getheight(), nlr.right.getheight())
				return nlr
			}
		} else {
			return n
		}
	}
	panic("")
}

func (t *Avl3) Delete(key []byte) bool {
	var current Ref3 = t.root
	loop: for {
		switch n := current.(type) {
		case nil:
			return false
		case *Leaf3:
			if bytes.Equal(key, n.key) {
				break loop
			} else {
				return false
			}
		case *Fork3:
			switch t.compare(key, n.left.getmax()) {
			case 0, -1:
				current = n.left
			case 1:
				current = n.right
			}
		case *Arrow3:
			current, _ = t.deserialisePage(n.pageId, n.max, n.height)
			if current == nil {
				panic("")
			}
			if current.getheight() != n.height {
				panic(fmt.Sprintf("deseailised size %d, arrow height %d", current.getheight(), n.height))
			}
		}
	}
	t.root = t.delete(t.root, key)
	return true
}

func (t *Avl3) delete(current Ref3, key []byte) Ref3 {
	trace := t.trace
	switch n := current.(type) {
	case nil:
		panic("nil")
	case *Arrow3:
		return t.delete(t.Peel(n, key, 6), key)
	case *Leaf3:
		// Assuming that key is equal to n.key
		if trace {
			fmt.Printf("Deleting on leaf %s\n", n.nkey())
		}
		if n.arrow == nil {
			// Don't release the value because it is still used by the previous version
			t.freeValueId(n.valueId)
		}
		return nil
	case *Fork3:
		c := t.compare(key, n.left.getmax())
		if n.arrow != nil {
			t.moveArrowOverFork(n.arrow, n)
		}
		// Special cases when both right and left are leaves
		if t.IsLeaf(n.left) && t.IsLeaf(n.right) {
			nl := t.Peel(n.left, key, 7).(*Leaf3)
			nr := t.Peel(n.right, key, 8).(*Leaf3)
			if trace {
				fmt.Printf("Special case: Leaf, Leaf\n")
			}
			switch c {
			case 0, -1:
				if nl.arrow == nil {
					t.freeValueId(nl.valueId)
				}
				return nr
			case 1:
				if nr.arrow == nil {
					t.freeValueId(nr.valueId)
				}
				return nl
			}
			panic("")
		}
		if trace {
			fmt.Printf("Deleting %s, on node %s, height %d\n", key, n.max, n.height)
		}
		switch c {
		case 0, -1:
			n.left = t.delete(n.left, key)
			if n.left == nil {
				return n.right
			}
		case 1:
			n.right = t.delete(n.right, key)
			if n.right == nil {
				return n.left
			}
			n.max = n.right.getmax()
		}
		lHeight := n.left.getheight()
		rHeight := n.right.getheight()
		n.height = 1 + maxu32(lHeight, rHeight)
		if rHeight > lHeight {
			if rHeight - lHeight > 1 {
				// nr is a Fork, because its height is at least 3
				nr := t.Peel(n.right, key, 9).(*Fork3)
				if nr.arrow != nil {
					t.moveArrowOverFork(nr.arrow, nr)
				}
				if nr.right.getheight() >= nr.left.getheight() {
					if trace {
						fmt.Printf("Single rotation from right to left, n %s, nr %s\n",
							n.nkey(), nr.nkey())
					}
					n.right = nr.left
					n.height = 1 + maxu32(n.left.getheight(), n.right.getheight())
					n.max = nr.left.getmax()
					nr.left = n
					nr.height = 1 + maxu32(nr.left.getheight(), n.right.getheight())
					return nr
				} else {
					if trace {
						fmt.Printf("Double rotation from right to left, n %s, nr %s\n",
							n.nkey(), nr.nkey())
					}
					// height of nrl is more than height of nrr. nr has height of at least 3, therefore nrl has a height of at least 2
					// nrl is a Fork
					nrl := t.Peel(nr.left, key, 10).(*Fork3)
					if nrl.arrow != nil {
						t.moveArrowOverFork(nrl.arrow, nrl)
					}
					n.right = nrl.left
					n.height = 1 + maxu32(n.left.getheight(), n.right.getheight())
					n.max = nrl.left.getmax()
					nrl.left = n
					nr.left = nrl.right
					nr.height = 1 + maxu32(nr.left.getheight(), nr.right.getheight())
					nrl.right = nr
					nrl.height = 1 + maxu32(nrl.left.getheight(), nrl.right.getheight())
					nrl.max = nr.max
					return nrl
				}
			} else {
				return n
			}
		} else if lHeight - rHeight > 1 {
			nl := t.Peel(n.left, key, 11).(*Fork3)
			if nl.arrow != nil {
				t.moveArrowOverFork(nl.arrow, nl)
			}
			if nl.left.getheight() >= nl.right.getheight() {
				if trace {
					fmt.Printf("Single rotation from left to right, n %s, nl %s\n",
						n.nkey(), nl.nkey())
				}
				n.left = nl.right
				n.height = 1 + maxu32(n.left.getheight(), n.right.getheight())
				nl.right = n
				nl.height = 1 + maxu32(nl.left.getheight(), nl.right.getheight())
				nl.max = n.max
				return nl
			} else {
				if trace {
					fmt.Printf("Double rotation from left to right, n %s, nl %s, nlr %s\n",
						n.nkey(), nl.nkey(), nl.right.nkey())
				}
				nlr := t.Peel(nl.right, key, 12).(*Fork3)
				if nlr.arrow != nil {
					t.moveArrowOverFork(nlr.arrow, nlr)
				}
				n.left = nlr.right
				n.height = 1 + maxu32(n.left.getheight(), n.right.getheight())
				nlr.right = n
				nlr.max = n.max
				nl.right = nlr.left
				nl.height = 1 + maxu32(nl.left.getheight(), nl.right.getheight())
				nl.max = nlr.left.getmax()
				nlr.left = nl
				nlr.height = 1 + maxu32(nlr.left.getheight(), nlr.right.getheight())
				return nlr
			}
		} else {
			return n
		}
	}
	panic("")
}

func (t *Avl3) Dot() *graphContext {
	fmt.Printf("Dotting the root\n")
	ctx := &graphContext{}
	gn := &graphNode{
		Attrs: map[string]string{},
		Path: "root0",
	}
	for k, v := range defaultGraphNodeAttrs {
		gn.Attrs[k] = v
	}
	gn.Label = mkLabel("root", 16, "sans-serif")
	ctx.Nodes = append(ctx.Nodes, gn)
	t.root.dot(t, ctx, "root")
	ctx.Edges = append(ctx.Edges, &graphEdge{
		From: "root0",
		To: "root",
	})
	if t.prevRoot != nil {
		fmt.Printf("Dotting the prevRoot\n")
		gn := &graphNode{
			Attrs: map[string]string{},
			Path: "prevRoot0",
		}
		for k, v := range defaultGraphNodeAttrs {
			gn.Attrs[k] = v
		}
		gn.Label = mkLabel("prev", 16, "sans-serif")
		ctx.Nodes = append(ctx.Nodes, gn)
		t.prevRoot.dot(t, ctx, "prevRoot")
		ctx.Edges = append(ctx.Edges, &graphEdge{
			From: "prevRoot0",
			To: "prevRoot",
		})
	}
	return ctx
}

func (a *Arrow3) dot(t *Avl3, ctx *graphContext, path string) {
	fmt.Printf("Dotting the arrow P.%d max %s, height %d\n", a.pageId, a.max, a.height)
	gn := &graphNode{
		Attrs: map[string]string{},
		Path: path,
	}
	for k, v := range defaultGraphNodeAttrs {
		gn.Attrs[k] = v
	}
	gn.Label = mkLabel(fmt.Sprintf("P.%d", a.pageId), 16, "sans-serif")
	gn.Label += mkLabel(string(a.max), 16, "sans-serif")
	gn.Label += mkLabel(fmt.Sprintf("%d", a.height), 10, "sans-serif")
	ctx.Nodes = append(ctx.Nodes, gn)

	if a.pageId != PageID(0) {
		point, _ := t.deserialisePage(a.pageId, a.max, a.height)
		if point == nil {
			panic("")
		} else {
			pointPath := fmt.Sprintf("%p", point)
			if point != nil {
				point.dot(t, ctx, pointPath)
			}
			ctx.Edges = append(ctx.Edges, &graphEdge{
				From: path,
				To: pointPath,
			})
		}
	}
}

func (l *Leaf3) dot(t *Avl3, ctx *graphContext, path string) {
	gn := &graphNode{
		Attrs: map[string]string{},
		Path: path,
	}
	for k, v := range defaultGraphNodeAttrs {
		gn.Attrs[k] = v
	}
	gn.Label = mkLabel(string(l.nkey()), 16, "sans-serif")
	gn.Label += mkLabel(string(l.nvalue(t)), 10, "sans-serif")
	if l.pinnedPageId != PageID(0) {
		gn.Label += mkLabel(fmt.Sprintf("* %d", l.pinnedPageId), 10, "sans-serif")
	} else if l.arrow != nil {
		gn.Label += mkLabel("*", 10, "sans-serif")
	}
	ctx.Nodes = append(ctx.Nodes, gn)
}

func (f *Fork3) dot(t *Avl3, ctx *graphContext, path string) {
	gn := &graphNode{
		Attrs: map[string]string{},
		Path: path,
	}
	for k, v := range defaultGraphNodeAttrs {
		gn.Attrs[k] = v
	}
	gn.Label = mkLabel(string(f.max), 16, "sans-serif")
	gn.Label += mkLabel(fmt.Sprintf("%d", f.height), 10, "sans-serif")
	if f.pinnedPageId != PageID(0) {
		gn.Label += mkLabel(fmt.Sprintf("* %d", f.pinnedPageId), 10, "sans-serif")
	} else if f.arrow != nil {
		gn.Label += mkLabel("*", 10, "sans-serif")
	}
	ctx.Nodes = append(ctx.Nodes, gn)
	leftPath := fmt.Sprintf("%p", f.left)
	f.left.dot(t, ctx, leftPath)
	ctx.Edges = append(ctx.Edges, &graphEdge{
		From: path,
		To: leftPath,
	})
	rightPath := fmt.Sprintf("%p", f.right)
	f.right.dot(t, ctx, rightPath)
	ctx.Edges = append(ctx.Edges, &graphEdge{
		From: path,
		To: rightPath,
	})
}

func (l *Leaf3) heightsCorrect(path string) (uint32, bool) {
	return 1, true
}

func (f *Fork3) heightsCorrect(path string) (uint32, bool) {
	leftHeight, leftCorrect := f.left.heightsCorrect(path + "l")
	rightHeight, rightCorrect := f.right.heightsCorrect(path + "r")
	height, correct := 1+maxu32(leftHeight, rightHeight), leftCorrect&&rightCorrect&&(1+maxu32(leftHeight, rightHeight) == f.height)
	if !correct {
		fmt.Printf("At path %s, key %s, expected %d, got %d\n", path, f.max, height, f.height)
	}
	return height, correct
}

func (l *Leaf3) balanceCorrect() bool {
	return true
}

func (f *Fork3) balanceCorrect() bool {
	lHeight := f.left.getheight()
	rHeight := f.right.getheight()
	var balanced bool
	if rHeight >= lHeight {
		balanced = (rHeight - lHeight) < 2
	} else {
		balanced = (lHeight - rHeight) < 2
	}
	return balanced && f.left.balanceCorrect() && f.right.balanceCorrect()
}

func (a *Arrow3) heightsCorrect(path string) (uint32, bool) {
	return a.height, true
}

func (a *Arrow3) balanceCorrect() bool {
	return true
}

func (t *Avl3) pageSize(keyCount, pageCount, keyBodySize, valBodySize, structBits uint32, prefix []byte) uint32 {
	nodeCount := keyCount + pageCount
	prefixLen := uint32(len(prefix))

	return 4 /* nodeCount */ +
		4*((nodeCount+31)/32) /* pageBits */ +
		4 /* prefixOffset */ +
		4*nodeCount + 4 /* key header */ +
		(12+t.hashLength)*pageCount /* arrow header */ +
		4*keyCount /* value header */ +
		4*((structBits+31)/32) /* structBits */ +
		prefixLen +
		keyBodySize - nodeCount*prefixLen /* Discount bodySize using prefixLen */ +
		valBodySize
}

// Split current buffer into pages and commit them
func (t *Avl3) Commit() uint64 {
	if t.root == nil {
		return 0
	}
	startCounter := t.commitedCounter
	var mpid PageID = PageID(0)
	prefix, keyCount, pageCount, keyBodySize, valBodySize, structBits, pinnedPageId := t.root.serialisePass1(t, &mpid)
	currentId := t.commitPage(t.root, prefix, keyCount, pageCount, keyBodySize, valBodySize, structBits, pinnedPageId)
	if t.prevRoot != nil {
		var mpid PageID = t.maxPageId
		prefix, keyCount, pageCount, keyBodySize, valBodySize, structBits, pinnedPageId := t.prevRoot.serialisePass1(t, &mpid)
		prevId := t.commitPage(t.prevRoot, prefix, keyCount, pageCount, keyBodySize, valBodySize, structBits, pinnedPageId)
		t.versions[t.currentVersion] = prevId
		if t.verFile != nil {
			var verdata [8]byte
			binary.BigEndian.PutUint64(verdata[:], uint64(prevId))
			t.verFile.WriteAt(verdata[:], int64(t.currentVersion)*int64(8))
		}
	}
	for pageId := range t.pagesToRecycle {
		t.freePageId(pageId)
	}
	t.pagesToRecycle = make(map[PageID]struct{})
	t.currentVersion++
	t.versions[t.currentVersion] = currentId
	if t.verFile != nil {
		var verdata [8]byte
		binary.BigEndian.PutUint64(verdata[:], uint64(currentId))
		t.verFile.WriteAt(verdata[:], int64(t.currentVersion)*int64(8))
	}
	prevRootArrow := &Arrow3{pageId: currentId, height: t.root.getheight(), max: t.root.getmax()}
	t.root = &Arrow3{pageId: currentId, height: t.root.getheight(), max: t.root.getmax(), arrow: prevRootArrow}
	t.prevRoot = prevRootArrow
	return t.commitedCounter - startCounter
}

func (t *Avl3) commitPage(r Ref3, prefix []byte, keyCount, pageCount, keyBodySize, valBodySize, structBits uint32, pinnedPageId PinnedPageID) PageID {
	trace := t.trace
	if trace {
		fmt.Printf("commitPage %s\n", r.getmax())
	}
	nodeCount := keyCount + pageCount
	size := t.pageSize(keyCount, pageCount, keyBodySize, valBodySize, structBits, prefix)
	data := make([]byte, size)
	offset := uint32(0)
	binary.BigEndian.PutUint32(data[offset:], nodeCount)
	offset += 4
	pageBitsOffset := offset
	offset += 4*((nodeCount+31)/32)
	prefixOffsetOffset := offset
	offset += 4
	keyHeaderOffset := offset
	offset += 4*nodeCount /* key offset per node */ + 4 /* end key offset */
	arrowHeaderOffset := offset
	offset += (12+t.hashLength)*pageCount /* (pageId, size - minSize, hash) per arrow */
	valueHeaderOffset := offset
	offset += 4*keyCount /* value length per leaf */
	structBitsOffset := offset
	if trace {
		//fmt.Printf("StructBitsOffset %d\n", structBitsOffset)
	}
	offset += 4*((structBits+31)/32) // Structure encoding
	// key prefix begins here
	binary.BigEndian.PutUint32(data[prefixOffsetOffset:], uint32(offset))
	copy(data[offset:], prefix)
	offset += uint32(len(prefix))
	keyBodyOffset := offset
	offset += keyBodySize - nodeCount*uint32(len(prefix))
	valBodyOffset := offset

	var nodeIndex uint32
	var structBit uint32
	if trace {
		//fmt.Printf("valueHeaderOffset %d\n", valueHeaderOffset)
	}
	if trace {
		//fmt.Printf("valBodyOffset %d\n", valBodyOffset)
	}
	var id PageID
	if pinnedPageId.pinned {
		id = pinnedPageId.id
	} else {
		id = t.nextPageId()
	}
	t.serialisePass2(r, id, data, len(prefix), false, pageBitsOffset, structBitsOffset,
		&nodeIndex, &structBit, &keyHeaderOffset, &arrowHeaderOffset, &valueHeaderOffset, &keyBodyOffset, &valBodyOffset)
	// end key offset
	if trace {
		fmt.Printf("valBodyOffset %d\n", keyBodyOffset)
	}
	binary.BigEndian.PutUint32(data[keyHeaderOffset:], keyBodyOffset)
	if valBodyOffset != size {
		panic(fmt.Sprintf("valBodyOffset %d (%d) != size %d, valBodySize %d, nodeCount %d, len(prefix): %d",
			valBodyOffset, offset, size, valBodySize, nodeCount, len(prefix)))
	}
	if nodeIndex != nodeCount {
		panic("n != nodeCount")
	}
	if structBit != structBits {
		panic("sb != structBits")
	}
	if t.trace {
		fmt.Printf("Committed page %d, nodeCount %d, prefix %s\n", id, nodeCount, prefix)
	}
	if t.pageFile != nil {
		t.pageFile.WriteAt(data, int64(id)*int64(PageSize))
	} else {
		t.pageMap[id] = data
	}
	t.commitedCounter++
	t.pageSpace += uint64(size)
	return id
}

// Computes all the dynamic parameters that allow calculation of the page length and pre-allocation of all buffers
func (l *Leaf3) serialisePass1(t *Avl3, maxPageId *PageID) (prefix []byte, keyCount, pageCount, keyBodySize, valBodySize, structBits uint32, pinnedPageId PinnedPageID) {
	keyCount = 1
	keyBodySize = uint32(len(l.key))
	if l.valueLen > InlineValueMax {
		valBodySize = 8 + HashLength // Size of value id + valueHash
	} else {
		valBodySize = l.valueLen
	}
	structBits = 1 // one bit per leaf
	prefix = l.key
	if l.pinnedPageId != PageID(0) {
		// Old pin
		pinnedPageId = PinnedPageID{id: l.pinnedPageId, pinned: true}
	} else if l.arrow != nil {
		// New pin
		*maxPageId++
		pinnedPageId = PinnedPageID{id: *maxPageId, pinned: false}
	}
	return
}

func (f *Fork3) serialisePass1(t *Avl3, maxPageId *PageID) (prefix []byte, keyCount, pageCount, keyBodySize, valBodySize, structBits uint32, pinnedPageId PinnedPageID) {
	if f.pinnedPageId != PageID(0) {
		// Old pin
		pinnedPageId = PinnedPageID{id: f.pinnedPageId, pinned: true}
	} else if f.arrow != nil {
		// New pin
		*maxPageId++
		pinnedPageId = PinnedPageID{id: *maxPageId, pinned: false}
	}

	prefixL, keyCountL, pageCountL, keyBodySizeL, valBodySizeL, structBitsL, pinnedPageL := f.left.serialisePass1(t, maxPageId)
	prefixR, keyCountR, pageCountR, keyBodySizeR, valBodySizeR, structBitsR, pinnedPageR := f.right.serialisePass1(t, maxPageId)
	// Fork and both children fit in the page
	var mergable3 bool
	var mergePin3 PinnedPageID
	if pinnedPageL.id == 0 && pinnedPageR.id == 0 {
		mergable3 = true
		mergePin3 = pinnedPageId
	} else if pinnedPageL.id == 0 && pinnedPageId.id == 0 {
		mergable3 = true
		mergePin3 = pinnedPageR
	} else if pinnedPageR.id == 0 && pinnedPageId.id == 0 {
		mergable3 = true
		mergePin3 = pinnedPageL
	} else if pinnedPageL.id == pinnedPageR.id && pinnedPageL.pinned == pinnedPageR.pinned && pinnedPageR.id == pinnedPageId.id && pinnedPageR.pinned == pinnedPageId.pinned {
		mergable3 = true
		mergePin3 = pinnedPageId
	}
	if mergable3 {
		keyCountLFR := keyCountL+keyCountR
		pageCountLFR := pageCountL+pageCountR
		keyBodySizeLFR := keyBodySizeL+keyBodySizeR
		valBodySizeLFR := valBodySizeL+valBodySizeR
		structBitsLFR := structBitsL+structBitsR+2 // 2 bits for the fork
		prefixLFR := commonPrefix(prefixL, prefixR)
		sizeLFR := t.pageSize(keyCountLFR, pageCountLFR, keyBodySizeLFR, valBodySizeLFR, structBitsLFR, prefixLFR)
		if sizeLFR < PageSize {
			return prefixLFR, keyCountLFR, pageCountLFR, keyBodySizeLFR, valBodySizeLFR, structBitsLFR, mergePin3
		}
	}
	// Choose the biggest child and make a page out of it
	sizeL := t.pageSize(keyCountL, pageCountL, keyBodySizeL, valBodySizeL, structBitsL, prefixL)
	sizeR := t.pageSize(keyCountR, pageCountR, keyBodySizeR, valBodySizeR, structBitsR, prefixR)
	var mergableR bool
	var mergePinR PinnedPageID
	if pinnedPageR.id == 0 {
		mergableR = true
		mergePinR = pinnedPageId
	} else if pinnedPageId.id == 0 {
		mergableR = true
		mergePinR = pinnedPageR
	} else if pinnedPageR.id == pinnedPageId.id && pinnedPageR.pinned == pinnedPageId.pinned {
		mergableR = true
		mergePinR = pinnedPageId
	}
	var mergableL bool
	var mergePinL PinnedPageID
	if pinnedPageL.id == 0 {
		mergableL = true
		mergePinL = pinnedPageId
	} else if pinnedPageId.id == 0 {
		mergableL = true
		mergePinL = pinnedPageL
	} else if pinnedPageL.id == pinnedPageId.id && pinnedPageL.pinned == pinnedPageId.pinned {
		mergableL = true
		mergePinL = pinnedPageId
	}
	if sizeL > sizeR {
		var lArrow *Arrow3
		if la, ok := f.left.(*Arrow3); ok {
			lArrow = la
		} else {
			lid := t.commitPage(f.left, prefixL, keyCountL, pageCountL, keyBodySizeL, valBodySizeL, structBitsL, pinnedPageL)
			lArrow = &Arrow3{pageId: lid, height: f.left.getheight(), max: f.left.getmax()}
			f.left = lArrow
		}
		// Check if the fork and the right child still fit into a page
		pageCountFR := pageCountR+1 // 1 for the left arrow
		keyBodySizeFR := keyBodySizeR+uint32(len(lArrow.max))
		structBitsFR := structBitsR+3 // 2 for the fork and 1 for the left arrow
		prefixFR := commonPrefix(prefixR, lArrow.max)
		sizeFR := t.pageSize(keyCountR, pageCountFR, keyBodySizeFR, valBodySizeR, structBitsFR, prefixFR)
		if mergableR && sizeFR < PageSize {
			return prefixFR, keyCountR, pageCountFR, keyBodySizeFR, valBodySizeR, structBitsFR, mergePinR
		} else {
			// Have to commit right child too
			var rArrow *Arrow3
			if ra, ok := f.right.(*Arrow3); ok {
				rArrow = ra
			} else {
				rid := t.commitPage(f.right, prefixR, keyCountR, pageCountR, keyBodySizeR, valBodySizeR, structBitsR, pinnedPageR)
				rArrow = &Arrow3{pageId: rid, height: f.right.getheight(), max: f.right.getmax()}
				f.right = rArrow
			}
			return commonPrefix(rArrow.max, lArrow.max), 0, 2, uint32(len(lArrow.max))+uint32(len(rArrow.max)), 0, 4 /* 2 bits for arrows, 2 for the fork */, pinnedPageId
		}
	} else {
		var rArrow *Arrow3
		if ra, ok := f.right.(*Arrow3); ok {
			rArrow = ra
		} else {
			rid := t.commitPage(f.right, prefixR, keyCountR, pageCountR, keyBodySizeR, valBodySizeR, structBitsR, pinnedPageR)
			rArrow = &Arrow3{pageId: rid, height: f.right.getheight(), max: f.right.getmax()}
			f.right = rArrow
		}
		// Check if the fork and the let child still fit into a page
		pageCountFL := pageCountL+1 // 1 for the left arrow
		keyBodySizeFL := keyBodySizeL+uint32(len(rArrow.max))
		structBitsFL := structBitsL+3 // 2 for the fork and 1 for the left arrow
		prefixFL := commonPrefix(prefixL, rArrow.max)
		sizeFL := t.pageSize(keyCountL, pageCountFL, keyBodySizeFL, valBodySizeL, structBitsFL, prefixFL)
		if mergableL && sizeFL < PageSize {
			return prefixFL, keyCountL, pageCountFL, keyBodySizeFL, valBodySizeL, structBitsFL, mergePinL
		} else {
			// Have to commit left child too
			var lArrow *Arrow3
			if la, ok := f.left.(*Arrow3); ok {
				lArrow = la
			} else {
				lid := t.commitPage(f.left, prefixL, keyCountL, pageCountL, keyBodySizeL, valBodySizeL, structBitsL, pinnedPageL)
				lArrow = &Arrow3{pageId: lid, height: f.left.getheight(), max: f.left.getmax()}
				f.left = lArrow
			}
			return commonPrefix(rArrow.max, lArrow.max), 0, 2, uint32(len(lArrow.max))+uint32(len(rArrow.max)), 0, 4 /* 2 bits for arrows, 2 for the fork */, pinnedPageId
		}
	}
}

func (a *Arrow3) serialisePass1(t *Avl3, maxPageId *PageID) (prefix []byte, keyCount, pageCount, keyBodySize, valBodySize, structBits uint32, pinnedPageId PinnedPageID) {
	pageCount = 1
	structBits = 1 // one bit for page reference
	keyBodySize = uint32(len(a.max))
	prefix = a.max
	return
}

func (t *Avl3) serialiseKey(key, data []byte, keyHeaderOffset, keyBodyOffset *uint32) {
	binary.BigEndian.PutUint32(data[*keyHeaderOffset:], *keyBodyOffset)
	*keyHeaderOffset += 4
	copy(data[*keyBodyOffset:], key)
	*keyBodyOffset += uint32(len(key))
}

func (t *Avl3) serialiseVal(value []byte, valueId uint64, valueLen uint32, data []byte, valueHeaderOffset, valBodyOffset *uint32) uint64 {
	binary.BigEndian.PutUint32(data[*valueHeaderOffset:], valueLen)
	*valueHeaderOffset += 4
	if valueLen > InlineValueMax {
		var valueHash Hash
		if valueId == 0 {
			valueId = t.nextValueId()
			//valueHash = sha256.Sum256(value)
			//t.valueHashes[valueId] = valueHash
			t.addValue(valueId, value)
		} else {
			valueHash = t.valueHashes[valueId]
		}
		binary.BigEndian.PutUint64(data[*valBodyOffset:], valueId)
		*valBodyOffset += 8
		copy(data[*valBodyOffset:], valueHash[:])
		*valBodyOffset += HashLength
	} else {
		copy(data[*valBodyOffset:], value)
		*valBodyOffset += valueLen
	}
	return valueId
}

func (t *Avl3) serialisePass2(r Ref3, pageId PageID, data []byte, prefixLen int, subtreePinned bool, pageBitsOffset, structBitsOffset uint32,
	nodeIndex, structBit, keyHeaderOffset, arrowHeaderOffset, valueHeaderOffset, keyBodyOffset, valBodyOffset *uint32) {
	switch r := r.(type) {
	case *Leaf3:
		t.serialiseKey(r.key[prefixLen:], data, keyHeaderOffset, keyBodyOffset)
		r.valueId = t.serialiseVal(r.value, r.valueId, r.valueLen, data, valueHeaderOffset, valBodyOffset)
		// Update page bits
		*nodeIndex++
		// Update struct bits
		pinned := subtreePinned || r.pinnedPageId == pageId || r.arrow != nil
		if !pinned {
			// If the subtree containing this leaf of the leaf itself is pinned, we write "0" structural bit
			data[structBitsOffset+(*structBit>>3)] |= (uint8(1)<<(*structBit&7))
		} else {
			if t.trace {
				//fmt.Printf("Pinned leaf %s in page %d\n", r.key, pageId)
			}
		}
		*structBit++
		if r.arrow != nil {
			r.arrow.pageId = pageId
		}
	case *Fork3:
		pinned := subtreePinned || r.pinnedPageId == pageId || r.arrow != nil
		// Write opening parenthesis "0" (noop)
		t.serialisePass2(r.left, pageId, data, prefixLen, pinned, pageBitsOffset, structBitsOffset,
			nodeIndex, structBit, keyHeaderOffset, arrowHeaderOffset, valueHeaderOffset, keyBodyOffset, valBodyOffset)
		// Update struct bit
		*structBit++
		if r.arrow != nil {
			r.arrow.pageId = pageId
		}
		t.serialisePass2(r.right, pageId, data, prefixLen, pinned, pageBitsOffset, structBitsOffset,
			nodeIndex, structBit, keyHeaderOffset, arrowHeaderOffset, valueHeaderOffset, keyBodyOffset, valBodyOffset)
		// Write closing parenthesis "1"
		data[structBitsOffset+(*structBit>>3)] |= (uint8(1)<<(*structBit&7))
		*structBit++
	case *Arrow3:
		binary.BigEndian.PutUint64(data[*arrowHeaderOffset:], uint64(r.pageId))
		*arrowHeaderOffset += 8
		binary.BigEndian.PutUint32(data[*arrowHeaderOffset:], uint32(r.height))
		*arrowHeaderOffset += 4
		t.serialiseKey(r.max[prefixLen:], data, keyHeaderOffset, keyBodyOffset)
		// TODO: write page hash
		*arrowHeaderOffset += t.hashLength
		// Update page bit
		data[pageBitsOffset+(*nodeIndex>>3)] |= (uint8(1)<<(*nodeIndex&7))
		*nodeIndex++
		// Write closing parenthesis "1"
		if !subtreePinned {
			data[structBitsOffset+(*structBit>>3)] |= (uint8(1)<<(*structBit&7))
		} else {
			if t.trace {
				//fmt.Printf("Pinned arrow %s in page %d\n", r.max, pageId)
			}
		}
		*structBit++
	}
}

func (t *Avl3) deserialiseKey(data []byte, keyHeaderOffset *uint32, prefix []byte) []byte {
	keyStart := binary.BigEndian.Uint32(data[*keyHeaderOffset:])
	keyEnd := binary.BigEndian.Uint32(data[*keyHeaderOffset+4:]) // Start of the next key (or end offset)
	*keyHeaderOffset += 4
	return append(prefix, data[keyStart:keyEnd]...)
}

func (t *Avl3) deserialiseVal(data []byte, valueHeaderOffset, valBodyOffset *uint32) (value []byte, valueId uint64, valLen uint32) {
	valLen = binary.BigEndian.Uint32(data[*valueHeaderOffset:])
	*valueHeaderOffset += 4
	if valLen > InlineValueMax {
		valueId = binary.BigEndian.Uint64(data[*valBodyOffset:])
		*valBodyOffset += 8
		// Read the hash here
		*valBodyOffset += HashLength
	} else {
		value = make([]byte, valLen)
		copy(value, data[*valBodyOffset:])
		*valBodyOffset += valLen
	}
	return
}

func (t *Avl3) deserialisePage(pageId PageID, key []byte, height uint32) (point Ref3, releaseId bool) {
	trace := t.trace
	//if root, ok := t.pageCache.Get(pageId); ok {
	//	return root.(Ref3), false
	//}
	if trace {
		fmt.Printf("Deserialising page %d %s %d\n", pageId, key, height)
	}
	var data []byte
	if t.pageFile != nil {
		data = make([]byte, PageSize)
		if _, err := t.pageFile.ReadAt(data, int64(pageId)*int64(PageSize)); err != nil && err != io.EOF {
			panic(err)
		}
	} else {
		data = t.pageMap[pageId]
	}
	if data == nil {
		return nil, false
	}
	offset := uint32(0)
	// read node count
	nodeCount := binary.BigEndian.Uint32(data[offset:])
	if nodeCount == 0 {
		return nil, false
	}
	offset += 4
	pageBitsOffset := offset
	// Calculate number of pages
	var pageCount uint32
	pageBitsLen := 4*((nodeCount+31)/32)
	for i := uint32(0); i < pageBitsLen; i += 4 {
		pageCount += uint32(bits.OnesCount32(binary.BigEndian.Uint32(data[pageBitsOffset+i:])))
	}
	offset += pageBitsLen
	keyCount := nodeCount - pageCount
	prefixOffset := binary.BigEndian.Uint32(data[offset:])
	offset += 4
	keyHeaderOffset := offset
	prefixLen := binary.BigEndian.Uint32(data[keyHeaderOffset:]) - prefixOffset
	prefix := make([]byte, prefixLen, prefixLen) // To prevent this to be appended to
	copy(prefix, data[prefixOffset:])
	offset += 4*nodeCount
	valBodyOffset := binary.BigEndian.Uint32(data[offset:])
	offset += 4
	arrowHeaderOffset := offset
	offset += (12+t.hashLength)*pageCount
	valueHeaderOffset := offset
	offset += 4*keyCount
	structBitsOffset := offset
	if trace {
		//fmt.Printf("StructBitsOffset %d\n", structBitsOffset)
	}
	if trace {
		//fmt.Printf("valBodyOffset %d\n", valBodyOffset)
	}
	if trace {
		//fmt.Printf("valueHeaderOffset %d\n", valueHeaderOffset)
	}
	var forkStack []Ref3
	var pinnedStack []bool
	var forkStackTop int
	var nodeIndex uint32
	var structBit uint32
	var noLeaf bool
	releaseId = true
	var maxStructBits uint32 = (prefixOffset - structBitsOffset) << 3
	for nodeIndex < nodeCount || structBit < maxStructBits  {
		sbit := (data[structBitsOffset+(structBit>>3)] & (uint8(1)<<(structBit&7))) != 0
		if trace {
			//fmt.Printf("nodeIndex %d, nodeCount %d, structBit %d, maxStructBits %d, sbit %t, noLeaf %t\n", nodeIndex, nodeCount, structBit, maxStructBits, sbit, noLeaf)
		}
		if !sbit && nodeIndex == nodeCount {
			// We got to the padding "0" bits of the structure
			break
		}
		// Interpret the structural bit
		var r Ref3
		if noLeaf {
			if sbit {
				forkStackTop--
				x := forkStack[forkStackTop]
				xPinned := pinnedStack[forkStackTop]
				y := forkStack[forkStackTop-1].(*Fork3)
				yPinned := pinnedStack[forkStackTop-1]
				y.right = x
				y.height = maxu32(y.height, 1+x.getheight())
				y.max = x.getmax()
				if y.height == height && bytes.Equal(y.max, key) {
					point = y
				}
				if xPinned {
					if yPinned {
						// pinned flag of y stays the same
					} else {
						switch xt := x.(type) {
						case *Leaf3:
							xt.pinnedPageId = pageId
						case *Fork3:
							xt.pinnedPageId = pageId
						}
						// y stays unpinned
					}
				} else if yPinned {
					switch ylt := y.left.(type) {
					case *Leaf3:
						ylt.pinnedPageId = pageId
					case *Fork3:
						ylt.pinnedPageId = pageId
					}
					// unpin y
					pinnedStack[forkStackTop-1] = false
				}
				noLeaf = true
				if trace {
					//fmt.Printf("Fork finishing %s %d\n", y.max, y.height)
				}
			} else {
				x := forkStack[forkStackTop-1]
				y := &Fork3{left: x, height: 1+x.getheight()}
				forkStack[forkStackTop-1] = y
				// Pinned flags on top of the stack just transfers from x to y
				noLeaf = false
				if trace {
					//fmt.Printf("Fork starting %d\n", y.height)
				}
			}
		} else {
			isPage := (data[pageBitsOffset+(nodeIndex>>3)] & (uint8(1)<<(nodeIndex&7))) != 0
			if isPage {
				id := PageID(binary.BigEndian.Uint64(data[arrowHeaderOffset:]))
				arrowHeaderOffset += 8
				height := binary.BigEndian.Uint32(data[arrowHeaderOffset:])
				arrowHeaderOffset += 4
				// TODO read the page hash
				arrowHeaderOffset += t.hashLength
				max := t.deserialiseKey(data, &keyHeaderOffset, prefix)
				arrow := &Arrow3{pageId: id, height: height, max: max}
				r = arrow
				if trace {
					if !sbit {
						//fmt.Printf("Deserialised PIN arrow max %s, pageId %d\n", arrow.max, pageId)
					}
				}
			} else {
				l := &Leaf3{}
				l.key = t.deserialiseKey(data, &keyHeaderOffset, prefix)
				l.value, l.valueId, l.valueLen = t.deserialiseVal(data, &valueHeaderOffset, &valBodyOffset)
				if height == 1 && bytes.Equal(l.key, key) {
					point = l
				}
				if trace {
					if !sbit {
						//fmt.Printf("Deserialised PIN leaf key %s, pageId %d\n", l.key, pageId)
					}
				}
				r = l
			}
			nodeIndex++
			noLeaf = true
		}
		if r != nil {
			if !sbit {
				releaseId = false
			}
			// Push onto the stack
			if forkStackTop >= len(forkStack) {
				forkStack = append(forkStack, r)
				pinnedStack = append(pinnedStack, !sbit)
			} else {
				forkStack[forkStackTop] = r
				pinnedStack[forkStackTop] = !sbit
			}
			forkStackTop++
		}
		structBit++
	}
	for i := 0; i < forkStackTop; i++ {
		if pinnedStack[i] {
			switch rt := forkStack[i].(type) {
				case *Leaf3:
					rt.pinnedPageId = pageId
				case *Fork3:
					rt.pinnedPageId = pageId
			}
		}
	}
	if key == nil && height == 0 && point == nil {
		point = forkStack[0]
	}
	//t.pageCache.Add(pageId, root)
	return point, releaseId
}

// Checks whether WBT without pages is equivalent to one with pages
func equivalent33(t *Avl3, path string, r1 Ref3, r2 Ref3) bool {
	switch r2 := r2.(type) {
	case nil:
		if r1 != nil {
			fmt.Printf("At path %s, expected n1 nil, but it was %s\n", path, r1.nkey())
			return false
		}
		return true
	case *Leaf3:
		if l1, ok := r1.(*Leaf3); ok {
			if !bytes.Equal(l1.key, r2.key) {
				fmt.Printf("At path %s, l1.key %s, r2.key %s\n", path, l1.nkey(), r2.nkey())
				return false
			}
			if !bytes.Equal(l1.nvalue(t), r2.nvalue(t)) {
				fmt.Printf("At path %s, l1.value %s, r2.value %s\n", path, l1.nvalue(t), r2.nvalue(t))
				return false
			}
		} else {
			fmt.Printf("At path %s, expected leaf, got %T\n", path, r1)
			return false
		}
		return true
	case *Fork3:
		if t.trace {
			fmt.Printf("equivalent33 path %s, at fork %s, height %d\n", path, r2.max, r2.height)
		}
		if f1, ok := r1.(*Fork3); ok {
			if !bytes.Equal(f1.max, r2.max) {
				fmt.Printf("At path %s, f1.max %s, r2.max %s\n", path, f1.max, r2.max)
				return false
			}
			if f1.height != r2.height {
				fmt.Printf("At path %s, f1.height %d, r2.height %d\n", path, f1.height, r2.height)
				return false
			}
			eqL := equivalent33(t, path + "l", f1.left, r2.left)
			eqR := equivalent33(t, path + "r", f1.right, r2.right)
			return eqL && eqR
		}
	case *Arrow3:
		if t.trace {
			fmt.Printf("equivalent33 path %s, at arrow P.%d[%s], height %d\n", path, r2.pageId, r2.max, r2.height)
		}
		if !bytes.Equal(r1.getmax(), r2.max) {
			fmt.Printf("At path %s, r1.max %s, r2(arrow).max %s\n", path, r1.getmax(), r2.max)
			return false
		}
		if r1 != nil && r2 != nil && r1.getheight() != r2.height {
			fmt.Printf("At path %s, r1.height %d, r2(arrow).height %d\n", path, r1.getheight(), r2.height)
			return false
		}
		point, _ := t.deserialisePage(r2.pageId, r2.max, r2.height)
		if point == nil {
			panic("")
		}
		return equivalent33(t, path, r1, point)
	}
	return false
}

func (t *Avl3) PrintStats() {
	var totalValueLens uint64
	if t.valueFile == nil {
		for _, l := range t.valueLens {
			totalValueLens += uint64(l)
		}
	} else {
		totalValueLens = t.maxValueId
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("Total pages: %d, Mb %0.3f, page space: Mb %0.3f large vals: %d, Mb %0.3f, mem alloc %0.3fMb, sys %0.3fMb\n",
		t.maxPageId,
		float64(t.maxPageId)*float64(PageSize)/1024.0/1024.0,
		float64(t.pageSpace)/1024.0/1024.0,
		len(t.valueLens),
		float64(totalValueLens)/1024.0/1024.0,
		float64(m.Alloc)/1024.0/1024.0,
		float64(m.Alloc)/1024.0/1024.0,
		)
}