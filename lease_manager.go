package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"

	"github.com/psanford/dhcpeterd/internal/dhcp4d"
)

type leaseManager struct {
	path string
	lf   *LeaseFile

	leaseUpdate chan LeaseUpdate
}

func newLeaseManager(p string) *leaseManager {
	lm := leaseManager{
		path:        p,
		leaseUpdate: make(chan LeaseUpdate),
		lf: &LeaseFile{
			LeaseByInterface: make(map[string][]dhcp4d.Lease),
		},
	}

	if p == "" {
		return &lm
	}

	b, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("read lease file err", "err", err)
		}
		return &lm
	}

	var lf LeaseFile
	err = json.Unmarshal(b, &lf)
	if err != nil {
		slog.Error("parse lease file json err", "err", err)
		return &lm
	}
	lm.lf = &lf

	return &lm
}

func (lm *leaseManager) updateLeaseFileLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-lm.leaseUpdate:
			lm.lf.LeaseByInterface[update.IfaceName] = update.Leases
			if lm.path == "" {
				continue
			}
			b, err := json.Marshal(lm.lf)
			if err != nil {
				slog.Error("marshal lease file err", "err", err)
				continue
			}
			os.WriteFile(lm.path, b, 0600)
		}
	}
}

type LeaseFile struct {
	LeaseByInterface map[string][]dhcp4d.Lease `json:"lease_by_interface"`
}

type LeaseUpdate struct {
	IfaceName string
	Leases    []dhcp4d.Lease
}
