// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

//go:generate -command genxdr go run ../../Godeps/_workspace/src/github.com/calmh/xdr/cmd/genxdr/main.go
//go:generate genxdr -o leveldb_xdr.go leveldb.go

package db

import (
	"bytes"
	"fmt"
	"runtime"
	"sort"

	"github.com/syncthing/protocol"
	"github.com/syncthing/syncthing/internal/sync"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"github.com/syndtr/goleveldb/leveldb/util"
)

var (
	clockTick int64
	clockMut  = sync.NewMutex()
)

func clock(v int64) int64 {
	clockMut.Lock()
	defer clockMut.Unlock()
	if v > clockTick {
		clockTick = v + 1
	} else {
		clockTick++
	}
	return clockTick
}

const (
	KeyTypeDevice = iota
	KeyTypeGlobal
	KeyTypeBlock
	KeyTypeDeviceStatistic
	KeyTypeFolderStatistic
)

type fileVersion struct {
	version protocol.Vector
	device  []byte
}

type versionList struct {
	versions []fileVersion
}

func (l versionList) String() string {
	var b bytes.Buffer
	var id protocol.DeviceID
	b.WriteString("{")
	for i, v := range l.versions {
		if i > 0 {
			b.WriteString(", ")
		}
		copy(id[:], v.device)
		fmt.Fprintf(&b, "{%d, %v}", v.version, id)
	}
	b.WriteString("}")
	return b.String()
}

type fileList []protocol.FileInfo

func (l fileList) Len() int {
	return len(l)
}

func (l fileList) Swap(a, b int) {
	l[a], l[b] = l[b], l[a]
}

func (l fileList) Less(a, b int) bool {
	return l[a].Name < l[b].Name
}

type dbReader interface {
	Get([]byte, *opt.ReadOptions) ([]byte, error)
}

type dbWriter interface {
	Put([]byte, []byte)
	Delete([]byte)
}

// Flush batches to disk when they contain this many records.
const batchFlushSize = 64

// deviceKey returns a byte slice encoding the following information:
//	   keyTypeDevice (1 byte)
//	   folder (64 bytes)
//	   device (32 bytes)
//	   name (variable size)
func deviceKey(folder, device, file []byte) []byte {
	k := make([]byte, 1+64+32+len(file))
	k[0] = KeyTypeDevice
	if len(folder) > 64 {
		panic("folder name too long")
	}
	copy(k[1:], []byte(folder))
	copy(k[1+64:], device[:])
	copy(k[1+64+32:], []byte(file))
	return k
}

func deviceKeyName(key []byte) []byte {
	return key[1+64+32:]
}

func deviceKeyFolder(key []byte) []byte {
	folder := key[1 : 1+64]
	izero := bytes.IndexByte(folder, 0)
	if izero < 0 {
		return folder
	}
	return folder[:izero]
}

func deviceKeyDevice(key []byte) []byte {
	return key[1+64 : 1+64+32]
}

// globalKey returns a byte slice encoding the following information:
//	   keyTypeGlobal (1 byte)
//	   folder (64 bytes)
//	   name (variable size)
func globalKey(folder, file []byte) []byte {
	k := make([]byte, 1+64+len(file))
	k[0] = KeyTypeGlobal
	if len(folder) > 64 {
		panic("folder name too long")
	}
	copy(k[1:], []byte(folder))
	copy(k[1+64:], []byte(file))
	return k
}

func globalKeyName(key []byte) []byte {
	return key[1+64:]
}

func globalKeyFolder(key []byte) []byte {
	folder := key[1 : 1+64]
	izero := bytes.IndexByte(folder, 0)
	if izero < 0 {
		return folder
	}
	return folder[:izero]
}

type deletionHandler func(snap ldbSnapshot, folder, device, name []byte, dbi iterator.Iterator) int64

type ldb struct {
	files   *leveldb.DB
	globals *leveldb.DB
}

type ldbSnapshot struct {
	ldb
	filesSnap    *leveldb.Snapshot
	globalsSnap  *leveldb.Snapshot
	filesBatch   *leveldb.Batch
	globalsBatch *leveldb.Batch
}

