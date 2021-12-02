// Copyright 2020 PingCAP, Inc.
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
	"context"

	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/ticdc/cdc/processor/pipeline/system"
	"github.com/pingcap/ticdc/cdc/puller"
	"github.com/pingcap/ticdc/pkg/actor"
	"github.com/pingcap/ticdc/pkg/actor/message"
	"github.com/pingcap/ticdc/pkg/config"
	cdcContext "github.com/pingcap/ticdc/pkg/context"
	"github.com/pingcap/ticdc/pkg/pipeline"
	"github.com/pingcap/ticdc/pkg/regionspan"
	"github.com/pingcap/ticdc/pkg/util"
	"github.com/tikv/client-go/v2/oracle"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type pullerNode struct {
	tableName string // quoted schema and table, used in metircs only

	tableID     model.TableID
	replicaInfo *model.TableReplicaInfo
	cancel      context.CancelFunc
	wg          *errgroup.Group

	outputCh         chan pipeline.Message
	tableActorRouter *actor.Router
	isTableActorMode bool
	tableActorID     actor.ID
}

func newPullerNode(
	tableID model.TableID, replicaInfo *model.TableReplicaInfo, tableName string) pipeline.Node {
	return &pullerNode{
		tableID:     tableID,
		replicaInfo: replicaInfo,
		tableName:   tableName,
		outputCh:    make(chan pipeline.Message, defaultOutputChannelSize),
	}
}

func (n *pullerNode) tableSpan(config *config.ReplicaConfig) []regionspan.Span {
	// start table puller
	spans := make([]regionspan.Span, 0, 4)
	spans = append(spans, regionspan.GetTableSpan(n.tableID))

	if config.Cyclic.IsEnabled() && n.replicaInfo.MarkTableID != 0 {
		spans = append(spans, regionspan.GetTableSpan(n.replicaInfo.MarkTableID))
	}
	return spans
}

func (n *pullerNode) Init(ctx pipeline.NodeContext) error {
	return n.StartActorNode(ctx, nil, new(errgroup.Group), ctx.ChangefeedVars(), ctx.GlobalVars())
}

func (n *pullerNode) StartActorNode(ctx context.Context, tableActorRouter *actor.Router, wg *errgroup.Group, info *cdcContext.ChangefeedVars, vars *cdcContext.GlobalVars) error {
	n.wg = wg
	if tableActorRouter != nil {
		n.isTableActorMode = true
		n.tableActorRouter = tableActorRouter
		n.tableActorID = system.ActorID(info.ID, n.tableID)
	}
	metricTableResolvedTsGauge := tableResolvedTsGauge.WithLabelValues(info.ID, vars.CaptureInfo.AdvertiseAddr, n.tableName)
	ctxC, cancel := context.WithCancel(ctx)
	ctxC = util.PutTableInfoInCtx(ctxC, n.tableID, n.tableName)
	// NOTICE: always pull the old value internally
	// See also: https://github.com/pingcap/ticdc/issues/2301.
	plr := puller.NewPuller(ctxC, vars.PDClient, vars.GrpcPool, vars.RegionCache, vars.KVStorage,
		n.replicaInfo.StartTs, n.tableSpan(info.Info.Config), true)
	n.wg.Go(func() error {
		err := plr.Run(ctxC)
		if err != nil {
			log.Error("puller stopped", zap.Error(err))
		}
		if n.isTableActorMode {
			_ = tableActorRouter.SendB(ctxC, n.tableActorID, message.StopMessage())
		} else {
			ctx.(pipeline.NodeContext).Throw(err)
		}
		return nil
	})
	n.wg.Go(func() error {
		for {
			select {
			case <-ctxC.Done():
				return nil
			case rawKV := <-plr.Output():
				if rawKV == nil {
					continue
				}
				if rawKV.OpType == model.OpTypeResolved {
					metricTableResolvedTsGauge.Set(float64(oracle.ExtractPhysical(rawKV.CRTs)))
				}
				pEvent := model.NewPolymorphicEvent(rawKV)
				msg := pipeline.PolymorphicEventMessage(pEvent)
				if n.isTableActorMode {
					n.outputCh <- msg
					_ = tableActorRouter.Send(n.tableActorID, message.TickMessage())
				} else {
					ctx.(pipeline.NodeContext).SendToNextNode(msg)
				}
			}
		}
	})
	n.cancel = cancel
	return nil
}

// Receive receives the message from the previous node
func (n *pullerNode) Receive(ctx pipeline.NodeContext) error {
	// just forward any messages to the next node
	_, err := n.TryHandleDataMessage(ctx, ctx.Message())
	return err
}

func (n *pullerNode) TryHandleDataMessage(ctx context.Context, msg pipeline.Message) (bool, error) {
	return trySendMessageToNextNode(ctx, n.isTableActorMode, n.outputCh, msg), nil
}

func (n *pullerNode) Destroy(ctx pipeline.NodeContext) error {
	tableResolvedTsGauge.DeleteLabelValues(ctx.ChangefeedVars().ID, ctx.GlobalVars().CaptureInfo.AdvertiseAddr, n.tableName)
	n.cancel()
	return n.wg.Wait()
}

func (n *pullerNode) TryGetProcessedMessage() *pipeline.Message {
	return tryGetProcessedMessageFromChan(n.outputCh)
}
