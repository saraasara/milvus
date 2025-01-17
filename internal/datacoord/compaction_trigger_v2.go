package datacoord

import (
	"context"

	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/pkg/log"
)

type CompactionTriggerType int8

const (
	TriggerTypeLevelZeroViewChange CompactionTriggerType = iota + 1
	TriggerTypeLevelZeroViewIDLE
	TriggerTypeSegmentSizeViewChange
)

type TriggerManager interface {
	Notify(UniqueID, CompactionTriggerType, []CompactionView)
}

// CompactionTriggerManager registers Triggers to TriggerType
// so that when the certain TriggerType happens, the corresponding triggers can
// trigger the correct compaction plans.
// Trigger types:
// 1. Change of Views
//   - LevelZeroViewTrigger
//   - SegmentSizeViewTrigger
//
// 2. SystemIDLE & schedulerIDLE
// 3. Manual Compaction
type CompactionTriggerManager struct {
	scheduler Scheduler
	handler   compactionPlanContext // TODO replace with scheduler

	allocator allocator
}

func NewCompactionTriggerManager(alloc allocator, handler compactionPlanContext) *CompactionTriggerManager {
	m := &CompactionTriggerManager{
		allocator: alloc,
		handler:   handler,
	}

	return m
}

func (m *CompactionTriggerManager) Notify(taskID UniqueID, eventType CompactionTriggerType, views []CompactionView) {
	log := log.With(zap.Int64("taskID", taskID))
	for _, view := range views {
		switch eventType {
		case TriggerTypeLevelZeroViewChange:
			log.Debug("Start to trigger a level zero compaction by TriggerTypeLevelZeroViewChange")
			outView, reason := view.Trigger()
			if outView != nil {
				log.Info("Success to trigger a LevelZeroCompaction output view, try to sumit",
					zap.String("reason", reason),
					zap.String("output view", outView.String()))
				m.SubmitL0ViewToScheduler(taskID, outView)
			}

		case TriggerTypeLevelZeroViewIDLE:
			log.Debug("Start to trigger a level zero compaction by TriggerTypLevelZeroViewIDLE")
			outView, reason := view.Trigger()
			if outView == nil {
				log.Info("Start to force trigger a level zero compaction by TriggerTypLevelZeroViewIDLE")
				outView, reason = view.ForceTrigger()
			}

			if outView != nil {
				log.Info("Success to trigger a LevelZeroCompaction output view, try to submit",
					zap.String("reason", reason),
					zap.String("output view", outView.String()))
				m.SubmitL0ViewToScheduler(taskID, outView)
			}
		}
	}
}

func (m *CompactionTriggerManager) SubmitL0ViewToScheduler(taskID int64, outView CompactionView) {
	plan := m.buildL0CompactionPlan(outView)
	if plan == nil {
		return
	}

	label := outView.GetGroupLabel()
	signal := &compactionSignal{
		id:           taskID,
		isForce:      false,
		isGlobal:     true,
		collectionID: label.CollectionID,
		partitionID:  label.PartitionID,
		pos:          outView.(*LevelZeroSegmentsView).earliestGrowingSegmentPos,
	}

	// TODO, remove handler, use scheduler
	// m.scheduler.Submit(plan)
	m.handler.execCompactionPlan(signal, plan)
	log.Info("Finish to submit a LevelZeroCompaction plan",
		zap.Int64("taskID", taskID),
		zap.Int64("planID", plan.GetPlanID()),
		zap.String("type", plan.GetType().String()),
	)
}

func (m *CompactionTriggerManager) buildL0CompactionPlan(view CompactionView) *datapb.CompactionPlan {
	var segmentBinlogs []*datapb.CompactionSegmentBinlogs
	levelZeroSegs := lo.Map(view.GetSegmentsView(), func(segView *SegmentView, _ int) *datapb.CompactionSegmentBinlogs {
		return &datapb.CompactionSegmentBinlogs{
			SegmentID:    segView.ID,
			Level:        datapb.SegmentLevel_L0,
			CollectionID: view.GetGroupLabel().CollectionID,
			PartitionID:  view.GetGroupLabel().PartitionID,
			// Deltalogs:   deltalogs are filled before executing the plan
		}
	})
	segmentBinlogs = append(segmentBinlogs, levelZeroSegs...)

	plan := &datapb.CompactionPlan{
		Type:           datapb.CompactionType_Level0DeleteCompaction,
		SegmentBinlogs: segmentBinlogs,
		Channel:        view.GetGroupLabel().Channel,
	}

	if err := fillOriginPlan(m.allocator, plan); err != nil {
		return nil
	}

	return plan
}

// chanPartSegments is an internal result struct, which is aggregates of SegmentInfos with same collectionID, partitionID and channelName
type chanPartSegments struct {
	collectionID UniqueID
	partitionID  UniqueID
	channelName  string
	segments     []*SegmentInfo
}

func fillOriginPlan(alloc allocator, plan *datapb.CompactionPlan) error {
	// TODO context
	id, err := alloc.allocID(context.TODO())
	if err != nil {
		return err
	}

	plan.PlanID = id
	plan.TimeoutInSeconds = Params.DataCoordCfg.CompactionTimeoutInSeconds.GetAsInt32()
	return nil
}
