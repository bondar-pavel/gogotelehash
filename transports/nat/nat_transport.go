package nat

import (
	"fmt"
	"net"
	"sync"
	"time"

	"bitbucket.org/simonmenke/go-telehash/transports"
	"bitbucket.org/simonmenke/go-telehash/util/nat"
)

var (
	_ transports.Transport = (*transport)(nil)
	_ transports.Config    = Config{}
)

type NATableAddr interface {
	transports.Addr
	InternalAddr() (proto string, ip net.IP, port int)
	MakeGlobal(ip net.IP, port int) transports.Addr
}

type Config struct {
	Config transports.Config
}

type transport struct {
	t    transports.Transport
	nat  nat.NAT
	done chan struct{}

	mtx     sync.RWMutex
	mapping map[string]*natMapping
}

type natMapping struct {
	external transports.Addr
	internal transports.Addr
	stale    bool
}

func (c Config) Open() (transports.Transport, error) {
	t, err := c.Config.Open()
	if err != nil {
		return nil, err
	}

	nat := &transport{
		t:       t,
		mapping: make(map[string]*natMapping),
		done:    make(chan struct{}),
	}

	go nat.runMapper()

	return nat, nil
}

func (t *transport) LocalAddresses() []transports.Addr {
	addrs := t.t.LocalAddresses()

	t.mtx.RLock()
	for _, m := range t.mapping {
		addrs = append(addrs, m.external)
	}
	t.mtx.RUnlock()

	return addrs
}

func (t *transport) ReadMessage(p []byte) (int, transports.Addr, error) {
	return t.t.ReadMessage(p)
}

func (t *transport) WriteMessage(p []byte, dst transports.Addr) error {
	return t.t.WriteMessage(p, dst)
}

func (t *transport) Close() error {
	select {
	case <-t.done: // is closed
	default: // is opened
		close(t.done)
	}

	return t.t.Close()
}

func (t *transport) runMapper() {
	var closed bool
	for !closed {
		if t.nat == nil {
			closed = t.runDiscoverMode()
		} else {
			closed = t.runMappingMode()
		}
	}
}

func (t *transport) runDiscoverMode() bool {
	var discoverTicker = time.NewTicker(10 * time.Minute)
	defer discoverTicker.Stop()

	var updateTicker = time.NewTicker(5 * time.Second)
	defer updateTicker.Stop()

	var knownAddrs = make(map[string]bool)

	for {
		select {

		case <-t.done:
			return true // done

		case <-updateTicker.C:
			changed := t.updateKnownAddresses(knownAddrs)
			if changed {
				t.discoverNAT()
			}

		case <-discoverTicker.C:
			t.discoverNAT()

		}

		if t.nat != nil {
			return false // not done
		}
	}

	panic("unreachable")
}

func asNATableAddr(addr transports.Addr) (string, net.IP, int) {
	naddr, _ := addr.(NATableAddr)
	if naddr == nil {
		return "", nil, 0
	}

	proto, ip, port := naddr.InternalAddr()
	if proto == "" || ip == nil || port <= 0 {
		return "", nil, 0
	}

	return proto, ip, port
}

func (t *transport) updateKnownAddresses(known map[string]bool) bool {
	var (
		changed bool
	)

	for key := range known {
		known[key] = false
	}

	for _, addr := range t.t.LocalAddresses() {
		proto, ip, internal_port := asNATableAddr(addr)
		if proto == "" {
			continue
		}

		key := mappingKey(proto, ip, internal_port)

		if _, found := known[key]; !found {
			changed = true
		}

		known[key] = true
	}

	for key, ok := range known {
		if !ok {
			delete(known, key)
			changed = true
		}
	}

	return changed
}

func (t *transport) runMappingMode() bool {
	var refreshTicker = time.NewTicker(50 * time.Minute)
	defer refreshTicker.Stop()

	var updateTicker = time.NewTicker(5 * time.Second)
	defer updateTicker.Stop()

	for {
		select {

		case <-t.done:
			t.mapping = make(map[string]*natMapping)
			return true // done

		case <-refreshTicker.C:
			t.refreshMapping()

		case <-updateTicker.C:
			t.updateMappings()

		}

		if t.nat == nil {
			t.mapping = make(map[string]*natMapping)
			return false // not done
		}
	}

	panic("unreachable")
}

