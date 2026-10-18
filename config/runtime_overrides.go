package config

// SetPort overrides the HTTP listen port in memory without persisting it.
func SetPort(port int) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg != nil {
		cfg.Port = port
	}
}

// SetHost overrides the HTTP bind host in memory without persisting it.
func SetHost(host string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg != nil {
		cfg.Host = host
	}
}
