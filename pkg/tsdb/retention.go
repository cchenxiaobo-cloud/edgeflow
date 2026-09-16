// 保留策略与降采样（spec 0011 US-3）+ 容量水位泄洪（US-4）。
package tsdb

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"edgeflow/pkg/log"
)

// RunRetention 执行一轮保留与降采样（逐段判定）：
//   - 段 maxTs < now−Retention（启用时）→ 直接删除（raw/rollup 均适用）；
//   - 否则 raw 段且 maxTs < now−DownsampleAfter（启用时）→ 重写为 rollup 段
//     （原子替换：临时文件写完后 rename 覆盖；崩溃安全——rename 前崩溃保留
//     原段，下次重试；rename 后崩溃则新段就位）。
//   - 活动段不参与（段粒度；活动段内旧点保留到封段——登记 KI §39）。
func (db *DB) RunRetention(now int64) (RetentionResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return RetentionResult{}, ErrClosed
	}
	var res RetentionResult
	retMs := int64(db.opts.Retention / time.Millisecond)
	dsMs := int64(db.opts.DownsampleAfter / time.Millisecond)
	bucketMs := int64(db.opts.DownsampleEvery / time.Millisecond)
	for _, s := range db.series {
		s.mu.Lock()
		kept := make([]*segment, 0, len(s.sealed))
		for _, seg := range s.sealed {
			// 1) 超保留期 → 删除（删除失败保留索引，下轮重试）
			if retMs > 0 && seg.maxTs > 0 && seg.maxTs < now-retMs {
				if err := os.Remove(seg.path); err != nil {
					log.Warnf("tsdb: 删除过期段失败（%s）: %v", seg.path, err)
					kept = append(kept, seg)
					continue
				}
				db.points -= seg.count
				db.bytes -= seg.size
				res.Deleted++
				res.BytesFreed += seg.size
				continue
			}
			// 2) raw 段超降采样期 → 重写为 rollup（替换失败保留原段）
			if seg.kind == kindRaw && dsMs > 0 && seg.maxTs > 0 && seg.maxTs < now-dsMs {
				newSeg, err := downsampleSegment(seg, bucketMs)
				if err != nil {
					log.Warnf("tsdb: 降采样段失败（%s）: %v", seg.path, err)
					kept = append(kept, seg)
					continue
				}
				db.points += newSeg.count - seg.count
				db.bytes += newSeg.size - seg.size
				res.Downsampled++
				kept = append(kept, newSeg)
				continue
			}
			kept = append(kept, seg)
		}
		s.sealed = kept
		s.mu.Unlock()
	}
	return res, nil
}

// RunRetentionLoop 周期执行 RunRetention（interval≤0 → 默认 1 分钟）；
// stopCh 关闭时退出。
func (db *DB) RunRetentionLoop(interval time.Duration, stopCh <-chan struct{}) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := db.RunRetention(time.Now().UnixMilli()); err != nil && !errors.Is(err, ErrClosed) {
				log.Warnf("tsdb: 保留策略执行失败: %v", err)
			}
		case <-stopCh:
			return
		}
	}
}

// rollupBucket 是降采样桶的内部累积单元。
type rollupBucket struct {
	min   float64
	max   float64
	sum   float64
	last  float64
	count int64
}

// downsampleSegment 把 raw 段重写为 rollup 段（原子替换，路径不变）。
// 桶网格为绝对网格（idx = ts / bucketMs，桶 Ts = idx*bucketMs）。
func downsampleSegment(seg *segment, bucketMs int64) (*segment, error) {
	if bucketMs <= 0 {
		return nil, fmt.Errorf("降采样桶宽必须 >0")
	}
	// 读全部 raw 记录并聚桶
	f, err := os.Open(seg.path)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(segmentHeaderLen, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	buckets := make(map[int64]*rollupBucket)
	buf := make([]byte, rawRecordLen)
	for {
		if _, err := io.ReadFull(f, buf); err != nil {
			break // EOF / 尾部残缺
		}
		ts := int64(binary.LittleEndian.Uint64(buf[:8]))
		v := math.Float64frombits(binary.LittleEndian.Uint64(buf[8:16]))
		idx := ts / bucketMs
		b := buckets[idx]
		if b == nil {
			buckets[idx] = &rollupBucket{min: v, max: v, sum: v, last: v, count: 1}
			continue
		}
		if v < b.min {
			b.min = v
		}
		if v > b.max {
			b.max = v
		}
		b.sum += v
		b.last = v
		b.count++
	}
	_ = f.Close()

	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	// 写临时文件 → rename 原子覆盖
	tmpPath := seg.path + ".tmp"
	tf, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		_ = tf.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tf.Write(encodeSegmentHeader(kindRollup, bucketMs, seg.createdMs)); err != nil {
		cleanup()
		return nil, err
	}
	bw := bufio.NewWriterSize(tf, 64*1024)
	rec := make([]byte, rollupRecordLen)
	for _, idx := range keys {
		b := buckets[idx]
		binary.LittleEndian.PutUint64(rec[0:8], uint64(idx*bucketMs))
		binary.LittleEndian.PutUint64(rec[8:16], math.Float64bits(b.min))
		binary.LittleEndian.PutUint64(rec[16:24], math.Float64bits(b.max))
		binary.LittleEndian.PutUint64(rec[24:32], math.Float64bits(b.sum))
		binary.LittleEndian.PutUint64(rec[32:40], uint64(b.count))
		binary.LittleEndian.PutUint64(rec[40:48], math.Float64bits(b.last))
		if _, err := bw.Write(rec); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return nil, err
	}
	if err := tf.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, seg.path); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}

	newSeg := &segment{
		path:      seg.path,
		kind:      kindRollup,
		bucketMs:  bucketMs,
		createdMs: seg.createdMs,
		count:     int64(len(keys)),
		size:      segmentHeaderLen + int64(len(keys))*rollupRecordLen,
	}
	if len(keys) > 0 {
		newSeg.minTs = keys[0] * bucketMs
		newSeg.maxTs = keys[len(keys)-1] * bucketMs
	}
	return newSeg, nil
}

// shedLocked 容量水位泄洪：删除最老封段直到低于水位或仅剩活动段
// （db.mu 已持有；段粒度删除与保留策略同构，删除失败仅告警——
// 孤儿文件在下次 Open 扫描时回归索引）。
func (db *DB) shedLocked() {
	for db.bytes > db.opts.MaxBytes {
		var victim *series
		var victimSeg *segment
		for _, s := range db.series {
			s.mu.Lock()
			if len(s.sealed) > 0 {
				seg := s.sealed[0]
				if victimSeg == nil || cmpSegments(seg, victimSeg) < 0 {
					victimSeg = seg
					victim = s
				}
			}
			s.mu.Unlock()
		}
		if victimSeg == nil {
			return // 仅剩活动段，无法再泄洪
		}
		victim.mu.Lock()
		if len(victim.sealed) > 0 && victim.sealed[0] == victimSeg {
			victim.sealed = victim.sealed[1:]
			if err := os.Remove(victimSeg.path); err != nil {
				log.Warnf("tsdb: 泄洪删除段失败（%s）: %v", victimSeg.path, err)
			}
			db.points -= victimSeg.count
			db.bytes -= victimSeg.size
		}
		victim.mu.Unlock()
	}
}
