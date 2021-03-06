// Copyright 2017 PingCAP, Inc.
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

package server

import (
	"fmt"
	"io"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/pd/server/core"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//revive:disable:unused-parameter

// notLeaderError is returned when current server is not the leader and not possible to process request.
// TODO: work as proxy.
var notLeaderError = status.Errorf(codes.Unavailable, "not leader")

// GetMembers implements gRPC PDServer.
func (s *Server) GetMembers(context.Context, *pdpb.GetMembersRequest) (*pdpb.GetMembersResponse, error) {
	if s.isClosed() {
		return nil, status.Errorf(codes.Unknown, "server not started")
	}
	members, err := GetMembers(s.GetClient())
	if err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}

	var etcdLeader *pdpb.Member
	leadID := s.GetEtcdLeader()
	for _, m := range members {
		if m.MemberId == leadID {
			etcdLeader = m
			break
		}
	}

	return &pdpb.GetMembersResponse{
		Header:     s.header(),
		Members:    members,
		Leader:     s.GetLeader(),
		EtcdLeader: etcdLeader,
	}, nil
}

// Tso implements gRPC PDServer.
func (s *Server) Tso(stream pdpb.PD_TsoServer) error {
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.WithStack(err)
		}
		if err = s.validateRequest(request.GetHeader()); err != nil {
			return err
		}
		count := request.GetCount()
		ts, err := s.getRespTS(count)
		if err != nil {
			return status.Errorf(codes.Unknown, err.Error())
		}
		response := &pdpb.TsoResponse{
			Header:    s.header(),
			Timestamp: &ts,
			Count:     count,
		}
		if err := stream.Send(response); err != nil {
			return errors.WithStack(err)
		}
	}
}

// Bootstrap implements gRPC PDServer.
func (s *Server) Bootstrap(ctx context.Context, request *pdpb.BootstrapRequest) (*pdpb.BootstrapResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster != nil {
		err := &pdpb.Error{
			Type:    pdpb.ErrorType_ALREADY_BOOTSTRAPPED,
			Message: "cluster is already bootstrapped",
		}
		return &pdpb.BootstrapResponse{
			Header: s.errorHeader(err),
		}, nil
	}
	if _, err := s.bootstrapCluster(request); err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.BootstrapResponse{
		Header: s.header(),
	}, nil
}

// IsBootstrapped implements gRPC PDServer.
func (s *Server) IsBootstrapped(ctx context.Context, request *pdpb.IsBootstrappedRequest) (*pdpb.IsBootstrappedResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	return &pdpb.IsBootstrappedResponse{
		Header:       s.header(),
		Bootstrapped: cluster != nil,
	}, nil
}

// AllocID implements gRPC PDServer.
func (s *Server) AllocID(ctx context.Context, request *pdpb.AllocIDRequest) (*pdpb.AllocIDResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	// We can use an allocator for all types ID allocation.
	id, err := s.idAlloc.Alloc()
	if err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.AllocIDResponse{
		Header: s.header(),
		Id:     id,
	}, nil
}

// GetStore implements gRPC PDServer.
func (s *Server) GetStore(ctx context.Context, request *pdpb.GetStoreRequest) (*pdpb.GetStoreResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetStoreResponse{Header: s.notBootstrappedHeader()}, nil
	}

	store, err := cluster.GetStore(request.GetStoreId())
	if err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}
	return &pdpb.GetStoreResponse{
		Header: s.header(),
		Store:  store.Store,
	}, nil
}

// checkStore2 returns an error response if the store exists and is in tombstone state.
// It returns nil if it can't get the store.
// Copied from server/command.go
func checkStore2(cluster *RaftCluster, storeID uint64) *pdpb.Error {
	store, err := cluster.GetStore(storeID)
	if err == nil && store != nil {
		if store.GetState() == metapb.StoreState_Tombstone {
			return &pdpb.Error{
				Type:    pdpb.ErrorType_STORE_TOMBSTONE,
				Message: "store is tombstone",
			}
		}
	}
	return nil
}

// PutStore implements gRPC PDServer.
func (s *Server) PutStore(ctx context.Context, request *pdpb.PutStoreRequest) (*pdpb.PutStoreResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.PutStoreResponse{Header: s.notBootstrappedHeader()}, nil
	}

	store := request.GetStore()
	if pberr := checkStore2(cluster, store.GetId()); pberr != nil {
		return &pdpb.PutStoreResponse{
			Header: s.errorHeader(pberr),
		}, nil
	}

	if err := cluster.putStore(store); err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}

	log.Infof("put store ok - %v", store)
	cluster.cachedCluster.OnStoreVersionChange()

	return &pdpb.PutStoreResponse{
		Header: s.header(),
	}, nil
}

