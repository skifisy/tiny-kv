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
	"math/rand"
	"sort"

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
	randomizedElectionTimeout int
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
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	hs, cs, err := c.Storage.InitialState()
	if err != nil {
		panic(err)
	}
	if len(c.peers) > 0 && len(cs.Nodes) > 0 {
		panic("cannot specify both new peers and persisted ConfState")
	}

	r := &Raft{
		id:               c.ID,
		Term:             hs.Term,
		Vote:             hs.Vote,
		RaftLog:          newLog(c.Storage),
		Prs:              make(map[uint64]*Progress),
		State:            StateFollower,
		Lead:             None,
		heartbeatTimeout: c.HeartbeatTick,
		electionTimeout:  c.ElectionTick,
	}

	peers := c.peers
	if len(peers) == 0 {
		peers = cs.Nodes
	}
	lastIndex := r.RaftLog.LastIndex()
	for _, id := range peers {
		r.Prs[id] = &Progress{Next: lastIndex + 1}
	}
	if _, ok := r.Prs[r.id]; ok {
		r.Prs[r.id].Match = lastIndex
		r.Prs[r.id].Next = lastIndex + 1
	}

	r.RaftLog.committed = hs.Commit
	if c.Applied > 0 {
		r.RaftLog.applied = c.Applied
	} else if r.RaftLog.applied > r.RaftLog.committed {
		r.RaftLog.applied = r.RaftLog.committed
	}
	r.resetRandomizedElectionTimeout()
	return r
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	pr, ok := r.Prs[to]
	if !ok {
		return false
	}
	if pr.Next == 0 {
		pr.Next = 1
	}

	prevIndex := pr.Next - 1
	prevTerm, err := r.RaftLog.Term(prevIndex)
	if err != nil {
		return false
	}

	var entries []*pb.Entry
	if pr.Next <= r.RaftLog.LastIndex() {
		offset := r.RaftLog.entries[0].Index
		ents := r.RaftLog.entries[pr.Next-offset:]
		entries = make([]*pb.Entry, 0, len(ents))
		for i := range ents {
			ent := ents[i]
			entries = append(entries, &ent)
		}
	}

	r.msgs = append(r.msgs, pb.Message{
		MsgType: pb.MessageType_MsgAppend,
		From:    r.id,
		To:      to,
		Term:    r.Term,
		Index:   prevIndex,
		LogTerm: prevTerm,
		Commit:  r.RaftLog.committed,
		Entries: entries,
	})
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	commit := r.RaftLog.committed
	if pr, ok := r.Prs[to]; ok && pr.Match < commit {
		commit = pr.Match
	}
	r.msgs = append(r.msgs, pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		From:    r.id,
		To:      to,
		Term:    r.Term,
		Commit:  commit,
	})
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	switch r.State {
	case StateLeader:
		r.heartbeatElapsed++
		if r.heartbeatElapsed >= r.heartbeatTimeout {
			r.heartbeatElapsed = 0
			_ = r.Step(pb.Message{MsgType: pb.MessageType_MsgBeat})
		}
	default:
		r.electionElapsed++
		if r.electionElapsed >= r.randomizedElectionTimeout {
			r.electionElapsed = 0
			_ = r.Step(pb.Message{MsgType: pb.MessageType_MsgHup})
		}
	}
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	r.State = StateFollower
	if term > r.Term {
		r.Term = term
		r.Vote = None
	} else {
		r.Term = term
	}
	r.Lead = lead
	r.votes = nil
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.leadTransferee = None
	r.resetRandomizedElectionTimeout()
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	r.State = StateCandidate
	r.Term++
	r.Vote = r.id
	r.Lead = None
	r.votes = map[uint64]bool{r.id: true}
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.resetRandomizedElectionTimeout()
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	r.State = StateLeader
	r.Lead = r.id
	r.votes = nil
	r.electionElapsed = 0
	r.heartbeatElapsed = 0

	lastIndex := r.RaftLog.LastIndex()
	for id := range r.Prs {
		r.Prs[id].Match = 0
		r.Prs[id].Next = lastIndex + 1
	}
	if _, ok := r.Prs[r.id]; ok {
		r.Prs[r.id].Match = lastIndex
		r.Prs[r.id].Next = lastIndex + 1
	}

	noop := pb.Entry{Term: r.Term, Index: lastIndex + 1}
	r.RaftLog.entries = append(r.RaftLog.entries, noop)
	if _, ok := r.Prs[r.id]; ok {
		r.Prs[r.id].Match = noop.Index
		r.Prs[r.id].Next = noop.Index + 1
	}

	if len(r.Prs) == 1 {
		r.RaftLog.committed = noop.Index
		return
	}
	for id := range r.Prs {
		if id == r.id {
			continue
		}
		r.sendAppend(id)
	}
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	isLocalMsg := m.MsgType == pb.MessageType_MsgHup || m.MsgType == pb.MessageType_MsgBeat || m.MsgType == pb.MessageType_MsgPropose
	if !isLocalMsg {
		if m.Term > r.Term {
			r.becomeFollower(m.Term, None)
		}
		if m.Term < r.Term {
			switch m.MsgType {
			case pb.MessageType_MsgAppend:
				r.msgs = append(r.msgs, pb.Message{MsgType: pb.MessageType_MsgAppendResponse, From: r.id, To: m.From, Term: r.Term, Reject: true, Index: r.RaftLog.LastIndex()})
			case pb.MessageType_MsgHeartbeat:
				r.msgs = append(r.msgs, pb.Message{MsgType: pb.MessageType_MsgHeartbeatResponse, From: r.id, To: m.From, Term: r.Term, Reject: true})
			case pb.MessageType_MsgRequestVote:
				r.msgs = append(r.msgs, pb.Message{MsgType: pb.MessageType_MsgRequestVoteResponse, From: r.id, To: m.From, Term: r.Term, Reject: true})
			}
			return nil
		}
	}

	switch m.MsgType {
	case pb.MessageType_MsgHup:
		if r.State == StateLeader {
			return nil
		}
		r.becomeCandidate()
		if len(r.Prs) == 1 {
			r.becomeLeader()
			return nil
		}
		lastIndex := r.RaftLog.LastIndex()
		lastTerm, err := r.RaftLog.Term(lastIndex)
		if err != nil {
			return err
		}
		for id := range r.Prs {
			if id == r.id {
				continue
			}
			r.msgs = append(r.msgs, pb.Message{
				MsgType: pb.MessageType_MsgRequestVote,
				From:    r.id,
				To:      id,
				Term:    r.Term,
				Index:   lastIndex,
				LogTerm: lastTerm,
			})
		}
		return nil
	case pb.MessageType_MsgRequestVote:
		lastIndex := r.RaftLog.LastIndex()
		lastTerm, err := r.RaftLog.Term(lastIndex)
		if err != nil {
			return err
		}
		upToDate := m.LogTerm > lastTerm || (m.LogTerm == lastTerm && m.Index >= lastIndex)
		canVote := r.Vote == None || r.Vote == m.From
		reject := !(canVote && upToDate)
		if !reject {
			r.Vote = m.From
			r.electionElapsed = 0
		}
		r.msgs = append(r.msgs, pb.Message{MsgType: pb.MessageType_MsgRequestVoteResponse, From: r.id, To: m.From, Term: r.Term, Reject: reject})
		return nil
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
		return nil
	case pb.MessageType_MsgHeartbeat:
		r.handleHeartbeat(m)
		return nil
	}

	switch r.State {
	case StateFollower:
		switch m.MsgType {
		case pb.MessageType_MsgPropose:
			return ErrProposalDropped
		}
	case StateCandidate:
		switch m.MsgType {
		case pb.MessageType_MsgPropose:
			return ErrProposalDropped
		case pb.MessageType_MsgRequestVoteResponse:
			if r.votes == nil {
				r.votes = make(map[uint64]bool)
			}
			r.votes[m.From] = !m.Reject
			granted, rejected := 0, 0
			for _, v := range r.votes {
				if v {
					granted++
				} else {
					rejected++
				}
			}
			quorum := len(r.Prs)/2 + 1
			if granted >= quorum {
				r.becomeLeader()
			} else if rejected >= quorum {
				r.becomeFollower(r.Term, None)
			}
		}
	case StateLeader:
		switch m.MsgType {
		case pb.MessageType_MsgBeat:
			for id := range r.Prs {
				if id == r.id {
					continue
				}
				r.sendHeartbeat(id)
			}
		case pb.MessageType_MsgPropose:
			if len(m.Entries) == 0 {
				return nil
			}
			lastIndex := r.RaftLog.LastIndex()
			for _, ent := range m.Entries {
				lastIndex++
				r.RaftLog.entries = append(r.RaftLog.entries, pb.Entry{EntryType: ent.EntryType, Term: r.Term, Index: lastIndex, Data: ent.Data})
			}
			if _, ok := r.Prs[r.id]; ok {
				r.Prs[r.id].Match = lastIndex
				r.Prs[r.id].Next = lastIndex + 1
			}
			if len(r.Prs) == 1 {
				r.RaftLog.committed = lastIndex
				return nil
			}
			for id := range r.Prs {
				if id == r.id {
					continue
				}
				r.sendAppend(id)
			}
		case pb.MessageType_MsgAppendResponse:
			pr, ok := r.Prs[m.From]
			if !ok {
				return nil
			}
			if m.Reject {
				if pr.Next > 1 {
					pr.Next--
				}
				r.sendAppend(m.From)
				return nil
			}
			if m.Index > pr.Match {
				pr.Match = m.Index
			}
			pr.Next = pr.Match + 1

			matched := make([]uint64, 0, len(r.Prs))
			for _, p := range r.Prs {
				matched = append(matched, p.Match)
			}
			sort.Slice(matched, func(i, j int) bool { return matched[i] > matched[j] })
			newCommit := matched[len(matched)/2]
			if newCommit > r.RaftLog.committed {
				term, err := r.RaftLog.Term(newCommit)
				if err == nil && term == r.Term {
					r.RaftLog.committed = newCommit
					for id := range r.Prs {
						if id == r.id {
							continue
						}
						r.sendAppend(id)
					}
				}
			}
		case pb.MessageType_MsgHeartbeatResponse:
			r.sendAppend(m.From)
		case pb.MessageType_MsgAppend:
			r.becomeFollower(m.Term, m.From)
			r.handleAppendEntries(m)
		case pb.MessageType_MsgHeartbeat:
			r.becomeFollower(m.Term, m.From)
			r.handleHeartbeat(m)
		}
	}
	return nil
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	if m.Term < r.Term {
		r.msgs = append(r.msgs, pb.Message{MsgType: pb.MessageType_MsgAppendResponse, From: r.id, To: m.From, Term: r.Term, Reject: true, Index: r.RaftLog.LastIndex()})
		return
	}

	r.becomeFollower(m.Term, m.From)
	r.electionElapsed = 0

	if m.Index > r.RaftLog.LastIndex() {
		r.msgs = append(r.msgs, pb.Message{MsgType: pb.MessageType_MsgAppendResponse, From: r.id, To: m.From, Term: r.Term, Reject: true, Index: r.RaftLog.LastIndex()})
		return
	}
	term, err := r.RaftLog.Term(m.Index)
	if err != nil || term != m.LogTerm {
		r.msgs = append(r.msgs, pb.Message{MsgType: pb.MessageType_MsgAppendResponse, From: r.id, To: m.From, Term: r.Term, Reject: true, Index: m.Index - 1})
		return
	}

	for i, ent := range m.Entries {
		if ent.Index <= r.RaftLog.LastIndex() {
			t, _ := r.RaftLog.Term(ent.Index)
			if t != ent.Term {
				offset := r.RaftLog.entries[0].Index
				r.RaftLog.entries = r.RaftLog.entries[:ent.Index-offset]
				if r.RaftLog.stabled >= ent.Index {
					r.RaftLog.stabled = ent.Index - 1
				}
				for j := i; j < len(m.Entries); j++ {
					r.RaftLog.entries = append(r.RaftLog.entries, *m.Entries[j])
				}
				break
			}
			continue
		}
		for j := i; j < len(m.Entries); j++ {
			r.RaftLog.entries = append(r.RaftLog.entries, *m.Entries[j])
		}
		break
	}

	if m.Commit > r.RaftLog.committed {
		lastNewIndex := m.Index + uint64(len(m.Entries))
		r.RaftLog.committed = min(m.Commit, lastNewIndex)
	}
	r.msgs = append(r.msgs, pb.Message{MsgType: pb.MessageType_MsgAppendResponse, From: r.id, To: m.From, Term: r.Term, Index: r.RaftLog.LastIndex()})
}

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	if m.Term < r.Term {
		r.msgs = append(r.msgs, pb.Message{MsgType: pb.MessageType_MsgHeartbeatResponse, From: r.id, To: m.From, Term: r.Term, Reject: true})
		return
	}
	r.becomeFollower(m.Term, m.From)
	r.electionElapsed = 0
	if m.Commit > r.RaftLog.committed {
		r.RaftLog.committed = min(m.Commit, r.RaftLog.LastIndex())
	}
	r.msgs = append(r.msgs, pb.Message{MsgType: pb.MessageType_MsgHeartbeatResponse, From: r.id, To: m.From, Term: r.Term})
}

func (r *Raft) resetRandomizedElectionTimeout() {
	r.randomizedElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
}
