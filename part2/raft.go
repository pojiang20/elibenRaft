package raft

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"sync"
	"time"
)

const DebugCM = 1

type CMState int

const (
	Follower CMState = iota
	Candidate
	Leader
	Dead
)

// 把枚举值转换成字符串
func (s CMState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	case Dead:
		return "Dead"
	default:
		panic("unreachable")
	}
}

// ConsensusModule implements a single node of Raft consensus.
type ConsensusModule struct {
	//mu对cm的并发访问提供保护
	mu sync.Mutex

	//服务ID
	id int

	//集群中其他节点的id
	peerIds []int

	//给其他同辈发送RPC请求
	server *Server

	//持久化的raft状态
	currentTerm int
	votedFor    int
	log         []LogEntry

	//Volatile Raft state on all servers
	//QA 这里的volatile是字段的可见性？？
	state              CMState
	electionResetEvent time.Time

	//主与从id的相同位置是nextIndex[peerId]-1，也就是nextIndex指示了主从同步后带插入的位置
	nextIndex map[int]int
}

// NewConsensusModule creates a new CM with the given ID, list of peer IDs and
// server. The ready channel signals the CM that all peers are connected and
// it's safe to start its state machine.
// 创建一个节点，并分配id
func NewConsensusModule(id int, peerIds []int, server *Server, ready <-chan interface{}) *ConsensusModule {
	cm := new(ConsensusModule)
	cm.id = id
	cm.peerIds = peerIds
	cm.server = server
	cm.state = Follower
	cm.votedFor = -1

	go func() {
		// The CM is quiescent until ready is signaled; then, it starts a countdown
		// for leader election.
		// 这个协程是静止的，直到接收到信号量，然后开启选举倒计时。
		//TODO 它何时会被触发？
		<-ready
		cm.mu.Lock()
		cm.electionResetEvent = time.Now()
		cm.mu.Unlock()
		cm.runElectionTimer()
	}()

	return cm
}

// 对外提供的提交command的接口，只对leader有效
func (cm *ConsensusModule) Submit(cmd interface{}) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.state == Leader {
		item := LogEntry{
			cmd,
			cm.currentTerm,
		}
		//Propose阶段：将日志追加到本地
		cm.log = append(cm.log, item)
		return true
	}
	return false
}

