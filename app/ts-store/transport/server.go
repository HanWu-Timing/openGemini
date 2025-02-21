/*
Copyright 2022 Huawei Cloud Computing Technologies Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package transport

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
	"github.com/openGemini/openGemini/app/ts-store/storage"
	"github.com/openGemini/openGemini/app/ts-store/stream"
	"github.com/openGemini/openGemini/lib/bufferpool"
	"github.com/openGemini/openGemini/lib/config"
	"github.com/openGemini/openGemini/lib/errno"
	"github.com/openGemini/openGemini/lib/logger"
	"github.com/openGemini/openGemini/lib/netstorage"
	"github.com/openGemini/openGemini/lib/statisticsPusher/statistics"
	"github.com/openGemini/openGemini/lib/util"
	"github.com/openGemini/openGemini/open_src/vm/protoparser/influx"
	"go.uber.org/zap"
)

// Server processes connections from insert and select.
type Server struct {
	closed chan struct{}

	log *logger.Logger

	selectServer *SelectServer
	insertServer *InsertServer
}

// NewServer returns new Server.
func NewServer(ingestAddr string, selectAddr string) *Server {
	selectServer := NewSelectServer(selectAddr)
	insertServer := NewInsertServer(ingestAddr)

	return &Server{
		closed: make(chan struct{}),
		log:    logger.NewLogger(errno.ModuleStorageEngine),

		selectServer: selectServer,
		insertServer: insertServer,
	}
}

func (s *Server) Open() error {
	if err := s.insertServer.Open(); err != nil {
		return fmt.Errorf("cannot create a server with addr=%s: %s", s.insertServer.addr, err)
	}
	if err := s.selectServer.Open(); err != nil {
		return fmt.Errorf("cannot create a server with addr=%s: %s", s.selectServer.addr, err)
	}
	return nil
}

func (s *Server) Run(store *storage.Storage, stream stream.Engine) {
	go s.insertServer.Run(store, stream)
	//TODO stream support query
	go s.selectServer.Run(store)
	if stream != nil {
		go stream.Run()
	}
}

func (s *Server) setIsStopping() {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
}

func GetWritePointsWork() *WritePointsWork {
	v := writePointsWorkPool.Get()
	if v == nil {
		return &WritePointsWork{
			logger: logger.NewLogger(errno.ModuleWrite),
		}
	}
	return v.(*WritePointsWork)
}

func PutWritePointsWork(ww *WritePointsWork) {
	bufferpool.PutPoints(ww.reqBuf)
	ww.reset()
	writePointsWorkPool.Put(ww)
}

var writePointsWorkPool sync.Pool

type WritePointsWork struct {
	storage          *storage.Storage
	stream           stream.Engine
	reqBuf           []byte
	streamVars       []*netstorage.StreamVar
	rows             []influx.Row
	tagpools         []influx.Tag
	fieldpools       []influx.Field
	indexKeypools    []byte
	indexOptionpools []influx.IndexOption
	lastResetTime    uint64

	logger *logger.Logger
}

func (ww *WritePointsWork) GetRows() []influx.Row {
	return ww.rows
}

func (ww *WritePointsWork) SetRows(rows []influx.Row) {
	ww.rows = rows
}

func (ww *WritePointsWork) PutWritePointsWork() {
	PutWritePointsWork(ww)
}

func (ww *WritePointsWork) reset() {
	if (len(ww.reqBuf)*4 > cap(ww.reqBuf) || len(ww.rows)*4 > cap(ww.rows)) && fasttime.UnixTimestamp()-ww.lastResetTime > 10 {
		ww.reqBuf = nil
		ww.rows = nil
		ww.lastResetTime = fasttime.UnixTimestamp()
	}

	ww.tagpools = ww.tagpools[:0]
	ww.fieldpools = ww.fieldpools[:0]
	ww.indexKeypools = ww.indexKeypools[:0]
	ww.indexOptionpools = ww.indexOptionpools[:0]
	ww.storage = nil
	ww.stream = nil
	ww.rows = ww.rows[:0]
	ww.reqBuf = ww.reqBuf[:0]
	ww.streamVars = ww.streamVars[:0]
}

func (ww *WritePointsWork) decodePoints() (db string, rp string, ptId uint32, shard uint64, streamShardIdList []uint64, binaryRows []byte, err error) {
	tail := ww.reqBuf

	start := time.Now()

	if len(tail) < 2 {
		err = errors.New("invalid points buffer")
		ww.logger.Error(err.Error())
		return
	}
	ty := tail[0]
	if ty != netstorage.PackageTypeFast {
		err = errors.New("not a fast marshal points package")
		ww.logger.Error(err.Error())
		return
	}
	tail = tail[1:]

	l := int(tail[0])
	if len(tail) < l {
		err = errors.New("no data for db name")
		ww.logger.Error(err.Error())
		return
	}
	tail = tail[1:]
	db = util.Bytes2str(tail[:l])
	tail = tail[l:]

	l = int(tail[0])
	if len(tail) < l {
		err = errors.New("no data for rp name")
		ww.logger.Error(err.Error())
		return
	}
	tail = tail[1:]
	rp = util.Bytes2str(tail[:l])

	tail = tail[l:]

	if len(tail) < 16 {
		err = errors.New("no data for points data")
		ww.logger.Error(err.Error())
		return
	}
	ptId = encoding.UnmarshalUint32(tail)
	tail = tail[4:]

	shard = encoding.UnmarshalUint64(tail)
	tail = tail[8:]

	sdLen := encoding.UnmarshalUint32(tail)
	tail = tail[4:]

	streamShardIdList = make([]uint64, sdLen)
	tail, err = encoding.UnmarshalVarUint64s(streamShardIdList, tail)
	if err != nil {
		ww.logger.Error(err.Error())
		return
	}

	binaryRows = tail

	ww.rows = ww.rows[:0]
	ww.tagpools = ww.tagpools[:0]
	ww.fieldpools = ww.fieldpools[:0]
	ww.indexKeypools = ww.indexKeypools[:0]
	for i := range ww.indexOptionpools {
		ww.indexOptionpools[i].IndexList = ww.indexOptionpools[i].IndexList[:0]
	}
	ww.indexOptionpools = ww.indexOptionpools[:0]
	ww.rows, ww.tagpools, ww.fieldpools, ww.indexOptionpools, ww.indexKeypools, err =
		influx.FastUnmarshalMultiRows(tail, ww.rows, ww.tagpools, ww.fieldpools, ww.indexOptionpools, ww.indexKeypools)
	if err != nil {
		ww.logger.Error("unmarshal rows failed", zap.String("db", db),
			zap.String("rp", rp), zap.Uint32("ptId", ptId), zap.Uint64("shardId", shard), zap.Error(err))
		return
	}

	if len(streamShardIdList) > 0 {
		// set stream vars into the rows
		if len(ww.rows) != len(ww.streamVars) {
			errStr := "unmarshal rows failed, the num of the rows is not equal to the stream vars"
			ww.logger.Error(errStr, zap.String("db", db),
				zap.String("rp", rp), zap.Uint32("ptId", ptId), zap.Uint64("shardId", shard), zap.Error(err))
			err = errors.New(errStr)
			return
		}
		for i := 0; i < len(ww.rows); i++ {
			ww.rows[i].StreamOnly = ww.streamVars[i].Only
			ww.rows[i].StreamId = ww.streamVars[i].Id
		}
	}

	atomic.AddInt64(&statistics.PerfStat.WriteUnmarshalNs, time.Since(start).Nanoseconds())
	return
}

func (ww *WritePointsWork) WritePoints() error {
	db, rp, ptId, shard, _, binaryRows, err := ww.decodePoints()
	if err != nil {
		err = errno.NewError(errno.ErrUnmarshalPoints, err)
		ww.logger.Error("unmarshal rows failed", zap.String("db", db),
			zap.String("rp", rp), zap.Uint32("ptId", ptId), zap.Uint64("shardId", shard), zap.Error(err))
		return err
	}
	if err = ww.storage.WriteRows(db, rp, ptId, shard, ww.rows, binaryRows); err != nil {
		ww.logger.Error("write rows failed", zap.String("db", db),
			zap.String("rp", rp), zap.Uint32("ptId", ptId), zap.Uint64("shardId", shard), zap.Error(err))
	}

	if err == nil && config.IsReplication() {
		err = ww.storage.WriteRowsToSlave(ww.rows, db, rp, ptId, shard)
	}

	return err
}

func (ww *WritePointsWork) WriteStreamPoints() (error, bool) {
	var inUse bool
	db, rp, ptId, shard, streamShardIdList, binaryRows, err := ww.decodePoints()
	if err != nil {
		err = errno.NewError(errno.ErrUnmarshalPoints, err)
		ww.logger.Error("unmarshal rows failed", zap.String("db", db),
			zap.String("rp", rp), zap.Uint32("ptId", ptId), zap.Uint64("shardId", shard), zap.Error(err))
		return err, inUse
	}
	if err = ww.storage.WriteRows(db, rp, ptId, shard, ww.rows, binaryRows); err != nil {
		ww.logger.Error("write rows failed", zap.String("db", db),
			zap.String("rp", rp), zap.Uint32("ptId", ptId), zap.Uint64("shardId", shard), zap.Error(err))
	}
	if ww.stream == nil || len(streamShardIdList) == 0 {
		return err, inUse
	}

	streamIdDstShardIdMap := make(map[uint64]uint64)
	if len(streamShardIdList)%2 != 0 {
		err = errno.NewError(errno.ErrUnmarshalPoints, err)
		return err, inUse
	}
	for i := 0; i < len(streamShardIdList); i += 2 {
		streamIdDstShardIdMap[streamShardIdList[i]] = streamShardIdList[i+1]
	}
	if err == nil && len(streamShardIdList) > 0 {
		ww.stream.WriteRows(db, rp, ptId, shard, streamIdDstShardIdMap, ww)
		inUse = true
	}
	return err, inUse
}

func (s *Server) MustClose() {
	// Mark the server as stopping.
	s.setIsStopping()
	s.selectServer.Close()
	s.insertServer.Close()

}