func (s ldbSnapshot) commit() {
	if s.filesBatch.Len() > 0 {
		if debugSnap {
			l.Debugf("snapshot %p commit %d file writes", s, s.filesBatch.Len())
		}
		mustWrite(s.files, s.filesBatch, nil)
	}
	if s.globalsBatch.Len() > 0 {
		if debugSnap {
			l.Debugf("snapshot %p commit %d globals writes", s, s.globalsBatch.Len())
		}
		mustWrite(s.globals, s.globalsBatch, nil)
	}
	s.filesSnap.Release()
	s.globalsSnap.Release()
	if debugSnap {
		l.Debugf("snapshot %p committed/released", s, s.filesBatch.Len())
	}
}

func (s ldbSnapshot) periodicWrite() {
	if s.filesBatch.Len() > batchFlushSize {
		if debugSnap {
			l.Debugf("snapshot %p commit %d file writes", s, s.filesBatch.Len())
		}
		mustWrite(s.files, s.filesBatch, nil)
		s.filesBatch.Reset()
	}
	if s.globalsBatch.Len() > batchFlushSize {
		if debugSnap {
			l.Debugf("snapshot %p commit %d globals writes", s, s.globalsBatch.Len())
		}
		mustWrite(s.globals, s.globalsBatch, nil)
		s.globalsBatch.Reset()
	}
}

func newLDB(db *leveldb.DB) ldb {
	return ldb{
		files:   db,
		globals: db,
	}
}

func newMemDB() *leveldb.DB {
	db, err := leveldb.Open(storage.NewMemStorage(), nil)
	if err != nil {
		panic(err)
	}
	return db
}

func (db ldb) snapshot() ldbSnapshot {
	s := ldbSnapshot{
		ldb:          db,
		filesSnap:    mustSnapshot(db.files),
		globalsSnap:  mustSnapshot(db.globals),
		filesBatch:   new(leveldb.Batch),
		globalsBatch: new(leveldb.Batch),
	}
	if debugSnap {
		l.Debugf("snapshot %p created", s)
	}
	return s
}

func (db ldb) ldbGenericReplace(folder, device []byte, fs []protocol.FileInfo, deleteFn deletionHandler) int64 {
	runtime.GC()

	sort.Sort(fileList(fs)) // sort list on name, same as in the database

	start := deviceKey(folder, device, nil)                            // before all folder/device files
	limit := deviceKey(folder, device, []byte{0xff, 0xff, 0xff, 0xff}) // after all folder/device files

	snap := db.snapshot()
	defer snap.commit()

	dbi := snap.filesSnap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	defer dbi.Release()

	moreDb := dbi.Next()
	fsi := 0
	var maxLocalVer int64

	for {
		var newName, oldName []byte
		moreFs := fsi < len(fs)

		if !moreDb && !moreFs {
			break
		}

		if moreFs {
			newName = []byte(fs[fsi].Name)
		}

		if moreDb {
			oldName = deviceKeyName(dbi.Key())
		}

		cmp := bytes.Compare(newName, oldName)

		if debugDB {
			l.Debugf("generic replace; folder=%q device=%v moreFs=%v moreDb=%v cmp=%d newName=%q oldName=%q", folder, protocol.DeviceIDFromBytes(device), moreFs, moreDb, cmp, newName, oldName)
		}

		switch {
		case moreFs && (!moreDb || cmp == -1):
			if debugDB {
				l.Debugln("generic replace; missing - insert")
			}
			// Database is missing this file. Insert it.
			if lv := snap.ldbInsert(folder, device, fs[fsi]); lv > maxLocalVer {
				maxLocalVer = lv
			}
			if fs[fsi].IsInvalid() {
				snap.ldbRemoveFromGlobal(folder, device, newName)
			} else {
				snap.ldbUpdateGlobal(folder, device, newName, fs[fsi].Version)
			}
			fsi++

		case moreFs && moreDb && cmp == 0:
			// File exists on both sides - compare versions. We might get an
			// update with the same version and different flags if a device has
			// marked a file as invalid, so handle that too.
			if debugDB {
				l.Debugln("generic replace; exists - compare")
			}
			var ef FileInfoTruncated
			ef.UnmarshalXDR(dbi.Value())
			if !fs[fsi].Version.Equal(ef.Version) || fs[fsi].Flags != ef.Flags {
				if debugDB {
					l.Debugln("generic replace; differs - insert")
				}
				if lv := snap.ldbInsert(folder, device, fs[fsi]); lv > maxLocalVer {
					maxLocalVer = lv
				}
				if fs[fsi].IsInvalid() {
					snap.ldbRemoveFromGlobal(folder, device, newName)
				} else {
					snap.ldbUpdateGlobal(folder, device, newName, fs[fsi].Version)
				}
			} else if debugDB {
				l.Debugln("generic replace; equal - ignore")
			}

			fsi++
			moreDb = dbi.Next()

		case moreDb && (!moreFs || cmp == 1):
			if debugDB {
				l.Debugln("generic replace; exists - remove")
			}
			if lv := deleteFn(snap, folder, device, oldName, dbi); lv > maxLocalVer {
				maxLocalVer = lv
			}
			moreDb = dbi.Next()
		}

		// Write out and reuse the batch every few records, to avoid the batch
		// growing too large and thus allocating unnecessarily much memory.
		snap.periodicWrite()
	}

	return maxLocalVer
}

