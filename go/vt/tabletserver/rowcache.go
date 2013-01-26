// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tabletserver

import (
	"encoding/binary"
	"time"

	"code.google.com/p/vitess/go/sqltypes"
	"code.google.com/p/vitess/go/stats"
	"code.google.com/p/vitess/go/vt/schema"
)

var cacheStats = stats.NewTimings("Cache")
var cacheCounters = stats.NewCounters("CacheCounters")

var pack = binary.BigEndian

const (
	RC_DELETED = 1
)

type RowCache struct {
	tableInfo *TableInfo
	prefix    string
	cachePool *CachePool
}

func NewRowCache(tableInfo *TableInfo, hash string, cachePool *CachePool) *RowCache {
	prefix := hash + "."
	return &RowCache{tableInfo, prefix, cachePool}
}

func (rc *RowCache) Get(key string) (row []sqltypes.Value, cas uint64) {
	if key == "" {
		return nil, 0
	}
	mkey := rc.prefix + key
	conn := rc.cachePool.Get()
	defer conn.Recycle()

	defer cacheStats.Record("Exec", time.Now())
	b, f, cas, err := conn.Gets(mkey)
	if err != nil {
		conn.Close()
		panic(NewTabletError(FATAL, "%s", err))
	}
	if b == nil {
		return nil, 0
	}
	if f == RC_DELETED {
		// The row was recently invalidated.
		// If the caller reads the row from db, they can update it
		// back as long as it's not updated again.
		return nil, cas
	}
	if row = rc.decodeRow(b); row == nil {
		panic(NewTabletError(FAIL, "Corrupt data for %s", key))
	}
	// No cas. If you've read the row, we don't expect you to update it back.
	return row, 0
}

func (rc *RowCache) Set(key string, row []sqltypes.Value, cas uint64) {
	// This value is hardcoded for now.
	// We're assuming it's not worth caching rows that are too large.
	b := rc.encodeRow(row, 8000)
	if b == nil {
		return
	}
	conn := rc.cachePool.Get()
	defer conn.Recycle()
	mkey := rc.prefix + key

	var err error
	if cas == 0 {
		// Either caller didn't find the value at all
		// or they didn't look for it in the first place.
		_, err = conn.Add(mkey, 0, 0, b)
	} else {
		// Caller is trying to update a row that recently changed.
		_, err = conn.Cas(mkey, 0, 0, b, cas)
	}
	if err != nil {
		conn.Close()
		panic(NewTabletError(FATAL, "%s", err))
	}
}

func (rc *RowCache) Delete(key string) {
	conn := rc.cachePool.Get()
	defer conn.Recycle()
	mkey := rc.prefix + key

	_, err := conn.Set(mkey, RC_DELETED, rc.cachePool.DeleteExpiry, nil)
	if err != nil {
		conn.Close()
		panic(NewTabletError(FATAL, "%s", err))
	}
}

func (rc *RowCache) encodeRow(row []sqltypes.Value, max int) (b []byte) {
	length := 0
	for _, v := range row {
		length += len(v.Raw())
		if length > max {
			return nil
		}
	}
	datastart := 4 + len(row)*4
	b = make([]byte, datastart+length)
	data := b[datastart:datastart]
	pack.PutUint32(b, uint32(len(row)))
	for i, v := range row {
		if v.IsNull() {
			pack.PutUint32(b[4+i*4:], 0xFFFFFFFF)
			continue
		}
		data = append(data, v.Raw()...)
		pack.PutUint32(b[4+i*4:], uint32(len(v.Raw())))
	}
	return b
}

func (rc *RowCache) decodeRow(b []byte) (row []sqltypes.Value) {
	rowlen := pack.Uint32(b)
	data := b[4+rowlen*4:]
	row = make([]sqltypes.Value, rowlen)
	for i, _ := range row {
		length := pack.Uint32(b[4+i*4:])
		if length == 0xFFFFFFFF {
			continue
		}
		if length > uint32(len(data)) {
			// Corrupt data
			return nil
		}
		if rc.tableInfo.Columns[i].Category == schema.CAT_NUMBER {
			row[i] = sqltypes.MakeNumeric(data[:length])
		} else {
			row[i] = sqltypes.MakeString(data[:length])
		}
		data = data[length:]
	}
	return row
}

func rowLen(row []sqltypes.Value) int {
	length := 0
	for _, v := range row {
		length += len(v.Raw())
	}
	return length
}

type GenericCache struct {
	cachePool *CachePool
}

func NewGenericCache(cachePool *CachePool) *GenericCache {
	return &GenericCache{cachePool}
}

func (gc *GenericCache) Gets(key string) (value []byte, flags uint16, cas uint64, err error) {
	if gc.cachePool.IsClosed() {
		return
	}

	if key == "" {
		return
	}

	conn := gc.cachePool.Get()
	defer conn.Recycle()

	defer cacheStats.Record("Exec", time.Now())
	value, flags, cas, err = conn.Gets(key)
	if err != nil {
		conn.Close()
		panic(NewTabletError(FATAL, "%s", err))
	}
	return
}

func (gc *GenericCache) Set(key string, flags uint16, timeout uint64, value []byte) {
	if gc.cachePool.IsClosed() {
		return
	}

	if key == "" || value == nil {
		return
	}

	conn := gc.cachePool.Get()
	defer conn.Recycle()

	_, err := conn.Set(key, flags, timeout, value)
	if err != nil {
		conn.Close()
		panic(NewTabletError(FATAL, "%s", err))
	}
}

func (gc *GenericCache) PurgeCache() {
	if gc.cachePool.IsClosed() {
		return
	}

	conn := gc.cachePool.Get()
	defer conn.Recycle()

	err := conn.FlushAll()
	if err != nil {
		conn.Close()
		panic(NewTabletError(FATAL, "%s", err))
	}
	cacheCounters.Add("PurgeCache", 1)
}
