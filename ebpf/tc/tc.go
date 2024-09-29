package ebpf_tc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/cilium/ebpf/perf"
	"github.com/k8spacket/k8spacket/broker"
	ebpf_tools "github.com/k8spacket/k8spacket/ebpf/tools"
	"github.com/k8spacket/k8spacket/modules"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package ebpf_tc tc ./bpf/tc.bpf.c

type TcEbpf struct {
	Broker broker.IBroker
}

func (tcEbpf *TcEbpf) Init(iface string) {

	// Load pre-compiled programs and maps into the kernel.
	objs := tcObjects{}
	if err := loadTcObjects(&objs, nil); err != nil {
		slog.Error("[tc] Loading objects", "Error", err)
	}
	defer objs.Close()

	// get the file descriptor of the tc_filter program
	progFd := objs.tcPrograms.TcFilter.FD()

	// get link device by name (network interface name)
	link, err := netlink.LinkByName(iface)
	if err != nil {
		slog.Error("[tc] Cannot find network intefrace", "interface", iface, "Error", err)
	}

	// qdisc clsact - queueing discipline (qdisc) parent of ingress and egress filters
	attrs := netlink.QdiscAttrs{
		LinkIndex: link.Attrs().Index,
		Handle:    netlink.MakeHandle(0xffff, 0),
		Parent:    netlink.HANDLE_CLSACT,
	}

	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: attrs,
		QdiscType:  "clsact",
	}

	// try to delete previous added clsact qdisc on specific network interface, equivalent `tc qdisc del dev {{iface}} clsact`
	if err := netlink.QdiscDel(qdisc); err != nil {
		slog.Error("[tc] Cannot del clsact qdisc", "Error", err)
	}

	// add clsact qdisc on specific network interface, equivalent `tc qdisc add dev {{iface}} clsact`
	// check `qdisc show dev {{iface}}`
	if err := netlink.QdiscAdd(qdisc); err != nil {
		slog.Error("[tc] Cannot add clsact qdisc", "Error", err)
	}

	// add ingress filter
	addFilter(link, progFd, netlink.HANDLE_MIN_INGRESS)

	// add egress filter
	addFilter(link, progFd, netlink.HANDLE_MIN_EGRESS)

	// create new reader for ringbuf events
	rd, err := perf.NewReader(objs.OutputEvents, os.Getpagesize())
	if err != nil {
		slog.Error("[tc] Creating perf event reader", "Error", err)
	}
	defer rd.Close()

	go func() {
		// tcTlsHandshakeEvent is generated by bpf2go and represents ringbuf event type in eBPF program
		var event tcTlsHandshakeEvent
		for {
			record, err := rd.Read()
			if err != nil {
				if errors.Is(err, perf.ErrClosed) {
					slog.Info("[tc] Received signal, exiting..")
					return
				}
				slog.Error("[tc] Reading from reader", "Error", err)
				continue
			}

			// Parse the ringbuf event into a tcTlsHandshakeEvent structure.
			if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.BigEndian, &event); err != nil {
				slog.Error("[tc] Parsing ringbuf event", "Error", err)
				continue
			}

			distribute(event, tcEbpf)
		}
	}()

	// graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()

	slog.Info("[tc] Closed gracefully")
}

func addFilter(link netlink.Link, programFD int, parent uint32) {

	// filter attrs
	filterAttrs := netlink.FilterAttrs{
		LinkIndex: link.Attrs().Index,
		Parent:    parent,
		Handle:    netlink.MakeHandle(0, 1),
		Protocol:  unix.ETH_P_ALL,
		Priority:  1,
	}

	// bpf filter struct
	filter := &netlink.BpfFilter{
		FilterAttrs:  filterAttrs,
		Fd:           programFD,
		Name:         "tc",
		DirectAction: true,
	}

	// add ingress/egress filter, equivalent `tc filter add dev {{iface}} [ingress|egress]`
	// check `tc filter show dev {{iface}} [ingress|egress]`
	if err := netlink.FilterAdd(filter); err != nil {
		slog.Error("[tc] Cannot attach bpf object to filter", "Error", err)
	}
}

func distribute(event tcTlsHandshakeEvent, tc *TcEbpf) {

	tlsVersionsLen := int(event.TlsVersionsLength) / 2
	if tlsVersionsLen > len(event.TlsVersions) {
		tlsVersionsLen = len(event.TlsVersions)
	}

	ciphersLen := int(event.CiphersLength) / 2
	if ciphersLen > len(event.Ciphers) {
		ciphersLen = len(event.Ciphers)
	}

	serverNameLen := int(event.ServerNameLength)
	if serverNameLen > len(event.ServerName) {
		serverNameLen = len(event.ServerName)
	}

	tlsEvent := modules.TLSEvent{
		Client: modules.Address{
			Addr: intToIP4(event.Saddr),
			Port: event.Sport},
		Server: modules.Address{
			Addr: intToIP4(event.Daddr),
			Port: event.Dport},
		TlsVersions:    event.TlsVersions[:tlsVersionsLen],
		Ciphers:        event.Ciphers[:ciphersLen],
		ServerName:     string(event.ServerName[:serverNameLen]),
		UsedTlsVersion: event.UsedTlsVersion,
		UsedCipher:     event.UsedCipher}
	if len(tlsEvent.TlsVersions) <= 0 {
		tlsEvent.TlsVersions = append(tlsEvent.TlsVersions, event.TlsVersion)
	}

	ebpf_tools.EnrichAddress(&tlsEvent.Client)
	ebpf_tools.EnrichAddress(&tlsEvent.Server)
	tc.Broker.TLSEvent(tlsEvent)
}

func intToIP4(ipNum uint32) string {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, ipNum)
	return ip.String()
}