func (db ldb) ldbReplace(folder, device []byte, fs []protocol.FileInfo) int64 {
	// TODO: Return the remaining maxLocalVer?
	return db.ldbGenericReplace(folder, device, fs, func(snap ldbSnapshot, folder, device, name []byte, dbi iterator.Iterator) int64 {
		snap.ldbRemoveFromGlobal(folder, device, name)
		snap.filesBatch.Delete(dbi.Key())
		return 0
	})

}

func (db ldb) ldbReplaceWithDelete(folder, device []byte, fs []protocol.FileInfo, myID uint64) int64 {
	return db.ldbGenericReplace(folder, device, fs, func(snap ldbSnapshot, folder, device, name []byte, dbi iterator.Iterator) int64 {
		var tf FileInfoTruncated
		err := tf.UnmarshalXDR(dbi.Value())
		if err != nil {
			panic(err)
		}
		if !tf.IsDeleted() {
			if debugDB {
				l.Debugf("mark deleted; folder=%q device=%v name=%q", folder, protocol.DeviceIDFromBytes(device), name)
			}
			ts := clock(tf.LocalVersion)
			f := protocol.FileInfo{
				Name:         tf.Name,
				Version:      tf.Version.Update(myID),
				LocalVersion: ts,
				Flags:        tf.Flags | protocol.FlagDeleted,
				Modified:     tf.Modified,
			}
			bs, _ := f.MarshalXDR()
			snap.filesBatch.Put(dbi.Key(), bs)
			snap.ldbUpdateGlobal(folder, device, deviceKeyName(dbi.Key()), f.Version)
			return ts
		}
		return 0
	})
}

func (db ldb) ldbUpdate(folder, device []byte, fs []protocol.FileInfo) int64 {
	runtime.GC()

	snap := db.snapshot()
	defer snap.commit()

	var maxLocalVer int64
	for _, f := range fs {
		name := []byte(f.Name)
		fk := deviceKey(folder, device, name)
		bs, ok := mustGet(snap.filesSnap, fk)
		if !ok {
			if lv := snap.ldbInsert(folder, device, f); lv > maxLocalVer {
				maxLocalVer = lv
			}
			if f.IsInvalid() {
				snap.ldbRemoveFromGlobal(folder, device, name)
			} else {
				snap.ldbUpdateGlobal(folder, device, name, f.Version)
			}
			continue
		}

		var ef FileInfoTruncated
		err := ef.UnmarshalXDR(bs)
		if err != nil {
			panic(err)
		}
		// Flags might change without the version being bumped when we set the
		// invalid flag on an existing file.
		if !ef.Version.Equal(f.Version) || ef.Flags != f.Flags {
			if lv := snap.ldbInsert(folder, device, f); lv > maxLocalVer {
				maxLocalVer = lv
			}
			if f.IsInvalid() {
				snap.ldbRemoveFromGlobal(folder, device, name)
			} else {
				snap.ldbUpdateGlobal(folder, device, name, f.Version)
			}
		}

		snap.periodicWrite()
	}

	return maxLocalVer
}

