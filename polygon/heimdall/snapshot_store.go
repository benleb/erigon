package heimdall

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon/turbo/snapshotsync"
	"golang.org/x/sync/errgroup"
)

func NewSnapshotStore(base Store, snapshots *RoSnapshots) *SnapshotStore {
	return &SnapshotStore{
		Store:                       base,
		checkpoints:                 base.Checkpoints(),
		milestones:                  base.Milestones(),
		spans:                       &spanSnapshotStore{base.Spans(), snapshots},
		spanBlockProducerSelections: base.SpanBlockProducerSelections(),
	}
}

type SnapshotStore struct {
	Store
	checkpoints                 EntityStore[*Checkpoint]
	milestones                  EntityStore[*Milestone]
	spans                       EntityStore[*Span]
	spanBlockProducerSelections EntityStore[*SpanBlockProducerSelection]
}

func (s *SnapshotStore) Checkpoints() EntityStore[*Checkpoint] {
	return s.checkpoints
}

func (s *SnapshotStore) Milestones() EntityStore[*Milestone] {
	return s.milestones
}

func (s *SnapshotStore) Spans() EntityStore[*Span] {
	return s.spans
}

func (s *SnapshotStore) SpanBlockProducerSelections() EntityStore[*SpanBlockProducerSelection] {
	return s.spanBlockProducerSelections
}

func (s *SnapshotStore) Prepare(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error { return s.checkpoints.Prepare(ctx) })
	eg.Go(func() error { return s.milestones.Prepare(ctx) })
	eg.Go(func() error { return s.spans.Prepare(ctx) })
	eg.Go(func() error { return s.spanBlockProducerSelections.Prepare(ctx) })
	return eg.Wait()
}

var ErrSpanNotFound = errors.New("span not found")

type spanSnapshotStore struct {
	EntityStore[*Span]
	snapshots *RoSnapshots
}

func (s *spanSnapshotStore) Prepare(ctx context.Context) error {
	if err := s.EntityStore.Prepare(ctx); err != nil {
		return err
	}

	return <-s.snapshots.Ready(ctx)
}

func (s *spanSnapshotStore) WithTx(tx kv.Tx) EntityStore[*Span] {
	return &spanSnapshotStore{txEntityStore[*Span]{s.EntityStore.(*mdbxEntityStore[*Span]), tx}, s.snapshots}
}

func (r *spanSnapshotStore) LastFrozenEntityId() uint64 {
	if r.snapshots == nil {
		return 0
	}

	segments, release := r.snapshots.ViewType(r.SnapType())
	defer release()

	if len(segments) == 0 {
		return 0
	}
	// find the last segment which has a built index
	var lastSegment *snapshotsync.Segment
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i].Index() != nil {
			lastSegment = segments[i]
			break
		}
	}
	if lastSegment == nil {
		return 0
	}

	lastSpanID := SpanIdAt(lastSegment.To())
	if lastSpanID > 0 {
		lastSpanID--
	}
	return uint64(lastSpanID)
}

func (r *spanSnapshotStore) Entity(ctx context.Context, id uint64) (*Span, bool, error) {
	var endBlock uint64
	if id > 0 {
		endBlock = SpanEndBlockNum(SpanId(id))
	}

	maxBlockNumInFiles := r.snapshots.BlocksAvailable()
	if maxBlockNumInFiles == 0 || endBlock > maxBlockNumInFiles {
		return r.EntityStore.Entity(ctx, id)
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], id)

	segments, release := r.snapshots.ViewType(r.SnapType())
	defer release()

	for i := len(segments) - 1; i >= 0; i-- {
		sn := segments[i]
		idx := sn.Index()

		if idx == nil {
			continue
		}
		spanFrom := uint64(SpanIdAt(sn.From()))
		if id < spanFrom {
			continue
		}
		spanTo := uint64(SpanIdAt(sn.To()))
		if id >= spanTo {
			continue
		}
		if idx.KeyCount() == 0 {
			continue
		}
		offset := idx.OrdinalLookup(id - idx.BaseDataID())
		gg := sn.MakeGetter()
		gg.Reset(offset)
		result, _ := gg.Next(nil)

		var span Span
		if err := json.Unmarshal(result, &span); err != nil {
			return nil, false, err
		}

		return &span, true, nil
	}

	err := fmt.Errorf("span %d not found (snapshots)", id)
	return nil, false, fmt.Errorf("%w: %w", ErrSpanNotFound, err)
}

