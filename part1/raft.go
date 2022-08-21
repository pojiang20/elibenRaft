package raft

import (
	"fmt"
	"log"
	"sync"
	"time"
)

const DebugCM = 1

const (
	Follower CMState = iota
	Candidate
	Leader
	Dead
)

//ConsensusModule implements a single node of Raft consensus.
type ConsensusModule struct {
	//mu对cm的并发访问提供保护
	mu sync.Mutex

	//服务ID
	id int

	//集群中其他节点的id
	peersIds []int

	//给其他同辈发送RPC请求
	server *Server

	//持久化的raft状态
	currentTerm int
	votedFor    int
	log         []LogEntry

	//Volatile Raft state on all servers
	//QA 这里的volatile是字段的可见性？？
	state              CMState
	electionResetEvent time.time
}

// NewConsensusModule creates a new CM with the given ID, list of peer IDs and
// server. The ready channel signals the CM that all peers are connected and
// it's safe to start its state machine.
//创建一个节点，并分配id
func NewConsensusModule(id int, peerIds []int, server *Server, ready <-chan interface{}) *ConsensusModule {
	cm := new(ConsensusModule)
	cm.id = id
	cm.peersIds = peerIds
	cm.server = server
	cm.state = Follower
	cm.votedFor = -1

	go func() {
		// The CM is quiescent until ready is signaled; then, it starts a countdown
		// for leader election.
		// 这个协程是静止的，直到接收到信号量，然后开启选举倒计时。
		<-ready
		cm.mu.Lock()
		cm.electionResetEvent = time.Now()
		cm.mu.Unlock()
		cm.runElectionTimer()
	}()
}

// runElectionTimer implements an election timer. It should be launched whenever
// we want to start a timer towards becoming a candidate in a new election.
//
// This function is blocking and should be launched in a separate goroutine;
// it's designed to work for a single (one-shot) election timer, as it exits
// whenever the CM state changes from follower/candidate or the term changes.
func (cm *ConsensusModule) runElectionTimer() {
	timeoutDuration := cm.electionTimeout()
	cm.mu.Lock()
	termStarted := cm.currentTerm
	cm.mu.Unlock()
	cm.dlog("election timer started (%v),term=%d", timeoutDuration, termStarted)

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		<-ticker.C

		cm.mu.Lock()
		if cm.state != Candidate && cm.state != Follower {
			cm.dlog("in election timer state=%s,bailing out", cm.state)
			cm.mu.Unlock()
			return
		}

		if termStarted != cm.currentTerm {
			cm.dlog("in election timer term changed from %d to %d,bailing out", termStarted, cm.currentTerm)
			cm.mu.Unlock()
			return
		}

		//Start an election if we haven't heard from a leader or
		//haven't voted for someone for the duration of the timeout.
		//QA 计时期间没有给别人投票也会重新发起投票？这是对应分裂投票的情况吗？
		if elapsed := time.Since(cm.electionResetEvent); elapsed >= timeoutDuration {
			cm.startElection()
			cm.mu.Unlock()
			return
		}
		cm.mu.Unlock()
	}
}

// startElection starts a new election with this CM as a candidate.
// Expects cm.mu to be locked.
//当前cm节点作为候选人发起选举
func (cm *ConsensusModule) startElection() {
	cm.state = Candidate
	cm.currentTerm += 1
	//QA 为什么要有savedTerm，难道currentTerm在执行过程中会发生变化？
	savedCurrentTerm := cm.currentTerm
	cm.electionResetEvent = time.Now()
	cm.votedFor = cm.id
	cm.dlog("becomes Candidate (currentTerm=%d); log=%v", savedCurrentTerm, cm.log)

	votesReceived := 1

	// Send RequestVote RPCs to all other servers concurrently.
	//给所有其他节点发送RPC请求
	for _, peerId := range cm.peersIds {
		go func(peerId int) {
			args := RequestVoteArgs{
				Term:        savedCurrentTerm,
				CandidateId: cm.id,
			}
			var reply RequestVoteReply

			cm.dlog("sending RequestVote to %d: %+v", peerId, args)
			//使用服务所提供的RPC调用
			if err := cm.server.Call(peerId, "ConsensusModule.RequestVote", args, &reply); err == nil {
				cm.mu.Lock()
				defer cm.mu.Unlock()
				cm.dlog("received RequestVoteReply %+v", reply)

				if cm.state != Candidate {
					cm.dlog("while waiting for reply, state = %v", cm.state)
					return
				}

				if reply.Term > savedCurrentTerm {
					cm.dlog("term out of date in RequestVoteReply")
					cm.becomeFollower(reply.Term)
				} else if reply.Term == savedCurrentTerm {
					//QA 为什么不要判断投给了谁？
					if reply.VoteGranted {
						votesReceived += 1
						if votesReceived*2 > len(cm.peersIds)+1 {
							//赢得选举
							cm.dlog("wins election with %d votes", votesReceived)
							cm.startLeader()
							return
						}
					}
				}
			}
		}(peerId)
	}

	//候选人状态的计时器，如果这轮没有选出Leader，则重新发起投票
	go cm.runElectionTimer()
}

// becomeFollower makes cm a follower and resets its state.
// Expects cm.mu to be locked.
func (cm *ConsensusModule) becomeFollower(term int) {
	cm.dlog("becomes Follower with term=%d; log=%v", term, cm.log)
	cm.state = Follower
	cm.currentTerm = term
	cm.votedFor = -1
	cm.electionResetEvent = time.Now()

	go cm.runElectionTimer()
}

// startLeader switches cm into a leader state and begins process of heartbeats.
// Expects cm.mu to be locked.
func (cm *ConsensusModule) startLeader() {
	cm.state = Leader
	cm.dlog("becomes Leader; term=%d,log=%v", cm.currentTerm, cm.log)

	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			cm.leaderSendHeartbeats()
			<-ticker.C

			cm.mu.Lock()
			if cm.state != Leader {
				cm.mu.Unlock()
				return
			}
			cm.mu.Unlock()
		}
	}()
}

type RequestVoteArgs struct {
	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int
}

type RequestVoteReply struct {
	Term int
	//标记有无投票
	VoteGranted bool
}

//DebugCM>0 则打debug日志
func (cm *ConsensusModule) dlog(format string, args ...interface{}) {
	if DebugCM > 0 {
		format = fmt.Sprintf("[%d]", cm.id) + format
		log.Printf(format, args...)
	}
}