func (cm *ConsensusModule) electionTimeout() time.Duration {
	//如果设置了RAFT_FORCE_MORE_REELECTION，则表示有意进行压力测试
	//通过使用一个硬编码来产生碰撞，在不同的服务器之间强制进行更多的重新选举
	if len(os.Getenv("RAFT_FORCE_MORE_REELECTION")) > 0 && rand.Intn(3) == 0 {
		return time.Duration(150) * time.Millisecond
	} else {
		return time.Duration(150+rand.Intn(150)) * time.Millisecond
	}
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
	cm.dlog("election timer started (%v), term=%d", timeoutDuration, termStarted)

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		<-ticker.C

		cm.mu.Lock()
		if cm.state != Candidate && cm.state != Follower {
			cm.dlog("in election timer state=%s, bailing out", cm.state)
			cm.mu.Unlock()
			return
		}

		if termStarted != cm.currentTerm {
			cm.dlog("in election timer term changed from %d to %d, bailing out", termStarted, cm.currentTerm)
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
// 当前cm节点作为候选人发起选举
func (cm *ConsensusModule) startElection() {
	cm.state = Candidate
	cm.currentTerm += 1
	//QA 为什么要有savedTerm，难道currentTerm在执行过程中会发生变化？确实在发送过程中任期可能发生变化，使用savedTerm保证所有消息任期相同
	savedCurrentTerm := cm.currentTerm
	cm.electionResetEvent = time.Now()
	cm.votedFor = cm.id
	cm.dlog("becomes Candidate (currentTerm=%d); log=%v", savedCurrentTerm, cm.log)

	votesReceived := 1

	// Send RequestVote RPCs to all other servers concurrently.
	//给所有其他节点发送RPC请求
	for _, peerId := range cm.peerIds {
		go func(peerId int) {
			args := RequestVoteArgs{
				Term:        savedCurrentTerm,
				CandidateId: cm.id,
			}
			var reply RequestVoteReply

			cm.dlog("sending RequestVote to %d: %+v", peerId, args)
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
					return
				} else if reply.Term == savedCurrentTerm {
					//QA 为什么不要判断投给了谁？
					if reply.VoteGranted {
						votesReceived += 1
						if votesReceived*2 > len(cm.peerIds)+1 {
							// Won the election!
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
	cm.dlog("becomes Leader; term=%d, log=%v", cm.currentTerm, cm.log)

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

// 原来发送心跳只是为了与Follower建立连接且重置定时器
// 现在发送心跳还用于追加统一日志
func (cm *ConsensusModule) leaderSendHeartbeats() {
	cm.mu.Lock()
	if cm.state != Leader {
		cm.mu.Unlock()
		return
	}
	savedCurrentTerm := cm.currentTerm
	cm.mu.Unlock()

	for _, peerId := range cm.peerIds {
		go func(peerId int) {
			cm.mu.Lock()
			lastSameLogIndex := cm.nextIndex[peerId] - 1
			lastSameLogTerm := -1
			if lastSameLogIndex >= 0 {
				lastSameLogTerm = cm.log[lastSameLogIndex].Term
			}
			//主将相同日志后的所有内容都追加给从
			entriesL2F := cm.log[lastSameLogIndex+1:]

			args := AppendEntriesArgs{
				Term:             savedCurrentTerm,
				LeaderId:         cm.id,
				LastSameLogIndex: lastSameLogIndex,
				LastSameLogTerm:  lastSameLogTerm,
				Entries:          entriesL2F,
			}
			cm.dlog("sending AppendEntries to %v: ni=%d, args=%+v", peerId, 0, args)
			var reply AppendEntriesReply
			if err := cm.server.Call(peerId, "ConsensusModule.AppendEntries", args, &reply); err == nil {
				cm.mu.Lock()
				if reply.Term > savedCurrentTerm {
					cm.dlog("term out of date in heartbeat reply")
					cm.becomeFollower(reply.Term)
					return
				}
				//如果从节点返回错误，需要回退相同坐标
				//如果从节点返回正确，表示通过一致性检测并且已经同步坐标
				if !reply.Success {
					//lastSameLogIndex是相同位置，+1是即将插入，-1是即将插入回退
					cm.nextIndex[peerId] = (lastSameLogIndex + 1) - 1
				} else {
					//更新nextIndex
					cm.nextIndex[peerId] = lastSameLogIndex + len(entriesL2F) + 1
				}
			}
			cm.mu.Unlock()
		}(peerId)
	}
}

type AppendEntriesArgs struct {
	Term     int
	LeaderId int

	LastSameLogIndex int
	LastSameLogTerm  int
	Entries          []LogEntry
	LeaderCommit     int
}

type AppendEntriesReply struct {
	Term    int
	Success bool
}

// 为什么不需要Index
type LogEntry struct {
	Command interface{}
	Term    int
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

// DebugCM>0 则打debug日志
func (cm *ConsensusModule) dlog(format string, args ...interface{}) {
	if DebugCM > 0 {
		format = fmt.Sprintf("[%d] ", cm.id) + format
		log.Printf(format, args...)
	}
}

// TODO 没搞懂，这是作为接收者接收到的appendentries吗？
func (cm *ConsensusModule) AppendEntries(args AppendEntriesArgs, reply *AppendEntriesReply) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.state == Dead {
		return nil
	}
	cm.dlog("AppendEntries: %+v", args)

	if args.Term > cm.currentTerm {
		cm.dlog("... term out of date in AppendEntries")
		cm.becomeFollower(args.Term)
	}

	reply.Success = false
	if args.Term == cm.currentTerm {
		if cm.state != Follower {
			cm.becomeFollower(args.Term)
		}
		cm.electionResetEvent = time.Now()
		reply.Success = true
	}

	reply.Term = cm.currentTerm
	cm.dlog("AppendEntries reply: %+v", *reply)
	return nil
}

func (cm *ConsensusModule) RequestVote(args RequestVoteArgs, reply *RequestVoteReply) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.state == Dead {
		return nil
	}
	cm.dlog("RequestVote: %+v [currentTerm=%d, votedFor=%d]", args, cm.currentTerm, cm.votedFor)

	if args.Term > cm.currentTerm {
		cm.dlog("... term out of date in RequestVote")
		cm.becomeFollower(args.Term)
	}

	if cm.currentTerm == args.Term && (cm.votedFor == -1 || cm.votedFor == args.CandidateId) {
		reply.VoteGranted = true
		cm.votedFor = args.CandidateId
		cm.electionResetEvent = time.Now()
	} else {
		reply.VoteGranted = false
	}
	reply.Term = cm.currentTerm
	cm.dlog("... RequestVote reply: %+v", reply)
	return nil
}

func (cm *ConsensusModule) Report() (id int, term int, isLeader bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.id, cm.currentTerm, cm.state == Leader
}

func (cm *ConsensusModule) Stop() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state = Dead
	cm.dlog("becomes Dead")
}