// GetAllStores implements gRPC PDServer.
func (s *Server) GetAllStores(ctx context.Context, request *pdpb.GetAllStoresRequest) (*pdpb.GetAllStoresResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetAllStoresResponse{Header: s.notBootstrappedHeader()}, nil
	}

	return &pdpb.GetAllStoresResponse{
		Header: s.header(),
		Stores: cluster.GetStores(),
	}, nil
}

// StoreHeartbeat implements gRPC PDServer.
func (s *Server) StoreHeartbeat(ctx context.Context, request *pdpb.StoreHeartbeatRequest) (*pdpb.StoreHeartbeatResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	if request.GetStats() == nil {
		return nil, errors.Errorf("invalid store heartbeat command, but %v", request)
	}
	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.StoreHeartbeatResponse{Header: s.notBootstrappedHeader()}, nil
	}

	if pberr := checkStore2(cluster, request.GetStats().GetStoreId()); pberr != nil {
		return &pdpb.StoreHeartbeatResponse{
			Header: s.errorHeader(pberr),
		}, nil
	}

	err := cluster.cachedCluster.handleStoreHeartbeat(request.Stats)
	if err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.StoreHeartbeatResponse{
		Header: s.header(),
	}, nil
}

const regionHeartbeatSendTimeout = 5 * time.Second

var errSendRegionHeartbeatTimeout = errors.New("send region heartbeat timeout")

// heartbeatServer wraps PD_RegionHeartbeatServer to ensure when any error
// occurs on Send() or Recv(), both endpoints will be closed.
type heartbeatServer struct {
	stream pdpb.PD_RegionHeartbeatServer
	closed int32
}

func (s *heartbeatServer) Send(m *pdpb.RegionHeartbeatResponse) error {
	if atomic.LoadInt32(&s.closed) == 1 {
		return io.EOF
	}
	done := make(chan error, 1)
	go func() { done <- s.stream.Send(m) }()
	select {
	case err := <-done:
		if err != nil {
			atomic.StoreInt32(&s.closed, 1)
		}
		return errors.WithStack(err)
	case <-time.After(regionHeartbeatSendTimeout):
		atomic.StoreInt32(&s.closed, 1)
		return errors.WithStack(errSendRegionHeartbeatTimeout)
	}
}

func (s *heartbeatServer) Recv() (*pdpb.RegionHeartbeatRequest, error) {
	if atomic.LoadInt32(&s.closed) == 1 {
		return nil, io.EOF
	}
	req, err := s.stream.Recv()
	if err != nil {
		atomic.StoreInt32(&s.closed, 1)
		return nil, errors.WithStack(err)
	}
	return req, nil
}

// RegionHeartbeat implements gRPC PDServer.
func (s *Server) RegionHeartbeat(stream pdpb.PD_RegionHeartbeatServer) error {
	server := &heartbeatServer{stream: stream}
	cluster := s.GetRaftCluster()
	if cluster == nil {
		resp := &pdpb.RegionHeartbeatResponse{
			Header: s.notBootstrappedHeader(),
		}
		err := server.Send(resp)
		return errors.WithStack(err)
	}

	var lastBind time.Time
	for {
		request, err := server.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.WithStack(err)
		}

		if err = s.validateRequest(request.GetHeader()); err != nil {
			return err
		}

		storeID := request.GetLeader().GetStoreId()
		storeLabel := strconv.FormatUint(storeID, 10)

		regionHeartbeatCounter.WithLabelValues(storeLabel, "report", "recv").Inc()
		regionHeartbeatLatency.WithLabelValues(storeLabel).Observe(float64(time.Now().Unix()) - float64(request.GetInterval().GetEndTimestamp()))

		hbStreams := cluster.coordinator.hbStreams

		if time.Since(lastBind) > s.cfg.heartbeatStreamBindInterval.Duration {
			regionHeartbeatCounter.WithLabelValues(storeLabel, "report", "bind").Inc()
			hbStreams.bindStream(storeID, server)
			lastBind = time.Now()
		}

		region := core.RegionFromHeartbeat(request)
		if region.GetID() == 0 {
			msg := fmt.Sprintf("invalid request region, %v", request)
			hbStreams.sendErr(region, pdpb.ErrorType_UNKNOWN, msg, storeLabel)
			continue
		}
		if region.GetLeader() == nil {
			msg := fmt.Sprintf("invalid request leader, %v", request)
			hbStreams.sendErr(region, pdpb.ErrorType_UNKNOWN, msg, storeLabel)
			continue
		}

		err = cluster.HandleRegionHeartbeat(region)
		if err != nil {
			msg := err.Error()
			hbStreams.sendErr(region, pdpb.ErrorType_UNKNOWN, msg, storeLabel)
		}

		regionHeartbeatCounter.WithLabelValues(storeLabel, "report", "ok").Inc()
	}
}