func (s ldbSnapshot) ldbInsert(folder, device []byte, file protocol.FileInfo) int64 {
	if debugDB {
		l.Debugf("insert; folder=%q device=%v %v", folder, protocol.DeviceIDFromBytes(device), file)
	}

	if file.LocalVersion == 0 {
		file.LocalVersion = clock(0)
	}

	name := []byte(file.Name)
	nk := deviceKey(folder, device, name)
	s.filesBatch.Put(nk, file.MustMarshalXDR())

	return file.LocalVersion
}

// ldbUpdateGlobal adds this device+version to the version list for the given
// file. If the device is already present in the list, the version is updated.
// If the file does not have an entry in the global list, it is created.
func (s ldbSnapshot) ldbUpdateGlobal(folder, device, file []byte, version protocol.Vector) bool {
	if debugDB {
		l.Debugf("update global; folder=%q device=%v file=%q version=%d", folder, protocol.DeviceIDFromBytes(device), file, version)
	}
	gk := globalKey(folder, file)
	svl, ok := mustGet(s.globalsSnap, gk)

	var fl versionList

	// Remove the device from the current version list
	if ok {
		err := fl.UnmarshalXDR(svl)
		if err != nil {
			panic(err)
		}

		for i := range fl.versions {
			if bytes.Compare(fl.versions[i].device, device) == 0 {
				if fl.versions[i].version.Equal(version) {
					// No need to do anything
					return false
				}
				fl.versions = append(fl.versions[:i], fl.versions[i+1:]...)
				break
			}
		}
	}

	nv := fileVersion{
		device:  device,
		version: version,
	}
	for i := range fl.versions {
		// We compare  against ConcurrentLesser as well here because we need
		// to enforce a consistent ordering of versions even in the case of
		// conflicts.
		if comp := fl.versions[i].version.Compare(version); comp == protocol.Equal || comp == protocol.Lesser || comp == protocol.ConcurrentLesser {
			t := append(fl.versions, fileVersion{})
			copy(t[i+1:], t[i:])
			t[i] = nv
			fl.versions = t
			goto done
		}
	}

	fl.versions = append(fl.versions, nv)

done:
	s.globalsBatch.Put(gk, fl.MustMarshalXDR())

	return true
}

// ldbRemoveFromGlobal removes the device from the global version list for the
// given file. If the version list is empty after this, the file entry is
// removed entirely.
func (s ldbSnapshot) ldbRemoveFromGlobal(folder, device, file []byte) {
	if debugDB {
		l.Debugf("remove from global; folder=%q device=%v file=%q", folder, protocol.DeviceIDFromBytes(device), file)
	}

	gk := globalKey(folder, file)
	svl, ok := mustGet(s.globalsSnap, gk)
	if !ok {
		// We might be called to "remove" a global version that doesn't exist
		// if the first update for the file is already marked invalid.
		return
	}

	var fl versionList
	err := fl.UnmarshalXDR(svl)
	if err != nil {
		panic(err)
	}

	for i := range fl.versions {
		if bytes.Compare(fl.versions[i].device, device) == 0 {
			fl.versions = append(fl.versions[:i], fl.versions[i+1:]...)
			break
		}
	}

	if len(fl.versions) == 0 {
		s.globalsBatch.Delete(gk)
	} else {
		s.globalsBatch.Put(gk, fl.MustMarshalXDR())
	}
}

