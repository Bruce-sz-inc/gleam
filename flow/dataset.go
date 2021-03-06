package flow

import (
	"fmt"
	"time"

	"github.com/chrislusf/gleam/util"
)

func newDataset(context *FlowContext) *Dataset {
	d := &Dataset{
		Id:          len(context.Datasets),
		FlowContext: context,
		Meta:        &DasetsetMetadata{TotalSize: -1},
	}
	context.Datasets = append(context.Datasets, d)
	return d
}

func (d *Dataset) GetShards() []*DatasetShard {
	return d.Shards
}

func (d *Dataset) Script(scriptType string) *Dataset {
	d.FlowContext.Script(scriptType)
	return d
}

func (d *Dataset) Init(scriptPart string) *Dataset {
	d.FlowContext.Init(scriptPart)
	return d
}

// Run starts the whole flow. This is a convenient method, same as *FlowContext.Run()
func (d *Dataset) Run(option ...FlowOption) {
	if len(option) == 0 {
		Local.RunFlowContext(d.FlowContext)
	} else {
		option[0].GetFlowRunner().RunFlowContext(d.FlowContext)
	}
}

func (d *Dataset) setupShard(n int) {
	for i := 0; i < n; i++ {
		ds := &DatasetShard{
			Id:           i,
			Dataset:      d,
			IncomingChan: util.NewPiper(),
		}
		d.Shards = append(d.Shards, ds)
	}
}

func (s *DatasetShard) Closed() bool {
	return !s.CloseTime.IsZero()
}

func (s *DatasetShard) TimeTaken() time.Duration {
	if s.Closed() {
		return s.CloseTime.Sub(s.ReadyTime)
	}
	return time.Now().Sub(s.ReadyTime)
}

func (s *DatasetShard) Name() string {
	return fmt.Sprintf("f%d-d%d-s%d", s.Dataset.FlowContext.HashCode, s.Dataset.Id, s.Id)
}
