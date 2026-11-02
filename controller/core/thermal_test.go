package core

import (
	"encoding/json"
	"testing"

	"aicluster/common/proto"
	"aicluster/controller/store"
)

func TestIsHot(t *testing.T) {
	node := func(h proto.Health) *store.Node {
		b, _ := json.Marshal(h)
		return &store.Node{Health: b}
	}
	for name, tc := range map[string]struct {
		h   proto.Health
		hot bool
	}{
		"old agent":      {proto.Health{}, false},
		"cool":           {proto.Health{Thermal: &proto.Thermal{CPUTempC: 60}}, false},
		"cpu throttling": {proto.Health{Thermal: &proto.Thermal{CPUTempC: 80, Throttled: true}}, true},
		"cpu very hot":   {proto.Health{Thermal: &proto.Thermal{CPUTempC: 96}}, true},
		"gpu throttling": {proto.Health{Thermal: &proto.Thermal{CPUTempC: 50}, GPUs: []proto.GPUHealth{{Throttled: true}}}, true},
	} {
		if got := isHot(node(tc.h)); got != tc.hot {
			t.Errorf("%s: isHot = %v", name, got)
		}
	}
	if isHot(&store.Node{}) {
		t.Error("a node without health is hot")
	}
}