func (db ldb) ldbWithHave(folder, device []byte, truncate bool, fn Iterator) {
	snap := db.snapshot()
	defer snap.commit()

	dbi := snap.filesSnap.NewIterator(util.BytesPrefix(deviceKey(folder, device, nil)), nil)
	defer dbi.Release()

	for dbi.Next() {
		f, err := unmarshalTrunc(dbi.Value(), truncate)
		if err != nil {
			panic(err)
		}
		if cont := fn(f); !cont {
			return
		}
	}
}

func (db ldb) ldbWithAllFolderTruncated(folder []byte, fn func(device []byte, f FileInfoTruncated) bool) {
	runtime.GC()

	snap := db.snapshot()
	defer snap.commit()

	dbi := snap.filesSnap.NewIterator(util.BytesPrefix(deviceKey(folder, nil, nil)), nil)
	defer dbi.Release()

	for dbi.Next() {
		device := deviceKeyDevice(dbi.Key())
		var f FileInfoTruncated
		err := f.UnmarshalXDR(dbi.Value())
		if err != nil {
			panic(err)
		}

		switch f.Name {
		case "", ".", "..", "/": // A few obviously invalid filenames
			l.Infof("Dropping invalid filename %q from database", f.Name)
			snap.ldbRemoveFromGlobal(folder, device, nil)
			snap.filesBatch.Delete(dbi.Key())
			continue
		}

		if cont := fn(device, f); !cont {
			return
		}
	}
}

func (db ldb) ldbGet(folder, device, file []byte) (protocol.FileInfo, bool) {
	nk := deviceKey(folder, device, file)
	bs, ok := mustGet(db.files, nk)
	if !ok {
		return protocol.FileInfo{}, false
	}

	var f protocol.FileInfo
	err := f.UnmarshalXDR(bs)
	if err != nil {
		panic(err)
	}
	return f, true
}

func (db ldb) ldbGetGlobal(folder, file []byte, truncate bool) (FileIntf, bool) {
	k := globalKey(folder, file)
	bs, ok := mustGet(db.globals, k)
	if !ok {
		return nil, false
	}

	var vl versionList
	err := vl.UnmarshalXDR(bs)
	if err != nil {
		panic(err)
	}
	if len(vl.versions) == 0 {
		l.Debugln(k)
		panic("no versions?")
	}

	k = deviceKey(folder, vl.versions[0].device, file)
	bs, ok = mustGet(db.files, k)
	if !ok {
		return nil, false
	}

	fi, err := unmarshalTrunc(bs, truncate)
	if err != nil {
		panic(err)
	}
	return fi, true
}

func (db ldb) ldbWithGlobal(folder, prefix []byte, truncate bool, fn Iterator) {
	runtime.GC()

	snap := db.snapshot()
	defer snap.commit()

	dbi := snap.globalsSnap.NewIterator(util.BytesPrefix(globalKey(folder, prefix)), nil)
	defer dbi.Release()

	for dbi.Next() {
		var vl versionList
		err := vl.UnmarshalXDR(dbi.Value())
		if err != nil {
			panic(err)
		}
		if len(vl.versions) == 0 {
			l.Debugln(dbi.Key())
			panic("no versions?")
		}
		name := globalKeyName(dbi.Key())
		fk := deviceKey(folder, vl.versions[0].device, name)
		bs, ok := mustGet(snap.filesSnap, fk)
		if !ok {
			l.Debugf("folder: %q (%x)", folder, folder)
			l.Debugf("key: %q (%x)", dbi.Key(), dbi.Key())
			l.Debugf("vl: %v", vl)
			l.Debugf("vl.versions[0].device: %x", vl.versions[0].device)
			l.Debugf("name: %q (%x)", name, name)
			l.Debugf("fk: %q", fk)
			l.Debugf("fk: %x %x %x", fk[1:1+64], fk[1+64:1+64+32], fk[1+64+32:])
			panic(err)
		}

		f, err := unmarshalTrunc(bs, truncate)
		if err != nil {
			panic(err)
		}

		if cont := fn(f); !cont {
			return
		}
	}
}

