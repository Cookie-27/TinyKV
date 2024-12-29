// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"errors"
	"log"
	"sort"
	"sync"
	"time"

	"math/rand"

	"github.com/pingcap-incubator/tinykv/kv/raftstore/util"
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// 用于随机数
type lockedRand struct {
	mu   sync.Mutex
	rand *rand.Rand
}

var globalRand = &lockedRand{
	rand: rand.New(rand.NewSource(time.Now().UnixNano())),
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64
}

type Raft struct {
	id uint64

	Term uint64
	Vote uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress

	// this peer's role
	State StateType

	// votes records
	votes map[uint64]bool

	// msgs need to send
	msgs []pb.Message

	// the leader id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int
	// Ticks since it reached last electionTimeout when it is leader or candidate.
	// Number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64

	heartbeatResp map[uint64]bool
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	raftLog := newLog(c.Storage)
	hs, cs, err := c.Storage.InitialState()
	if err != nil {
		panic(err)
	}
	if c.peers == nil {
		c.peers = cs.Nodes
	}
	prs := make(map[uint64]*Progress)
	for _, pr := range c.peers {
		prs[pr] = &Progress{
			Next:  0,
			Match: 0,
		}
	}
	raft := &Raft{
		id:               c.ID,
		Term:             hs.Term,
		Vote:             hs.Vote,
		RaftLog:          raftLog,
		Prs:              prs,
		State:            StateFollower,
		votes:            make(map[uint64]bool),
		heartbeatResp:    make(map[uint64]bool),
		Lead:             None,
		heartbeatTimeout: c.HeartbeatTick,
		electionTimeout:  c.ElectionTick,
		leadTransferee:   0,
	}
	if c.Applied > 0 {
		raftLog.appliedTo(c.Applied)
	}
	// Your Code Here (2A).
	return raft
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	pr, ok := r.Prs[to]
	if !ok {
		return false
	}
	prevLogIndex := pr.Next - 1
	term := r.Term
	leaderId := r.id
	committedIndex := r.RaftLog.committed
	prevLogTerm, err := r.RaftLog.Term(prevLogIndex)

	if err != nil || r.RaftLog.FirstIndex()-1 > prevLogIndex {
		r.sendSnapshot(to)
		return true
	}

	firstIndex := r.RaftLog.FirstIndex()
	var entries []*pb.Entry
	for i := pr.Next; i < r.RaftLog.LastIndex()+1; i++ {
		entries = append(entries, &r.RaftLog.entries[i-firstIndex])
	}

	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppend,
		To:      to,
		From:    leaderId,
		Term:    term,
		LogTerm: prevLogTerm,
		Index:   prevLogIndex,
		Entries: entries,
		Commit:  committedIndex,
	}
	r.msgs = append(r.msgs, msg)
	return true
}

func (r *Raft) softState() *SoftState {
	return &SoftState{Lead: r.Lead, RaftState: r.State}
}

func (r *Raft) hardState() pb.HardState {
	return pb.HardState{
		Term:   r.Term,
		Vote:   r.Vote,
		Commit: r.RaftLog.committed,
	}
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	term := r.Term
	_, ok := r.Prs[to]
	if !ok {
		log.Panic("peer not in the cluster")
	}
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		Term:    term,
		Commit:  util.RaftInvalidIndex,
		To:      to,
		From:    r.id,
	}
	r.msgs = append(r.msgs, msg)
	// Your Code Here (2A).
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	r.electionElapsed++
	switch r.State {
	case StateFollower:
		if r.electionElapsed >= r.electionTimeout {
			r.electionElapsed = 0
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgHup})
			if err != nil {
				return
			}
		}
	case StateCandidate:
		if r.electionElapsed >= r.electionTimeout {
			r.electionElapsed = 0
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgHup})
			if err != nil {
				return
			}
		}
	case StateLeader:
		r.heartbeatElapsed++
		hbrNum := len(r.heartbeatResp)
		total := len(r.Prs)
		// 选举超时
		if r.electionElapsed >= r.electionTimeout {
			r.electionElapsed = 0
			r.heartbeatResp = make(map[uint64]bool)
			r.heartbeatResp[r.id] = true
			// 心跳回应数不超过一半，说明成为孤岛，重新开始选举
			if hbrNum*2 <= total {
				r.startElection()
			}
			// leader 转移失败，目标节点可能挂了，放弃转移
			if r.leadTransferee != None {
				r.leadTransferee = None
			}
		}
		// 心跳超时
		if r.heartbeatElapsed >= r.heartbeatTimeout {
			// 发送心跳
			r.heartbeatElapsed = 0
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgBeat})
			if err != nil {
				return
			}
		}
	}

	// Your Code Here (2A).
}

