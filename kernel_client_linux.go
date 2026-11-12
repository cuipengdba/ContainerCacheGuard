//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"time"
)

type kernelClient struct {
	requests chan kernelHeartbeat
	results  chan KernelStatus
	status   KernelStatus
}

func newKernelClient(ctx context.Context, c Config) *kernelClient {
	if !c.KernelReclaim.Enabled {
		return nil
	}
	k := &kernelClient{requests: make(chan kernelHeartbeat, 1), results: make(chan KernelStatus, 1), status: KernelStatus{Phase: "UNAVAILABLE", Reason: "waiting_agent", HoldFile: true}}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-k.requests:
				s := KernelStatus{Phase: "UNAVAILABLE", Reason: "agent_unreachable_or_invalid", Time: time.Now(), HoldFile: true}
				conn, e := net.DialTimeout("unix", c.KernelReclaim.Socket, 150*time.Millisecond)
				if e == nil {
					conn.SetDeadline(time.Now().Add(250 * time.Millisecond))
					cred, err := kernelPeer(conn.(*net.UnixConn))
					if err == nil && (cred.Uid == 0 || cred.Uid == uint32(os.Geteuid())) {
						if json.NewEncoder(conn).Encode(req) == nil {
							var received KernelStatus
							dec := json.NewDecoder(io.LimitReader(conn, 256<<10))
							if dec.Decode(&received) == nil && freshKernel(received.Time, time.Now(), 3*time.Second) {
								s = received
							}
						}
					}
					conn.Close()
				}
				select {
				case k.results <- s:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return k
}
func (k *kernelClient) sample(c Config, paused, fileIdle bool, now time.Time) *KernelStatus {
	if k == nil {
		return nil
	}
	select {
	case s := <-k.results:
		k.status = s
	default:
	}
	if !freshKernel(k.status.Time, now, 3*time.Second) {
		k.status = KernelStatus{Phase: "UNAVAILABLE", Reason: "agent_status_stale", Time: now, HoldFile: true}
	}
	ack := ""
	if k.status.HoldFile && fileIdle {
		ack = k.status.Episode
	}
	select {
	case k.requests <- kernelHeartbeat{c.KernelReclaim.Target, c.Action, paused, ack, fileIdle}:
	default:
	}
	copy := k.status
	return &copy
}