func (db ldb) ldbAvailability(folder, file []byte) []protocol.DeviceID {
	k := globalKey(folder, file)
	bs, err := db.globals.Get(k, nil)
	if err == leveldb.ErrNotFound {
		return nil
	}
	if err != nil {
		panic(err)
	}

	var vl versionList
	err = vl.UnmarshalXDR(bs)
	if err != nil {
		panic(err)
	}

	var devices []protocol.DeviceID
	for _, v := range vl.versions {
		if !v.version.Equal(vl.versions[0].version) {
			break
		}
		n := protocol.DeviceIDFromBytes(v.device)
		devices = append(devices, n)
	}

	return devices
}

func (db ldb) ldbWithNeed(folder, device []byte, truncate bool, fn Iterator) {
	runtime.GC()

	start := globalKey(folder, nil)
	limit := globalKey(folder, []byte{0xff, 0xff, 0xff, 0xff})
	snap := db.snapshot()
	defer snap.commit()

	dbi := snap.globalsSnap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	defer dbi.Release()

nextFile:
	for dbi.Next() {
		var vl versionList
		err := vl.UnmarshalXDR(dbi.Value())
		if err != nil {
			panic(err)
		}
		if len(vl.versions) == 0 {
			l.Debugln(dbi.Key())
			panic("no versions?")
		}

		have := false // If we have the file, any version
		need := false // If we have a lower version of the file
		var haveVersion protocol.Vector
		for _, v := range vl.versions {
			if bytes.Compare(v.device, device) == 0 {
				have = true
				haveVersion = v.version
				// XXX: This marks Concurrent (i.e. conflicting) changes as
				// needs. Maybe we should do that, but it needs special
				// handling in the puller.
				need = !v.version.GreaterEqual(vl.versions[0].version)
				break
			}
		}

		if need || !have {
			name := globalKeyName(dbi.Key())
			needVersion := vl.versions[0].version

		nextVersion:
			for i := range vl.versions {
				if !vl.versions[i].version.Equal(needVersion) {
					// We haven't found a valid copy of the file with the needed version.
					continue nextFile
				}
				fk := deviceKey(folder, vl.versions[i].device, name)
				bs, ok := mustGet(snap.filesSnap, fk)
				if !ok {
					var id protocol.DeviceID
					copy(id[:], device)
					l.Debugf("device: %v", id)
					l.Debugf("need: %v, have: %v", need, have)
					l.Debugf("key: %q (%x)", dbi.Key(), dbi.Key())
					l.Debugf("vl: %v", vl)
					l.Debugf("i: %v", i)
					l.Debugf("fk: %q (%x)", fk, fk)
					l.Debugf("name: %q (%x)", name, name)
					panic(err)
				}

				gf, err := unmarshalTrunc(bs, truncate)
				if err != nil {
					panic(err)
				}

				if gf.IsInvalid() {
					// The file is marked invalid for whatever reason, don't use it.
					continue nextVersion
				}

				if gf.IsDeleted() && !have {
					// We don't need deleted files that we don't have
					continue nextFile
				}

				if debugDB {
					l.Debugf("need folder=%q device=%v name=%q need=%v have=%v haveV=%d globalV=%d", folder, protocol.DeviceIDFromBytes(device), name, need, have, haveVersion, vl.versions[0].version)
				}

				if cont := fn(gf); !cont {
					return
				}

				// This file is handled, no need to look further in the version list
				continue nextFile
			}
		}
	}
}

func (db ldb) ldbListFolders() []string {
	runtime.GC()

	snap := db.snapshot()
	defer snap.commit()

	dbi := snap.filesSnap.NewIterator(util.BytesPrefix([]byte{KeyTypeGlobal}), nil)
	defer dbi.Release()

	folderExists := make(map[string]bool)
	for dbi.Next() {
		folder := string(globalKeyFolder(dbi.Key()))
		if !folderExists[folder] {
			folderExists[folder] = true
		}
	}

	folders := make([]string, 0, len(folderExists))
	for k := range folderExists {
		folders = append(folders, k)
	}

	sort.Strings(folders)
	return folders
}

