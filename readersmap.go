package main

import (
	"sync"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/base"
)

type Reader interface {
	OnReaderFrame(trackId int, streamType base.StreamType, buf []byte)
}

type ReadersMap struct {
	mutex sync.RWMutex
	ma    map[Reader]struct{}
}

func NewReadersMap() *ReadersMap {
	return &ReadersMap{
		ma: make(map[Reader]struct{}),
	}
}

func (m *ReadersMap) Add(reader Reader) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.ma[reader] = struct{}{}
}

func (m *ReadersMap) Remove(reader Reader) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	delete(m.ma, reader)
}

func (m *ReadersMap) ForwardFrame(trackId int, streamType gortsplib.StreamType, buf []byte) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	for c := range m.ma {
		c.OnReaderFrame(trackId, streamType, buf)
	}
}
