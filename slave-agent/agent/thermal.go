package agent

import (
	"context"
	"sync"
	"time"

	"aicluster/common/proto"
	"aicluster/slave-agent/thermal"
)

// Temperatures and fans (protocol §15). The loop samples the sensors every
// few seconds for the heartbeat and, when the Controller asks for boost,
// drives the fans the OS can control. Leaving boost, or stopping, hands
// them back to the firmware.

const thermalEvery = 3 * time.Second

type thermState struct {
	mu   sync.Mutex
	last *proto.Thermal
	gpus []proto.GPUHealth
}

func (a *Agent) monitor() *thermal.Monitor {
	if a.cfg.Thermal == nil {
		a.cfg.Thermal = &thermal.Monitor{}
	}
	return a.cfg.Thermal
}

// latestThermal is the newest sample, for the heartbeat.
func (a *Agent) latestThermal() (*proto.Thermal, []proto.GPUHealth) {
	a.therm.mu.Lock()
	defer a.therm.mu.Unlock()
	return a.therm.last, a.therm.gpus
}

func (a *Agent) thermalLoop(ctx context.Context) {
	mon := a.monitor()
	defer a.cfg.Fans.Restore()
	for {
		t, gpus := mon.Read(a.cfg.Fans)
		a.therm.mu.Lock()
		a.therm.last, a.therm.gpus = t, gpus
		a.therm.mu.Unlock()
		if a.cfg.Fans != nil {
			a.mu.RLock()
			mode := proto.FansFirmware
			if a.adopted != nil && a.desired.Fans != nil {
				mode = a.desired.Fans.Mode
			}
			a.mu.RUnlock()
			a.cfg.Fans.Apply(mon, mode, t)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(thermalEvery):
		}
	}
}