func (r *spanSnapshotStore) LastEntityId(ctx context.Context) (uint64, bool, error) {
	lastId, ok, err := r.EntityStore.LastEntityId(ctx)

	snapshotLastId := r.LastFrozenEntityId()
	if snapshotLastId > lastId {
		return snapshotLastId, true, nil
	}

	return lastId, ok, err
}

type milestoneSnapshotStore struct {
	EntityStore[*Milestone]
	snapshots *RoSnapshots
}

func (r *milestoneSnapshotStore) LastFrozenEntityId() uint64 {
	if r.snapshots == nil {
		return 0
	}

	segments, release := r.snapshots.ViewType(r.SnapType())
	defer release()

	if len(segments) == 0 {
		return 0
	}
	// find the last segment which has a built index
	var lastSegment *snapshotsync.Segment
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i].Index() != nil {
			lastSegment = segments[i]
			break
		}
	}
	if lastSegment == nil {
		return 0
	}

	index := lastSegment.Index()

	return index.BaseDataID() + index.KeyCount() - 1
}

func (r *milestoneSnapshotStore) LastEntityId(ctx context.Context) (uint64, bool, error) {
	lastId, ok, err := r.EntityStore.LastEntityId(ctx)

	snapshotLastId := r.LastFrozenEntityId()
	if snapshotLastId > lastId {
		return snapshotLastId, true, nil
	}

	return lastId, ok, err
}

func (r *milestoneSnapshotStore) Entity(ctx context.Context, id uint64) (*Milestone, bool, error) {
	entity, ok, err := r.EntityStore.Entity(ctx, id)

	if ok {
		return entity, ok, nil
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], id)

	segments, release := r.snapshots.ViewType(r.SnapType())
	defer release()

	for i := len(segments) - 1; i >= 0; i-- {
		sn := segments[i]
		idx := sn.Index()

		if idx == nil {
			continue
		}

		if idx.KeyCount() == 0 {
			continue
		}

		if id < idx.BaseDataID() {
			continue
		}

		offset := idx.OrdinalLookup(id - idx.BaseDataID())
		gg := sn.MakeGetter()
		gg.Reset(offset)
		result, _ := gg.Next(nil)

		var entity Milestone
		if err := json.Unmarshal(result, &entity); err != nil {
			return nil, false, err
		}

		return &entity, true, nil
	}

	err = fmt.Errorf("milestone %d not found", id)
	return nil, false, fmt.Errorf("%w: %w", ErrMilestoneNotFound, err)
}

type checkpointSnapshotStore struct {
	EntityStore[*Checkpoint]
	snapshots *RoSnapshots
}

func (r *checkpointSnapshotStore) LastCheckpointId(ctx context.Context, tx kv.Tx) (uint64, bool, error) {
	lastId, ok, err := r.EntityStore.LastEntityId(ctx)

	snapshotLastCheckpointId := r.LastFrozenEntityId()

	if snapshotLastCheckpointId > lastId {
		return snapshotLastCheckpointId, true, nil
	}

	return lastId, ok, err
}

func (r *checkpointSnapshotStore) Entity(ctx context.Context, id uint64) (*Checkpoint, bool, error) {
	entity, ok, err := r.EntityStore.Entity(ctx, id)

	if ok {
		return entity, ok, nil
	}

	segments, release := r.snapshots.ViewType(r.SnapType())
	defer release()

	for i := len(segments) - 1; i >= 0; i-- {
		sn := segments[i]
		index := sn.Index()

		if index == nil || index.KeyCount() == 0 || id < index.BaseDataID() {
			continue
		}

		offset := index.OrdinalLookup(id - index.BaseDataID())
		gg := sn.MakeGetter()
		gg.Reset(offset)
		result, _ := gg.Next(nil)

		var entity Checkpoint
		if err := json.Unmarshal(result, &entity); err != nil {
			return nil, false, err
		}

		return &entity, true, nil
	}

	err = fmt.Errorf("checkpoint %d not found", id)
	return nil, false, fmt.Errorf("%w: %w", ErrCheckpointNotFound, err)
}

func (r *checkpointSnapshotStore) LastFrozenEntityId() uint64 {
	if r.snapshots == nil {
		return 0
	}

	segments, release := r.snapshots.ViewType(Checkpoints)
	defer release()
	if len(segments) == 0 {
		return 0
	}
	// find the last segment which has a built index
	var lastSegment *snapshotsync.Segment
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i].Index() != nil {
			lastSegment = segments[i]
			break
		}
	}

	if lastSegment == nil {
		return 0
	}

	index := lastSegment.Index()

	return index.BaseDataID() + index.KeyCount() - 1
}
