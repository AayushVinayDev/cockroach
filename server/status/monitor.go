// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Matt Tracy (matt.r.tracy@gmail.com)

package status

import (
	"sync"

	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/storage"
	"github.com/cockroachdb/cockroach/util"
)

// StoreStatusMonitor monitors the status of a single store on the server.
// Status information is collected from event feeds provided by lower level
// components.
type StoreStatusMonitor struct {
	rangeDataAccumulator
	ID proto.StoreID
}

// NodeStatusMonitor monitors the status of a server node. Status information
// is collected from event feeds provided by lower level components.
//
// This structure contains collections of other StatusMonitor types which monitor
// interesting subsets of data on the node. NodeStatusMonitor is responsible
// for passing event feed data to these subset structures for accumulation.
type NodeStatusMonitor struct {
	sync.RWMutex
	stores map[proto.StoreID]*StoreStatusMonitor
}

// NewNodeStatusMonitor initializes a new NodeStatusMonitor instance.
func NewNodeStatusMonitor() *NodeStatusMonitor {
	return &NodeStatusMonitor{
		stores: make(map[proto.StoreID]*StoreStatusMonitor),
	}
}

// GetStoreMonitor is a helper method which retrieves the StoreStatusMonitor for the
// given StoreID, creating it if it does not already exist.
func (nsm *NodeStatusMonitor) GetStoreMonitor(id proto.StoreID) *StoreStatusMonitor {
	nsm.RLock()
	s, ok := nsm.stores[id]
	nsm.RUnlock()
	if ok {
		return s
	}

	// Rare case where store did not already exist, we need to take an actual
	// lock.
	nsm.Lock()
	defer nsm.Unlock()
	if s, ok := nsm.stores[id]; ok {
		return s
	}
	s = &StoreStatusMonitor{
		ID: id,
	}
	nsm.stores[id] = s
	return s
}

// VisitStoreMonitors calls the supplied visitor function with every
// StoreStatusMonitor currently in this monitor's collection. A lock is taken on
// each StoreStatusMonitor before it is passed to the visitor function.
func (nsm *NodeStatusMonitor) VisitStoreMonitors(visitor func(*StoreStatusMonitor)) {
	nsm.RLock()
	defer nsm.RUnlock()
	for _, ssm := range nsm.stores {
		ssm.Lock()
		visitor(ssm)
		ssm.Unlock()
	}
}

// StartMonitorFeed starts a goroutine which processes events published to the
// supplied Subscription. The goroutine will continue running until the
// Subscription's Events feed is closed.
func (nsm *NodeStatusMonitor) StartMonitorFeed(feed *util.Feed) {
	sub := feed.Subscribe()
	go storage.ProcessStoreEvents(nsm, sub)
}

// OnAddRange receives AddRangeEvents retrieved from an storage event
// subscription. This method is part of the implementation of
// store.StoreEventListener.
func (nsm *NodeStatusMonitor) OnAddRange(event *storage.AddRangeEvent) {
	nsm.GetStoreMonitor(event.StoreID).addRange(event)
}

// OnUpdateRange receives UpdateRangeEvents retrieved from an storage event
// subscription. This method is part of the implementation of
// store.StoreEventListener.
func (nsm *NodeStatusMonitor) OnUpdateRange(event *storage.UpdateRangeEvent) {
	nsm.GetStoreMonitor(event.StoreID).updateRange(event)
}

// OnRemoveRange receives RemoveRangeEvents retrieved from an storage event
// subscription. This method is part of the implementation of
// store.StoreEventListener.
func (nsm *NodeStatusMonitor) OnRemoveRange(event *storage.RemoveRangeEvent) {
	nsm.GetStoreMonitor(event.StoreID).removeRange(event)
}

// OnSplitRange receives SplitRangeEvents retrieved from an storage event
// subscription. This method is part of the implementation of
// store.StoreEventListener.
func (nsm *NodeStatusMonitor) OnSplitRange(event *storage.SplitRangeEvent) {
	nsm.GetStoreMonitor(event.StoreID).splitRange(event)
}