func (r *Raft) startElection() {
	if _, ok := r.Prs[r.id]; !ok {
		return
	}
	if len(r.Prs) == 1 {
		r.becomeLeader()
		r.Term++
	} else {
		r.becomeCandidate()
		r.sendAllRequsetVote()
	}
}

func (r *Raft) sendAllRequsetVote() {
	for pr := range r.Prs {
		if pr != r.id {
			r.sendRequestVote(pr)
		}
	}
}

func (r *Raft) sendRequestVote(to uint64) {
	_, ok := r.Prs[to]
	if !ok {
		return
	}
	term := r.Term
	lastLogIndex := r.RaftLog.LastIndex()
	logTerm, err := r.RaftLog.Term(lastLogIndex)
	if err != nil {
		return
	}

	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVote,
		Term:    term,
		LogTerm: logTerm,
		Index:   lastLogIndex,
		To:      to,
		From:    r.id,
	}
	r.msgs = append(r.msgs, msg)
	return
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	r.State = StateFollower
	r.reset(term)
	r.Lead = lead
	// Your Code Here (2A).
}

func (r *Raft) reset(term uint64) {
	if r.Term != term {
		r.Term = term
		r.Vote = None
	}
	r.Lead = None
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	//rand Elcetion TimeoutTime
	rand := globalRand.rand.Intn(r.electionTimeout)
	r.electionTimeout += rand
	for r.electionTimeout >= 20 {
		r.electionTimeout -= 10
	}

	r.leadTransferee = None
	r.Vote = None
	r.votes = make(map[uint64]bool)
	r.heartbeatResp = make(map[uint64]bool)
	r.heartbeatResp[r.id] = true
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	r.State = StateCandidate
	r.reset(r.Term + 1)
	r.Vote = r.id
	r.votes[r.id] = true
	// Your Code Here (2A).
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	if r.State == StateFollower && len(r.Prs) != 1 {
		log.Panic("invalid transition [follower -> leader]")
	}
	r.reset(r.Term)
	r.State = StateLeader
	r.Lead = r.id

	lastIndex := r.RaftLog.LastIndex()
	for pr := range r.Prs {
		r.Prs[pr].Next = lastIndex + 1
		r.Prs[pr].Match = 0
	}
	r.RaftLog.entries = append(r.RaftLog.entries, pb.Entry{Term: r.Term, Index: lastIndex + 1})
	r.Prs[r.id].Next = r.RaftLog.LastIndex() + 1
	r.Prs[r.id].Match = r.Prs[r.id].Next - 1

	for pr := range r.Prs {
		if pr != r.id {
			r.sendAppend(pr)
		}
	}
	r.updateCommitIndex()
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
}

// 更新 commitIndex
func (r *Raft) updateCommitIndex() uint64 {
	// 假设存在 N 满足N > commitIndex，使得大多数的 matchIndex[i] ≥ N以及log[N].term == currentTerm 成立，则令 commitIndex = N
	match := make(uint64Slice, len(r.Prs))
	i := 0
	for _, prs := range r.Prs {
		match[i] = prs.Match
		i++
	}
	sort.Sort(match)
	// 大多数的 matchIndex[i] ≥ N
	maxN := match[(len(r.Prs)-1)/2]
	N := maxN
	for ; N > r.RaftLog.committed; N-- {
		if term, _ := r.RaftLog.Term(N); term == r.Term {
			break
		}
	}
	r.RaftLog.committed = N
	return r.RaftLog.committed
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	var err error = nil
	switch r.State {
	case StateFollower:
		err = r.FollowerStep(m)
	case StateCandidate:
		err = r.CandidateStep(m)
	case StateLeader:
		err = r.LeaderStep(m)
	}
	return err
}

