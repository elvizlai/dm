// Copyright 2019 PingCAP, Inc.
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

package master

import (
	"context"
	"strings"
	"time"

	"github.com/pingcap/dm/dm/pb"
	"github.com/pingcap/dm/pkg/log"
	"github.com/pingcap/failpoint"

	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const (
	oneselfLeader = "oneself"
)

func (s *Server) electionNotify(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case leaderInfo := <-s.election.LeaderNotify():
			// retire from leader
			if leaderInfo == nil {
				if s.leader == oneselfLeader {
					s.retireLeader()
					log.L().Info("current member retire from the leader", zap.String("current member", s.cfg.Name))
				} else {
					// leader retire before
					log.L().Error("current member is not the leader, can't retire", zap.String("current member", s.cfg.Name))
				}

				continue
			}

			if leaderInfo.ID == s.cfg.Name {
				// this member become leader
				log.L().Info("current member become the leader", zap.String("current member", s.cfg.Name))

				ok := s.startLeaderComponent(ctx)

				if !ok {
					s.retireLeader()
					s.election.Resign()
					continue
				}

				s.Lock()
				s.leader = oneselfLeader
				s.closeLeaderClient()
				s.Unlock()
			} else {
				// this member is not leader
				log.L().Info("get new leader", zap.String("leader", leaderInfo.ID), zap.String("current member", s.cfg.Name))

				s.Lock()
				s.leader = leaderInfo.ID
				s.createLeaderClient(leaderInfo.Addr)
				s.Unlock()
			}

		case err := <-s.election.ErrorNotify():
			// handle errors here, we do no meaningful things now.
			// but maybe:
			// 1. trigger an alert
			// 2. shutdown the DM-master process
			log.L().Error("receive error from election", zap.Error(err))
		}
	}
}

func (s *Server) createLeaderClient(leaderAddr string) {
	s.closeLeaderClient()

	conn, err := grpc.Dial(leaderAddr, grpc.WithInsecure(), grpc.WithBackoffMaxDelay(3*time.Second))
	if err != nil {
		log.L().Error("can't create grpc connection with leader, can't forward request to leader", zap.String("leader", leaderAddr), zap.Error(err))
		return
	}
	s.leaderGrpcConn = conn
	s.leaderClient = pb.NewMasterClient(conn)
}

func (s *Server) closeLeaderClient() {
	if s.leaderGrpcConn != nil {
		s.leaderGrpcConn.Close()
		s.leaderGrpcConn = nil
	}
}

func (s *Server) isLeaderAndNeedForward() (isLeader bool, needForward bool) {
	s.RLock()
	defer s.RUnlock()

	isLeader = (s.leader == oneselfLeader)
	needForward = (s.leaderGrpcConn != nil)
	return
}

func (s *Server) startLeaderComponent(ctx context.Context) bool {
	err := s.scheduler.Start(ctx, s.etcdClient)
	if err != nil {
		log.L().Error("scheduler do not started", zap.Error(err))
		return false
	}

	err = s.pessimist.Start(ctx, s.etcdClient)
	if err != nil {
		log.L().Error("pessimist do not started", zap.Error(err))
		return false
	}

	err = s.optimist.Start(ctx, s.etcdClient)
	if err != nil {
		log.L().Error("optimist do not started", zap.Error(err))
		return false
	}

	failpoint.Inject("FailToStartLeader", func(val failpoint.Value) {
		masterStrings := val.(string)
		if strings.Contains(masterStrings, s.cfg.Name) {
			log.L().Info("fail to start leader", zap.String("failpoint", "FailToStartLeader"))
			failpoint.Return(false)
		}
	})

	return true
}

func (s *Server) retireLeader() {
	s.pessimist.Close()
	s.optimist.Close()
	s.scheduler.Close()

	s.Lock()
	s.leader = ""
	s.closeLeaderClient()
	s.Unlock()
}