func (db ldb) ldbDropFolder(folder []byte) {
	runtime.GC()

	snap := mustSnapshot(db.files)
	if debugDB {
		l.Debugf("created snapshot %p", snap)
	}
	defer func() {
		if debugDB {
			l.Debugf("close snapshot %p", snap)
		}
		snap.Release()
	}()

	// Remove all items related to the given folder from the device->file bucket
	dbi := snap.NewIterator(util.BytesPrefix([]byte{KeyTypeDevice}), nil)
	for dbi.Next() {
		itemFolder := deviceKeyFolder(dbi.Key())
		if bytes.Compare(folder, itemFolder) == 0 {
			db.files.Delete(dbi.Key(), nil)
		}
	}
	dbi.Release()

	// Remove all items related to the given folder from the global bucket
	dbi = snap.NewIterator(util.BytesPrefix([]byte{KeyTypeGlobal}), nil)
	for dbi.Next() {
		itemFolder := globalKeyFolder(dbi.Key())
		if bytes.Compare(folder, itemFolder) == 0 {
			db.files.Delete(dbi.Key(), nil)
		}
	}
	dbi.Release()
}

func unmarshalTrunc(bs []byte, truncate bool) (FileIntf, error) {
	if truncate {
		var tf FileInfoTruncated
		err := tf.UnmarshalXDR(bs)
		return tf, err
	}

	var tf protocol.FileInfo
	err := tf.UnmarshalXDR(bs)
	return tf, err
}

func (db ldb) ldbCheckGlobals(folder []byte) {
	defer runtime.GC()

	snap := mustSnapshot(db.files)
	if debugDB {
		l.Debugf("created snapshot %p", snap)
	}
	defer func() {
		if debugDB {
			l.Debugf("close snapshot %p", snap)
		}
		snap.Release()
	}()

	start := globalKey(folder, nil)
	limit := globalKey(folder, []byte{0xff, 0xff, 0xff, 0xff})
	dbi := snap.NewIterator(&util.Range{Start: start, Limit: limit}, nil)
	defer dbi.Release()

	batch := new(leveldb.Batch)
	if debugDB {
		l.Debugf("new batch %p", batch)
	}
	for dbi.Next() {
		gk := dbi.Key()
		var vl versionList
		err := vl.UnmarshalXDR(dbi.Value())
		if err != nil {
			panic(err)
		}

		// Check the global version list for consistency. An issue in previous
		// versions of goleveldb could result in reordered writes so that
		// there are global entries pointing to no longer existing files. Here
		// we find those and clear them out.

		name := globalKeyName(gk)
		var newVL versionList
		for _, version := range vl.versions {
			fk := deviceKey(folder, version.device, name)
			_, ok := mustGet(snap, fk)
			if !ok {
				continue
			}
			newVL.versions = append(newVL.versions, version)
		}

		if len(newVL.versions) != len(vl.versions) {
			l.Infof("db repair: rewriting global version list for %x %x", gk[1:1+64], gk[1+64:])
			batch.Put(dbi.Key(), newVL.MustMarshalXDR())
		}
	}
	if debugDB {
		l.Infoln("db check completed for %q", folder)
	}
	mustWrite(db.files, batch, nil)
}

func mustSnapshot(db *leveldb.DB) *leveldb.Snapshot {
	snap, err := db.GetSnapshot()
	if err != nil {
		panic(err)
	}
	return snap
}

func mustWrite(db *leveldb.DB, b *leveldb.Batch, wo *opt.WriteOptions) {
	if debugDB {
		l.Debugf("db.Write %p", b)
	}
	err := db.Write(b, wo)
	if err != nil {
		panic(err)
	}
}

func mustGet(db dbReader, k []byte) ([]byte, bool) {
	if debugDB {
		l.Debugf("db.Get %x", k)
	}

	val, err := db.Get(k, nil)
	if err == leveldb.ErrNotFound {
		return nil, false
	}
	if err != nil {
		panic(err)
	}
	return val, true
}
