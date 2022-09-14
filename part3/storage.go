package raft

type Storage interface {
	Set(key string, value []byte)
	Get(key string) ([]byte, bool)

	HashData() bool
}