func (r *Raft) FollowerStep(m pb.Message) error {
	var err error = nil
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		// 开始选举
		if _, ok := r.Prs[r.id]; ok {
			r.startElection()
		}
	case pb.MessageType_MsgBeat:
	case pb.MessageType_MsgPropose:
		err = ErrProposalDropped
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
	case pb.MessageType_MsgTransferLeader:
		if r.Lead != None {
			m.To = r.Lead
			r.msgs = append(r.msgs, m)
		}
	case pb.MessageType_MsgTimeoutNow:
		r.electionElapsed = 0
		r.startElection()
	}
	return err
}

func (r *Raft) CandidateStep(m pb.Message) error {
	var err error = nil
	switch m.MsgType {
	case pb.MessageType_MsgHup:
		r.startElection()
	case pb.MessageType_MsgBeat:
	case pb.MessageType_MsgPropose:
		err = ErrProposalDropped
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
		total := len(r.Prs) // 集群数
		agrNum := 0         // 赞同票数
		denNum := 0         // 反对票数
		r.votes[m.From] = !m.Reject
		for _, vote := range r.votes {
			if vote {
				agrNum++
			} else {
				denNum++
			}
		}
		if 2*agrNum > total {
			r.becomeLeader()
		} else if 2*denNum >= total {

			r.becomeFollower(r.Term, None)
		}
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
	case pb.MessageType_MsgTransferLeader:
		if r.Lead != None {
			m.To = r.Lead
			r.msgs = append(r.msgs, m)
		}
	case pb.MessageType_MsgTimeoutNow:
		r.electionElapsed = 0
		r.startElection()
	}
	return err
}

func (r *Raft) LeaderStep(m pb.Message) error {
	var err error = nil
	switch m.MsgType {
	case pb.MessageType_MsgHup:
	case pb.MessageType_MsgBeat:
		for pr := range r.Prs {
			if pr != r.id {
				r.sendHeartbeat(pr)
			}
		}
	case pb.MessageType_MsgPropose:
		if r.leadTransferee == None {
			r.handlePropose(m)
		} else {
			err = ErrProposalDropped
		}
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
		r.handleAppendResponse(m)
	case pb.MessageType_MsgRequestVote:
		r.handleRequestVote(m)
	case pb.MessageType_MsgRequestVoteResponse:
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
		r.handleHeartbeatResponse(m)
	case pb.MessageType_MsgTransferLeader:
		r.handleTransferLeader(m)
	case pb.MessageType_MsgTimeoutNow:
		r.electionElapsed = 0
		r.startElection()
	}
	return err
}

func (r *Raft) handleTransferLeader(m pb.Message) {
	if r.State != StateLeader {
		log.Panic("only leader can transfer leader")
		return
	}

	if _, ok := r.Prs[m.From]; !ok {
		return
	}

	// 强制执行本次 transfer
	r.leadTransferee = m.From

	if r.Prs[m.From].Match == r.RaftLog.LastIndex() {
		msg := pb.Message{
			MsgType: pb.MessageType_MsgTimeoutNow,
			From:    r.id,
			To:      m.From,
		}
		r.msgs = append(r.msgs, msg)
	} else {
		r.sendAppend(m.From)
	}

}

func (r *Raft) handleHeartbeatResponse(m pb.Message) {

	// 前置，更新 term 和 State
	if r.Term < m.Term {
		r.Term = m.Term
		if r.State != StateFollower {
			r.becomeFollower(r.Term, None)
		}
	}
	r.heartbeatResp[m.From] = true
	// 如果节点落后了，append
	if m.Commit < r.RaftLog.committed {
		r.sendAppend(m.From)
	}
}

// 发快照
func (r *Raft) sendSnapshot(to uint64) {
	var snapshot pb.Snapshot
	var err error
	if !IsEmptySnap(r.RaftLog.pendingSnapshot) {
		snapshot = *r.RaftLog.pendingSnapshot // 挂起的还未处理的快照
	} else {
		snapshot, err = r.RaftLog.storage.Snapshot() // 生成一份快照
	}

	if err != nil {
		return
	}

	msg := pb.Message{
		MsgType:  pb.MessageType_MsgSnapshot,
		Term:     r.Term,
		Snapshot: &snapshot,
		To:       to,
		From:     r.id,
	}
	r.msgs = append(r.msgs, msg)
	r.Prs[to].Next = snapshot.Metadata.Index + 1
}