func (t *transport) discoverNAT() {
	nat, err := nat.Discover()
	if err != nil {
		return
	}

	_, err = nat.GetDeviceAddress()
	if err != nil {
		return
	}

	t.nat = nat
}

func (t *transport) updateMappings() {
	var (
		mapping map[string]*natMapping
	)

	t.mtx.Lock()
	mapping = make(map[string]*natMapping, len(t.mapping))
	for k, v := range t.mapping {
		mapping[k] = v
		v.stale = true
	}
	t.mtx.Unlock()

	external_ip, err := t.nat.GetExternalAddress()
	if err != nil {
		t.nat = nil
		return
	}

	internal_ip, err := t.nat.GetInternalAddress()
	if err != nil {
		t.nat = nil
		return
	}

	// map new addrs
	for _, addr := range t.t.LocalAddresses() {
		nataddr, ok := addr.(NATableAddr)
		if !ok {
			continue // not a natble address
		}

		proto, ip, internal_port := nataddr.InternalAddr()
		if proto == "" || ip == nil || internal_port <= 0 {
			continue // not a natble address
		}

		if !ip.Equal(internal_ip) {
			continue // not a natble address
		}

		key := mappingKey(proto, ip, internal_port)
		if m := t.mapping[key]; m != nil {
			m.stale = false
			continue // Already exists
		}

		external_port, err := t.nat.AddPortMapping(proto, internal_port, "Telehash", 60*time.Minute)
		if err != nil {
			continue // unable to map address
		}

		globaddr := nataddr.MakeGlobal(external_ip, external_port)
		if globaddr == nil {
			continue // unable to map address
		}

		mapping[key] = &natMapping{external: globaddr, internal: addr, stale: false}
	}

	for key, m := range mapping {
		if !m.stale {
			continue
		}

		nataddr, ok := m.internal.(NATableAddr)
		if !ok {
			continue
		}

		proto, _, internal_port := nataddr.InternalAddr()
		if proto == "" || internal_port <= 0 {
			continue
		}

		t.nat.DeletePortMapping(proto, internal_port)
		delete(mapping, key)
	}

	t.mtx.Lock()
	t.mapping = mapping
	t.mtx.Unlock()
}

func (t *transport) refreshMapping() {
	var (
		droplist []string
		mapping  map[string]*natMapping
	)

	t.mtx.Lock()
	mapping = make(map[string]*natMapping, len(t.mapping))
	for k, v := range t.mapping {
		mapping[k] = v
		v.stale = true
	}
	t.mtx.Unlock()

	external_ip, err := t.nat.GetExternalAddress()
	if err != nil {
		t.nat = nil
		return
	}

	internal_ip, err := t.nat.GetInternalAddress()
	if err != nil {
		t.nat = nil
		return
	}

	// remap addrs
	for key, m := range mapping {
		proto, ip, internal_port := asNATableAddr(m.internal)
		if proto == "" {
			droplist = append(droplist, key)
			continue
		}

		// did our internal ip change?
		if !ip.Equal(internal_ip) {
			droplist = append(droplist, key)
			continue
		}

		external_port, err := t.nat.AddPortMapping(proto, internal_port, "Telehash", 60*time.Minute)
		if err != nil {
			droplist = append(droplist, key)
			continue
		}

		globaddr := m.internal.(NATableAddr).MakeGlobal(external_ip, external_port)
		if globaddr == nil {
			droplist = append(droplist, key)
			continue
		}

		m.external = globaddr
	}

	for _, key := range droplist {
		delete(mapping, key)
	}

	t.mtx.Lock()
	t.mapping = mapping
	t.mtx.Unlock()
}

func mappingKey(proto string, ip net.IP, internal_port int) string {
	return fmt.Sprintf("%s:%s:%d", proto, ip, internal_port)
}