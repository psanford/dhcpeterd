package config

import (
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Networks  []Network `toml:"networks"`
	LeaseFile string    `toml:"lease_file"`
}

type Network struct {
	Interface     string        `toml:"interface"`
	StartIP       string        `toml:"start_ip"`
	Range         int           `toml:"range"`
	NetMask       string        `toml:"net_mask"`
	LeaseDuration time.Duration `toml:"lease_duration"`
	StaticLeases  []StaticLease `toml:"static_leases"`
	DNSServers    []string      `toml:"dns_servers"`
}

type StaticLease struct {
	MacAddress string `toml:"mac"`
	Name       string `toml:"name"`
	IP         string `toml:"ip"`
}

func Load(path string) (*Config, error) {
	tml, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var conf Config
	err = toml.Unmarshal(tml, &conf)
	if err != nil {
		return nil, err
	}

	return &conf, nil
}
