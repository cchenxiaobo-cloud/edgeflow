// 查询与聚合（spec 0011 US-2）：时间窗查询 + 窗口聚合。
//
// 读语义：查询前自动 flush 目标序列活动段缓冲（正确性优先）；
// 降采样（rollup）段参与查询——查询返回桶代表点（Ts=桶起点、Value=avg），
// 聚合在 rollup 段上正确重聚合（avg 以 count 加权）。
package tsdb

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"edgeflow/pkg/log"
)

// Query 时间窗查询 [from, to)（from=0 不限起点、to=0 不限终点）。
// 返回按时间升序排列的点；未知序列返回空。降采样段的代表点
// 为桶起点 + 桶内均值。
func (db *DB) Query(metric string, tags map[string]string, from, to int64) ([]Point, error) {
	segs, err := db.snapshotSegments(metric, tags)
	if err != nil {
		return nil, err
	}
	out := make([]Point, 0)
	for _, seg := range segs {
		pts, err := readSegmentRange(seg, metric, from, to)
		if err != nil {
			// 单段读取失败不阻断整体：告警后跳过（损坏段在 Open 时已兜底；
			// 运行期失败为罕见路径，逐次告警不刷屏）
			log.Warnf("tsdb: 段读取失败（跳过，%s）: %v", seg.path, err)
			continue
		}
		out = append(out, pts...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts < out[j].Ts })
	return out, nil
}

// Aggregate 窗口聚合：窗口自 from 起按 window 对齐切分（from=0 时按绝对
// 网格 ts/window 对齐）；fn ∈ {avg, min, max, count, sum}。
func (db *DB) Aggregate(metric string, tags map[string]string, from, to, window int64, fn AggFn) ([]Bucket, error) {
	if window <= 0 {
		return nil, fmt.Errorf("tsdb: window 必须 >0")
	}
	switch fn {
	case AggAvg, AggMin, AggMax, AggCount, AggSum:
	default:
		return nil, fmt.Errorf("tsdb: 未知聚合函数 %q", fn)
	}
	segs, err := db.snapshotSegments(metric, tags)
	if err != nil {
		return nil, err
	}
	type acc struct {
		sum   float64
		count int64
		min   float64
		max   float64
	}
	groups := make(map[int64]*acc)
	merge := func(ts int64, sum float64, count int64, mn, mx float64) {
		if from > 0 && ts < from {
			return
		}
		if to > 0 && ts >= to {
			return
		}
		var idx int64
		if from > 0 {
			idx = (ts - from) / window
		} else {
			idx = ts / window
		}
		key := idx * window
		if from > 0 {
			key = from + idx*window
		}
		a := groups[key]
		if a == nil {
			a = &acc{min: mn, max: mx}
			groups[key] = a
		} else {
			if mn < a.min {
				a.min = mn
			}
			if mx > a.max {
				a.max = mx
			}
		}
		a.sum += sum
		a.count += count
	}
	for _, seg := range segs {
		for _, ap := range readSegmentAgg(seg) {
			merge(ap.ts, ap.sum, ap.count, ap.min, ap.max)
		}
	}
	out := make([]Bucket, 0, len(groups))
	for ts, a := range groups {
		if a.count <= 0 {
			continue
		}
		b := Bucket{Ts: ts, Count: a.count}
		switch fn {
		case AggAvg:
			b.Value = a.sum / float64(a.count)
		case AggMin:
			b.Value = a.min
		case AggMax:
			b.Value = a.max
		case AggCount:
			b.Value = float64(a.count)
		case AggSum:
			b.Value = a.sum
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts < out[j].Ts })
	return out, nil
}

// snapshotSegments 返回目标序列的段快照（sealed + active），并 flush
// 活动段缓冲。库已关闭返回 ErrClosed；未知序列返回 (nil, nil)。
func (db *DB) snapshotSegments(metric string, tags map[string]string) ([]*segment, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, ErrClosed
	}
	s := db.series[seriesID(metric, tags)]
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active.bw != nil {
		_ = s.active.bw.Flush()
	}
	segs := make([]*segment, 0, len(s.sealed)+1)
	segs = append(segs, s.sealed...)
	if s.active != nil {
		segs = append(segs, s.active)
	}
	return segs, nil
}

// readSegmentRange 读取段内时间窗口 [from, to) 的记录（from/to=0 不限）。
// 尾部残缺记录忽略；rollup 段返回桶代表点（Value=avg）。
func readSegmentRange(seg *segment, metric string, from, to int64) ([]Point, error) {
	f, err := os.Open(seg.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(segmentHeaderLen, io.SeekStart); err != nil {
		return nil, err
	}
	recLen := recordLen(seg.kind)
	buf := make([]byte, recLen)
	var out []Point
	for {
		if _, err := io.ReadFull(f, buf); err != nil {
			break // EOF / 尾部残缺
		}
		ts := int64(binary.LittleEndian.Uint64(buf[:8]))
		if from > 0 && ts < from {
			continue
		}
		if to > 0 && ts >= to {
			continue
		}
		if seg.kind == kindRollup {
			sum := math.Float64frombits(binary.LittleEndian.Uint64(buf[24:32]))
			cnt := int64(binary.LittleEndian.Uint64(buf[32:40]))
			if cnt <= 0 {
				continue
			}
			out = append(out, Point{Metric: metric, Ts: ts, Value: sum / float64(cnt)})
		} else {
			v := math.Float64frombits(binary.LittleEndian.Uint64(buf[8:16]))
			out = append(out, Point{Metric: metric, Ts: ts, Value: v})
		}
	}
	return out, nil
}

// aggPoint 是聚合读取的内部单元：raw 点为 (v, 1, v, v)；
// rollup 桶为 (sum, count, min, max)。
type aggPoint struct {
	ts    int64
	sum   float64
	count int64
	min   float64
	max   float64
}

// readSegmentAgg 读取段的聚合单元（不按时间过滤；窗口过滤由调用方 merge 完成）。
// 读取失败静默返回 nil（聚合路径容忍；罕见路径，损坏段在 Open 时已兜底）。
func readSegmentAgg(seg *segment) []aggPoint {
	f, err := os.Open(seg.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(segmentHeaderLen, io.SeekStart); err != nil {
		return nil
	}
	recLen := recordLen(seg.kind)
	buf := make([]byte, recLen)
	var out []aggPoint
	for {
		if _, err := io.ReadFull(f, buf); err != nil {
			break
		}
		ts := int64(binary.LittleEndian.Uint64(buf[:8]))
		if seg.kind == kindRollup {
			mn := math.Float64frombits(binary.LittleEndian.Uint64(buf[8:16]))
			mx := math.Float64frombits(binary.LittleEndian.Uint64(buf[16:24]))
			sum := math.Float64frombits(binary.LittleEndian.Uint64(buf[24:32]))
			cnt := int64(binary.LittleEndian.Uint64(buf[32:40]))
			if cnt <= 0 {
				continue
			}
			out = append(out, aggPoint{ts: ts, sum: sum, count: cnt, min: mn, max: mx})
		} else {
			v := math.Float64frombits(binary.LittleEndian.Uint64(buf[8:16]))
			out = append(out, aggPoint{ts: ts, sum: v, count: 1, min: v, max: v})
		}
	}
	return out
}
