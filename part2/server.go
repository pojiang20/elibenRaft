package raft

import (
	"fmt"
	"net/rpc"
	"sync"
)

type Server struct {
	mu sync.Mutex

	peerIds []int

	cm *ConsensusModule

	peerClients map[int]*rpc.Client
}

func (s *Server) Call(id int, serviceMethod string, args interface{}, reply interface{}) error {
	s.mu.Lock()
	peer := s.peerClients[id]
	s.mu.Unlock()

	if peer == nil {
		return fmt.Errorf("call client %s after it's closed", id)
	} else {
		return peer.Call(serviceMethod, args, reply)
	}
}
