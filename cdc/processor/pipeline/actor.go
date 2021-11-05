// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package pipeline

import (
	"container/list"
	"context"
	"time"

	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/entry"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/ticdc/cdc/sink"
	"github.com/pingcap/ticdc/cdc/sink/common"
	"github.com/pingcap/ticdc/pkg/actor"
	"github.com/pingcap/ticdc/pkg/actor/message"
	serverConfig "github.com/pingcap/ticdc/pkg/config"
	cdcContext "github.com/pingcap/ticdc/pkg/context"
	cerror "github.com/pingcap/ticdc/pkg/errors"
	"github.com/pingcap/ticdc/pkg/pipeline"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

var (
	defaultSystem *actor.System
	defaultRouter *actor.Router
)

func init() {
	defaultSystem, defaultRouter = actor.NewSystemBuilder("table").Build()
	defaultSystem.Start(context.Background())
}

type tableActor struct {
	cancel    context.CancelFunc
	wg        *errgroup.Group
	reportErr func(error)

	mb actor.Mailbox

	changefeedID string
	// quoted schema and table, used in metrics only
	tableName     string
	tableID       int64
	markTableID   int64
	cyclicEnabled bool
	memoryQuota   uint64
	mounter       entry.Mounter
	replicaInfo   *model.TableReplicaInfo
	sink          sink.Sink
	targetTs      model.Ts

	started bool
	stopped bool
	err     error

	info *cdcContext.ChangefeedVars
	vars *cdcContext.GlobalVars

	pullerNode *pullerNode
	sorterNode *sorterNode
	cyclicNode *cyclicMarkNode
	sinkNode   *sinkNode

	nodes []*node

	lastBarrierTsUpdateTime time.Time
}

var _ TablePipeline = (*tableActor)(nil)
var _ actor.Actor = (*tableActor)(nil)

func (t *tableActor) Poll(ctx context.Context, msgs []message.Message) bool {
	for i := range msgs {
		if t.stopped {
			// No need to handle remaining messages.
			break
		}

		switch msgs[i].Tp {
		case message.TypeStop:
			t.stop(nil)
			break
		case message.TypeTick:
		case message.TypeBarrier:
			err := t.sinkNode.HandleMessages(ctx, msgs[i])
			if err != nil {
				t.stop(err)
			}
		}
		// process message for each node
		// get message from puller
		for _, n := range t.nodes {
			n.tryRun(ctx)
		}
	}
	// Report error to processor if there is any.
	t.checkError()
	return !t.stopped
}

type node struct {
	eventStash     *pipeline.Message
	messageFetcher MessageFetcher
	messageSender  MessageSender
}

type MessageSender interface {
	TrySendMessage(ctx context.Context, message *pipeline.Message) bool
}

type sorterMessageSender struct {
	sorter *sorterNode
}

func (s *sorterMessageSender) TrySendMessage(ctx context.Context, message *pipeline.Message) bool {
	return s.sorter.sorter.TryAddEntry(ctx, message.PolymorphicEvent)
}

type cyclicMessageSender struct {
	tableActor *tableActor
	cyclic     *cyclicMarkNode
}

func (s *cyclicMessageSender) TrySendMessage(ctx context.Context, message *pipeline.Message) bool {
	sent, err := s.cyclic.handleMsg(*message, s.cyclic)
	if err != nil {
		s.tableActor.reportErr(err)
	}
	return sent
}

type sinkMessageSender struct {
	tableActor *tableActor
	sink       *sinkNode
}

func (s *sinkMessageSender) TrySendMessage(ctx context.Context, message *pipeline.Message) bool {
	if message.PolymorphicEvent.RawKV.OpType != model.OpTypeResolved && !message.PolymorphicEvent.IsPrepared() {
		return false
	}
	err := s.sink.HandleMessage(ctx, *message)
	if err != nil {
		s.tableActor.reportErr(err)
	}
	return true
}

type MessageFetcher interface {
	TryGetMessage() *pipeline.Message
}

type ChannelMessageFetcher struct {
	OutputChan chan pipeline.Message
}

func (c *ChannelMessageFetcher) TryGetMessage() *pipeline.Message {
	var msg pipeline.Message
	select {
	case msg = <-c.OutputChan:
		return &msg
	default:
		return nil
	}
}

type ListMessageFetcher struct {
	MessageList list.List
}

func (q *ListMessageFetcher) TryGetMessage() *pipeline.Message {
	el := q.MessageList.Front()
	if el == nil {
		return nil
	}
	msg := q.MessageList.Remove(el).(pipeline.Message)
	return &msg
}

func (n *node) tryRun(ctx context.Context) {
	for {
		// batch?
		if n.eventStash == nil {
			n.eventStash = n.messageFetcher.TryGetMessage()
		}
		if n.eventStash == nil {
			return
		}
		if n.messageSender.TrySendMessage(ctx, n.eventStash) {
			n.eventStash = nil
		} else {
			return
		}
	}
}

func (t *tableActor) start(ctx cdcContext.Context) error {
	if t.started {
		log.Panic("start an already started table",
			zap.String("changefeedID", t.changefeedID),
			zap.Int64("tableID", t.tableID),
			zap.String("tableName", t.tableName))
	}
	log.Debug("creating table flow controller",
		zap.String("changefeedID", t.changefeedID),
		zap.Int64("tableID", t.tableID),
		zap.String("tableName", t.tableName),
		zap.Uint64("quota", t.memoryQuota))

	t.pullerNode = newPullerNode(t.tableID, t.replicaInfo, t.tableName).(*pullerNode)
	if err := t.pullerNode.Start(ctx, true, t.wg, t.info, t.vars); err != nil {
		log.Error("puller fails to start", zap.Error(err))
		return err
	}

	flowController := common.NewTableFlowController(t.memoryQuota)
	t.sorterNode = newSorterNode(t.tableName, t.tableID, t.replicaInfo.StartTs, flowController, t.mounter)
	if err := t.sorterNode.Start(ctx, true, t.wg, t.info, t.vars); err != nil {
		log.Error("sorter fails to start", zap.Error(err))
		return err
	}
	t.nodes = append(t.nodes,
		&node{
			messageFetcher: &ChannelMessageFetcher{OutputChan: t.pullerNode.outputCh},
			messageSender:  &sorterMessageSender{sorter: t.sorterNode},
		})

	if t.cyclicEnabled {
		t.cyclicNode = newCyclicMarkNode(t.replicaInfo.MarkTableID).(*cyclicMarkNode)
		if err := t.cyclicNode.Start(true, t.info); err != nil {
			log.Error("sink fails to start", zap.Error(err))
			return err
		}
		t.nodes = append(t.nodes, &node{
			messageFetcher: &ChannelMessageFetcher{OutputChan: t.sorterNode.outputCh},
			messageSender:  &cyclicMessageSender{tableActor: t, cyclic: t.cyclicNode},
		},
		)
	}

	t.sinkNode = newSinkNode(t.sink, t.replicaInfo.StartTs, t.targetTs, flowController)
	if err := t.sinkNode.Start(ctx, t.info, t.vars); err != nil {
		log.Error("sink fails to start", zap.Error(err))
		return err
	}
	if t.cyclicEnabled {
		t.nodes = append(t.nodes, &node{
			messageFetcher: &ListMessageFetcher{MessageList: t.cyclicNode.queue},
			messageSender:  &sinkMessageSender{tableActor: t, sink: t.sinkNode},
		})
	} else {
		t.nodes = append(t.nodes, &node{
			messageFetcher: &ChannelMessageFetcher{OutputChan: t.sorterNode.outputCh},
			messageSender:  &sinkMessageSender{tableActor: t, sink: t.sinkNode},
		})
	}
	t.started = true
	log.Info("table actor is started", zap.Int64("tableID", t.tableID))
	return nil
}

func (t *tableActor) stop(err error) {
	if t.stopped {
		return
	}
	t.stopped = true
	t.err = err
	t.cancel()
	log.Info("table actor will be stopped",
		zap.Int64("tableID", t.tableID), zap.Error(err))
}

func (t *tableActor) checkError() {
	if t.err != nil {
		t.reportErr(t.err)
		t.err = nil
	}
}

// ============ Implement TablePipline, must be threadsafe ============

// ResolvedTs returns the resolved ts in this table pipeline
func (t *tableActor) ResolvedTs() model.Ts {
	return t.sinkNode.ResolvedTs()
}

// CheckpointTs returns the checkpoint ts in this table pipeline
func (t *tableActor) CheckpointTs() model.Ts {
	return t.sinkNode.CheckpointTs()
}

// UpdateBarrierTs updates the barrier ts in this table pipeline
func (t *tableActor) UpdateBarrierTs(ts model.Ts) {
	if t.sinkNode.barrierTs != ts || t.lastBarrierTsUpdateTime.Add(time.Minute).Before(time.Now()) {
		msg := message.BarrierMessage(ts)
		err := defaultRouter.Send(actor.ID(t.tableID), msg)
		if err != nil {
			log.Warn("send fails", zap.Reflect("msg", msg), zap.Error(err))
		}
		t.lastBarrierTsUpdateTime = time.Now()
	}
}

// AsyncStop tells the pipeline to stop, and returns true is the pipeline is already stopped.
func (t *tableActor) AsyncStop(targetTs model.Ts) bool {
	msg := message.StopMessage()
	err := defaultRouter.Send(actor.ID(t.tableID), msg)
	log.Info("send async stop signal to table", zap.Int64("tableID", t.tableID), zap.Uint64("targetTs", targetTs))
	if err != nil {
		if cerror.ErrMailboxFull.Equal(err) {
			return false
		}
		if cerror.ErrSendToClosedPipeline.Equal(err) {
			return true
		}
		log.Panic("send fails", zap.Reflect("msg", msg), zap.Error(err))
	}
	return true
}

// Workload returns the workload of this table
func (t *tableActor) Workload() model.WorkloadInfo {
	// We temporarily set the value to constant 1
	return workload
}

// Status returns the status of this table pipeline
func (t *tableActor) Status() TableStatus {
	return t.sinkNode.Status()
}

// ID returns the ID of source table and mark table
func (t *tableActor) ID() (tableID, markTableID int64) {
	return t.tableID, t.markTableID
}

// Name returns the quoted schema and table name
func (t *tableActor) Name() string {
	return t.tableName
}

// Cancel stops this table actor immediately and destroy all resources
// created by this table pipeline
func (t *tableActor) Cancel() {
	// TODO(neil): pass context.
	if err := t.mb.SendB(context.TODO(), message.StopMessage()); err != nil {
		log.Warn("fails to send Stop message",
			zap.Uint64("tableID", uint64(t.tableID)))
	}
}

// Wait waits for table pipeline destroyed
func (t *tableActor) Wait() {
	_ = t.wg.Wait()
}

// NewTableActor creates a table actor.
func NewTableActor(cdcCtx cdcContext.Context,
	mounter entry.Mounter,
	tableID model.TableID,
	tableName string,
	replicaInfo *model.TableReplicaInfo,
	sink sink.Sink,
	targetTs model.Ts,
) (TablePipeline, error) {
	config := cdcCtx.ChangefeedVars().Info.Config
	cyclicEnabled := config.Cyclic != nil && config.Cyclic.IsEnabled()
	info := cdcCtx.ChangefeedVars()
	vars := cdcCtx.GlobalVars()

	mb := actor.NewMailbox(actor.ID(tableID), defaultOutputChannelSize)
	// All sub-goroutines should be spawn in the wait group.
	wg, wgCtx := errgroup.WithContext(cdcCtx)
	// Cancel should be able to release all sub-goroutines in the actor.
	_, cancel := context.WithCancel(wgCtx)
	table := &tableActor{
		reportErr: cdcCtx.Throw,
		mb:        mb,
		wg:        wg,
		cancel:    cancel,

		tableID:       tableID,
		markTableID:   replicaInfo.MarkTableID,
		tableName:     tableName,
		cyclicEnabled: cyclicEnabled,
		memoryQuota:   serverConfig.GetGlobalServerConfig().PerTableMemoryQuota,
		mounter:       mounter,
		replicaInfo:   replicaInfo,
		sink:          sink,
		targetTs:      targetTs,
		started:       false,

		info: info,
		vars: vars,
	}

	log.Info("spawn and start table actor", zap.Int64("tableID", tableID))
	if err := table.start(cdcCtx); err != nil {
		table.stop(err)
		return nil, err
	}
	err := defaultSystem.Spawn(mb, table)
	if err != nil {
		return nil, err
	}
	log.Info("spawn and start table actor done", zap.Int64("tableID", tableID))
	return table, nil
}