// OnMergeRange receives MergeRangeEvents retrieved from an storage event
// subscription. This method is part of the implementation of
// store.StoreEventListener.
func (nsm *NodeStatusMonitor) OnMergeRange(event *storage.MergeRangeEvent) {
	nsm.GetStoreMonitor(event.StoreID).mergeRange(event)
}

// OnStartStore receives StartStoreEvents retrieved from an storage event
// subscription. This method is part of the implementation of
// store.StoreEventListener.
func (nsm *NodeStatusMonitor) OnStartStore(event *storage.StartStoreEvent) {
	nsm.GetStoreMonitor(event.StoreID)
}

// OnBeginScanRanges receives BeginScanRangesEvents retrieved from an storage
// event subscription. This method is part of the implementation of
// store.StoreEventListener.
func (nsm *NodeStatusMonitor) OnBeginScanRanges(event *storage.BeginScanRangesEvent) {
	nsm.GetStoreMonitor(event.StoreID).beginScanRanges(event)
}

// OnEndScanRanges receives EndScanRangesEvents retrieved from an storage event
// subscription. This method is part of the implementation of
// store.StoreEventListener.
func (nsm *NodeStatusMonitor) OnEndScanRanges(event *storage.EndScanRangesEvent) {
	nsm.GetStoreMonitor(event.StoreID).endScanRanges(event)
}

// rangeDataAccumulator maintains a set of accumulated stats for a set of
// ranges, computed from an incoming stream of storage events. Stats will be
// changed by any events sent to this type; higher level components are
// responsible for selecting the specific ranges accumulated by a
// rangeDataAccumulator instance.
type rangeDataAccumulator struct {
	sync.Mutex
	stats      proto.MVCCStats
	rangeCount int64
	// 'scanning' is a special mode used to initialize a rangeDataAccumulator.
	// During typical operation stats are monitored using per-operation deltas;
	// however, when a rangeDataAccumulator is initialized it must first read
	// the total value of all stats at the time when it is created.
	//
	// The scanning mode is used to facilitate this: the underlying store will
	// initiate a scan with "beginScanRanges", and then send an AddRangeEvent
	// for each range in the store.
	//
	// During a scan it is not possible for ranges to be added, removed, split
	// or merged; however, it is possible for UpdateRangeEvents to occur during
	// a scan. The seenScan collection is used to properly handle
	// UpdateRangeEvents in this case.
	isScanning bool
	seenScan   map[int64]struct{}
}

func (rda *rangeDataAccumulator) addRange(event *storage.AddRangeEvent) {
	rda.Lock()
	defer rda.Unlock()
	if rda.isScanning {
		rda.seenScan[event.Desc.RaftID] = struct{}{}
		rda.rangeCount++
		rda.stats.Add(&event.Stats)
	}
}

func (rda *rangeDataAccumulator) updateRange(event *storage.UpdateRangeEvent) {
	rda.Lock()
	defer rda.Unlock()
	if rda.isScanning {
		// Skip if we are in an active scan and have not yet accumulated the
		// data for this range.
		if _, seen := rda.seenScan[event.Desc.RaftID]; !seen {
			return
		}
	}
	rda.stats.Add(&event.Delta)
}

func (rda *rangeDataAccumulator) removeRange(event *storage.RemoveRangeEvent) {
	rda.Lock()
	defer rda.Unlock()
	rda.stats.Subtract(&event.Stats)
	rda.rangeCount--
}

func (rda *rangeDataAccumulator) splitRange(event *storage.SplitRangeEvent) {
	rda.Lock()
	defer rda.Unlock()
	rda.rangeCount++
}

func (rda *rangeDataAccumulator) mergeRange(event *storage.MergeRangeEvent) {
	rda.Lock()
	defer rda.Unlock()
	rda.rangeCount--
}

func (rda *rangeDataAccumulator) beginScanRanges(event *storage.BeginScanRangesEvent) {
	rda.Lock()
	defer rda.Unlock()
	rda.isScanning = true
	rda.stats = proto.MVCCStats{}
	rda.rangeCount = 0
	rda.seenScan = make(map[int64]struct{})
}

func (rda *rangeDataAccumulator) endScanRanges(event *storage.EndScanRangesEvent) {
	rda.Lock()
	defer rda.Unlock()
	rda.isScanning = false
	rda.seenScan = nil
}