// GetRegion implements gRPC PDServer.
func (s *Server) GetRegion(ctx context.Context, request *pdpb.GetRegionRequest) (*pdpb.GetRegionResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetRegionResponse{Header: s.notBootstrappedHeader()}, nil
	}
	region, leader := cluster.GetRegionByKey(request.GetRegionKey())
	return &pdpb.GetRegionResponse{
		Header: s.header(),
		Region: region,
		Leader: leader,
	}, nil
}

// GetPrevRegion implements gRPC PDServer
func (s *Server) GetPrevRegion(ctx context.Context, request *pdpb.GetRegionRequest) (*pdpb.GetRegionResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetRegionResponse{Header: s.notBootstrappedHeader()}, nil
	}

	region, leader := cluster.GetPrevRegionByKey(request.GetRegionKey())
	return &pdpb.GetRegionResponse{
		Header: s.header(),
		Region: region,
		Leader: leader,
	}, nil
}

// GetRegionByID implements gRPC PDServer.
func (s *Server) GetRegionByID(ctx context.Context, request *pdpb.GetRegionByIDRequest) (*pdpb.GetRegionResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetRegionResponse{Header: s.notBootstrappedHeader()}, nil
	}
	id := request.GetRegionId()
	region, leader := cluster.GetRegionByID(id)
	return &pdpb.GetRegionResponse{
		Header: s.header(),
		Region: region,
		Leader: leader,
	}, nil
}

// AskSplit implements gRPC PDServer.
func (s *Server) AskSplit(ctx context.Context, request *pdpb.AskSplitRequest) (*pdpb.AskSplitResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.AskSplitResponse{Header: s.notBootstrappedHeader()}, nil
	}
	if request.GetRegion() == nil {
		return nil, errors.New("missing region for split")
	}
	req := &pdpb.AskSplitRequest{
		Region: request.Region,
	}
	split, err := cluster.handleAskSplit(req)
	if err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.AskSplitResponse{
		Header:      s.header(),
		NewRegionId: split.NewRegionId,
		NewPeerIds:  split.NewPeerIds,
	}, nil
}

// AskBatchSplit implements gRPC PDServer.
func (s *Server) AskBatchSplit(ctx context.Context, request *pdpb.AskBatchSplitRequest) (*pdpb.AskBatchSplitResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.AskBatchSplitResponse{Header: s.notBootstrappedHeader()}, nil
	}
	if !cluster.cachedCluster.IsFeatureSupported(BatchSplit) {
		return &pdpb.AskBatchSplitResponse{Header: s.incompatibleVersion("batch_split")}, nil
	}
	if request.GetRegion() == nil {
		return nil, errors.New("missing region for split")
	}
	req := &pdpb.AskBatchSplitRequest{
		Region:     request.Region,
		SplitCount: request.SplitCount,
	}
	split, err := cluster.handleAskBatchSplit(req)
	if err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.AskBatchSplitResponse{
		Header: s.header(),
		Ids:    split.Ids,
	}, nil
}

// ReportSplit implements gRPC PDServer.
func (s *Server) ReportSplit(ctx context.Context, request *pdpb.ReportSplitRequest) (*pdpb.ReportSplitResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.ReportSplitResponse{Header: s.notBootstrappedHeader()}, nil
	}
	_, err := cluster.handleReportSplit(request)
	if err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.ReportSplitResponse{
		Header: s.header(),
	}, nil
}

// ReportBatchSplit implements gRPC PDServer.
func (s *Server) ReportBatchSplit(ctx context.Context, request *pdpb.ReportBatchSplitRequest) (*pdpb.ReportBatchSplitResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.ReportBatchSplitResponse{Header: s.notBootstrappedHeader()}, nil
	}

	_, err := cluster.handleBatchReportSplit(request)
	if err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.ReportBatchSplitResponse{
		Header: s.header(),
	}, nil
}

// GetClusterConfig implements gRPC PDServer.
func (s *Server) GetClusterConfig(ctx context.Context, request *pdpb.GetClusterConfigRequest) (*pdpb.GetClusterConfigResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetClusterConfigResponse{Header: s.notBootstrappedHeader()}, nil
	}
	return &pdpb.GetClusterConfigResponse{
		Header:  s.header(),
		Cluster: cluster.GetConfig(),
	}, nil
}

// PutClusterConfig implements gRPC PDServer.
func (s *Server) PutClusterConfig(ctx context.Context, request *pdpb.PutClusterConfigRequest) (*pdpb.PutClusterConfigResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.PutClusterConfigResponse{Header: s.notBootstrappedHeader()}, nil
	}
	conf := request.GetCluster()
	if err := cluster.putConfig(conf); err != nil {
		return nil, status.Errorf(codes.Unknown, err.Error())
	}

	log.Infof("put cluster config ok - %v", conf)

	return &pdpb.PutClusterConfigResponse{
		Header: s.header(),
	}, nil
}

