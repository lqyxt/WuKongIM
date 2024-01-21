package replica

import "time"

type Options struct {
	NodeID          uint64     // 当前节点ID
	ShardNo         string     // 分区编号
	Replicas        []uint64   // 副本节点ID集合
	Transport       ITransport // 传输协议
	SyncLimit       uint32
	CheckInterval   time.Duration                                // 检测间隔
	AppliedIndex    uint64                                       // 已应用的日志下标
	CommitLimit     uint32                                       // 每次提交日志的大小限制
	OnApply         func(logs []Log) (applied uint64, err error) // 应用日志
	OnCommit        func(oldCommittedIndex, newCommitted uint64) // 提交事件
	Storage         IStorage
	LastSyncInfoMap map[uint64]SyncInfo // 各个副本最后一次来同步日志的下标
	CurrentTerm     uint32              // 副本当前任期
	ProposeTimeout  time.Duration       // 提议超时时间
}

func NewOptions() *Options {
	return &Options{
		SyncLimit:       20,
		CheckInterval:   time.Millisecond * 500,
		CommitLimit:     20,
		LastSyncInfoMap: make(map[uint64]SyncInfo),
		CurrentTerm:     1,
		ProposeTimeout:  time.Second * 5,
	}
}

type Option func(o *Options)

func WithNodeID(nodeID uint64) Option {

	return func(o *Options) {
		o.NodeID = nodeID
	}
}

func WithReplicas(replicas []uint64) Option {

	return func(o *Options) {
		o.Replicas = replicas
	}
}

func WithTransport(t ITransport) Option {
	return func(o *Options) {
		o.Transport = newProxyTransport(t)
	}
}

func WithAppliedIndex(appliedIndex uint64) Option {
	return func(o *Options) {
		o.AppliedIndex = appliedIndex
	}
}

func WithOnApply(onApply func(logs []Log) (uint64, error)) Option {

	return func(o *Options) {
		o.OnApply = onApply
	}
}

func WithStorage(storage IStorage) Option {
	return func(o *Options) {
		o.Storage = storage
	}
}

func WithLastSyncInfoMap(lastSyncInfoMap map[uint64]SyncInfo) Option {

	return func(o *Options) {
		o.LastSyncInfoMap = lastSyncInfoMap
	}
}

func WithCurrentTerm(currentTerm uint32) Option {

	return func(o *Options) {
		o.CurrentTerm = currentTerm
	}
}

func WithOnCommit(onCommit func(oldCommittedIndex, newCommitted uint64)) Option {
	return func(o *Options) {
		o.OnCommit = onCommit
	}
}