func (r *Raft) handleRequestVote(m pb.Message) {
	// 前置，更新 term 和 State
	if r.Term < m.Term {
		r.Vote = None
		r.Term = m.Term
		if r.State != StateFollower {
			r.becomeFollower(r.Term, None)
		}
	}
	// 返回假 如果 term < currentTerm
	if m.Term < r.Term {
		r.sendRequestVoteResponse(true, m.From)
		return
	}
	// 如果 votedFor 为空或者等于 candidateID，进入投票判断
	if r.Vote == None || r.Vote == m.From {
		lastIndex := r.RaftLog.LastIndex()
		lastTerm, _ := r.RaftLog.Term(lastIndex)
		if m.LogTerm > lastTerm || (m.LogTerm == lastTerm && m.Index >= lastIndex) {
			r.sendRequestVoteResponse(false, m.From)
			r.Vote = m.From
		} else {
			r.sendRequestVoteResponse(true, m.From)
		}
	} else {
		r.sendRequestVoteResponse(true, m.From)
	}
}

func (r *Raft) handlePropose(m pb.Message) {
	// 追加日志
	r.appendEntry(m.Entries)
	// 发送追加RPC
	for pr := range r.Prs {
		if pr != r.id {
			r.sendAppend(pr)
		}
	}
	if len(r.Prs) == 1 {
		r.RaftLog.commitTo(r.Prs[r.id].Match)
	}
}

func (r *Raft) appendEntry(es []*pb.Entry) {
	lastIndex := r.RaftLog.LastIndex()
	for i := range es {
		es[i].Term = r.Term
		es[i].Index = lastIndex + 1 + uint64(i)
		r.RaftLog.entries = append(r.RaftLog.entries, *es[i])
	}
	r.Prs[r.id].Match = r.RaftLog.LastIndex()
	r.Prs[r.id].Next = r.Prs[r.id].Match + 1
}

func (r *Raft) sendRequestVoteResponse(reject bool, to uint64) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVoteResponse,
		Term:    r.Term,
		Reject:  reject,
		To:      to,
		From:    r.id,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) handleAppendResponse(m pb.Message) {

	// 同步失败，跳转 next 重新同步
	if m.Reject {
		r.Prs[m.From].Next = min(m.Index+1, r.Prs[m.From].Next-1)
		r.sendAppend(m.From)
		return
	}

	// 同步成功, 更新 match 和 next
	r.Prs[m.From].Match = m.Index
	r.Prs[m.From].Next = m.Index + 1

	// 更新 commit
	oldCom := r.RaftLog.committed
	r.updateCommitIndex()
	// 更新完后向所有节点再发一个Append，用于给同步committed
	if r.RaftLog.committed != oldCom {
		for pr := range r.Prs {
			if pr != r.id {
				r.sendAppend(pr)
			}
		}
	}

	// 如果是正在 transfer 的目标，transfer
	if m.From == r.leadTransferee {
		r.Step(pb.Message{MsgType: pb.MessageType_MsgTransferLeader, From: m.From})
	}
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	// 前置，更新 term 和 State
	if r.Term <= m.Term {
		r.Term = m.Term
		if r.State != StateFollower {
			r.becomeFollower(r.Term, None)
		}
	}
	if r.State == StateLeader {
		return
	}

	// 返回假 如果领导人的任期小于接收者的当前任期
	if m.Term < r.Term {
		r.sendAppendResponse(true, m.From, r.RaftLog.LastIndex())
		return
	}

	// 转换 leadr
	if m.From != r.Lead {
		r.Lead = m.From
	}
	prevLogIndex := m.Index
	prevLogTerm := m.LogTerm

	// 返回假, 如果超范围
	if prevLogIndex > r.RaftLog.LastIndex() {
		r.sendAppendResponse(true, m.From, r.RaftLog.LastIndex())
		return
	}
	// 返回假，如果接收者日志中没有包含这样一个条目 即该条目的任期在 prevLogIndex 上能和 prevLogTerm 匹配上
	if tmpTerm, _ := r.RaftLog.Term(prevLogIndex); tmpTerm != prevLogTerm {
		r.sendAppendResponse(true, m.From, r.RaftLog.LastIndex())
		return
	}
	// 追加新条目，同时删除冲突
	for _, en := range m.Entries {
		index := en.Index
		oldTerm, err := r.RaftLog.Term(index)
		if index-r.RaftLog.FirstIndex() > uint64(len(r.RaftLog.entries)) || index > r.RaftLog.LastIndex() {
			r.RaftLog.entries = append(r.RaftLog.entries, *en)
		} else if oldTerm != en.Term || err != nil {
			// 不匹配，删除从此往后的所有条目
			if index < r.RaftLog.FirstIndex() {
				r.RaftLog.entries = make([]pb.Entry, 0)
			} else {
				r.RaftLog.entries = r.RaftLog.entries[0 : index-r.RaftLog.FirstIndex()]
			}
			// 更新stable
			r.RaftLog.stabled = min(r.RaftLog.stabled, index-1)
			// 追加新条目
			r.RaftLog.entries = append(r.RaftLog.entries, *en)

		}
	}

	r.RaftLog.lastAppend = m.Index + uint64(len(m.Entries))

	// 返回真
	r.sendAppendResponse(false, m.From, r.RaftLog.LastIndex())
	// 更新commitIndex
	if m.Commit > r.RaftLog.committed {
		r.RaftLog.committed = min(m.Commit, r.RaftLog.lastAppend)
	}
}

