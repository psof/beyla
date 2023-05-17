package ebpf

import (
	"encoding/binary"
	"fmt"
	"io"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	ebpfcommon "github.com/grafana/ebpf-autoinstrument/pkg/ebpf/common"
	"github.com/grafana/ebpf-autoinstrument/pkg/goexec"
	"golang.org/x/exp/slog"
	"golang.org/x/sys/unix"
)

type instrumenter struct {
	offsets   *goexec.Offsets
	exe       *link.Executable
	closables []io.Closer
}

func (i *instrumenter) goprobes(p Tracer) error {
	// TODO: not running program if it does not find the required probes
	for funcName, funcPrograms := range p.GoProbes() {
		offs, ok := i.offsets.Funcs[funcName]
		if !ok {
			// the program function is not in the detected offsets. Ignoring
			continue
		}
		slog.Debug("going to instrument function", "function", funcName, "offsets", offs, "programs", funcPrograms)
		if err := i.goprobe(ebpfcommon.Probe{
			Offsets:  offs,
			Programs: funcPrograms,
		}); err != nil {
			return fmt.Errorf("instrumenting function %q: %w", funcName, err)
		}
		p.AddCloser(i.closables...)
	}

	return nil
}

func (i *instrumenter) goprobe(probe ebpfcommon.Probe) error {
	// Attach BPF programs as start and return probes
	if probe.Programs.Start != nil {
		up, err := i.exe.Uprobe("", probe.Programs.Start, &link.UprobeOptions{
			Address: probe.Offsets.Start,
		})
		if err != nil {
			return fmt.Errorf("setting uprobe: %w", err)
		}
		i.closables = append(i.closables, up)
	}

	if probe.Programs.End != nil {
		// Go won't work with Uretprobes because of the way Go manages the stack. We need to set uprobes just before the return
		// values: https://github.com/iovisor/bcc/issues/1320
		for _, ret := range probe.Offsets.Returns {
			urp, err := i.exe.Uprobe("", probe.Programs.End, &link.UprobeOptions{
				Address: ret,
			})
			if err != nil {
				return fmt.Errorf("setting uretprobe: %w", err)
			}
			i.closables = append(i.closables, urp)
		}
	}

	return nil
}

func (i *instrumenter) kprobes(p Tracer) error {
	for kfunc, kprobes := range p.KProbes() {
		slog.Debug("going to add kprobe to function", "function", kfunc, "probes", kprobes)

		if err := i.kprobe(kfunc, kprobes); err != nil {
			return fmt.Errorf("instrumenting function %q: %w", kfunc, err)
		}
		p.AddCloser(i.closables...)
	}

	return nil
}

func (i *instrumenter) kprobe(funcName string, programs ebpfcommon.FunctionPrograms) error {
	if programs.Start != nil {
		kp, err := link.Kprobe(funcName, programs.Start, nil)
		if err != nil {
			return fmt.Errorf("setting kprobe: %w", err)
		}
		i.closables = append(i.closables, kp)
	}

	if programs.End != nil {
		kp, err := link.Kretprobe(funcName, programs.End, nil)
		if err != nil {
			return fmt.Errorf("setting kretprobe: %w", err)
		}
		i.closables = append(i.closables, kp)
	}

	return nil
}

func attachSocketFilter(filter *ebpf.Program) (int, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err == nil {
		ssoErr := syscall.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_BPF, filter.FD())
		if ssoErr != nil {
			return -1, ssoErr
		}
		return int(fd), nil
	}

	return -1, err
}

func htons(a uint16) uint16 {
	var arr [2]byte
	binary.LittleEndian.PutUint16(arr[:], a)
	return binary.BigEndian.Uint16(arr[:])
}

func (i *instrumenter) sockfilters(p Tracer) error {
	for _, filter := range p.SocketFilters() {
		fd, err := attachSocketFilter(filter)
		if err != nil {
			return fmt.Errorf("attaching socket filter: %w", err)
		}

		p.AddCloser(&ebpfcommon.Filter{Fd: fd})
	}

	return nil
}