// ScatterRegion implements gRPC PDServer.
func (s *Server) ScatterRegion(ctx context.Context, request *pdpb.ScatterRegionRequest) (*pdpb.ScatterRegionResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.ScatterRegionResponse{Header: s.notBootstrappedHeader()}, nil
	}

	region := cluster.GetRegionInfoByID(request.GetRegionId())
	if region == nil {
		if request.GetRegion() == nil {
			return nil, errors.Errorf("region %d not found", request.GetRegionId())
		}
		region = core.NewRegionInfo(request.GetRegion(), request.GetLeader())
	}

	co := cluster.coordinator
	if op := co.regionScatterer.Scatter(region); op != nil {
		co.opController.AddOperator(op)
	}

	return &pdpb.ScatterRegionResponse{
		Header: s.header(),
	}, nil
}

// GetGCSafePoint implements gRPC PDServer.
func (s *Server) GetGCSafePoint(ctx context.Context, request *pdpb.GetGCSafePointRequest) (*pdpb.GetGCSafePointResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetGCSafePointResponse{Header: s.notBootstrappedHeader()}, nil
	}

	safePoint, err := s.kv.LoadGCSafePoint()
	if err != nil {
		return nil, err
	}

	return &pdpb.GetGCSafePointResponse{
		Header:    s.header(),
		SafePoint: safePoint,
	}, nil
}

// SyncRegions syncs the regions.
func (s *Server) SyncRegions(stream pdpb.PD_SyncRegionsServer) error {
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.WithStack(err)
		}
		if request.GetHeader().GetClusterId() != s.clusterID {
			return status.Errorf(codes.FailedPrecondition, "mismatch cluster id, need %d but got %d", s.clusterID, request.GetHeader().GetClusterId())
		}
		log.Infof("establish sync region stream with %s [%s]", request.GetMember().GetName(), request.GetMember().GetClientUrls()[0])
		if s.cluster.regionSyncer != nil {
			s.cluster.regionSyncer.BindStream(request.GetMember().GetName(), stream)
		}
	}
}

// UpdateGCSafePoint implements gRPC PDServer.
func (s *Server) UpdateGCSafePoint(ctx context.Context, request *pdpb.UpdateGCSafePointRequest) (*pdpb.UpdateGCSafePointResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, err
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.UpdateGCSafePointResponse{Header: s.notBootstrappedHeader()}, nil
	}

	oldSafePoint, err := s.kv.LoadGCSafePoint()
	if err != nil {
		return nil, err
	}

	newSafePoint := request.SafePoint

	// Only save the safe point if it's greater than the previous one
	if newSafePoint > oldSafePoint {
		if err := s.kv.SaveGCSafePoint(newSafePoint); err != nil {
			return nil, err
		}
		log.Infof("updated gc safe point to %d", newSafePoint)
	} else if newSafePoint < oldSafePoint {
		log.Warnf("trying to update gc safe point from %d to %d", oldSafePoint, newSafePoint)
		newSafePoint = oldSafePoint
	}

	return &pdpb.UpdateGCSafePointResponse{
		Header:       s.header(),
		NewSafePoint: newSafePoint,
	}, nil
}

// validateRequest checks if Server is leader and clusterID is matched.
// TODO: Call it in gRPC intercepter.
func (s *Server) validateRequest(header *pdpb.RequestHeader) error {
	if !s.IsLeader() {
		return errors.WithStack(notLeaderError)
	}
	if header.GetClusterId() != s.clusterID {
		return status.Errorf(codes.FailedPrecondition, "mismatch cluster id, need %d but got %d", s.clusterID, header.GetClusterId())
	}
	return nil
}

func (s *Server) header() *pdpb.ResponseHeader {
	return &pdpb.ResponseHeader{ClusterId: s.clusterID}
}

func (s *Server) errorHeader(err *pdpb.Error) *pdpb.ResponseHeader {
	return &pdpb.ResponseHeader{
		ClusterId: s.clusterID,
		Error:     err,
	}
}

func (s *Server) notBootstrappedHeader() *pdpb.ResponseHeader {
	return s.errorHeader(&pdpb.Error{
		Type:    pdpb.ErrorType_NOT_BOOTSTRAPPED,
		Message: "cluster is not bootstrapped",
	})
}

func (s *Server) incompatibleVersion(tag string) *pdpb.ResponseHeader {
	msg := fmt.Sprintf("%s incompatible with current cluster version %s", tag, s.scheduleOpt.loadClusterVersion())
	return s.errorHeader(&pdpb.Error{
		Type:    pdpb.ErrorType_INCOMPATIBLE_VERSION,
		Message: msg,
	})
}