func (r *Raft) sendAppendResponse(reject bool, to uint64, index uint64) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		Term:    r.Term,
		To:      to,
		Reject:  reject,
		From:    r.id,
		Index:   index,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	// 前置，更新 term 和 State
	if r.Term <= m.Term {
		r.Term = m.Term
		if r.State != StateFollower {
			r.becomeFollower(r.Term, None)
		}
	}
	// 转换 leader
	if m.From != r.Lead {
		r.Lead = m.From
	}
	// 重置时间
	r.electionElapsed = 0
	// 回应
	r.sendHeartBeatResponse(m.From)
}

func (r *Raft) sendHeartBeatResponse(to uint64) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeatResponse,
		Term:    r.Term,
		To:      to,
		From:    r.id,
		Commit:  r.RaftLog.committed,
	}
	r.msgs = append(r.msgs, msg)
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	//fmt.Printf("%x receive snapshot from %x\n",r.id,m.From)
	// Your Code Here (2C).
	// 前置，更新 term 和 State
	if r.Term < m.Term {
		r.Term = m.Term
		if r.State != StateFollower {
			r.becomeFollower(r.Term, None)
		}
	}
	if m.Term < r.Term {
		return
	}

	metaData := m.Snapshot.Metadata
	shotIndex := metaData.Index
	shotTerm := metaData.Term
	shotConf := metaData.ConfState

	if shotIndex < r.RaftLog.committed || shotIndex < r.RaftLog.FirstIndex() {
		return
	}
	if r.Lead != m.From {
		r.Lead = m.From
	}

	// 丢弃之前的所有 entry
	if len(r.RaftLog.entries) > 0 {
		if shotIndex >= r.RaftLog.LastIndex() {
			r.RaftLog.entries = nil
		} else {
			r.RaftLog.entries = r.RaftLog.entries[shotIndex-r.RaftLog.FirstIndex()+1:]
		}
	}

	r.RaftLog.committed = shotIndex
	r.RaftLog.applied = shotIndex
	r.RaftLog.stabled = shotIndex

	// 集群节点变更
	if shotConf != nil {
		r.Prs = make(map[uint64]*Progress)
		for _, node := range shotConf.Nodes {
			r.Prs[node] = &Progress{}
			r.Prs[node].Next = r.RaftLog.LastIndex() + 1
			r.Prs[node].Match = 0
		}
	}

	if r.RaftLog.LastIndex() < shotIndex {
		// 加一个空条目，以指明 lastIndex 和 lastTerm 与快照一致
		entry := pb.Entry{
			EntryType: pb.EntryType_EntryNormal,
			Index:     shotIndex,
			Term:      shotTerm,
		}
		r.RaftLog.entries = append(r.RaftLog.entries, entry)
	}

	r.RaftLog.pendingSnapshot = m.Snapshot
	r.sendAppendResponse(false, m.From, r.RaftLog.LastIndex())
}

// addNode add a new node to raft group
// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
	_, ok := r.Prs[id]
	if ok {
		//log.Panic("node exists")
		return
	} else {
		r.Prs[id] = &Progress{
			Match: 0,
			Next:  r.RaftLog.LastIndex() + 1,
		}
	}
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
}
