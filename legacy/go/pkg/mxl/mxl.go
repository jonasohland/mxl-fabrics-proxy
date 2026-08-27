package mxl

import (
	"encoding/binary"
	"path/filepath"

	"golang.org/x/exp/mmap"
)

const (
	FlowDataOffset_Version       = 0
	FlowDataOffset_Size          = 4
	FlowDataOffset_ConfigInfo    = FlowDataOffset_Size + 4
	FlowDataOffset_RuntimeInfo   = FlowDataOffset_ConfigInfo + 128 + 64
	FlowDataOffset_HeadIndex     = FlowDataOffset_RuntimeInfo
	FlowDataOffset_LastWriteTime = FlowDataOffset_HeadIndex + 8
	FlowDataOffset_LastReadTime  = FlowDataOffset_LastWriteTime + 8
)

type Flow struct {
	data *mmap.ReaderAt
}

func OpenFlow(domain string, id string) (*Flow, error) {
	data, err := mmap.Open(filepath.Join(domain, id+".mxl-flow", "data"))
	if err != nil {
		return nil, err
	}

	return &Flow{data}, nil
}

func (c *Flow) Close() {
	_ = c.data.Close()
}

func (c *Flow) readDataAt(l int, off int64) ([]byte, error) {
	buf := make([]byte, l)
	if _, err := c.data.ReadAt(buf, off); err != nil {
		return nil, err
	}

	return buf, nil
}

func (c *Flow) HeadIndex() (uint64, error) {
	buf, err := c.readDataAt(binary.Size(uint64(0)), FlowDataOffset_HeadIndex)
	if err != nil {
		return 0, err
	}

	return binary.NativeEndian.Uint64(buf), nil
}

func (c *Flow) LastReadTime() (uint64, error) {
	buf, err := c.readDataAt(binary.Size(uint64(0)), FlowDataOffset_LastReadTime)
	if err != nil {
		return 0, err
	}

	return binary.NativeEndian.Uint64(buf), nil
}

func (c *Flow) LastWriteTime() (uint64, error) {
	buf, err := c.readDataAt(binary.Size(uint64(0)), FlowDataOffset_LastWriteTime)
	if err != nil {
		return 0, err
	}

	return binary.NativeEndian.Uint64(buf), nil
}

type StateTracker struct {
	lastHeadIndex uint64
	lastReadTime  uint64

	isActive  bool
	hasReader bool
}

func (st *StateTracker) Observe(flow *Flow) error {
	rt, err := flow.LastReadTime()
	if err != nil {
		return err
	}

	hi, err := flow.HeadIndex()
	if err != nil {
		return err
	}

	st.isActive = st.lastHeadIndex < hi
	st.hasReader = st.lastReadTime < rt
	st.lastHeadIndex = hi
	st.lastReadTime = rt

	return nil
}

func (st *StateTracker) IsActive() bool {
	return st.isActive
}

func (st *StateTracker) HasReader() bool {
	return st.hasReader
}
