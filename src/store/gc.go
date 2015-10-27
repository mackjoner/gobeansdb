package store

import (
	"fmt"
	"time"
)

type GCMgr struct {
	bucketID int
	stat     *GCState // curr or laste
	ki       KeyInfo
}

type GCState struct {
	BeginTS time.Time
	EndTS   time.Time

	Begin int
	End   int

	// curr
	Src int
	Dst int

	StopReason error
	Running    bool

	// sum
	GCFileState
}

type GCFileState struct {
	Src                int
	NumBefore          int
	NumReleased        int
	NumReleasedDeleted int
	SizeBefore         uint32
	SizeReleased       uint32
	SizeDeleted        uint32
	SizeBroken         uint32
}

func (s *GCFileState) add(size uint32, isRetained, isDeleted bool, sizeBroken uint32) {
	if !isRetained {
		s.NumReleased += 1
		s.SizeReleased += size
		if isDeleted {
			s.NumReleasedDeleted += 1
		}
	}
	s.SizeReleased += sizeBroken
	s.SizeBroken += sizeBroken
	s.SizeBefore += (size + sizeBroken)
	s.NumBefore += 1
}

func (s *GCFileState) String() string {
	return fmt.Sprintf("%#v", s)
}

func (mgr *GCMgr) ShouldRetainRecord(bkt *Bucket, rec *Record, oldPos Position) bool {
	ki := &mgr.ki
	ki.Key = rec.Key
	meta, pos, found := bkt.htree.get(ki)
	if !found {
		logger.Errorf("old key not found in htree bucket %d %s %#v %#v",
			bkt.id, ki.StringKey, meta, oldPos)
		return true
	}
	return pos == oldPos
}

func (mgr *GCMgr) UpdatePos(bkt *Bucket, ki *KeyInfo, oldPos, newPos Position) {
	// TODO: should be a api of htree to be atomic
	meta, pos, _ := bkt.htree.get(ki)
	if pos != oldPos {
		logger.Warnf("old key update when updating pos bucket %d %s %#v %#v",
			bkt.id, ki.StringKey, meta, oldPos)
		return
	}
	bkt.htree.set(ki, meta, newPos)
}

func (mgr *GCMgr) BeforeBucket(bkt *Bucket) {
	bkt.removeHtree()
	bkt.hints.StopMerge()
}

func (mgr *GCMgr) AfterBucket(bkt *Bucket) {
	bkt.dumpHtree()
	bkt.hints.StartMerge()
}

func (mgr *GCMgr) gc(bkt *Bucket, startChunkID, endChunkID int) (err error) {
	if endChunkID < 0 {
		endChunkID = bkt.datas.newHead
	}
	bkt.GCHistory = append(bkt.GCHistory, GCState{})
	gc := &bkt.GCHistory[len(bkt.GCHistory)-1]
	mgr.stat = gc

	gc.Begin = startChunkID
	gc.End = endChunkID
	gc.Dst = startChunkID
	gc.Running = true
	defer func() {
		gc.Running = false
	}()

	var oldPos Position
	var newPos Position
	var rec *Record
	var r *DataStreamReader
	var w *DataStreamWriter
	mfs := dataConfig.MaxFileSize

	mgr.BeforeBucket(bkt)
	defer mgr.AfterBucket(bkt)
	for gc.Src = gc.Begin; gc.Src < gc.End; gc.Src++ {
		oldPos.ChunkID = gc.Src
		var fileState GCFileState
		// reader must have a larger buffer
		logger.Infof("begin GC file %d -> %d", gc.Src, gc.Dst)
		if r, err = bkt.datas.GetStreamReader(gc.Src); err != nil {
			logger.Errorf("gc failed: %s", err.Error())
			return
		}
		w, err = bkt.datas.GetStreamWriter(gc.Dst, gc.Dst != gc.Src)
		if err != nil {
			return
		}
		for {
			var sizeBroken uint32
			rec, oldPos.Offset, sizeBroken, err = r.Next()
			if err != nil {
				logger.Errorf("gc failed: %s", err.Error())
				return
			}
			if rec == nil {
				break
			}

			_, recsize := rec.Sizes()

			if recsize+w.Offset() > mfs {
				w.Close()
				gc.Dst++
				newPos.ChunkID = gc.Dst
				if w, err = bkt.datas.GetStreamWriter(gc.Dst, gc.Dst != gc.Src); err != nil {
					logger.Errorf("gc failed: %s", err.Error())
					return
				}
			}
			isRetained := mgr.ShouldRetainRecord(bkt, rec, oldPos)
			if isRetained {
				if newPos.Offset, err = w.Append(rec); err != nil {
					logger.Errorf("gc failed: %s", err.Error())
					return
				}
				keyinfo := NewKeyInfoFromBytes(rec.Key, getKeyHash(rec.Key), false)
				mgr.UpdatePos(bkt, keyinfo, oldPos, newPos)
			}
			fileState.add(recsize, isRetained, rec.Payload.Ver < 0, sizeBroken)
		}
		w.Close()
		size := w.Offset()
		bkt.datas.Truncate(gc.Dst, size)
		if gc.Src != gc.Dst {
			bkt.datas.DeleteFile(gc.Src)
		}
		logger.Infof("end GC file %#v", fileState)
	}
	return nil
}
